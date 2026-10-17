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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bborbe/errors"
)

// The local backend's fixed settings.
const (
	// DefaultWeztermBinary is the wezterm binary's absolute path.
	//
	// ⚠️ IT IS ABSOLUTE ON PURPOSE AND A BARE NAME MUST NEVER BE SUBSTITUTED. wezterm is not
	// on a launchd job's PATH — it exists only inside the app bundle — and `exec.Command`
	// resolves a bare name with `exec.LookPath` against THIS process's PATH, never against the
	// child's `cmd.Env`. Prefixing the child's PATH therefore cannot repair it, and the failure
	// is silent: the spawn never happens and nothing says why.
	DefaultWeztermBinary = "/Applications/WezTerm.app/Contents/MacOS/wezterm"
	// DefaultLocalTasksDir is the vault directory a task file lives in, relative to the
	// configured vault root. It is the private-personal vault's own layout, which is the vault
	// a local worker serves.
	DefaultLocalTasksDir = "25 Tasks"
	// localStateDir is the ownership store's directory under the home directory. The store is
	// state, not configuration, so it lives where the XDG layout puts state.
	localStateDir = ".local/state/a2a-worker-controller"
	// localStateFileName is the ownership store's file name.
	localStateFileName = "local-workers.json"
	// localTabNamePrefix marks a tab as a controller worker, so a supervised tab is
	// identifiable in the tab bar. It is the supervisor's own convention.
	localTabNamePrefix = "⚙ "
	// localWorkPrompt is the message a worker's session starts with. It is the supervisor's
	// own prompt for a tab worker — anchoring the vault task is what links the session to the
	// task — so a worker opened here is driven exactly like one the supervisor opened.
	localWorkPrompt = "/vault-cli:work-on-task "
	// localSocketDir is where WezTerm keeps its per-GUI sockets, under the home directory.
	localSocketDir = ".local/share/wezterm"
	// localEnvNamePattern is a legal shell variable name. The tab path exports each entry
	// literally into a shell command line, so a name that is not one of these is refused
	// rather than quoted.
	localEnvNamePattern = `^[A-Za-z_][A-Za-z0-9_]*$`
	// localStoreDirMode and localStoreFileMode are the ownership store's permissions. The file
	// is 0600 and its directory 0750: the store names panes and sessions the controller owns,
	// so no other user has any business reading it.
	localStoreDirMode  = 0750
	localStoreFileMode = 0600
)

// DefaultLocalStateFile returns the ownership store's default path, i.e.
// ~/.local/state/a2a-worker-controller/local-workers.json.
func DefaultLocalStateFile(ctx context.Context) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(ctx, err, "resolve home directory failed")
	}
	return filepath.Join(home, localStateDir, localStateFileName), nil
}

// LocalConfig holds the local backend's settings. It carries no credential: a tab worker
// authenticates nothing — the caller reaches it through its pane, not through a URL.
type LocalConfig struct {
	// VaultDir is the vault root the workers' task files live in. It is also the directory
	// each worker's tab starts in, so the worker runs where its task lives.
	VaultDir string
	// ClaudeScript is the absolute path to the vault's claude_script launcher — the `cc-*`
	// script every worker's tab execs.
	ClaudeScript string
	// WindowID optionally names the WezTerm window a worker's tab opens in. Empty omits the
	// flag entirely, and the tab then lands in the window the spawning process is associated
	// with — which under launchd is the default window. It exists because the flag is part of
	// the spawn this backend mirrors; the controller has no role map to resolve one from, so
	// it is configured rather than derived.
	WindowID string
}

// LocalWorker is one worker the local backend owns, as the ownership store records it.
//
// The store is this backend's resource state. It exists because a WezTerm pane carries no
// labels: unlike a container, a pane cannot be asked which task it serves, so the mapping
// from worker to pane has to be written down somewhere the controller can read back after a
// restart.
type LocalWorker struct {
	WorkerID WorkerID `json:"worker_id"`
	Task     TaskName `json:"task"`
	// PaneID is the WezTerm pane the worker runs in. It is the worker's only handle.
	PaneID string `json:"pane_id"`
	// SessionID is the Claude Code session the worker's tab bound to the task, empty until
	// that binding lands.
	SessionID string `json:"session_id"`
	// CreatedAt is when the worker's tab was opened.
	CreatedAt time.Time `json:"created_at"`
}

