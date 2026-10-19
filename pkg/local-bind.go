// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bborbe/errors"
	libtime "github.com/bborbe/time"
	vaultconfig "github.com/bborbe/vault-cli/pkg/config"
)

const (
	// DefaultLocalBindTimeout is how long Create polls the session registry for the session its
	// tab started before giving up on binding it. It is the supervisor's own window
	// (server/supervisor.mjs polls 8 s): a session registers about a second after its pane
	// opens, so the window is generous rather than tight.
	DefaultLocalBindTimeout = 8 * time.Second
	// localBindPollInterval is how often the registry is re-read inside that window — again the
	// supervisor's own value.
	localBindPollInterval = 250 * time.Millisecond
	// localSessionRegistryDir is Claude Code's session registry under the home directory: one
	// `<pid>.json` per live interactive session, carrying its `sessionId` and its `name`.
	localSessionRegistryDir = ".claude/sessions"
	// localOwnerKey is the task-scoped frontmatter field a session's binding is stamped into.
	localOwnerKey = "claude_session_id"
	// BindActionStamped is the bind outcome when the task had no owner and this session became it.
	BindActionStamped = "stamped"
	// BindActionAppended is the bind outcome when the task already had an owner, which is left
	// alone, and this session was appended to the task's metrics sessions instead.
	BindActionAppended = "appended"
)

// LocalVault is one vault a local worker can be opened in: the directory its task files live
// under, the directory inside it that holds them, the launcher its tabs run when nothing more
// specific names one, and the name vault-cli knows it by.
//
// ⚠️ NAME MAY BE EMPTY, AND THEN NO vault-cli WRITE IS ATTEMPTED. The configured default vault is
// a DIRECTORY (`-local-vault`); its name is looked up in vault-cli's config by path, and a vault
// no entry names cannot be addressed by `--vault` — omitting the flag instead would write into
// vault-cli's own default vault, which may be a different vault entirely.
type LocalVault struct {
	Name         string
	Dir          string
	TasksDir     string
	ClaudeScript string
}

// tasksDir returns the vault's tasks directory, DefaultLocalTasksDir when none was resolved.
func (v LocalVault) tasksDir() string {
	if v.TasksDir == "" {
		return DefaultLocalTasksDir
	}
	return v.TasksDir
}

//counterfeiter:generate -o ../mocks/pkg-local-vault-resolver.go --fake-name PkgLocalVaultResolver . LocalVaultResolver

// LocalVaultResolver resolves a vault from vault-cli's own config.
//
// ⚠️ vault-cli's CONFIG IS THE ONE SOURCE OF VAULT NAMES IN THE FLEET, so it is read rather than
// mirrored: the supervisor's own spawn path resolves `--vault <name>` through the same file, and a
// second list here would be a second answer to "where does vault X live" that could drift from it.
type LocalVaultResolver interface {
	// ResolveName returns the vault vault-cli's config names `name`, or an error when none does.
	ResolveName(ctx context.Context, name string) (LocalVault, error)
	// ResolveDir returns the vault vault-cli's config records at `dir`, or an error when none does.
	ResolveDir(ctx context.Context, dir string) (LocalVault, error)
}

// NewVaultCLIConfigResolver creates the LocalVaultResolver backed by vault-cli's config file —
// the same loader vaultGoalsDir already reads.
func NewVaultCLIConfigResolver() LocalVaultResolver {
	return localVaultResolver{}
}

type localVaultResolver struct{}

// ResolveName looks the vault up by its vault-cli name.
func (localVaultResolver) ResolveName(ctx context.Context, name string) (LocalVault, error) {
	vaults, err := vaultconfig.NewLoader("").GetAllVaults(ctx)
	if err != nil {
		return LocalVault{}, errors.Wrap(ctx, err, "load vault-cli config failed")
	}
	for _, vault := range vaults {
		if vault.Name == name {
			return localVaultFrom(vault), nil
		}
	}
	return LocalVault{}, errors.Errorf(ctx, "no vault-cli config entry names the vault %q", name)
}

// ResolveDir looks the vault up by its path, the same match vaultGoalsDir makes.
func (localVaultResolver) ResolveDir(ctx context.Context, dir string) (LocalVault, error) {
	vaults, err := vaultconfig.NewLoader("").GetAllVaults(ctx)
	if err != nil {
		return LocalVault{}, errors.Wrap(ctx, err, "load vault-cli config failed")
	}
	wanted := filepath.Clean(expandHome(dir))
	for _, vault := range vaults {
		if filepath.Clean(expandHome(vault.Path)) == wanted {
			return localVaultFrom(vault), nil
		}
	}
	return LocalVault{}, errors.Errorf(ctx, "no vault-cli config entry has the path %q", dir)
}

