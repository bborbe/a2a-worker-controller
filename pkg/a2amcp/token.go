// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package a2amcp

import (
	"context"
	"os/exec"
	"strings"

	"github.com/bborbe/errors"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// SecurityCommand is the macOS Keychain CLI the local-target branch shells out to.
const SecurityCommand = "security"

//counterfeiter:generate -o ../../mocks/a2amcp-token-source.go --fake-name A2amcpTokenSource . TokenSource

// TokenSource resolves the bearer token the A2A client presents to the target agent.
//
// The returned value is a secret: it must never be logged, echoed into a tool result, or
// written to a file.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

//counterfeiter:generate -o ../../mocks/a2amcp-keychain-reader.go --fake-name A2amcpKeychainReader . KeychainReader

// KeychainReader reads one generic password from the macOS Keychain.
type KeychainReader interface {
	Read(ctx context.Context, service string, account string) (string, error)
}

// TokenConfig is the precedence the stdio shim's wrapper (a2a-mcp-wrapper.sh) applies, carried
// unchanged so the move is footprint-only.
type TokenConfig struct {
	// Token is the explicit value; when non-empty it wins outright.
	Token string
	// TargetURL is the agent the token is presented to. The Keychain branch applies only when
	// it names localhost or 127.0.0.1 — the local worker's token never leaves this machine.
	TargetURL string
	// KeychainService and KeychainAccount name the local worker's Keychain item.
	KeychainService string
	KeychainAccount string
	// TeamVaultKey is the final fallback, resolved through TeamVault.
	TeamVaultKey string
}

// NewTokenSource returns the TokenSource applying config's precedence: explicit token, else
// (local target only) the Keychain item, else the TeamVault key.
//
// It resolves on every call rather than caching: a rotated token takes effect without a
// restart, and a2a_send is rare enough that one subprocess per call costs nothing.
func NewTokenSource(
	config TokenConfig,
	keychain KeychainReader,
	teamVault pkg.TeamVaultResolver,
) TokenSource {
	return &tokenSource{config: config, keychain: keychain, teamVault: teamVault}
}

type tokenSource struct {
	config    TokenConfig
	keychain  KeychainReader
	teamVault pkg.TeamVaultResolver
}

func (t *tokenSource) Token(ctx context.Context) (string, error) {
	if t.config.Token != "" {
		return t.config.Token, nil
	}
	// The wrapper swallows a Keychain failure (`|| true`) and falls through to TeamVault, so a
	// missing item is not an error here either — only an empty result from every branch is.
	if isLocalTarget(t.config.TargetURL) && t.keychain != nil {
		value, err := t.keychain.Read(ctx, t.config.KeychainService, t.config.KeychainAccount)
		if err == nil && value != "" {
			return value, nil
		}
	}
	if t.teamVault != nil && t.config.TeamVaultKey != "" {
		value, err := t.teamVault.Resolve(ctx, t.config.TeamVaultKey)
		if err == nil && value != "" {
			return value, nil
		}
	}
	return "", errors.Errorf(ctx, "failed to resolve A2A token (env, local Keychain, or TeamVault)")
}

// isLocalTarget mirrors the wrapper's `*localhost* | *127.0.0.1*` case pattern — a substring
// test, deliberately as loose as the original so no target changes branch in the move.
func isLocalTarget(targetURL string) bool {
	return strings.Contains(targetURL, "localhost") || strings.Contains(targetURL, "127.0.0.1")
}

// NewKeychainReader returns the KeychainReader that shells out to `security`.
func NewKeychainReader() KeychainReader {
	return &keychainCLI{}
}

type keychainCLI struct{}

func (k *keychainCLI) Read(ctx context.Context, service string, account string) (string, error) {
	// #nosec G204 -- fixed binary and flags; service and account are operator configuration,
	// passed as discrete argv entries, never through a shell.
	cmd := exec.CommandContext(
		ctx,
		SecurityCommand,
		"find-generic-password",
		"-s",
		service,
		"-a",
		account,
		"-w",
	)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// stderr is not captured: it names only the item, and the error is never surfaced
		// beyond the fall-through to TeamVault.
		return "", errors.Wrapf(ctx, err, "keychain lookup for service %s failed", service)
	}
	return strings.TrimSpace(stdout.String()), nil
}
