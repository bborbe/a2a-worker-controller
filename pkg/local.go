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
	"unicode"

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
	// DefaultLivenessFloor is how long Create waits for a pane it has just spawned to prove
	// itself alive before reporting the worker.
	//
	// ⚠️ IT IS A BOUNDED WAIT, NOT A DELAY, AND THE DIFFERENCE IS THE WHOLE GUARD. A launcher
	// that exits immediately — a bad path, a missing binary, a permissions error, or the
	// `claude: command not found` this was written for — leaves a pane id WezTerm has already
	// dropped, and the spawn call reports success either way. Nothing in the spawn reply
	// distinguishes the two, so waiting is the only way to tell them apart. The wait is paid on
	// every create and bought against a supervisor being told a worker started when its pane is
	// already dead.
	DefaultLivenessFloor = 1500 * time.Millisecond
	// localLivenessPollInterval is how often the pane set is re-read while waiting out the
	// floor. A pane that dies mid-wait is caught on the next tick rather than at the deadline,
	// so the failing case costs one interval instead of the whole floor.
	localLivenessPollInterval = 100 * time.Millisecond
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
	// script a worker's tab execs when neither the task nor its goals names one.
	ClaudeScript string
	// WindowID names the WezTerm window a worker's tab opens in when the task's role does not
	// resolve one, from LOCAL_WINDOW_ID. Empty omits the flag entirely, and the tab then lands in
	// the window the spawning process is associated with — which under launchd is the default
	// window.
	//
	// ⚠️ IT IS A FALLBACK, NOT THE RULE, AND IT IS THE WRONG SHAPE FOR THE RULE. A worker's
	// window comes from its task's `role:` resolved against the WezTerm role map (see
	// resolveRole), because one configured window cannot serve three roles: measured 2026-10-17,
	// two creates fifteen minutes apart landed in windows 2 and 0, both for `role: agent` rows, so
	// placement was luck. This value is used only when the role map has no usable entry, and the
	// fallback is logged rather than silent.
	WindowID string
	// RoleMapPath is the WezTerm role map a task's `role:` is resolved against. Empty uses
	// ~/.cache/wezterm-role-map.json, which is the file the WezTerm configuration itself writes.
	//
	// It exists so a test can drive role resolution against a temporary map; a deployment never
	// sets it, and an unusable map degrades to WindowID above rather than refusing a create.
	RoleMapPath string
	// LivenessFloor is how long Create waits for a freshly spawned pane to prove itself alive
	// before reporting the worker, overriding DefaultLivenessFloor. It is configuration rather
	// than a test hook: the wait is paid on every create, so a deployment whose launcher is
	// slower to settle can raise it. Zero or negative means DefaultLivenessFloor.
	LivenessFloor time.Duration
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
	// GuiID names the WezTerm GUI instance the pane belongs to: the live GUI socket's path,
	// which carries the GUI process's own pid. It is what makes PaneID an identity rather than
	// a bare counter — a pane id restarts from a low number when the mux is recreated, so after
	// a GUI restart a stale entry's id can name a pane this controller never opened.
	GuiID string `json:"gui_id"`
	// SessionID is the Claude Code session the worker's tab bound to the task, empty until
	// that binding lands.
	SessionID string `json:"session_id"`
	// CreateSessionID is the session id the task file ALREADY CARRIED when this worker was
	// created — in practice the id of an earlier session of the same task, because this
	// worker's own session cannot have started before its tab did.
	//
	// ⚠️ IT EXISTS SO THAT AN EARLIER SESSION'S ID IS NEVER REPORTED AS THIS WORKER'S. The
	// task-scoped `claude_session_id` frontmatter field is stamped by the vault when it is
	// empty and LEFT ALONE on every later binding, so once a worker has been stopped and
	// another created for the same task, the file still names the STOPPED worker's session —
	// and nothing ever rewrites it, so that value would otherwise be permanent. The snapshot
	// is what refreshSessionIDs measures against: a value that was already there when this
	// worker started cannot be its own, and a value that appeared afterwards can.
	CreateSessionID string `json:"create_session_id"`
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

// WorkerStore persists the workers the local backend owns. It is injected, so the backend
// reads no global and a test drives it against a temporary file.
//
// List and Save are the whole surface: the backend reads the set, changes it, and writes it
// back under its own mutex, so there is no per-entry mutation to get wrong.
//
// ⚠️ THE STORE IS SINGLE-WRITER BY CONTRACT, AND THAT IS A PROPERTY OF THE CONFIGURATION
// RATHER THAN OF THIS INTERFACE. Save replaces the file's whole contents, so two controllers
// pointed at one path would each overwrite the other's set: the last write wins and the other
// controller's workers are left running with no record naming them, which is exactly the
// state Stop cannot reach. The backend's mutex serializes its calls within ONE process and
// nothing serializes two processes, so one state file belongs to exactly one controller.
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
//
// ⚠️ NO LOAD-TIME RULE REPAIRS AN ENTRY WHOSE BASELINE IS EMPTY, AND THIS IS WHERE ONE WOULD
// HAVE TO GO. Such an entry cannot be told apart from an ordinary one: a worker created for a
// task that had not bound a session yet ALSO carries an empty baseline — the task file
// legitimately held nothing when that worker started — and its id arrives afterwards and must be
// reported, so treating an empty baseline as unverifiable would suppress the id of every
// ordinary worker. Clearing the stored id does not repair the entry either: the value is still
// in the task file, and refreshSessionIDs re-reads it and re-derives the very same answer on the
// next list. The only entries this could ever matter for are ones written before the baseline
// existed, and the feature is unreleased, so the store holds none.
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
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, localStoreDirMode); err != nil {
		return errors.Wrapf(ctx, err, "create worker store directory for %q failed", s.path)
	}
	// ⚠️ WRITTEN TO A TEMPORARY FILE AND RENAMED INTO PLACE, NEVER TRUNCATED IN PLACE. A plain
	// os.WriteFile truncates before it writes, so a crash — or a concurrent reader — between
	// the truncate and the last byte observes a half-written file: invalid JSON, which List
	// then refuses to parse, taking down create, list and stop until someone repairs the file
	// by hand. rename is atomic within a filesystem, so a reader sees the old file or the new
	// one and never a mixture.
	//nolint:gosec // G304: the directory is the controller's own configured state directory
	tmp, err := os.CreateTemp(dir, localStateFileName+".tmp-*")
	if err != nil {
		return errors.Wrapf(ctx, err, "create temporary worker store in %q failed", dir)
	}
	tmpName := tmp.Name()
	// A failure at any point below must not leave the temporary file behind. After a
	// successful rename this remove is a no-op, because the name has moved.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(localStoreFileMode); err != nil {
		_ = tmp.Close()
		return errors.Wrapf(ctx, err, "set worker store mode on %q failed", tmpName)
	}
	if _, err := tmp.Write(append(content, '\n')); err != nil {
		_ = tmp.Close()
		return errors.Wrapf(ctx, err, "write worker store %q failed", tmpName)
	}
	if err := tmp.Close(); err != nil {
		return errors.Wrapf(ctx, err, "close worker store %q failed", tmpName)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return errors.Wrapf(ctx, err, "replace worker store %q failed", s.path)
	}
	return nil
}