func localVaultFrom(vault *vaultconfig.Vault) LocalVault {
	return LocalVault{
		Name:         vault.Name,
		Dir:          expandHome(vault.Path),
		TasksDir:     vault.GetTasksDir(),
		ClaudeScript: vault.ClaudeScript,
	}
}

//counterfeiter:generate -o ../mocks/pkg-vault-cli.go --fake-name PkgVaultCLI . VaultCLI

// VaultCLI runs one vault-cli command. It is the seam the ownership rule writes through, so a
// spec asserts the exact argv without a vault-cli binary or a real vault.
type VaultCLI interface {
	Run(ctx context.Context, args ...string) error
}

// NewExecVaultCLI creates the VaultCLI that execs the `vault-cli` binary.
func NewExecVaultCLI() VaultCLI {
	return vaultCLI{}
}

type vaultCLI struct{}

// Run execs vault-cli with args as argv — never through a shell — and names its stderr on failure.
func (vaultCLI) Run(ctx context.Context, args ...string) error {
	var stderr bytes.Buffer
	//nolint:gosec // G204: argv is fixed verbs plus a validated vault name, task name and session id
	command := exec.CommandContext(ctx, "vault-cli", args...)
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return errors.Wrapf(
			ctx,
			err,
			"vault-cli %s failed: %s",
			strings.Join(args, " "),
			strings.TrimSpace(stderr.String()),
		)
	}
	return nil
}

// localVaultName is the shape a requested vault name must have.
//
// ⚠️ A LEADING DASH IS REFUSED, because the name is passed to vault-cli as the argument of
// `--vault`, and a name that is itself a flag is a request this backend never meant to send.
var localVaultName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// validateVaultName refuses a requested vault name this backend will not pass on.
func validateVaultName(ctx context.Context, name string) error {
	if !localVaultName.MatchString(name) || strings.Contains(name, "..") {
		return errors.Errorf(
			ctx,
			"vault %q is not a valid vault name: it must be letters, digits, `.`, `_` or `-`, "+
				"not starting with `-` or `.`",
			name,
		)
	}
	return nil
}

// validateFirstStep refuses a first step that cannot travel as one argv element.
//
// A NUL is the one byte a C argv cannot carry, so a prompt containing one would be cut short at it
// by exec — a prompt that no longer says what the caller sent. Everything else is safe: the whole
// prompt is single-quoted by tabCommand, so it never becomes shell syntax.
func validateFirstStep(ctx context.Context, firstStep string) error {
	if strings.ContainsRune(firstStep, 0) {
		return errors.Errorf(ctx, "first_step must contain no NUL byte")
	}
	return nil
}

// resolveVault returns the vault a create is opened in.
//
// ⚠️ AN EMPTY NAME IS THE CONFIGURED VAULT, EXACTLY AS BEFORE THE FIELD EXISTED — same directory,
// same launcher, same `25 Tasks`. Its name is looked up by path only so the ownership rule can
// address it through `--vault`; a lookup that fails costs the bind its write — reported on the
// reply — and never the create.
//
// ⚠️ AN UNKNOWN NAMED VAULT IS REFUSED, AND BEFORE ANY TAB OPENS. A worker opened in the default
// vault for a task that lives elsewhere would read the wrong file for its launcher and role, start
// in a directory its task is not in, and bind its session to nothing.
func (b *localBackend) resolveVault(ctx context.Context, name string) (LocalVault, error) {
	if name == "" {
		vault := LocalVault{Dir: b.config.VaultDir, ClaudeScript: b.config.ClaudeScript}
		named, err := b.vaultResolver().ResolveDir(ctx, b.config.VaultDir)
		if err != nil {
			slog.Warn(
				"the configured vault has no vault-cli name; its session binding cannot be written",
				"vault", b.config.VaultDir,
				"error", err,
			)
			return vault, nil
		}
		vault.Name = named.Name
		return vault, nil
	}
	if err := validateVaultName(ctx, name); err != nil {
		return LocalVault{}, err
	}
	vault, err := b.vaultResolver().ResolveName(ctx, name)
	if err != nil {
		return LocalVault{}, errors.Wrapf(ctx, err, "resolve vault %q failed", name)
	}
	if vault.Dir == "" {
		return LocalVault{}, errors.Errorf(ctx, "vault %q has no path in vault-cli's config", name)
	}
	// A vault naming no claude_script of its own falls back to the configured launcher, with a
	// warning, rather than refusing: the launcher precedence still lets the task or a goal choose.
	if vault.ClaudeScript == "" {
		slog.Warn(
			"the vault names no claude_script; falling back to the configured launcher",
			"vault", name,
			"launcher", b.config.ClaudeScript,
		)
		vault.ClaudeScript = b.config.ClaudeScript
	}
	return vault, nil
}