// worker renders the stored worker as the control API's Worker shape.
//
// A2AURL is empty and that is the point: a tab worker serves no A2A endpoint, so the pane is
// the only handle a caller has on it. That is why pane_id and session_id exist on the shared
// schema at all — see the Worker doc comment in types.go.
func (w LocalWorker) worker() Worker {
	return Worker{
		WorkerID:  w.WorkerID,
		Task:      w.Task,
		A2AURL:    "",
		Backend:   BackendLocal,
		State:     WorkerStateRunning,
		PaneID:    w.PaneID,
		SessionID: w.SessionID,
	}
}

//counterfeiter:generate -o ../mocks/pkg-worker-store.go --fake-name PkgWorkerStore . WorkerStore

// WorkerStore persists the workers the local backend owns. It is injected, so the backend
// reads no global and a test drives it against a temporary file.
//
// List and Save are the whole surface: the backend reads the set, changes it, and writes it
// back under its own mutex, so there is no per-entry mutation to get wrong.
type WorkerStore interface {
	// List returns every worker the store holds, empty when the store does not exist yet.
	List(ctx context.Context) ([]LocalWorker, error)
	// Save replaces the store's contents with workers.
	Save(ctx context.Context, workers []LocalWorker) error
}

// localWorkerFile is the store's on-disk shape. It is an object rather than a bare array so a
// later field can be added without a reader having to guess what the array is.
type localWorkerFile struct {
	Workers []LocalWorker `json:"workers"`
}

// jsonWorkerStore is the WorkerStore that keeps the ownership record in a JSON file.
type jsonWorkerStore struct {
	path string
}

var _ WorkerStore = (*jsonWorkerStore)(nil)

// NewJSONWorkerStore creates the WorkerStore backed by the JSON file at path. A leading ~ is
// resolved against the home directory, so the default path can be passed through unchanged.
func NewJSONWorkerStore(path string) WorkerStore {
	return &jsonWorkerStore{path: expandHome(path)}
}

// List reads the store. A store that does not exist yet is an empty store rather than an
// error: the first create on a fresh machine has no file to read.
func (s *jsonWorkerStore) List(ctx context.Context) ([]LocalWorker, error) {
	//nolint:gosec // G304: the path is the controller's own configured state file, not input
	content, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []LocalWorker{}, nil
		}
		return nil, errors.Wrapf(ctx, err, "read worker store %q failed", s.path)
	}
	var file localWorkerFile
	if err := json.Unmarshal(content, &file); err != nil {
		return nil, errors.Wrapf(ctx, err, "parse worker store %q failed", s.path)
	}
	if file.Workers == nil {
		return []LocalWorker{}, nil
	}
	return file.Workers, nil
}

// Save writes the store, creating its directory if it does not exist.
//
// The file is written with 0600 and its directory with 0750. The record names panes and
// sessions the controller owns, and nothing outside this controller reads it, so the file is
// never world-readable.
func (s *jsonWorkerStore) Save(ctx context.Context, workers []LocalWorker) error {
	if workers == nil {
		workers = []LocalWorker{}
	}
	content, err := json.MarshalIndent(localWorkerFile{Workers: workers}, "", "  ")
	if err != nil {
		return errors.Wrap(ctx, err, "marshal worker store failed")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), localStoreDirMode); err != nil {
		return errors.Wrapf(ctx, err, "create worker store directory for %q failed", s.path)
	}
	//nolint:gosec // G306: 0600 is the point — the store is this controller's private state
	if err := os.WriteFile(s.path, append(content, '\n'), localStoreFileMode); err != nil {
		return errors.Wrapf(ctx, err, "write worker store %q failed", s.path)
	}
	return nil
}

//counterfeiter:generate -o ../mocks/pkg-wezterm-client.go --fake-name PkgWeztermClient . WeztermClient