// WeztermClient is the local backend's only door to WezTerm. It is injected, so the backend
// reads no global and a test drives the real code path with a fake that records the command
// line it was handed — no GUI and no pane required.
type WeztermClient interface {
	// GuiID returns an identifier for the GUI instance the other three calls talk to — the
	// live GUI socket's path, which carries the GUI process's own pid. It differs after a GUI
	// restart, and that is the whole point of recording it: a pane id on its own is a per-mux
	// counter that restarts with the mux, so only a GUI id makes it interpretable.
	//
	// ⚠️ IT IS RESOLVED ONCE PER BACKEND CALL AND THEN PASSED TO EVERY OTHER METHOD, RATHER
	// THAN RESOLVED AGAIN INSIDE EACH. The resolution picks the NEWEST live socket, so two
	// resolutions within one call can name two different GUIs, and a GUI that restarts between
	// them would have a pane opened in one GUI recorded under the other's identity — which is
	// the single thing the recorded identity exists to prevent.
	GuiID(ctx context.Context) (string, error)
	// Spawn opens a tab running inner as `bash -lc inner`, in the GUI guiID names, in the
	// window windowID names when it is not empty, and returns the new pane's id.
	Spawn(ctx context.Context, guiID string, windowID string, inner string) (string, error)
	// PaneIDs returns the id of every pane the GUI guiID names has.
	PaneIDs(ctx context.Context, guiID string) ([]string, error)
	// KillPane closes the pane paneID names in the GUI guiID names.
	KillPane(ctx context.Context, guiID string, paneID string) error
	// SendText types text into the pane paneID names, submits it, and does not return until the
	// pane's own output shows marker.
	//
	// ⚠️ THE SEND'S EXIT CODE IS NOT THE CONFIRMATION, WHICH IS WHY marker EXISTS. `wezterm cli
	// send-text` reports that keystrokes reached the pty, and that is true whether or not the
	// message submitted — measured on the supervisor's own path, where a submit can be swallowed
	// outright (3 of 6 spawns, 2026-09-20) and a colour was reported as applied while it never was.
	// A marker that never appears is therefore an ERROR rather than a success.
	//
	// ⚠️ IT ACTIVATES THE PANE FIRST, AND THAT IS NOT OPTIONAL. `send-text` reaches a pane only
	// once its tab is activated — measured 2026-09-14, where two attempts against a running worker
	// did nothing and running `activate-tab` first made the same call land immediately. The
	// activation steals focus, which is why this capability is confined to the local backend's own
	// freshly-spawned panes and is never offered on a worker mid-run.
	SendText(ctx context.Context, guiID string, paneID string, text string, marker string) error
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
	guiID string,
	windowID string,
	inner string,
) (string, error) {
	stdout, err := c.run(ctx, guiID, spawnArgs(windowID, inner))
	if err != nil {
		return "", err
	}
	// ⚠️ THE FIRST LINE, NOT THE WHOLE TRIMMED OUTPUT. Anything wezterm prints after the id —
	// a warning, a deprecation note — would otherwise be folded into the pane id, and a pane
	// id that names no pane is not a visible error: the worker is recorded, and every later
	// reconcile reads it as a worker whose pane is gone.
	paneID := firstLine(stdout)
	if paneID == "" {
		return "", errors.Errorf(ctx, "wezterm cli spawn returned no pane id")
	}
	return paneID, nil
}

// PaneIDs lists the GUI's panes.
func (c *execWeztermClient) PaneIDs(ctx context.Context, guiID string) ([]string, error) {
	stdout, err := c.run(ctx, guiID, []string{"cli", "list", "--format", "json"})
	if err != nil {
		return nil, err
	}
	return parsePaneIDs(ctx, stdout)
}

// KillPane closes a pane. Killing the pane is what stops the worker: the pane's process is
// the worker, so there is nothing else to stop.
func (c *execWeztermClient) KillPane(ctx context.Context, guiID string, paneID string) error {
	_, err := c.run(ctx, guiID, []string{"cli", "kill-pane", "--pane-id", paneID})
	return err
}

// SendText activates the pane, waits for its input box, types text and submits it, then confirms
// the submission against the pane's own output.
//
// ⚠️ EVERY STEP IS A MEASURED REQUIREMENT RATHER THAN CEREMONY, and each one fails silently when
// dropped: without the activation the keystrokes reach nothing; without the readiness poll the text
// lands in a composer that is still painting its placeholder and is swallowed; without the carriage
// return the message is typed and never submitted; and without the marker poll a send that never
// took effect is reported as delivered. See claude-supervisor's server/tab.mjs, where all four were
// measured against live panes.
func (c *execWeztermClient) SendText(
	ctx context.Context,
	guiID string,
	paneID string,
	text string,
	marker string,
) error {
	// ⚠️ ACTIVATED BY PANE ID, NEVER BY TAB ID. A tab id is renumbered when its tab moves windows
	// (measured 2026-09-18: tabs 158/159/160 in window 0 became 163/164/165 in window 2, and
	// `activate-tab --tab-id 159` failed outright while `activate-pane --pane-id 239` worked). A
	// pane id survives the move, and it is the handle this backend already holds.
	if _, err := c.run(ctx, guiID, []string{"cli", "activate-pane", "--pane-id", paneID}); err != nil {
		return errors.Wrapf(ctx, err, "activate pane %q failed", paneID)
	}
	if err := c.waitUntilReady(ctx, guiID, paneID); err != nil {
		return err
	}
	// ⚠️ A CARRIAGE RETURN SUBMITS; A LINE FEED DOES NOT. "\n" types a line break into the input
	// box without submitting it, which from the caller's side looks exactly like a message that was
	// ignored.
	if _, err := c.run(ctx, guiID, []string{
		"cli", "send-text", "--pane-id", paneID, "--no-paste", text + "\r",
	}); err != nil {
		return errors.Wrapf(ctx, err, "send-text to pane %q failed", paneID)
	}
	return c.confirmInPane(ctx, guiID, paneID, marker)
}

// waitUntilReady polls until the pane's input box is drawn and EMPTY.
//
// ⚠️ AN EMPTY COMPOSER IS THE TEST, NOT A DRAWN ONE. The glyph alone appears too early: the TUI
// paints the composer with a placeholder suggestion before it will honour a submit, and a message
// sent into that phase is accepted and then swallowed — the text lands in the composer and the
// Enter does nothing (measured 2026-09-20: two spawns whose text arrived 81ms and 137ms after a
// placeholder sample both stranded unsubmitted, while the one that arrived against a plain prompt
// submitted). A fixed sleep is a race that fails on a loaded machine.
func (c *execWeztermClient) waitUntilReady(ctx context.Context, guiID string, paneID string) error {
	for attempt := 0; attempt < localPaneReadyAttempts; attempt++ {
		text, err := c.paneText(ctx, guiID, paneID, 0)
		if err == nil && composerEmpty(text) {
			return nil
		}
		if err := sleepContext(ctx, localPaneReadyInterval); err != nil {
			return err
		}
	}
	return errors.Errorf(
		ctx,
		"pane %q is not accepting input within %dms",
		paneID,
		(localPaneReadyAttempts*localPaneReadyInterval)/time.Millisecond,
	)
}

