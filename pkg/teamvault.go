// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bborbe/errors"
)

// TeamVaultConfigEnv is the only environment variable this controller reads for TeamVault. It
// names the teamvault-cli config file, not a credential: the CLI resolves its own authentication
// from that file, so the controller never holds a TeamVault password of its own.
//
// ⚠️ The NAME is teamvault-cli's, not this controller's invention, and the controller must not
// grow a second one. teamvault-cli already documents a precedence for this value
// ($TEAMVAULT_CONFIG, else ~/.config/teamvault-cli/config.json, else ~/.teamvault.json), so a
// private variable here would be a second configuration surface that disagrees with the CLI the
// moment the CLI's default moves.
const TeamVaultConfigEnv = "TEAMVAULT_CONFIG"

// TeamVaultCommand is the CLI this controller shells out to. It is a separate binary rather
// than an in-process client on purpose: the launchd wrapper
// (~/.local/bin/a2a-worker-controller.sh) and claude-supervisor's scripts/mcp-start.py already
// resolve TeamVault this way, so one implementation of the lookup serves all three and the
// controller adds no second auth path to keep in step.
const TeamVaultCommand = "teamvault-cli"

// TeamVaultResolver resolves a TeamVault lookup key to the secret it names.
type TeamVaultResolver interface {
	// Resolve returns the value for key. The returned value is a secret: it must never be
	// logged, echoed into a transcript, or written to a file. An error names the KEY and the
	// CLI's own stderr, never the value.
	Resolve(ctx context.Context, key string) (string, error)
}

// teamVaultCLI resolves keys by running teamvault-cli.
type teamVaultCLI struct {
	command string
	config  string
	// run is injected so the resolver can be exercised without a TeamVault instance, and so a
	// test can assert what argv was built without a real lookup happening.
	run func(ctx context.Context, argv []string) (string, string, error)
}

// NewTeamVaultResolver creates the resolver that shells out to teamvault-cli.
//
// The config path comes from TeamVaultConfigEnv when set, and is otherwise left to
// teamvault-cli's own default — passing no flag at all, rather than guessing a path here. That
// keeps one precedence rule in the system instead of two that can disagree.
func NewTeamVaultResolver() TeamVaultResolver {
	return &teamVaultCLI{
		command: TeamVaultCommand,
		config:  os.Getenv(TeamVaultConfigEnv),
		run:     runTeamVaultCLI,
	}
}

// Resolve returns the value teamvault-cli reports for key.
//
// ⚠️ THE VALUE IS RETURNED AND NEVER LOGGED — not on success, not on failure, not in an error.
// A TeamVault lookup key is a short identifier rather than a secret, so naming the KEY is what
// makes a failure diagnosable without exposing anything, and that is all this function ever
// puts in a message.
func (t *teamVaultCLI) Resolve(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", errors.Errorf(ctx, "teamvault key is empty")
	}
	argv := []string{t.command, "password", "--teamvault-key=" + key}
	if t.config != "" {
		argv = append(argv, "--teamvault-config="+t.config)
	}
	stdout, stderr, err := t.run(ctx, argv)
	if err != nil {
		// The CLI's own stderr names the problem and carries no secret — it is the only
		// diagnostic available, and the alternative (reporting the raw error) would name the
		// argv, which holds the key and nothing else of use.
		return "", errors.Errorf(
			ctx,
			"teamvault lookup for key %s failed: %s",
			key,
			strings.TrimSpace(stderr),
		)
	}
	value := strings.TrimSpace(stdout)
	if value == "" {
		return "", errors.Errorf(ctx, "teamvault lookup for key %s returned an empty value", key)
	}
	return value, nil
}

// runTeamVaultCLI runs argv and returns its stdout and stderr separately.
//
// ⚠️ stdout IS THE SECRET, so it is returned to the caller rather than folded into an error, and
// stderr is kept apart from it. Merging the two streams — the obvious convenience — would put a
// resolved secret into the diagnostic path of every failure.
func runTeamVaultCLI(ctx context.Context, argv []string) (string, string, error) {
	if len(argv) == 0 {
		return "", "", errors.Errorf(ctx, "no command to run")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is built here
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

// resolveSecrets looks each declared secret up and returns the values paired with their names.
//
// ⚠️ A NIL RESOLVER REFUSES A CREATE THAT DECLARES A SECRET rather than starting a worker without
// it. A worker missing a credential it was told to expect starts successfully and fails at its
// first authenticated call — a failure far from its cause, and on a surface where the caller has
// already been told the create succeeded.
//
// The values live in this return value and in the caller's local variable only: nothing here
// logs, and a resolver error names the TeamVault KEY rather than the value it failed to fetch.
func resolveSecrets(
	ctx context.Context,
	resolver TeamVaultResolver,
	secrets []Secret,
) ([]ResolvedSecret, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	if resolver == nil {
		return nil, errors.Errorf(
			ctx,
			"this controller has no TeamVault resolver, so it cannot resolve the %d declared secret(s)",
			len(secrets),
		)
	}
	resolved := make([]ResolvedSecret, 0, len(secrets))
	for _, secret := range secrets {
		value, err := resolver.Resolve(ctx, secret.TeamVaultKey)
		if err != nil {
			return nil, errors.Wrap(ctx, err, "resolve declared secret failed")
		}
		resolved = append(resolved, ResolvedSecret{Name: secret.Name, Value: value})
	}
	return resolved, nil
}

// DefaultTeamVaultConfigPath is the path teamvault-cli uses when TeamVaultConfigEnv is unset and
// its XDG and legacy candidates are absent. It exists so a caller can REPORT where a lookup was
// expected to read from without re-deriving teamvault-cli's precedence — never so this package
// can pass it as a flag, which would freeze a default the CLI owns.
func DefaultTeamVaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "teamvault-cli", "config.json")
}
