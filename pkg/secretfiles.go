// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"os"
	"path/filepath"

	"github.com/bborbe/errors"
)

// SecretContainerDir is the directory inside the worker where resolved secrets appear. A file
// per secret, named after the declared Name.
//
// ⚠️ IT IS A FILE MOUNT RATHER THAN AN ENVIRONMENT VARIABLE, and that choice is forced rather
// than aesthetic. An env route — `-e NAME=value` or an `--env-file` — lands the value in the
// container's `Config.Env`, where `docker inspect` prints it for anyone with daemon access. A
// bind-mounted file appears in `docker inspect` as the HOST path only, so the value stays out of
// both argv and the container's recorded spec.
const SecretContainerDir = "/run/secrets"

// ResolvedSecret is one secret after the controller has looked its value up. It exists only
// between resolution and the write that puts it on disk.
//
// ⚠️ ITS Value FIELD IS A CREDENTIAL. The struct is passed by value into the writer and dropped
// immediately; it is never stored on a backend, never logged, and never placed in an error.
type ResolvedSecret struct {
	Name  string
	Value string
}

// DefaultSecretDir is the host directory resolved secret files are written into when the
// operator configures none. It sits beside the controller's other state, under the user's own
// home, so it inherits that directory's permissions rather than a world-readable temp dir.
func DefaultSecretDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "a2a-worker-controller", "secrets")
}

// SecretMount is one resolved secret as the run command needs it: the host file holding the
// value, and the name it appears under inside the worker. It carries no value, which is what
// lets it be passed around and reported without a credential travelling with it.
type SecretMount struct {
	// HostPath is the 0600 file on the host holding the value.
	HostPath string
	// Name is the declared name; the file appears at SecretContainerDir/<Name> in the worker.
	Name string
}

// WriteSecretFiles writes one file per resolved secret under dir/<workerID>/ and returns the
// mounts to hand the run command, in the same order as the input.
//
// ⚠️ EVERY FILE IS 0600 AND THE DIRECTORY IS 0700. The value is a credential, and the container
// is not the only reader on this host — the operator's own account, any process running as them,
// and anything that walks the tree can read a 0644 file. The narrow mode is what makes the
// difference between a secret this worker can read and a secret every local process can.
//
// A failure part-way through removes the directory it created rather than leaving a partial set
// on disk: a half-written secret directory is a credential store nobody will clean up, because
// the create that would have owned it failed.
func WriteSecretFiles(
	ctx context.Context,
	dir string,
	workerID WorkerID,
	secrets []ResolvedSecret,
) ([]SecretMount, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	if dir == "" {
		return nil, errors.Errorf(ctx, "no secret directory is configured")
	}
	workerDir := filepath.Join(dir, string(workerID))
	if err := os.MkdirAll(workerDir, 0o700); err != nil {
		return nil, errors.Wrap(ctx, err, "create secret directory failed")
	}
	mounts := make([]SecretMount, 0, len(secrets))
	for _, secret := range secrets {
		path := filepath.Join(workerDir, secret.Name)
		// O_EXCL so a name colliding with an existing file fails loudly instead of
		// overwriting a credential another worker is still reading.
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = os.RemoveAll(workerDir)
			return nil, errors.Wrap(ctx, err, "create secret file failed")
		}
		if _, err := file.WriteString(secret.Value); err != nil {
			_ = file.Close()
			_ = os.RemoveAll(workerDir)
			return nil, errors.Wrap(ctx, err, "write secret file failed")
		}
		if err := file.Close(); err != nil {
			_ = os.RemoveAll(workerDir)
			return nil, errors.Wrap(ctx, err, "close secret file failed")
		}
		mounts = append(mounts, SecretMount{HostPath: path, Name: secret.Name})
	}
	return mounts, nil
}

// RemoveSecretFiles removes the directory holding one worker's secret files. It is called when
// the worker is stopped: the files are mounted for the container's lifetime, so they must
// outlive the create that wrote them, and removing them is what keeps a long-lived host from
// accumulating credentials for workers that no longer exist.
//
// A missing directory is not an error — a worker created without secrets has none, and the stop
// that follows must not fail on that account.
func RemoveSecretFiles(ctx context.Context, dir string, workerID WorkerID) error {
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(dir, string(workerID))); err != nil {
		return errors.Wrap(ctx, err, "remove secret directory failed")
	}
	return nil
}