// confirmInPane polls the pane for marker, retrying the Enter when it does not appear.
//
// ⚠️ AN EMPTY COMPOSER IS DELIBERATELY NOT THE SUCCESS SIGNAL. A stranded message can also be
// discarded without ever submitting (observed 2026-09-20: the text sat unsubmitted for 14 minutes,
// then vanished, with the colour never applied), so a cleared composer proves nothing. Only the
// marker does. The retry is a bare Enter repeated until it takes — extra Enters on an empty composer
// are harmless — which is the remedy measured for a submit that was swallowed outright.
//
// ⚠️ THE READ CARRIES SCROLLBACK. A success marker is drawn once and then scrolls away, so a
// viewport-only read can miss a submission that really happened.
func (c *execWeztermClient) confirmInPane(
	ctx context.Context,
	guiID string,
	paneID string,
	marker string,
) error {
	for attempt := 0; attempt <= localConfirmRetries; attempt++ {
		if attempt > 0 {
			if _, err := c.run(ctx, guiID, []string{
				"cli", "send-text", "--pane-id", paneID, "--no-paste", "\r",
			}); err != nil {
				return errors.Wrapf(ctx, err, "retry the Enter into pane %q failed", paneID)
			}
		}
		for poll := 0; poll < localConfirmAttempts; poll++ {
			text, err := c.paneText(ctx, guiID, paneID, localPaneScrollbackLines)
			if err == nil && strings.Contains(text, marker) {
				return nil
			}
			if err := sleepContext(ctx, localConfirmInterval); err != nil {
				return err
			}
		}
	}
	return errors.Errorf(
		ctx,
		"pane %q never showed %q after %d attempts — the message did not take effect and must not "+
			"be reported as delivered",
		paneID,
		marker,
		localConfirmRetries+1,
	)
}

// paneText reads the pane's text, optionally reaching back into its scrollback.
func (c *execWeztermClient) paneText(
	ctx context.Context,
	guiID string,
	paneID string,
	scrollback int,
) (string, error) {
	args := []string{"cli", "get-text", "--pane-id", paneID}
	if scrollback > 0 {
		args = append(args, "--start-line", "-"+strconv.Itoa(scrollback))
	}
	return c.run(ctx, guiID, args)
}

// composerLine returns the last line the prompt glyph starts, or the empty string when there is
// none. It is the LAST one because every echoed submitted prompt above it carries the same glyph, so
// position — not the glyph alone — is what identifies the input box.
func composerLine(text string) string {
	lines := strings.Split(text, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if strings.HasPrefix(lines[index], localPromptGlyph) {
			return lines[index]
		}
	}
	return ""
}

// composerEmpty reports whether the pane's input box is drawn and holds nothing. A box already
// holding text is not ready either — this call must never press Enter into a line it did not write.
func composerEmpty(text string) bool {
	line := composerLine(text)
	if line == "" {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, localPromptGlyph)) == ""
}

// sleepContext waits for d, or returns as soon as ctx is done.
//
// A plain time.Sleep would keep polling a pane whose request has already been cancelled — and the
// poll is a subprocess, so the whole wait would outlive the caller that asked for it.
func sleepContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return errors.Wrap(ctx, ctx.Err(), "waiting interrupted")
	case <-time.After(d):
		return nil
	}
}

// GuiID returns the live GUI socket's path. The path carries the GUI process's pid, so it
// names one GUI instance and is a different value after a restart.
func (c *execWeztermClient) GuiID(ctx context.Context) (string, error) {
	dir, err := weztermSocketDir(ctx)
	if err != nil {
		return "", err
	}
	return liveGuiSocket(ctx, dir)
}

// run runs one wezterm CLI invocation against the GUI guiID names, and returns its standard
// output.
//
// ⚠️ THE CHILD'S ENVIRONMENT CARRIES WEZTERM_UNIX_SOCKET, AND THAT IS NOT DECORATION. A
// launchd job has no WEZTERM_UNIX_SOCKET, so `wezterm cli` falls back to
// ~/.local/share/wezterm/sock — a separate mux server, not the GUI — and then answers with no
// panes rather than with an error. The socket is resolved once per backend call rather than
// once at construction, because the controller starts at login and the GUI may start later,
// and because the GUI's socket changes every time it restarts; within one call every
// invocation is handed the same one, so they cannot address two different GUIs.
func (c *execWeztermClient) run(ctx context.Context, guiID string, args []string) (string, error) {
	//nolint:gosec // G204: the binary is this backend's configured absolute path, not input
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Env = envWithSocket(os.Environ(), guiID)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// ⚠️ THE ARGUMENTS AFTER `--` ARE DELIBERATELY ABSENT FROM THIS MESSAGE, AND THE
		// EARLIER CLAIM THAT args "carries no credential by construction" WAS FALSE ON THE
		// SPAWN PATH. `wezterm cli spawn` has no environment flag, so a caller's environment
		// travels in the tab's COMMAND LINE — which is this argv's last element. Joining the
		// whole argv into the error would persist the caller's values to the log and to Sentry
		// on any spawn failure. describeArgs stops at the `--` separator, so the message names
		// the operation and never the child's command.
		return "", errors.Wrapf(
			ctx,
			err,
			"wezterm %s failed: %s",
			describeArgs(args),
			firstLine(stderr.String()),
		)
	}
	return stdout.String(), nil
}

// describeArgs renders an argv for an error message, stopping at the `--` separator.
//
// ⚠️ IT IS A REDACTION, NOT A FORMATTING CHOICE. Everything after `--` is the child command a
// subcommand was asked to run — on the spawn path that is the tab's command line, which
// carries the caller's environment values (see tabCommand). Naming the operation alone is
// enough to diagnose a failure, and the dropped half is the half that must not be persisted.
func describeArgs(args []string) string {
	for index, arg := range args {
		if arg == "--" {
			return strings.Join(args[:index], " ")
		}
	}
	return strings.Join(args, " ")
}

// firstLine returns the first non-empty line of s, trimmed, or the empty string.
//
// ⚠️ WEZTERM'S STDERR IS DELIBERATELY REDUCED TO ONE LINE HERE. This text is joined into an
// error that the executor logs and that Sentry captures, so an unbounded diagnostic is
// persisted in full — while the line that actually names the failure ("window not found") is
// the first one, and the remainder is context that need not outlive the request. The stderr is
// wezterm's own output rather than caller-controlled, so this is a size bound rather than the
// redaction `describeArgs` performs; the two are separate concerns and neither replaces the
// other.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// weztermPane is the part of `wezterm cli list --format json` this backend reads. The list
// carries a pane's window, title and cwd as well; none of them is an ownership signal, so
// none of them is parsed.
type weztermPane struct {
	// ⚠️ A POINTER, SO AN ABSENT FIELD IS DISTINGUISHABLE FROM THE VALUE 0. As a plain int a
	// renamed or dropped field unmarshals to 0, and the pane list then reads as a GUI holding
	// one pane numbered "0" — every real pane missing, so every stored worker is judged dead
	// and erased. A field wezterm no longer sends must fail this call loudly instead.
	PaneID *int `json:"pane_id"`
}