// WeztermClient is the local backend's only door to WezTerm. It is injected, so the backend
// reads no global and a test drives the real code path with a fake that records the command
// line it was handed — no GUI and no pane required.
type WeztermClient interface {
	// Spawn opens a tab running inner as `bash -lc inner`, in the window windowID names when
	// it is not empty, and returns the new pane's id.
	Spawn(ctx context.Context, windowID string, inner string) (string, error)
	// PaneIDs returns the id of every pane the GUI currently has.
	PaneIDs(ctx context.Context) ([]string, error)
	// KillPane closes the pane paneID names.
	KillPane(ctx context.Context, paneID string) error
}

// execWeztermClient is the WeztermClient that shells out to the wezterm binary.
type execWeztermClient struct {
	binary string
}

var _ WeztermClient = (*execWeztermClient)(nil)

// NewWeztermCLIClient creates the WeztermClient that runs the wezterm binary at binary.
//
// binary is an absolute path — see DefaultWeztermBinary for why a name would not resolve
// under launchd.
func NewWeztermCLIClient(binary string) WeztermClient {
	return &execWeztermClient{binary: binary}
}

// Spawn opens a tab and returns its pane id. The pane id is the spawn's trimmed standard
// output, which is what wezterm documents as its success signal.
func (c *execWeztermClient) Spawn(
	ctx context.Context,
	windowID string,
	inner string,
) (string, error) {
	stdout, err := c.run(ctx, spawnArgs(windowID, inner))
	if err != nil {
		return "", err
	}
	paneID := strings.TrimSpace(stdout)
	if paneID == "" {
		return "", errors.Errorf(ctx, "wezterm cli spawn returned no pane id")
	}
	return paneID, nil
}

// PaneIDs lists the GUI's panes.
func (c *execWeztermClient) PaneIDs(ctx context.Context) ([]string, error) {
	stdout, err := c.run(ctx, []string{"cli", "list", "--format", "json"})
	if err != nil {
		return nil, err
	}
	return parsePaneIDs(ctx, stdout)
}

// KillPane closes a pane. Killing the pane is what stops the worker: the pane's process is
// the worker, so there is nothing else to stop.
func (c *execWeztermClient) KillPane(ctx context.Context, paneID string) error {
	_, err := c.run(ctx, []string{"cli", "kill-pane", "--pane-id", paneID})
	return err
}

// run runs one wezterm CLI invocation and returns its standard output.
//
// ⚠️ THE CHILD'S ENVIRONMENT CARRIES WEZTERM_UNIX_SOCKET, AND THAT IS NOT DECORATION. A
// launchd job has no WEZTERM_UNIX_SOCKET, so `wezterm cli` falls back to
// ~/.local/share/wezterm/sock — a separate mux server, not the GUI — and then answers with no
// panes rather than with an error. The socket is resolved per call rather than once at
// construction, because the controller starts at login and the GUI may start later, and
// because the GUI's socket changes every time it restarts.
func (c *execWeztermClient) run(ctx context.Context, args []string) (string, error) {
	socket, err := liveGuiSocket(ctx)
	if err != nil {
		return "", err
	}
	//nolint:gosec // G204: the binary is this backend's configured absolute path, not input
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Env = envWithSocket(os.Environ(), socket)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// args carries no credential by construction: a caller's environment travels in the
		// tab's command line and never on this argv, so this message is safe to emit.
		return "", errors.Wrapf(
			ctx,
			err,
			"wezterm %s failed: %s",
			strings.Join(args, " "),
			strings.TrimSpace(stderr.String()),
		)
	}
	return stdout.String(), nil
}

// weztermPane is the part of `wezterm cli list --format json` this backend reads. The list
// carries a pane's window, title and cwd as well; none of them is an ownership signal, so
// none of them is parsed.
type weztermPane struct {
	PaneID int `json:"pane_id"`
}

// parsePaneIDs reads the pane ids out of `wezterm cli list --format json`. The pane id is a
// JSON number there and a string on the control API's schema, so it is formatted rather than
// passed through.
func parsePaneIDs(ctx context.Context, stdout string) ([]string, error) {
	var panes []weztermPane
	if err := json.Unmarshal([]byte(stdout), &panes); err != nil {
		return nil, errors.Wrap(ctx, err, "parse wezterm pane list failed")
	}
	paneIDs := make([]string, 0, len(panes))
	for _, pane := range panes {
		paneIDs = append(paneIDs, strconv.Itoa(pane.PaneID))
	}
	return paneIDs, nil
}

