// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/bborbe/errors"
)

// tokenStateFileName is the token store's file name under the controller's state directory.
// It is a sibling of the local backend's ownership store, so one controller keeps all of its
// state in one directory.
const tokenStateFileName = "docker-workers.json" //nolint:gosec // G101: a file name, not a credential

// DefaultTokenStateFile returns the token store's default path, i.e.
// ~/.local/state/a2a-worker-controller/docker-workers.json.
func DefaultTokenStateFile(ctx context.Context) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(ctx, err, "resolve home directory failed")
	}
	return filepath.Join(home, localStateDir, tokenStateFileName), nil
}

//counterfeiter:generate -o ../mocks/pkg-token-store.go --fake-name PkgTokenStore . TokenStore

// TokenStore keeps each worker's bearer token so a repeated create can hand back a usable
// handle instead of refusing.
//
// ⚠️ THIS IS THE ONE PLACE A WORKER TOKEN IS AT REST, AND THAT IS A DELIBERATE TRADE. Before
// it existed the controller returned a token once and kept no copy, so a second create for the
// same task could only refuse — and the refusal forced every caller into stop-then-recreate,
// which is what "orphan" means in practice. Keeping the token costs a credential on disk; it
// buys a retried create that returns the live worker. The file is written 0600 into a 0750
// directory and nothing outside this controller reads it, and the token still never reaches a
// list response, a log line or a supervisor record — see CreateResponse.Token.
//
// ⚠️ THE STORE IS SINGLE-WRITER BY CONTRACT, on the same terms as WorkerStore: Save replaces
// the whole file, so two controllers pointed at one path would overwrite each other. The
// backend's mutex serializes its own calls within one process; nothing serializes two.
type TokenStore interface {
	// Get returns the token stored for workerID, or the empty string when none is stored. An
	// absent entry is not an error: a worker created before the store existed, or one whose
	// entry was removed, simply cannot be handed back.
	Get(ctx context.Context, workerID WorkerID) (string, error)
	// Put stores token for workerID, replacing any existing entry for that worker.
	Put(ctx context.Context, workerID WorkerID, token string) error
	// Delete removes workerID's entry. Deleting an absent entry is a no-op.
	Delete(ctx context.Context, workerID WorkerID) error
}

// tokenStoreFile is the store's on-disk shape. It is an object rather than a bare map so a
// later field can be added without a reader having to guess what the keys are.
type tokenStoreFile struct {
	Workers []workerToken `json:"workers"`
}

// workerToken is one stored token. The worker id is the key: it is stable across a controller
// restart because it is read back from the worker's own labels, not minted per request.
type workerToken struct {
	WorkerID WorkerID `json:"worker_id"`
	Token    string   `json:"token"`
}

// jsonTokenStore is the TokenStore that keeps tokens in a JSON file.
type jsonTokenStore struct {
	path string
}

var _ TokenStore = (*jsonTokenStore)(nil)

// NewJSONTokenStore creates the TokenStore backed by the JSON file at path. A leading ~ is
// resolved against the home directory, so the default path can be passed through unchanged.
func NewJSONTokenStore(path string) TokenStore {
	return &jsonTokenStore{path: expandHome(path)}
}

// Get returns the token stored for workerID.
func (s *jsonTokenStore) Get(ctx context.Context, workerID WorkerID) (string, error) {
	entries, err := s.read(ctx)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.WorkerID == workerID {
			return entry.Token, nil
		}
	}
	return "", nil
}

// Put stores token for workerID, replacing any existing entry for that worker.
func (s *jsonTokenStore) Put(ctx context.Context, workerID WorkerID, token string) error {
	entries, err := s.read(ctx)
	if err != nil {
		return err
	}
	replaced := false
	for index := range entries {
		if entries[index].WorkerID == workerID {
			entries[index].Token = token
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, workerToken{WorkerID: workerID, Token: token})
	}
	return s.write(ctx, entries)
}

// Delete removes workerID's entry. Deleting an absent entry is a no-op, so a stop can call it
// without first asking whether the worker was ever stored.
func (s *jsonTokenStore) Delete(ctx context.Context, workerID WorkerID) error {
	entries, err := s.read(ctx)
	if err != nil {
		return err
	}
	kept := make([]workerToken, 0, len(entries))
	for _, entry := range entries {
		if entry.WorkerID != workerID {
			kept = append(kept, entry)
		}
	}
	if len(kept) == len(entries) {
		return nil
	}
	return s.write(ctx, kept)
}

// read returns the store's entries. A store that does not exist yet is empty rather than an
// error: the first create on a fresh machine has no file to read.
func (s *jsonTokenStore) read(ctx context.Context) ([]workerToken, error) {
	//nolint:gosec // G304: the path is the controller's own configured state file, not input
	content, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []workerToken{}, nil
		}
		return nil, errors.Wrapf(ctx, err, "read token store %q failed", s.path)
	}
	var file tokenStoreFile
	if err := json.Unmarshal(content, &file); err != nil {
		return nil, errors.Wrapf(ctx, err, "parse token store %q failed", s.path)
	}
	if file.Workers == nil {
		return []workerToken{}, nil
	}
	return file.Workers, nil
}

// write replaces the store's contents, creating its directory if it does not exist.
//
// ⚠️ WRITTEN TO A TEMPORARY FILE AND RENAMED INTO PLACE, NEVER TRUNCATED IN PLACE. The file
// holds a credential, so a crash must not leave it half-written: a plain os.WriteFile would
// truncate first, and a reader between the truncate and the last byte would see invalid JSON
// and lose every stored token. rename is atomic within a filesystem.
//
// The file is 0600 and its directory 0750: the value is a bearer credential, so no other user
// on the host has any business reading it.
func (s *jsonTokenStore) write(ctx context.Context, entries []workerToken) error {
	content, err := json.MarshalIndent(tokenStoreFile{Workers: entries}, "", "  ")
	if err != nil {
		return errors.Wrap(ctx, err, "marshal token store failed")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, localStoreDirMode); err != nil {
		return errors.Wrapf(ctx, err, "create token store directory for %q failed", s.path)
	}
	//nolint:gosec // G304: the directory is the controller's own configured state directory
	tmp, err := os.CreateTemp(dir, tokenStateFileName+".tmp-*")
	if err != nil {
		return errors.Wrapf(ctx, err, "create temporary token store in %q failed", dir)
	}
	tmpName := tmp.Name()
	// A failure at any point below must not leave the temporary file behind. After a
	// successful rename this remove is a no-op, because the name has moved.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(localStoreFileMode); err != nil {
		_ = tmp.Close()
		return errors.Wrapf(ctx, err, "set token store mode on %q failed", tmpName)
	}
	if _, err := tmp.Write(append(content, '\n')); err != nil {
		_ = tmp.Close()
		return errors.Wrapf(ctx, err, "write token store %q failed", tmpName)
	}
	if err := tmp.Close(); err != nil {
		return errors.Wrapf(ctx, err, "close token store %q failed", tmpName)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return errors.Wrapf(ctx, err, "replace token store %q failed", s.path)
	}
	return nil
}