// parsePaneIDs reads the pane ids out of `wezterm cli list --format json`. The pane id is a
// JSON number there and a string on the control API's schema, so it is formatted rather than
// passed through.
//
// A pane carrying no pane_id is an error rather than a skipped entry: the list is what decides
// which workers are alive, so an entry that cannot be read must not quietly shrink the set.
// An empty list is not an error — a GUI with no panes has none, and that answer is a real one.
func parsePaneIDs(ctx context.Context, stdout string) ([]string, error) {
	var panes []weztermPane
	if err := json.Unmarshal([]byte(stdout), &panes); err != nil {
		return nil, errors.Wrap(ctx, err, "parse wezterm pane list failed")
	}
	paneIDs := make([]string, 0, len(panes))
	for index, pane := range panes {
		if pane.PaneID == nil {
			return nil, errors.Errorf(
				ctx,
				"parse wezterm pane list failed: entry %d carries no pane_id",
				index,
			)
		}
		paneIDs = append(paneIDs, strconv.Itoa(*pane.PaneID))
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

// weztermSocketDir returns the directory WezTerm keeps its per-GUI sockets in,
// ~/.local/share/wezterm.
func weztermSocketDir(ctx context.Context) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(ctx, err, "resolve home directory failed")
	}
	return filepath.Join(home, localSocketDir), nil
}