// spawnArgs is the wezterm invocation that opens a worker's tab, mirroring the supervisor's
// own tab spawn: `wezterm cli spawn [--window-id N] -- bash -lc <inner>`.
func spawnArgs(windowID string, inner string) []string {
	args := []string{"cli", "spawn"}
	if strings.TrimSpace(windowID) != "" {
		args = append(args, "--window-id", strings.TrimSpace(windowID))
	}
	return append(args, "--", "bash", "-lc", inner)
}

// envWithSocket returns env with WEZTERM_UNIX_SOCKET set to socket.
//
// ⚠️ ANY INHERITED VALUE IS DROPPED FIRST, NOT APPENDED TO. The child environment is a plain
// slice, and a duplicate key leaves the first entry winning in the child's own `getenv` — so
// appending would keep a stale socket and discard the resolved one. A controller started by
// hand from a WezTerm tab inherits exactly such a value.
func envWithSocket(env []string, socket string) []string {
	const name = "WEZTERM_UNIX_SOCKET="
	entries := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, name) {
			continue
		}
		entries = append(entries, entry)
	}
	return append(entries, name+socket)
}

// weztermSocketName matches the per-GUI socket files WezTerm creates, gui-sock-<pid>.
var weztermSocketName = regexp.MustCompile(`^gui-sock-(\d+)$`)

// liveGuiSocket returns the newest live GUI socket under ~/.local/share/wezterm.
//
// Liveness is the pid in the file's own name: a socket whose GUI has exited is left behind by
// the kernel, so its presence proves nothing and its pid does. A pid recycled by an unrelated
// process reads as live, which is why the newest one is preferred — a stale file is old by
// definition. No live socket is an error rather than a fallback to the mux socket, because
// the mux socket is exactly the wrong server this resolution exists to avoid.
func liveGuiSocket(ctx context.Context) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(ctx, err, "resolve home directory failed")
	}
	dir := filepath.Join(home, localSocketDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", errors.Wrapf(ctx, err, "read wezterm socket directory %q failed", dir)
	}
	newest := ""
	var newestTime time.Time
	for _, entry := range entries {
		match := weztermSocketName.FindStringSubmatch(entry.Name())
		if match == nil || !processAlive(match[1]) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestTime) {
			newest = filepath.Join(dir, entry.Name())
			newestTime = info.ModTime()
		}
	}
	if newest == "" {
		return "", errors.Errorf(ctx, "no live wezterm gui socket in %q", dir)
	}
	return newest, nil
}