// vaultResolver returns the configured resolver, vault-cli's own config when none is set.
func (b *localBackend) vaultResolver() LocalVaultResolver {
	if b.config.VaultResolver != nil {
		return b.config.VaultResolver
	}
	return NewVaultCLIConfigResolver()
}

// vaultCLIRunner returns the configured vault-cli runner, the real binary when none is set.
func (b *localBackend) vaultCLIRunner() VaultCLI {
	if b.config.VaultCLI != nil {
		return b.config.VaultCLI
	}
	return NewExecVaultCLI()
}

// sessionRegistryDir returns the session registry directory, the configured one when set.
func (b *localBackend) sessionRegistryDir() string {
	if b.config.SessionRegistryDir != "" {
		return expandHome(b.config.SessionRegistryDir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, localSessionRegistryDir)
}

// sessionRegistry returns the configured session registry, one over sessionRegistryDir when none is set.
func (b *localBackend) sessionRegistry() SessionRegistry {
	if b.config.SessionRegistry != nil {
		return b.config.SessionRegistry
	}
	return NewSessionRegistry(b.sessionRegistryDir())
}

// clock returns the configured clock, the real one when none is set.
func (b *localBackend) clock() libtime.CurrentDateTimeGetter {
	if b.config.CurrentDateTimeGetter != nil {
		return b.config.CurrentDateTimeGetter
	}
	return libtime.NewCurrentDateTime()
}

// registryEntry is the slice of a session registry file this backend reads. The field names are
// Claude Code's own, as claude-supervisor's server/liveness.mjs reads them.
//
// PID is a pointer so an entry WITHOUT one is distinguishable from pid 0: the supervisor's reader
// admits an entry only when its pid is an integer, and this one applies the same rule.
type registryEntry struct {
	PID       *int   `json:"pid"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
}

//counterfeiter:generate -o ../mocks/pkg-session-registry.go --fake-name PkgSessionRegistry . SessionRegistry

// SessionRegistry reads Claude Code's session registry.
type SessionRegistry interface {
	// SessionIDsNamed returns every session id the registry currently holds under name, or nil
	// when the registry cannot be read.
	//
	// ⚠️ nil IS "NO INFORMATION", NOT "NOBODY". A file that is foreign or half-written is skipped
	// rather than raised — it is not ours to fail on — and an entry without an integer pid is not
	// a session, exactly as the supervisor's readRegistry treats it.
	SessionIDsNamed(name string) map[string]bool
}

// NewSessionRegistry creates the SessionRegistry that scans the `<pid>.json` files in dir.
func NewSessionRegistry(dir string) SessionRegistry {
	return sessionRegistry{dir: dir}
}

type sessionRegistry struct {
	dir string
}

func (r sessionRegistry) SessionIDsNamed(name string) map[string]bool {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	ids := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		//nolint:gosec // G304: a file inside the session registry directory
		content, err := os.ReadFile(filepath.Join(r.dir, entry.Name()))
		if err != nil {
			continue
		}
		var parsed registryEntry
		if err := json.Unmarshal(content, &parsed); err != nil {
			continue
		}
		if parsed.PID == nil || parsed.SessionID == "" || parsed.Name != name {
			continue
		}
		ids[parsed.SessionID] = true
	}
	return ids
}

// awaitRegisteredSession polls the registry for a session registered under name that was not
// already there before the spawn, and returns its id — empty when none appears within the window.
//
// ⚠️ THE EXCLUSION IS WHAT MAKES THE JOIN SAFE. The tab name is the only join from a pane to its
// session, and the name is per task, so an earlier session of the same task that is still live
// answers to it too. Snapshotting the holders before the spawn means the poll can only resolve to
// the session THIS create started, or to nothing — the honest "never registered" the reply reports
// as a nil bind.
//
// ⚠️ IT IS BOUNDED BY BindTimeout BECAUSE IT RUNS UNDER THE BACKEND'S MUTEX, on the same terms as
// the colour send: the bound is what keeps a session that never registers from stalling list and
// stop for longer than the supervisor itself would have waited.
func (b *localBackend) awaitRegisteredSession(
	ctx context.Context,
	name string,
	preexisting map[string]bool,
) string {
	timeout := b.config.BindTimeout
	if timeout <= 0 {
		timeout = DefaultLocalBindTimeout
	}
	interval := localBindPollInterval
	if interval > timeout {
		interval = timeout
	}
	registry := b.sessionRegistry()
	clock := b.clock()
	deadline := clock.Now().Add(libtime.Duration(timeout))
	for {
		for id := range registry.SessionIDsNamed(name) {
			if !preexisting[id] {
				return id
			}
		}
		if !clock.Now().Before(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(interval):
		}
	}
}

// bindSession applies the fleet's ownership rule to a session this create started: stamp the
// task's claude_session_id when it is empty, otherwise leave the owner and append the session to
// the task's metrics sessions. It mirrors claude-supervisor's server/task-binding.mjs, and the
// argv is the same: `vault-cli task set <task> claude_session_id <id> --vault <v>` or
// `vault-cli task append-metrics-session <task> <id> --vault <v>`.
//
// ⚠️ THE OWNER IS READ FROM THE TASK FILE, NOT THROUGH `vault-cli task get`, AND THAT IS THE ONE
// DIVERGENCE FROM THE REFERENCE. This backend already reads that exact field from that exact file
// for its baseline (readTaskSessionID), so a second reader would be a second answer to the same
// question; the WRITES still go through vault-cli, which owns the file's format.
//
// ⚠️ NEVER FATAL. The worker is open and recorded by the time this runs, so a failed bind is
// reported on the reply's bind.error and the create still succeeds — failing it would hide a live,
// recorded tab behind an error.
func (b *localBackend) bindSession(
	ctx context.Context,
	vault LocalVault,
	task TaskName,
	sessionID string,
) *Bind {
	if vault.Name == "" {
		return &Bind{Error: "the vault has no vault-cli name, so the binding cannot be written"}
	}
	owner, err := readTaskSessionID(ctx, vault, task)
	if err != nil {
		return &Bind{Error: err.Error()}
	}
	action := BindActionStamped
	args := make([]string, 0, 7)
	if owner == "" {
		args = append(args, "task", "set", string(task), localOwnerKey, sessionID)
	} else {
		action = BindActionAppended
		args = append(args, "task", "append-metrics-session", string(task), sessionID)
	}
	args = append(args, "--vault", vault.Name)
	if err := b.vaultCLIRunner().Run(ctx, args...); err != nil {
		return &Bind{Error: err.Error()}
	}
	return &Bind{Action: action}
}

// storeSessionID writes a bound session id onto the worker's store entry.
//
// A failure is logged and not returned: the id is still on the reply, and on the stamped path the
// next list re-derives it from the task file anyway.
func (b *localBackend) storeSessionID(ctx context.Context, workerID WorkerID, sessionID string) {
	workers, err := b.store.List(ctx)
	if err != nil {
		slog.Warn("record bound session id failed", "worker", workerID, "error", err)
		return
	}
	for index := range workers {
		if workers[index].WorkerID == workerID {
			workers[index].SessionID = sessionID
		}
	}
	if err := b.store.Save(ctx, workers); err != nil {
		slog.Warn("record bound session id failed", "worker", workerID, "error", err)
	}
}

// bindCreatedWorker joins a freshly created worker to the session its tab started and binds that
// session to the task, returning the reply's Bind — nil when no session registered in the window.
//
// The id lands on the worker and its store entry BEFORE the vault-cli write, so a failed write
// still leaves the worker reporting the session it really has.
func (b *localBackend) bindCreatedWorker(
	ctx context.Context,
	vault LocalVault,
	worker *LocalWorker,
	preexisting map[string]bool,
) *Bind {
	sessionID := b.awaitRegisteredSession(ctx, tabNameFor(worker.Task), preexisting)
	if sessionID == "" {
		return nil
	}
	worker.SessionID = sessionID
	b.storeSessionID(ctx, worker.WorkerID, sessionID)
	bind := b.bindSession(ctx, vault, worker.Task, sessionID)
	if bind.Error != "" {
		slog.Warn(
			"bind worker session to its task failed",
			"task",
			worker.Task,
			"error",
			bind.Error,
		)
	}
	return bind
}

// validateLocalCreate refuses a request carrying a reserved, unexportable or path-shaped value,
// before any work — so it fails fast rather than after a tab has been opened for a worker that
// must not start.
func validateLocalCreate(ctx context.Context, req CreateRequest) error {
	if err := validateTaskName(ctx, req.Task); err != nil {
		return err
	}
	if err := validateLocalEnv(ctx, req.Env); err != nil {
		return err
	}
	return validateFirstStep(ctx, req.FirstStep)
}

// resolveCreate resolves the create's vault, then the launcher and role from that vault's task
// file — every one of them refused, when it must be, before any tab is opened.
func (b *localBackend) resolveCreate(
	ctx context.Context,
	req CreateRequest,
) (LocalVault, resolvedLauncher, resolvedRole, error) {
	vault, err := b.resolveVault(ctx, req.Vault)
	if err != nil {
		return LocalVault{}, resolvedLauncher{}, resolvedRole{}, err
	}
	chosen, role, err := b.resolveTask(ctx, vault, req.Task)
	if err != nil {
		return LocalVault{}, resolvedLauncher{}, resolvedRole{}, err
	}
	return vault, chosen, role, nil
}