// liveGuiSocket returns the newest live GUI socket in dir.
//
// Liveness is the pid in the file's own name: a socket whose GUI has exited is left behind by
// the kernel, so its presence proves nothing and its pid does. A pid recycled by an unrelated
// process reads as live, which is why the newest one is preferred — a stale file is old by
// definition. No live socket is an error rather than a fallback to the mux socket, because
// the mux socket is exactly the wrong server this resolution exists to avoid.
//
// The directory is a parameter rather than resolved here so a spec can drive this against a
// temporary directory: the alternative is a test that either needs a running GUI or asserts
// nothing about the selection rule at all.
func liveGuiSocket(ctx context.Context, dir string) (string, error) {
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

// guiAlive reports whether the GUI instance guiID names is still running.
//
// ⚠️ IT IS THE PREDICATE THAT SEPARATES A SECOND GUI FROM A RESTART, AND BOTH OF ITS HALVES ARE
// LOAD-BEARING. An entry whose GUI is not the current one cannot have its pane checked at all,
// so the only honest reading of it is "this controller cannot tell"; erasing it on that basis
// alone would destroy, irreversibly, the record of a worker a second, still-running GUI holds —
// and a pane with no record is a pane no stop can reach.
//
//   - The socket being GONE proves the GUI is gone: a restart removes it.
//   - The pid in the socket's own file name being DEAD proves it too, and is what stops a GUI
//     that died UNCLEANLY from wedging a task forever. The kernel leaves such a socket on disk,
//     so existence alone would keep its entries alive: never reported by list, because they
//     belong to another GUI, and still seen by create's dedup, which refuses every later create
//     for their tasks while the id that would clear them is reachable from nowhere a caller can
//     look.
//
// A GUI genuinely running alongside the current one satisfies both halves, so its records
// survive — which is the case neither half may mistake for a restart.
//
// ⚠️ IT CAN BE WRONG IN TWO DIRECTIONS, AND THEY DO NOT COST THE SAME:
//
//   - A pid RECYCLED by an unrelated process makes a dead GUI read as alive. Its entry is kept,
//     and a later stop attempts a kill through that socket, which fails and leaves the entry.
//     The failure mode is "entry kept", never "record destroyed" — the cheaper error, since a
//     record kept too long is removable while an erased one is not recoverable. This is the same
//     recycled-pid caveat liveGuiSocket already documents for socket selection.
//   - An os.Stat error other than absence — EACCES on the socket directory, say — also answers
//     false, so the entry is dropped. That is the destructive direction, and it is the one case
//     where this predicate erases something: an unreadable socket directory is not evidence that
//     the GUI is gone, only that this controller cannot check. It is accepted rather than
//     guarded because the same directory being unreadable also stops every other wezterm call,
//     so the entries it drops are ones no call in this process could act on anyway.
func guiAlive(guiID string) bool {
	if guiID == "" {
		return false
	}
	if _, err := os.Stat(guiID); err != nil {
		return false
	}
	match := weztermSocketName.FindStringSubmatch(filepath.Base(guiID))
	if match == nil {
		return false
	}
	return processAlive(match[1])
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
//
// ⚠️ IT IS NOT MADE UNIQUE THE WAY THE SUPERVISOR'S OWN TAB SPAWN MAKES A TAB NAME UNIQUE, and
// that is harmless here rather than an oversight. The supervisor's uniqueTabName exists because
// it opens several tabs for one task and later joins on the name; this backend opens at most one
// tab per task and never joins on a name at all — ownership is resolved from the store, by
// worker id and pane id. The name is for a human reading the tab bar.
func tabNameFor(task TaskName) string {
	return localTabNamePrefix + string(task)
}

// workPrompt is the message a worker's tab is started with: the work command, then the task
// name in DOUBLE quotes.
//
// ⚠️ DOUBLE QUOTES, NOT shellQuote, AND THE DIFFERENCE IS NOT COSMETIC. tabCommand single-quotes
// the whole prompt as one unit, so this inner quoting never reaches a shell — it is prompt TEXT,
// and it is the fleet's own convention: the supervisor spawns a tab worker with
// `/vault-cli:work-on-task "<task>"` (claude-supervisor `docs/fleet-surface.md` § Spawn a worker,
// `commands/open.md` § Create branch). shellQuote here would instead put a literal `'…'` into the
// prompt text — which is not what any other spawn site sends — and a task name containing an
// apostrophe would arrive mangled as `'it'\”s'`.
func workPrompt(task TaskName) string {
	return localWorkPrompt + `"` + string(task) + `"`
}

// tabCommand builds the shell command a worker's tab runs, mirroring the supervisor's own tab
// spawn: the caller's environment exported, then the vault directory, then the launcher with the
// tab name and the work prompt.
//
// The launcher is exec'd, so the tab's process IS the worker and closing the pane stops it.
//
// ⚠️ THE LAUNCHER IS A PARAMETER RATHER THAN A CONFIG FIELD, AND THAT IS WHAT MAKES A TASK'S OWN
// `launcher:` REACH THE TAB. It used to be read from the config here, which is why every tab ran
// the vault default however the task was declared. It arrives already resolved and expanded — see
// chooseLauncher — so the value this function quotes is the same file the executability check
// stat'ed and the same one the create reply reports.
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
func tabCommand(vaultDir string, launcher string, env map[string]string, task TaskName) string {
	return shellEnvExports(env) +
		"cd " + shellQuote(expandHome(vaultDir)) +
		" && exec " + shellQuote(launcher) +
		" -n " + shellQuote(tabNameFor(task)) +
		" " + shellQuote(workPrompt(task))
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

// validateTaskName refuses a task name this backend cannot turn into a task file path.
//
// ⚠️ THE NAME BECOMES A PATH COMPONENT, SO IT IS VALIDATED AS ONE. readTaskSessionID joins it
// into `<vault>/25 Tasks/<name>.md`, and the name arrives unvalidated from the control API —
// validateControlRequest checks only which operation field is set, never its contents. A name
// carrying a separator or a `..` therefore escapes the tasks directory: `../../.claude/settings`
// makes the controller read an arbitrary file and return its `claude_session_id` in the create
// reply. The name is also the tab's name, so the same characters would reach a shell-adjacent
// position there.
func validateTaskName(ctx context.Context, task TaskName) error {
	name := string(task)
	if strings.TrimSpace(name) == "" {
		return errors.Errorf(ctx, "task name must not be empty")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return errors.Errorf(
			ctx,
			"task %q is not a valid task name: it must contain no path separator and no %q",
			name,
			"..",
		)
	}
	// ⚠️ THE DOUBLE QUOTE IS REFUSED FOR THE WORK PROMPT, NOT FOR THE PATH, AND IT IS THE ONE
	// GAP IN AN OTHERWISE COMPLETE QUOTING STORY. workPrompt renders the name between unescaped
	// double quotes — that is the fleet's own prompt convention, and the quoting is never
	// shell-evaluated because tabCommand single-quotes the whole prompt as one argv element —
	// so a name containing `"` produces a prompt whose quoting is broken: `a"b` yields
	// `/vault-cli:work-on-task "a"b"`, which is not the command any other spawn site would
	// have sent. It is not an injection; it is a prompt that no longer says what it means, and
	// the name is refused rather than escaped because no vault task name needs one.
	if strings.ContainsRune(name, '"') {
		return errors.Errorf(
			ctx,
			"task %q is not a valid task name: it must contain no %s",
			name,
			`"`,
		)
	}
	// A control character is refused for the same reason and a second one: the name is also a
	// tab name, and a newline in it splits the tab bar entry rather than being displayed.
	if strings.ContainsFunc(name, unicode.IsControl) {
		return errors.Errorf(
			ctx,
			"task %q is not a valid task name: it must contain no control character",
			name,
		)
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
// ⚠️ AN ENTRY KEPT FOR A GUI THIS CONTROLLER CANNOT SEE COUNTS AS AN EXISTING WORKER HERE. That
// pane may still be running and cannot be checked, so refusing is the only answer that cannot
// start a second tab beside it; the alternative — treating an unverifiable worker as absent —
// is the one outcome the one-worker-per-task rule exists to prevent.
//
// The returned Token is EMPTY, and that is not a failure: a tab worker serves no A2A
// endpoint, so there is nothing to authenticate to. The pane id is the handle instead.
//
// ⚠️ The returned SessionID is EMPTY TOO, AND THAT IS ALSO BY CONSTRUCTION RATHER THAN A GAP.
// The session a worker binds to is written by that session, which the tab starts — after this
// call has already returned — so anything the task file holds now belongs to an EARLIER session
// of the same task. It is read once and kept as the worker's CreateSessionID, the baseline
// `list` measures a real binding against, and it is never reported as this worker's own.
func (b *localBackend) Create(ctx context.Context, req CreateRequest) (*CreateResponse, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// Refused before any work, so a request carrying a reserved, unexportable or path-shaped
	// value fails fast rather than after a tab has been opened for a worker that must not start.
	if err := validateTaskName(ctx, req.Task); err != nil {
		return nil, err
	}
	if err := validateLocalEnv(ctx, req.Env); err != nil {
		return nil, err
	}

	// ⚠️ BOTH RESOLUTIONS ARE REFUSED BEFORE ANY TAB IS OPENED. A launcher that is not an executable
	// file, or a role this backend does not know, must be discovered here rather than from a pane
	// that died on its own — after a worker had already been recorded as running.
	chosen, role, err := b.resolveTask(ctx, req.Task)
	if err != nil {
		return nil, err
	}

	workers, err := b.store.List(ctx)
	if err != nil {
		return nil, err
	}
	// ⚠️ RESOLVED ONCE AND THEN PASSED TO EVERY WEZTERM CALL BELOW. Resolving it again inside
	// the spawn would let a GUI restart between the two resolutions open the pane in one GUI
	// and record it under the other's identity — the one thing the recorded identity exists to
	// prevent, and the state that makes a live pane unreachable to a later stop.
	guiID, err := b.guiID(ctx)
	if err != nil {
		return nil, err
	}
	// ⚠️ RECONCILED BEFORE THE DEDUP. A worker whose pane died on its own is not a worker any
	// more, and leaving it in the set would refuse every later create for its task — naming a
	// worker whose stop then fails because there is no pane left to kill. The reconcile is
	// what keeps "stop and create another" a recovery a caller can actually complete.
	live, err := b.livePaneIDs(ctx, guiID)
	if err != nil {
		return nil, err
	}
	workers = reconcileWorkers(workers, live, guiID)
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

	// ⚠️ READ BEFORE THE SPAWN, SO THE GUARANTEE IS STRUCTURAL RATHER THAN A TIMING BET. This
	// worker's own session cannot have started, because the tab that starts it does not exist
	// yet — the read is on the far side of the spawn that creates it. Read after the spawn it
	// would hold only by timing, and a read that did catch this worker's own id would make that
	// id the baseline, which refreshSessionIDs then suppresses for the worker's whole life: a
	// value equal to the baseline is never accepted, so the one id the worker does have would be
	// the one id never reported.
	baselineSessionID := b.taskSessionID(ctx, req.Task)

	paneID, err := b.client.Spawn(
		ctx,
		guiID,
		b.roleWindowID(req.Task, role),
		tabCommand(b.config.VaultDir, chosen.Path, req.Env, req.Task),
	)
	if err != nil {
		// ⚠️ THE PANE ID IS GENUINELY UNKNOWN ON THIS PATH, WHICH IS WHY NONE IS NAMED HERE.
		// A spawn that reports failure can still have opened the pane: run uses
		// exec.CommandContext, so a cancelled request kills the wezterm CLI while the GUI it
		// had already asked for a tab keeps that tab. There is then a live pane with no id to
		// kill and none to report — a different case from the failed record below, where the
		// id is known and is named precisely because this one cannot be.
		return nil, errors.Wrapf(ctx, err, "spawn worker tab for task %q failed", req.Task)
	}

	// ⚠️ THE SPAWN REPLY CANNOT DISTINGUISH A LIVE PANE FROM ONE THAT ALREADY EXITED, SO IT IS
	// NOT TRUSTED FOR THAT. `wezterm cli spawn` answers with a pane id and succeeds whether or not
	// the tab's command survived: a launcher that dies at once — a bad path, a missing binary, a
	// permissions error — leaves an id WezTerm has already dropped, and the reply is otherwise
	// identical. Measured live 2026-10-17: a tab running the vault's `cc-private` hit
	// `claude: command not found` (exit 127) while Claude Code was auto-updating, and Create
	// reported a worker with `state: running` for the dead pane. The guard below is what turns
	// that into a failure instead of a success the caller has no reason to doubt.
	//
	// ⚠️ IT RUNS BEFORE recordWorker, SO THE FAILURE PATH WRITES NO STORE ENTRY. Recording first
	// and checking after would leave an entry for a pane that is already gone — the entry `list`
	// then reconciles away, which is a later observation rather than a failure signal at the
	// moment of the call, and by then the caller has been told a worker started.
	if err := b.awaitLivePane(ctx, guiID, paneID); err != nil {
		return nil, err
	}

	worker := LocalWorker{
		WorkerID: workerID,
		Task:     req.Task,
		PaneID:   paneID,
		GuiID:    guiID,
		// ⚠️ SessionID IS DELIBERATELY LEFT EMPTY AND CreateSessionID IS THE BASELINE IT MUST
		// DIFFER FROM. The baseline was read before the tab existed, so whatever the task file
		// held then belongs to an EARLIER session of the same task and must never be reported as
		// this worker's.
		CreateSessionID: baselineSessionID,
	}
	if err := b.recordWorker(ctx, guiID, workers, worker); err != nil {
		return nil, err
	}
	// ⚠️ THE COLOUR IS APPLIED AFTER THE RECORD, NOT BEFORE, AND THE ORDER IS LOAD-BEARING. The
	// store write is what makes a pane stoppable, so the window between the spawn and that write is
	// the only one in which a live tab can be orphaned; the colour send blocks for up to a few
	// seconds and must not widen it. A colour that fails to apply is logged and non-fatal — the
	// worker still has its task — see applyRoleColor.
	//
	// ⚠️ IT BLOCKS WHILE THIS BACKEND'S MUTEX IS HELD, WHICH IS A DELIBERATE TRADE RATHER THAN AN
	// OVERSIGHT. The send polls the pane, so a slow-starting TUI holds the lock for a few seconds and
	// a concurrent list waits behind it — but the alternative is worse: releasing the lock first
	// would let a stop kill the pane mid-send, and a create that returned before its colour landed
	// would report an applied colour it never confirmed.
	b.applyRoleColor(ctx, guiID, paneID, role)
	return &CreateResponse{
		Worker:         worker.worker(),
		Token:          "",
		Launcher:       chosen.Path,
		LauncherSource: string(chosen.Source),
	}, nil
}

// awaitLivePane waits out the liveness floor for the pane a create has just spawned, failing as
// soon as that pane is gone.
//
// ⚠️ THE FAILURE IS RETURNED AS EARLY AS IT CAN BE, SO THE COMMON CASE IS NOT THE SLOW ONE. A
// launcher that dies instantly is caught on the first read — one pane listing, not the whole
// floor — while only a pane that survives the floor pays for it. Both are the same check: the
// pane set is re-read on an interval until either the pane is absent or the floor is reached, so
// a launcher that dies a second after spawn is caught exactly like one that never started.
//
// ⚠️ THE FLOOR IS THE GUARD'S WHOLE EXTENT. It covers the window between the spawn returning and
// this reply being built, and nothing later: a pane that dies an hour into its worker's life is
// outside it by design, and `list`'s own reconcile is what answers for that one.
func (b *localBackend) awaitLivePane(ctx context.Context, guiID string, paneID string) error {
	floor := b.config.LivenessFloor
	if floor <= 0 {
		floor = DefaultLivenessFloor
	}
	// ⚠️ THE INTERVAL IS DERIVED FROM THE FLOOR, NOT FIXED. A fixed interval would make the
	// guard's reach a property of the constant rather than of the floor: at a floor shorter than
	// the interval the loop would read once and never again, so a launcher dying mid-wait would
	// be missed and the guard would silently degrade to "died before the first read". Deriving it
	// keeps a short floor — a spec's, or a deployment's — as well covered as the default.
	interval := min(floor/10, localLivenessPollInterval)
	if interval <= 0 {
		interval = time.Millisecond
	}
	deadline := time.NewTimer(floor)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		live, err := b.livePaneIDs(ctx, guiID)
		if err != nil {
			// ⚠️ A CHECK THAT FAILED IS NOT A PANE THAT DIED, AND THE DIFFERENCE DECIDES WHETHER
			// THE PANE IS CLOSED. A failed pane listing says nothing about this pane — it may be
			// perfectly alive — so returning here would leave a live tab that no caller was told
			// about, that `stop` cannot reach (it resolves ownership from the store, which has no
			// entry), and that a retry of the same create would duplicate, because the retry
			// finds no entry for the task and passes the dedup. That is the same hazard
			// recordWorker closes a pane for, so it is closed here too.
			b.closeUnrecordedPane(ctx, guiID, paneID)
			return err
		}
		if !live[paneID] {
			// ⚠️ THE PANE ID GOES AFTER THE PHRASE, NOT INSIDE IT. "pane exited immediately" is
			// the substring a caller matches on and the one the criterion names, so an id
			// interpolated between the words — `pane "pane-1001" exited immediately` — would
			// read as the same sentence to a human and fail every substring match.
			//
			// ⚠️ THIS IS THE ONE FAILURE THAT CLOSES NOTHING, AND THAT IS WHY IT IS THE ONE THE
			// CALLER MAY RETRY BLINDLY. The pane is already absent, so there is no tab to clean
			// up and no second tab a retry could open.
			return errors.Errorf(ctx, "pane exited immediately: %q", paneID)
		}
		select {
		case <-ctx.Done():
			// Same reasoning as the failed listing above: a cancelled wait reaches no verdict,
			// so the pane is closed rather than left behind unrecorded.
			b.closeUnrecordedPane(ctx, guiID, paneID)
			return errors.Wrapf(ctx, ctx.Err(), "await worker pane %q", paneID)
		case <-deadline.C:
			return nil
		case <-ticker.C:
		}
	}
}

// closeUnrecordedPane closes a pane a create has decided not to record.
//
// ⚠️ IT EXISTS SO A PANE IS NEVER LEFT OPEN AND UNRECORDED, AND IT MIRRORS recordWorker'S OWN
// CLEANUP FOR EXACTLY THAT REASON. The store is the only thing that makes a pane stoppable —
// Stop resolves ownership from it and refuses an id it does not hold — so a pane missing from
// the store is a live tab no caller was told about, no stop can reach, and any retry of the same
// create would duplicate, because that retry finds no entry for the task and passes the dedup.
//
// ⚠️ A FAILED KILL IS LOGGED, NOT RETURNED. The caller is already on an error path carrying an
// error of its own, and replacing it would lose the reason the create failed; the pane id goes
// into the log so an operator can close the tab by hand.
func (b *localBackend) closeUnrecordedPane(ctx context.Context, guiID string, paneID string) {
	if err := b.client.KillPane(ctx, guiID, paneID); err != nil {
		slog.Warn(
			"close unrecorded worker pane failed",
			"pane", paneID,
			"error", err,
		)
	}
}

// recordWorker appends the new worker to the store, closing its pane again when that write
// fails.
//
// ⚠️ THE PANE IS ALREADY OPEN BY THE TIME THIS RUNS, SO A FAILED WRITE MUST NOT LEAVE IT
// RUNNING UNRECORDED. The store is the only thing that makes a pane stoppable — Stop resolves
// ownership from it and refuses an id it does not hold — so a pane missing from the store is a
// live tab no caller was ever told about and no stop can reach, and a retry of the same create
// would find no entry for the task, pass the dedup, and open a SECOND tab for it. The pane is
// therefore closed again before the error is returned, and the error names it either way so an
// operator can close it by hand when the kill fails too.
func (b *localBackend) recordWorker(
	ctx context.Context,
	guiID string,
	workers []LocalWorker,
	worker LocalWorker,
) error {
	if err := b.store.Save(ctx, append(workers, worker)); err != nil {
		if killErr := b.client.KillPane(ctx, guiID, worker.PaneID); killErr != nil {
			slog.Warn(
				"close orphaned worker pane failed",
				"task", worker.Task,
				"pane", worker.PaneID,
				"error", killErr,
			)
			return errors.Wrapf(
				ctx,
				err,
				"record worker for task %q failed; pane %q left open",
				worker.Task,
				worker.PaneID,
			)
		}
		return errors.Wrapf(
			ctx,
			err,
			"record worker for task %q failed; pane %q was closed",
			worker.Task,
			worker.PaneID,
		)
	}
	return nil
}

// List returns every worker this controller owns whose pane is still open in the CURRENT GUI,
// reconciled against the live panes on every call.
//
// It never serves the store alone. A pane that is gone — killed by hand, or lost with the GUI
// — is dropped, and the store is rewritten without it, so the record converges on what is
// actually running rather than on what was once created.
//
// ⚠️ AN ENTRY OF ANOTHER GUI IS REPORTED AND KEPT, NEVER ERASED. This call cannot see that
// GUI's panes, so it cannot show such a worker to be dead — and that is the whole reason it
// stays in the store: erasing it would delete the only record naming a pane a still-running GUI
// holds. It is reported for the same reason, and the pair is what makes the record usable.
// Withholding the entry from the reply while keeping it in the store leaves the task it serves
// refused by create's dedup and reachable by no stop — an id a caller can never obtain, because
// a backend failure carries no detail. So the id is published, and Stop resolves it: a worker of
// another GUI is closed through that GUI's own socket.
//
// A worker's PaneID is meaningful only within the GUI this controller is running against, so a
// caller should address one by WorkerID and let this backend resolve the pane.
//
// ⚠️ IT IS ALSO WHERE A WORKER'S SESSION ID BECOMES AVAILABLE, WHICH IS WHY IT REFRESHES IT.
// Create cannot report it: the binding is written by the session the tab starts, and the spawn
// returns as soon as the pane exists — before that session has begun. So create reads the task
// file once, records what it finds as the baseline the worker's own binding must differ from,
// and reports no session at all; this call re-reads EVERY entry's task file and reports the id
// once the file holds a value that is not that baseline. A caller that needs the id reads list;
// that is what the README promises, and it is why create stays fast and non-blocking.
//
// ⚠️ A GUI THAT IS NOT RUNNING IS AN ERROR HERE, NOT AN EMPTY LIST. Every entry's liveness is
// the GUI's own answer, so with no GUI there is nothing to reconcile against, and an empty reply
// would say "no workers" for what is really "cannot tell". The error is the honest answer, and a
// caller that must distinguish the two reads this error rather than an empty list.
func (b *localBackend) List(ctx context.Context) (*ListResponse, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	workers, err := b.store.List(ctx)
	if err != nil {
		return nil, err
	}
	guiID, err := b.guiID(ctx)
	if err != nil {
		return nil, err
	}
	live, err := b.livePaneIDs(ctx, guiID)
	if err != nil {
		return nil, err
	}
	reconciled := reconcileWorkers(workers, live, guiID)
	reconciled, refreshed := b.refreshSessionIDs(ctx, reconciled)
	if len(reconciled) != len(workers) || refreshed {
		// ⚠️ LOGGED, NOT RETURNED. The reply is fully computable without this write: the
		// reconcile already decided what is running, and the store is only being brought into
		// line with that answer. Failing the call would report "cannot tell you" for a question
		// this call has just answered — on the fleet's polling path, and over a bookkeeping
		// write whose failure the next call retries anyway.
		if err := b.store.Save(ctx, reconciled); err != nil {
			slog.Warn("persist reconciled worker store failed", "error", err)
		}
	}
	// ⚠️ EVERY ENTRY RECONCILE KEPT IS REPORTED, INCLUDING THE ONES OF ANOTHER GUI. Leaving those
	// out looked like the careful answer and was the harmful one: the entry stays in the store,
	// so create's dedup still refuses that task, while the id that would clear it appears in no
	// reply and a backend failure reaches a caller without detail — a dead end for the task until
	// the other GUI exits. Reporting it makes the id obtainable, and Stop already closes such a
	// worker through its own socket, so the id is actionable as well. Nor is the running state a
	// claim this call cannot support: reconcile kept the entry precisely because its GUI is not
	// provably gone, and a worker this controller owns and cannot show to be dead is a worker.
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
//
// ⚠️ A WORKER OF ANOTHER, STILL-RUNNING GUI IS KILLED THROUGH THAT GUI'S OWN SOCKET, NOT
// UNTRACKED. The recorded GuiID is that GUI's socket, so the kill can reach a pane this call
// cannot list — and reaching it is the point: a record removed without the pane being closed
// leaves a live tab that nothing names any more. The record is removed without a kill only when
// the worker's GUI is provably gone, because then there is no pane left to close.
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
	guiID, err := b.guiID(ctx)
	if err != nil {
		return nil, err
	}
	live, err := b.livePaneIDs(ctx, guiID)
	if err != nil {
		return nil, err
	}
	// ⚠️ THE GUI ID IS CHECKED AS WELL AS THE PANE ID, AND THAT IS WHAT STOPS A STALE ENTRY FROM
	// KILLING A STRANGER'S PANE. A pane id restarts from a low number when the mux is recreated,
	// so after a GUI restart an entry recorded before the restart can carry an id that now names
	// a live pane this controller never opened; killing on the pane id alone would close it.
	switch {
	case worker.GuiID == guiID:
		// The pane is in the GUI this call is already talking to, so its own pane list is the
		// truth about it: a pane that is not there has nothing left to kill, and the entry is
		// removed without one.
		if live[worker.PaneID] {
			if err := b.killPane(ctx, guiID, worker); err != nil {
				return nil, err
			}
		}
	case guiAlive(worker.GuiID):
		// ⚠️ THE KILL GOES THROUGH THE WORKER'S OWN SOCKET, WHICH IS WHAT MAKES A STOP STOP THE
		// WORKER INSTEAD OF MERELY UNTRACKING IT. A worker of a GUI that is still running beside
		// this one is one this call cannot see, so it cannot be checked — but it can be reached:
		// the recorded GuiID is that GUI's socket, and kill-pane addressed to it closes the pane.
		// Removing the record instead would leave a live tab running with nothing naming it, which
		// is precisely the state no later call could ever clean up. The pane is not pre-checked
		// here because this GUI's pane list says nothing about that one; killPane re-reads THAT
		// GUI's panes when the kill fails, which is where "already gone" is decided.
		if err := b.killPane(ctx, worker.GuiID, worker); err != nil {
			return nil, err
		}
	default:
		// The GUI is provably gone, so the panes went with it and there is nothing to kill. The
		// record is removed — that is the restart case, and the one recovery that has to keep
		// working, since a caller cannot kill a pane whose GUI no longer exists.
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

// guiID returns the identity of the GUI instance the client is talking to.
//
// Every call resolves it afresh rather than caching it, for the reason the socket is resolved
// per call: the controller starts at login and the GUI may start later, and it changes its
// socket — and so its identity — every time it restarts.
func (b *localBackend) guiID(ctx context.Context) (string, error) {
	guiID, err := b.client.GuiID(ctx)
	if err != nil {
		return "", errors.Wrap(ctx, err, "resolve wezterm gui identity failed")
	}
	return guiID, nil
}

// livePaneIDs returns the panes the GUI guiID names has, keyed by pane id.
func (b *localBackend) livePaneIDs(ctx context.Context, guiID string) (map[string]bool, error) {
	paneIDs, err := b.client.PaneIDs(ctx, guiID)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "list wezterm panes failed")
	}
	live := make(map[string]bool, len(paneIDs))
	for _, paneID := range paneIDs {
		live[paneID] = true
	}
	return live, nil
}

// killPane closes a worker's pane, treating a pane that died on its own as already closed.
//
// ⚠️ A PANE THAT EXITED BETWEEN THE RECONCILE AND THE KILL IS NOT A FAILURE. wezterm reports
// the kill of a pane that is no longer there as an error, and returning it would fail a stop
// whose entire purpose — that pane being gone — has already been met, while leaving the entry
// in the store for a retry to remove. The pane list is re-read to tell that case from a kill
// that genuinely failed, where the pane is still there and the error is real.
//
// The re-read goes to guiID — the GUI the kill was addressed to — because that is the one whose
// pane list can answer the question. On the cross-GUI path guiID is the worker's own socket
// rather than this controller's, and that GUI's list is the only one that says anything about
// the pane.
func (b *localBackend) killPane(ctx context.Context, guiID string, worker *LocalWorker) error {
	if err := b.client.KillPane(ctx, guiID, worker.PaneID); err != nil {
		still, listErr := b.livePaneIDs(ctx, guiID)
		if listErr != nil || still[worker.PaneID] {
			return errors.Wrapf(ctx, err, "kill worker pane %q failed", worker.PaneID)
		}
		slog.Warn("worker pane was already gone when the kill failed", "pane", worker.PaneID)
	}
	return nil
}

// taskSessionID returns the session id the task's file currently records, or an empty string
// when it cannot be resolved. It is what Create records as a worker's CreateSessionID — the
// value that worker's own binding must differ from — and is never reported as a worker's own
// session.
//
// ⚠️ AN EMPTY RESULT IS THE ORDINARY CASE AND MUST NOT BE FILLED IN BY GUESSING. The binding
// is written by the session the tab starts — the work prompt's own task-anchoring step — and
// Create calls this BEFORE spawning that tab, so on a fresh spawn the file usually holds nothing
// at all. It is read once and synchronously, because one file read is not what the caller is
// waiting on; what it must not become is the worker's reported session, since anything already
// in the file at that point belongs to an earlier session of the same task.
//
// An unresolvable read is reported empty rather than derived from the tab name, from the task
// name or from a clock: a session id a caller cannot trust is worse than an absent one, because
// it resolves to nothing while looking like a handle.
func (b *localBackend) taskSessionID(ctx context.Context, task TaskName) string {
	sessionID, err := readTaskSessionID(ctx, b.config.VaultDir, task)
	if err != nil {
		// Never fatal: the tab is already open by this point, so failing the create would
		// orphan it — a live pane no caller was ever told about.
		slog.Warn("resolve task session id failed", "task", task, "error", err)
		return ""
	}
	return sessionID
}

// refreshSessionIDs resolves the session id of every worker from the task file it serves, and
// reports whether any of them changed.
//
// ⚠️ EVERY ENTRY IS RE-READ, INCLUDING THE ONES THAT ALREADY HAVE AN ID, AND A VALUE IS
// ACCEPTED ONLY WHEN IT DIFFERS FROM THAT ENTRY'S OWN CreateSessionID. Skipping a non-empty
// entry is exactly what made a stale id permanent: `claude_session_id` is a TASK-scoped
// frontmatter field, the vault stamps it only when it is empty and leaves it alone on every
// later binding, so the id a stopped worker's session wrote is still there when the next worker
// for that same task is created — and nothing rewrites it. Trusting the stored value meant the
// replacement worker reported the stopped worker's session for its whole life: a handle that
// looks valid and resolves to a session that ended. Measuring against the create-time snapshot
// instead accepts a value only when the file has changed since this worker started, which is
// the only way this worker's own binding can appear, and never accepts the value that was
// already there when it began.
//
// ⚠️ AN UNRESOLVED ID IS LEFT EMPTY, AND THAT IS NOT A GAP TO PAPER OVER. The binding is written
// by the worker's own session — the work prompt's task-anchoring step — and the supervisor
// records that `claude -n` sets the session record's `name` but writes NO `custom-title` line
// into the transcript, while work-on-task's session-connect resolves a session by scanning
// transcripts for `customTitle`. So the binding may never appear at all when the launcher is
// given `-n`, and this read then stays empty for the worker's whole life. Deriving one from the
// tab name, the task name or a clock would hand a caller a handle that resolves to nothing while
// looking like one, which is worse than an absent one.
func (b *localBackend) refreshSessionIDs(
	ctx context.Context,
	workers []LocalWorker,
) ([]LocalWorker, bool) {
	changed := false
	for index := range workers {
		sessionID, err := readTaskSessionID(ctx, b.config.VaultDir, workers[index].Task)
		if err != nil {
			// Never fatal: the entry's pane is live, so the worker is real whether or not its
			// task file can be read — and failing the whole list over one unreadable file
			// would hide every other worker.
			slog.Warn("resolve worker session id failed", "task", workers[index].Task, "error", err)
			continue
		}
		if sessionID == "" || sessionID == workers[index].CreateSessionID {
			continue
		}
		if workers[index].SessionID == sessionID {
			continue
		}
		workers[index].SessionID = sessionID
		changed = true
	}
	return workers, changed
}

// readTaskSessionID reads the session id the task's own frontmatter records.
func readTaskSessionID(ctx context.Context, vaultDir string, task TaskName) (string, error) {
	path := filepath.Join(expandHome(vaultDir), DefaultLocalTasksDir, string(task)+".md")
	//nolint:gosec // G304: the path is the controller's own configured vault plus the task name
	content, err := os.ReadFile(path)
	if err != nil {
		return "", errors.Wrapf(ctx, err, "read task file %q failed", path)
	}
	return frontmatterSessionID(string(content)), nil
}

// frontmatterSessionID returns the claude_session_id a page's leading YAML frontmatter block
// records, or an empty string when the block or the key is absent. Only the first block is read:
// everything after its closing delimiter is body text, where a `key: value` line means something
// else.
//
// The key is fixed rather than a parameter because it is the only one the controller reads — a
// parameter with one possible value is variation no caller uses.
func frontmatterSessionID(content string) string {
	const key = "claude_session_id"
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

// reconcileWorkers returns the entries that should remain in the store, in their stored order.
//
// ⚠️ A PANE ID MATCHES ONLY WITHIN ONE GUI INSTANCE. A pane id is a per-mux counter, and the mux
// is recreated when the GUI restarts, so the ids restart from a low number: an entry recorded
// before a restart can carry an id that now names an unrelated pane, and matching on the id
// alone would report a phantom worker and let a stop kill a pane this controller never opened.
// The recorded GUI id is what makes the two distinguishable.
//
// ⚠️ AN UNMATCHED GUI IS NOT ON ITS OWN A REASON TO DROP THE ENTRY. Two different things can be
// wrong with an entry and they must not be treated alike. A pane that is gone is decidable: its
// GUI is the current one, so the GUI's own pane list is the whole truth about it. A different
// GUI is not decidable at all — its panes are invisible from here — and this function's answer
// is what List persists, so dropping on a mismatch would ERASE the record of a worker a second,
// still-running GUI holds: irreversibly, and leaving a live tab that no stop can reach. The
// entry is therefore kept unless the GUI it names is provably gone, which is what guiAlive
// answers — and it answers it both ways round, so a restart's entries go while a second live
// GUI's stay.
func reconcileWorkers(workers []LocalWorker, live map[string]bool, guiID string) []LocalWorker {
	reconciled := make([]LocalWorker, 0, len(workers))
	for _, worker := range workers {
		if worker.GuiID == guiID {
			if live[worker.PaneID] {
				reconciled = append(reconciled, worker)
			}
			continue
		}
		if guiAlive(worker.GuiID) {
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