// processAlive reports whether the pid in a socket's name still names a live process. A
// process this controller may not signal is alive too, and signal 0 asks without delivering.
func processAlive(pidText string) bool {
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// shellQuote single-quotes a value for POSIX sh, so a task name containing a quote or a space
// reaches the tab as one argument.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// shellEnvExports renders env as the `export NAME='value'; ` prefixes a worker's command line
// is built with.
//
// ⚠️ `export`, and placed BEFORE the `cd`. A `VAR=value` prefix on the `cd` applies to that
// one command and is gone before the `exec` that starts the worker; and `bash -lc` sources the
// profile files first, so an export placed after the `cd` is the one a profile default
// overwrites.
//
// Sorted, because ranging a map in Go is deliberately randomised and an unsorted command line
// would make two identical creates produce different tab commands.
func shellEnvExports(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, name := range sortedKeys(env) {
		builder.WriteString("export " + name + "=" + shellQuote(env[name]) + "; ")
	}
	return builder.String()
}

// tabNameFor is a worker's tab name. The task-level dedup is what makes it unique — a second
// create for a task is refused — so no suffix is needed to disambiguate it.
func tabNameFor(task TaskName) string {
	return localTabNamePrefix + string(task)
}

// tabCommand builds the shell command a worker's tab runs, mirroring the supervisor's own tab
// spawn: the caller's environment exported, then the vault directory, then the vault's
// launcher with the tab name and the work prompt.
//
// The launcher is exec'd, so the tab's process IS the worker and closing the pane stops it.
//
// ⚠️ ON THIS PATH A CALLER'S ENVIRONMENT VALUES TRAVEL IN ARGV, WHICH THE DOCKER BACKEND GOES
// OUT OF ITS WAY TO PREVENT. The two are not inconsistent, they are constrained differently:
// `docker run` has `-e NAME` with the value in the child's environment, while
// `wezterm cli spawn` has no environment flag at all — the command line is the only channel,
// which is why the supervisor's own tab path exports values the same way and why that path
// documents them as visible in the host process table for the pane's lifetime. So a caller
// must not hand this backend a secret it would not accept appearing in `ps`; the reserved-name
// guard and the shell-name guard below are what stop a name from becoming syntax, and neither
// can hide a value.
func tabCommand(config LocalConfig, env map[string]string, task TaskName) string {
	return shellEnvExports(env) +
		"cd " + shellQuote(expandHome(config.VaultDir)) +
		" && exec " + shellQuote(expandHome(config.ClaudeScript)) +
		" -n " + shellQuote(tabNameFor(task)) +
		" " + shellQuote(localWorkPrompt+shellQuote(string(task)))
}

// localEnvName matches a legal shell variable name.
var localEnvName = regexp.MustCompile(localEnvNamePattern)

// validateLocalEnv refuses a caller-supplied environment this backend cannot export safely.
//
// It is stricter than the docker backend's guard, and the difference is not cosmetic. Docker
// receives each name as one argv element, so a name is only ever a name. This backend renders
// the same entries into a SHELL COMMAND LINE, where a name containing a space, a semicolon or
// a backtick is shell syntax — `{"X; rm -rf /": "1"}` would export `X` and then run the rest.
// The values are single-quoted and therefore safe; the names are not, so they are refused
// rather than quoted.
func validateLocalEnv(ctx context.Context, extraEnv map[string]string) error {
	if err := validateExtraEnv(ctx, extraEnv); err != nil {
		return err
	}
	for _, name := range sortedKeys(extraEnv) {
		if !localEnvName.MatchString(name) {
			return errors.Errorf(
				ctx,
				"env %q is not a valid shell variable name and cannot be exported to a worker tab",
				name,
			)
		}
	}
	return nil
}

// localBackend runs each worker as a WezTerm tab on this Mac, started with the vault's own
// claude_script launcher.
//
// It is the third implementation of WorkerBackend and the first that is not a remote
// workload: a tab worker serves no A2A endpoint, so the pane is the handle and this backend
// reports it. Everything above the seam still names no backend.
//
// Ownership lives in an injected store rather than in the backend, because a WezTerm pane
// cannot carry a label the way a container can. The store is the backend's resource state and
// the live panes are the truth: every call reconciles the two, so a pane killed by hand stops
// being a worker without anything having to be told about it.
type localBackend struct {
	client WeztermClient
	store  WorkerStore
	config LocalConfig
	// mutex serializes every call, because each is a read-then-act pair over the store: create
	// reads to dedup before it spawns, and list and stop both read the set before writing it
	// back. Two overlapping creates for one task would otherwise open two tabs. It closes that
	// within this process only — two controllers sharing one store still race — which is why
	// create reconciles against the live panes rather than trusting its own reading.
	mutex sync.Mutex
}

var _ WorkerBackend = (*localBackend)(nil)

// NewLocalBackend creates the WorkerBackend that opens each worker as a WezTerm tab.
//
// client is the backend's only door to WezTerm and store is its ownership record; both are
// injected, so the backend reads no global and a test drives it without a GUI. config carries
// the vault directory and the launcher every tab runs.
func NewLocalBackend(client WeztermClient, store WorkerStore, config LocalConfig) WorkerBackend {
	return &localBackend{client: client, store: store, config: config}
}

// Create opens one worker tab for the requested task and returns it.
//
// ⚠️ A TASK THAT ALREADY HAS A LIVE WORKER IS REFUSED, NOT ANSWERED, exactly as the docker
// backend refuses: the reply names the existing worker's id, which is the only handle a
// caller has on a worker it cannot authenticate to, and `stop` resolves ownership from that
// id alone. The refusal's wording is the docker backend's verbatim, so a caller sees one
// refusal shape whichever backend answered — even though this backend mints no token to lose.
//
// The returned Token is EMPTY, and that is not a failure: a tab worker serves no A2A
// endpoint, so there is nothing to authenticate to. The pane id is the handle instead.
func (b *localBackend) Create(ctx context.Context, req CreateRequest) (*CreateResponse, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// Refused before any work, so a request carrying a reserved or unexportable name fails
	// fast rather than after a tab has been opened for a worker that must not start.
	if err := validateLocalEnv(ctx, req.Env); err != nil {
		return nil, err
	}

	workers, err := b.store.List(ctx)
	if err != nil {
		return nil, err
	}
	// ⚠️ RECONCILED BEFORE THE DEDUP. A worker whose pane died on its own is not a worker any
	// more, and leaving it in the set would refuse every later create for its task — naming a
	// worker whose stop then fails because there is no pane left to kill. The reconcile is
	// what keeps "stop and create another" a recovery a caller can actually complete.
	live, err := b.livePaneIDs(ctx)
	if err != nil {
		return nil, err
	}
	workers = reconcileWorkers(workers, live)
	if existing := findLocalWorkerByTask(workers, req.Task); existing != nil {
		return nil, errors.Errorf(
			ctx,
			"task %q already has worker %q, whose token this controller did not keep; "+
				"stop that worker and create another",
			req.Task,
			existing.WorkerID,
		)
	}

	workerID, err := newWorkerID(ctx)
	if err != nil {
		return nil, err
	}

	paneID, err := b.client.Spawn(ctx, b.config.WindowID, tabCommand(b.config, req.Env, req.Task))
	if err != nil {
		return nil, errors.Wrapf(ctx, err, "spawn worker tab for task %q failed", req.Task)
	}

	worker := LocalWorker{
		WorkerID:  workerID,
		Task:      req.Task,
		PaneID:    paneID,
		SessionID: b.boundSessionID(ctx, req.Task),
		CreatedAt: time.Now().UTC(),
	}
	if err := b.store.Save(ctx, append(workers, worker)); err != nil {
		return nil, err
	}
	return &CreateResponse{Worker: worker.worker(), Token: ""}, nil
}

// List returns every worker this controller owns whose pane is still open, reconciled against
// the live panes on every call.
//
// It never serves the store alone. A pane that is gone — killed by hand, or lost with the GUI
// — is dropped, and the store is rewritten without it, so the record converges on what is
// actually running rather than on what was once created.
func (b *localBackend) List(ctx context.Context) (*ListResponse, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	workers, err := b.store.List(ctx)
	if err != nil {
		return nil, err
	}
	live, err := b.livePaneIDs(ctx)
	if err != nil {
		return nil, err
	}
	reconciled := reconcileWorkers(workers, live)
	if len(reconciled) != len(workers) {
		if err := b.store.Save(ctx, reconciled); err != nil {
			return nil, err
		}
	}
	response := make([]Worker, 0, len(reconciled))
	for _, worker := range reconciled {
		response = append(response, worker.worker())
	}
	return &ListResponse{Workers: response}, nil
}

// Stop closes the worker's pane and removes it from the store, returning it as stopped.
//
// Ownership is resolved from the store: a worker id the store does not hold is refused and
// nothing is killed, so a stop can never close a pane this controller did not open. A worker
// the store does hold whose pane has already gone is NOT an error — there is nothing left to
// kill, so its record is removed and it is reported stopped, which keeps a caller's recovery
// from being blocked by a pane that died on its own.
func (b *localBackend) Stop(ctx context.Context, req StopRequest) (*StopResponse, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	workers, err := b.store.List(ctx)
	if err != nil {
		return nil, err
	}
	worker := findLocalWorkerByID(workers, req.WorkerID)
	if worker == nil {
		return nil, errors.Errorf(ctx, "worker %q is not owned by this controller", req.WorkerID)
	}
	live, err := b.livePaneIDs(ctx)
	if err != nil {
		return nil, err
	}
	if live[worker.PaneID] {
		if err := b.client.KillPane(ctx, worker.PaneID); err != nil {
			return nil, errors.Wrapf(ctx, err, "kill worker pane %q failed", worker.PaneID)
		}
	}
	remaining := make([]LocalWorker, 0, len(workers))
	for _, entry := range workers {
		if entry.WorkerID != worker.WorkerID {
			remaining = append(remaining, entry)
		}
	}
	if err := b.store.Save(ctx, remaining); err != nil {
		return nil, err
	}
	stopped := worker.worker()
	stopped.State = WorkerStateStopped
	return &StopResponse{Worker: stopped}, nil
}

// livePaneIDs returns the panes the GUI currently has, keyed by pane id.
func (b *localBackend) livePaneIDs(ctx context.Context) (map[string]bool, error) {
	paneIDs, err := b.client.PaneIDs(ctx)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "list wezterm panes failed")
	}
	live := make(map[string]bool, len(paneIDs))
	for _, paneID := range paneIDs {
		live[paneID] = true
	}
	return live, nil
}

