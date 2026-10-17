// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/bborbe/errors"
)

// MountNotAllowedPrefix is the marker every refusal from this file carries. It is a stable
// token rather than a sentence so a caller can match on it without parsing prose, and it names
// the offending value after the colon — the caller's own input, never anything resolved.
const MountNotAllowedPrefix = "mount_not_allowed: "

// MountAllowlist is the set of absolute host paths a caller may ask the controller to expose.
//
// ⚠️ AN EMPTY ALLOWLIST ALLOWS NOTHING, which is the opposite of the usual "unset means
// unrestricted" convention and is deliberate. create is a public surface, so the safe reading
// of an unconfigured controller is "this controller mounts nothing" — a default that mounted
// everything would turn a forgotten flag into an arbitrary-host-path mount on every worker.
type MountAllowlist []string

// ParseMountAllowlist parses the comma-separated absolute host-path list the MOUNT_ALLOW
// setting takes (`/Users/bborbe/Documents/Obsidian/private-personal,/srv/data`). A malformed
// entry is refused rather than skipped, matching ParseEgressDestinations and for the same
// reason: a dropped entry silently denies a path the operator meant to allow, and the operator
// discovers it only when a worker cannot reach its vault.
//
// Every entry must be absolute. A relative entry is refused rather than resolved against the
// controller's own working directory, because that directory is not part of the setting and a
// path that depends on it would mean different things on the Mac and in a pod.
func ParseMountAllowlist(ctx context.Context, value string) (MountAllowlist, error) {
	var allowlist MountAllowlist
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !filepath.IsAbs(entry) {
			return nil, errors.Errorf(
				ctx,
				"mount allowlist entry %q is not an absolute path",
				entry,
			)
		}
		allowlist = append(allowlist, filepath.Clean(entry))
	}
	return allowlist, nil
}

// Allows reports whether hostPath sits at or under one of the allowlisted paths.
//
// ⚠️ THE COMPARISON IS ON PATH BOUNDARIES, NOT ON RAW PREFIXES. A plain strings.HasPrefix would
// let an allowlist entry of `/srv/data` admit `/srv/database`, which is a different directory
// that the operator never named — so the descendant case is matched against the entry plus a
// separator. Both sides are cleaned first, because `/srv/data/` and `/srv/./data` name the same
// directory and a caller should not be able to slip past by spelling it differently.
func (a MountAllowlist) Allows(hostPath string) bool {
	cleaned := filepath.Clean(hostPath)
	for _, allowed := range a {
		if cleaned == allowed || strings.HasPrefix(cleaned, allowed+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ValidateMounts refuses a caller's declared mounts that this controller will not honour, and
// names the first offending value so the caller can fix it rather than guess.
//
// It is called before any container is created: a refused mount means NO worker, not a worker
// with the mount silently missing. A worker that started without a mount it declared would look
// healthy and fail later at its first file access, which is the failure this ordering exists to
// prevent.
func ValidateMounts(ctx context.Context, allowlist MountAllowlist, mounts []Mount) error {
	for _, mount := range mounts {
		// A named volume is not a host path, so the allowlist cannot govern it — see the
		// Mount type for why the field exists at all rather than being omitted.
		if mount.Volume != "" {
			return errors.Errorf(ctx, "%svolume %s", MountNotAllowedPrefix, mount.Volume)
		}
		if mount.HostPath == "" {
			return errors.Errorf(
				ctx,
				"%smount declares neither host_path nor volume",
				MountNotAllowedPrefix,
			)
		}
		if !filepath.IsAbs(mount.HostPath) {
			return errors.Errorf(
				ctx,
				"%s%s is not an absolute path",
				MountNotAllowedPrefix,
				mount.HostPath,
			)
		}
		if mount.ContainerPath == "" {
			return errors.Errorf(
				ctx,
				"%s%s declares no container_path",
				MountNotAllowedPrefix,
				mount.HostPath,
			)
		}
		if !allowlist.Allows(mount.HostPath) {
			return errors.Errorf(ctx, "%s%s", MountNotAllowedPrefix, mount.HostPath)
		}
	}
	return nil
}

// ValidateSecrets refuses a caller's declared secrets that the controller cannot resolve.
//
// ⚠️ IT CHECKS SHAPE ONLY — it never resolves a key and never sees a value. Resolution is a
// separate call at create time, so a malformed request is refused before the controller touches
// TeamVault at all, and no error raised here can carry a secret even by accident.
//
// A repeated Name is refused rather than last-write-wins: two secrets under one name would make
// the worker's view of that name depend on the controller's iteration order, which is not
// something a caller can reason about.
func ValidateSecrets(ctx context.Context, secrets []Secret) error {
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		if secret.Name == "" {
			return errors.Errorf(ctx, "secret declares no name")
		}
		if secret.TeamVaultKey == "" {
			return errors.Errorf(ctx, "secret %q declares no teamvault_key", secret.Name)
		}
		if _, duplicate := seen[secret.Name]; duplicate {
			return errors.Errorf(ctx, "secret %q is declared more than once", secret.Name)
		}
		seen[secret.Name] = struct{}{}
	}
	return nil
}
