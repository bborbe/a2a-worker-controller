// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a2amcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bborbe/errors"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// BearerFileName is the local bearer's file name under the controller's state directory.
const BearerFileName = "mcp-bearer" //nolint:gosec // G101: a file name, not a credential

// bearerBytes is the bearer's entropy; it is stored hex-encoded.
const bearerBytes = 32

// DefaultBearerFile returns where the /mcp bearer lives: the controller's own state directory.
//
// ⚠️ WHY A LOCAL BEARER AT ALL. A loopback check cannot tell a docker worker from a local
// session: measured 2026-11-08 on OrbStack, a container calling host.docker.internal arrives
// with a loopback peer AND local address, and it chooses its own Host header. The bearer is
// what a worker cannot hold — the file is readable only by the operator's uid, and the
// docker backend's mount allowlist never reaches the state directory.
func DefaultBearerFile(ctx context.Context) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(ctx, err, "resolve home directory failed")
	}
	return filepath.Join(home, pkg.LocalStateDir, BearerFileName), nil
}

// LoadOrCreateBearer returns the bearer stored at path, creating it on first use.
//
// It persists rather than rotating per start: a running session holds the header its
// headersHelper produced at connect time, so a bearer that changed on every controller
// restart would cut off every connected session until it reconnected.
//
// The value is a secret: it must never be logged or returned in an error.
func LoadOrCreateBearer(ctx context.Context, path string) (string, error) {
	value, err := readBearer(ctx, path)
	if err == nil {
		return value, nil
	}
	if !stderrors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", errors.Wrapf(ctx, err, "create bearer directory %s failed", filepath.Dir(path))
	}
	raw := make([]byte, bearerBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.Wrap(ctx, err, "generate bearer failed")
	}
	secret := hex.EncodeToString(raw)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if stderrors.Is(err, fs.ErrExist) {
		// Another process created it between the read and the create: use theirs.
		return readBearer(ctx, path)
	}
	if err != nil {
		return "", errors.Wrapf(ctx, err, "create bearer file %s failed", path)
	}
	if _, err := file.WriteString(secret + "\n"); err != nil {
		_ = file.Close()
		// A partially written file must not survive: it would be read as the bearer on the
		// next start, refusing every caller for a reason nothing on disk explains.
		_ = os.Remove(path)
		return "", errors.Wrapf(ctx, err, "write bearer file %s failed", path)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", errors.Wrapf(ctx, err, "close bearer file %s failed", path)
	}
	return secret, nil
}

// readBearer reads an existing bearer, refusing a file another user could read: a bearer
// anyone on the machine can read authenticates nobody.
func readBearer(ctx context.Context, path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.Errorf(
			ctx,
			"bearer file %s is readable by group or others (%s); chmod 600 it",
			path,
			info.Mode().Perm(),
		)
	}
	content, err := os.ReadFile(path) // #nosec G304 -- operator-configured state path
	if err != nil {
		return "", errors.Wrapf(ctx, err, "read bearer file %s failed", path)
	}
	value := strings.TrimSpace(string(content))
	if value == "" {
		return "", errors.Errorf(ctx, "bearer file %s is empty", path)
	}
	return value, nil
}