// boundSessionID returns the session id the worker's own session bound to the task, or an
// empty string when it cannot be resolved.
//
// ⚠️ AN EMPTY RESULT IS THE ORDINARY CASE AT CREATE TIME AND MUST NOT BE FILLED IN BY GUESSING.
// The binding is written by the session the tab starts — the work prompt's own task-anchoring
// step — and the spawn returns as soon as the pane exists, which is before that session has
// even started. So this is a best-effort read of a value that usually lands a little later,
// and an unresolvable one is reported empty rather than derived from the tab name, from the
// task name or from a clock: a session id a caller cannot trust is worse than an absent one,
// because it resolves to nothing while looking like a handle.
func (b *localBackend) boundSessionID(ctx context.Context, task TaskName) string {
	sessionID, err := readTaskSessionID(ctx, b.config.VaultDir, task)
	if err != nil {
		// Never fatal: the tab is already open by this point, so failing the create would
		// orphan it — a live pane no caller was ever told about.
		slog.Warn("resolve worker session id failed", "task", task, "error", err)
		return ""
	}
	return sessionID
}

// readTaskSessionID reads the session id the task's own frontmatter records.
func readTaskSessionID(ctx context.Context, vaultDir string, task TaskName) (string, error) {
	path := filepath.Join(expandHome(vaultDir), DefaultLocalTasksDir, string(task)+".md")
	//nolint:gosec // G304: the path is the controller's own configured vault plus the task name
	content, err := os.ReadFile(path)
	if err != nil {
		return "", errors.Wrapf(ctx, err, "read task file %q failed", path)
	}
	return frontmatterValue(string(content), "claude_session_id"), nil
}

// frontmatterValue returns the value of key in a page's leading YAML frontmatter block, or an
// empty string when the block or the key is absent. Only the first block is read: everything
// after its closing delimiter is body text, where a `key: value` line means something else.
func frontmatterValue(content string, key string) string {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			return ""
		}
		name, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// expandHome resolves a leading ~ against the home directory, so a configured path may be
// written the way a shell writes it. Any other value is returned unchanged.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

// reconcileWorkers returns the workers whose pane is still open, in their stored order.
func reconcileWorkers(workers []LocalWorker, live map[string]bool) []LocalWorker {
	reconciled := make([]LocalWorker, 0, len(workers))
	for _, worker := range workers {
		if live[worker.PaneID] {
			reconciled = append(reconciled, worker)
		}
	}
	return reconciled
}

// findLocalWorkerByTask returns the worker serving task, or nil when none does.
func findLocalWorkerByTask(workers []LocalWorker, task TaskName) *LocalWorker {
	for index := range workers {
		if workers[index].Task == task {
			return &workers[index]
		}
	}
	return nil
}

// findLocalWorkerByID returns the worker carrying workerID, or nil when none does. Ownership
// is resolved from the store alone, so a stop can only ever name a pane this controller
// opened.
func findLocalWorkerByID(workers []LocalWorker, workerID WorkerID) *LocalWorker {
	for index := range workers {
		if workers[index].WorkerID == workerID {
			return &workers[index]
		}
	}
	return nil
}
