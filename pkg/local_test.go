// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// The vault layout and the tab-name prefix, repeated here rather than read from the package
// so a rename is a test failure rather than a test that follows the code.
const (
	testTasksDir      = "25 Tasks"
	testGoalsDir      = "24 Goals"
	testTabNamePrefix = "⚙ "
	// testLivenessFloor is the floor the specs drive the backend with, far below the shipped
	// DefaultLivenessFloor. Every healthy create pays the floor in full, so the default would add
	// minutes across the suite; what these specs exercise is the mechanism, and the default's own
	// value is asserted directly rather than by waiting it out.
	testLivenessFloor = 40 * time.Millisecond
)

// testClaudeScript is the vault launcher a spec's backend is configured with.
//
// ⚠️ IT IS A PER-SPEC EXECUTABLE FILE UNDER A TEMPORARY DIRECTORY, NOT THE OPERATOR'S REAL
// `cc-private`. A create now STATS the launcher it resolved and refuses a value that is not an
// executable file, so a spec pointed at a real path would pass or fail according to what happens to
// be installed on the machine running it — and the one spec that asserts a REFUSAL needs a path that
// is reliably absent. The scripts live side by side, so the bare-name case resolves against this
// file's own directory, exactly as it does against the real one.
var testClaudeScript string

// The GUI identities the specs drive the backend through.
//
// ⚠️ THEY ARE PATHS UNDER A PER-SPEC TEMPORARY DIRECTORY RATHER THAN FIXED STRINGS, AND THEIR
// EMBEDDED PIDS ARE CHOSEN DELIBERATELY. "Is that GUI still running" is answered by two facts —
// its socket being on disk AND the pid in the socket's own file name being alive — so a spec
// that wants a second GUI running beside the current one must both create the file and use a pid
// that is really alive, while a spec that wants a GUI which died uncleanly must create the file
// and use a pid that is not.
var (
	// testGuiID is the live GUI, so its pid is this test process's own.
	testGuiID string
	// testGuiIDAfterRestart is the GUI that replaced it: a different socket, and therefore a
	// different identity for the panes behind it. It is only ever the CURRENT gui in a spec, so
	// its pid is never consulted.
	testGuiIDAfterRestart string
	// testGuiIDDeadProcess is a GUI that died uncleanly and left its socket behind: the file is
	// on disk, the pid in its name is not.
	testGuiIDDeadProcess string
)

// deadPID is a pid no process holds, so a socket named for it reads as a dead GUI. It is far
// above macOS's own pid limit, so the probe is refused rather than delivered to a stranger.
const deadPID = "4194304"

// errWezterm stands in for a wezterm CLI failure.
var errWezterm = errors.New("wezterm unavailable")

// errStore stands in for a failing ownership-store write.
var errStore = errors.New("worker store unwritable")

// spawnCall is one recorded spawn: the GUI it was asked to open the tab in, the window it was
// asked for, and the command line it was handed.
type spawnCall struct {
	guiID    string
	windowID string
	inner    string
}

// killCall is one recorded kill, with the GUI socket it was addressed to. The socket is the
// assertion that matters for a worker of another GUI: its pane has to be closed through THAT
// GUI's socket, not this controller's.
type killCall struct {
	guiID  string
	paneID string
}

// sendCall is one recorded send into a pane, with the marker the caller asked to confirm against.
// The marker is recorded rather than assumed, because it is the whole difference between a send
// that reports success and a send that was confirmed: a call site that dropped it would still pass
// a spec that only asserted the text.
type sendCall struct {
	guiID  string
	paneID string
	text   string
	marker string
	// deadline is how much time the context the caller handed the send had left when it arrived, or
	// zero when that context carried no deadline at all.
	//
	// ⚠️ IT IS RECORDED BECAUSE THE BOUND IS A PROPERTY OF THE CALL SITE AND IS INVISIBLE OTHERWISE.
	// applyRoleColor wraps its send in a deadline of its own so a slow pane cannot hold the backend's
	// mutex indefinitely, and a fake that ignored the context could not show it — the alternative
	// would be a spec that waits the real timeout out, which the suite cannot afford.
	deadline time.Duration
}

// fakeWeztermClient is the injected WeztermClient seam. It records every spawn and every kill
// and answers with the panes it holds, so a spec drives the real backend through the real code
// path without a GUI, a socket or a pane.
//
// A spawn adds its pane to the held panes, because that is what really happens: the pane it
// returns is open by definition. A spec that wants a pane to have gone sets panes itself, or
// kills it.
type fakeWeztermClient struct {
	panes  []string
	spawns []spawnCall
	killed []killCall
	sent   []sendCall
	guiID  string
	// guiErr makes the GUI identity unresolvable, which is how a spec reaches the branch a
	// missing GUI takes before any pane is touched.
	guiErr   error
	spawnErr error
	paneErr  error
	killErr  error
	// sendErr makes the colour send fail, which is how a spec reaches the branch that must log it
	// and still return a worker.
	sendErr error
	// paneErrAfterSpawn makes the pane listing fail only once a spawn has happened, which is how
	// a spec reaches the guard's own error path: the pre-spawn reconcile has to succeed for the
	// create to get that far, so paneErr alone would fail the wrong call.
	paneErrAfterSpawn error
	// killFailed records that a kill has failed, and panesAfterFailedKill is what the pane list
	// then answers instead. Together they model the race the stop path covers: the pane exited
	// on its own between the reconcile and the kill, so wezterm's kill fails and a re-read of
	// the panes finds nothing.
	killFailed           bool
	panesAfterFailedKill []string
	// onSpawn runs inside Spawn, before it answers. It is how a spec models what really happens
	// on the other side of a spawn: the tab's session starts and writes its binding into the
	// task file, which is the event a baseline read placed after the spawn would swallow.
	onSpawn func()
	// paneDeathAfter models a launcher that dies on its own: past this delay since the spawn the
	// pane is HELD — it still exists, but LivePaneIDs drops it. Zero leaves it live throughout.
	paneDeathAfter time.Duration
	// spawnedAt is when Spawn last answered; paneDeathAfter is measured from it.
	spawnedAt time.Time
}

// Spawn records the GUI it was asked for as well as the window and command line, so a spec can
// assert that the identity the worker is recorded under is the one the tab was opened in.
func (f *fakeWeztermClient) Spawn(
	_ context.Context,
	guiID string,
	windowID string,
	inner string,
) (string, error) {
	f.spawns = append(f.spawns, spawnCall{guiID: guiID, windowID: windowID, inner: inner})
	if f.onSpawn != nil {
		f.onSpawn()
	}
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	paneID := "pane-" + strconv.Itoa(1000+len(f.spawns))
	f.panes = append(f.panes, paneID)
	f.spawnedAt = time.Now()
	return paneID, nil
}

func (f *fakeWeztermClient) PaneIDs(_ context.Context, _ string) ([]string, error) {
	if f.paneErr != nil {
		return nil, f.paneErr
	}
	if f.paneErrAfterSpawn != nil && !f.spawnedAt.IsZero() {
		return nil, f.paneErrAfterSpawn
	}
	if f.killFailed && f.panesAfterFailedKill != nil {
		return f.panesAfterFailedKill, nil
	}
	// ⚠️ A PANE WHOSE LAUNCHER DIED IS STILL LISTED HERE, AND THAT IS THE POINT: `Stop` reads
	// this list to decide whether to kill, so a held pane must appear or the tab is leaked.
	return f.panes, nil
}

// LivePaneIDs models the LIVENESS question, deliberately NOT the same list as PaneIDs: a pane
// whose launcher died is HELD, not gone — real wezterm keeps it open, so it exists and is
// closable but has no process. Modelling that death as VANISHING is what let v0.12.2 pass.
func (f *fakeWeztermClient) LivePaneIDs(_ context.Context, _ string) ([]string, error) {
	if f.paneErr != nil {
		return nil, f.paneErr
	}
	if f.paneErrAfterSpawn != nil && !f.spawnedAt.IsZero() {
		return nil, f.paneErrAfterSpawn
	}
	if f.paneDeathAfter > 0 && !f.spawnedAt.IsZero() &&
		time.Since(f.spawnedAt) >= f.paneDeathAfter {
		return nil, nil
	}
	return f.panes, nil
}

// GuiID answers with the GUI instance the fake is standing in for. It is a settable field, so a
// spec can restart the GUI mid-test by changing it — which is the whole reason the backend
// records it.
func (f *fakeWeztermClient) GuiID(_ context.Context) (string, error) {
	if f.guiErr != nil {
		return "", f.guiErr
	}
	return f.guiID, nil
}

// SendText records the send, including the marker the caller asked to be confirmed against. The
// real implementation blocks until the pane shows that marker; the fake records the request, so a
// spec asserts what the backend ASKED FOR rather than re-testing the confirmation loop, which the
// internal specs cover directly.
func (f *fakeWeztermClient) SendText(
	ctx context.Context,
	guiID string,
	paneID string,
	text string,
	marker string,
) error {
	var remaining time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	f.sent = append(f.sent, sendCall{
		guiID:    guiID,
		paneID:   paneID,
		text:     text,
		marker:   marker,
		deadline: remaining,
	})
	return f.sendErr
}

func (f *fakeWeztermClient) KillPane(_ context.Context, guiID string, paneID string) error {
	if f.killErr != nil {
		f.killFailed = true
		return f.killErr
	}
	f.killed = append(f.killed, killCall{guiID: guiID, paneID: paneID})
	panes := make([]string, 0, len(f.panes))
	for _, pane := range f.panes {
		if pane != paneID {
			panes = append(panes, pane)
		}
	}
	f.panes = panes
	return nil
}

// fakeWorkerStore is the injected WorkerStore seam, wrapping a real one so a spec can make a
// store operation fail while every other spec still reads and writes the same file.
//
// ⚠️ WITHOUT IT THE FAILURE PATHS ARE UNREACHABLE. A JSON store fails only when the disk is
// full or the path is unwritable, neither of which a spec can arrange — so the two paths that
// matter most, a failed write after a successful spawn and a failed bookkeeping write during a
// list, would otherwise be untested code.
type fakeWorkerStore struct {
	inner   pkg.WorkerStore
	listErr error
	saveErr error
}

func (f *fakeWorkerStore) List(ctx context.Context) ([]pkg.LocalWorker, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.inner.List(ctx)
}

func (f *fakeWorkerStore) Save(ctx context.Context, workers []pkg.LocalWorker) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	return f.inner.Save(ctx, workers)
}

// taskFile renders a task file with the given frontmatter, in the shape the backend reads.
func taskFile(sessionID string) string {
	content := "---\nstatus: in_progress\n"
	if sessionID != "" {
		content += "claude_session_id: " + sessionID + "\n"
	}
	return content + "---\n\nTags: [[Task]]\n"
}

// pageFile renders a vault page carrying exactly these frontmatter lines. It is how a spec declares
// a `launcher:`, a `role:` or a `goals:` field without a second YAML writer in the suite.
func pageFile(frontmatter ...string) string {
	return "---\n" + strings.Join(frontmatter, "\n") + "\n---\n\nTags: [[Task]]\n"
}

var _ = Describe("LocalBackend", func() {
	var (
		ctx         context.Context
		client      *fakeWeztermClient
		store       pkg.WorkerStore
		failing     *fakeWorkerStore
		vault       string
		roleMapPath string
		backend     pkg.WorkerBackend
	)

	const task = pkg.TaskName("local-backend-task")

	BeforeEach(func() {
		ctx = context.Background()
		vault = GinkgoT().TempDir()
		guiDir := GinkgoT().TempDir()
		testGuiID = filepath.Join(guiDir, "gui-sock-"+strconv.Itoa(os.Getpid()))
		testGuiIDAfterRestart = filepath.Join(guiDir, "gui-sock-2000")
		testGuiIDDeadProcess = filepath.Join(guiDir, "gui-sock-"+deadPID)
		// The vault launcher a spec's backend is configured with, created here so it is a real
		// executable file the create can stat and a real directory a bare `launcher:` name can
		// resolve beside. See testClaudeScript.
		testClaudeScript = filepath.Join(GinkgoT().TempDir(), "cc-private")
		Expect(os.WriteFile(testClaudeScript, []byte("#!/bin/sh\n"), 0o755)).To(BeNil())
		client = &fakeWeztermClient{guiID: testGuiID}
		store = pkg.NewJSONWorkerStore(filepath.Join(GinkgoT().TempDir(), "local-workers.json"))
		// ⚠️ AN EMPTY ROLE MAP IS THE DEFAULT, AND IT IS WHAT KEEPS EVERY PRE-EXISTING SPEC
		// MEANING WHAT IT MEANT. A role that does not resolve falls back to the configured window
		// and applies no colour, which is the behaviour those specs were written against. A spec
		// that wants a role to resolve writes its own map first.
		roleMapPath = filepath.Join(GinkgoT().TempDir(), "wezterm-role-map.json")
		Expect(os.WriteFile(roleMapPath, []byte("{}"), 0o600)).To(BeNil())
		// The backend is handed the wrapper, so every spec exercises the same code path a
		// failing store would take while the real store still holds the state it asserts on.
		failing = &fakeWorkerStore{inner: store}
		backend = pkg.NewLocalBackend(client, failing, pkg.LocalConfig{
			VaultDir:      vault,
			ClaudeScript:  testClaudeScript,
			LivenessFloor: testLivenessFloor,
			RoleMapPath:   roleMapPath,
		})
	})

	// writeTask puts a task file in the vault, so the backend's session-id read has something
	// to read. No file at all is the other case, and it is exercised on its own.
	writeTask := func(name pkg.TaskName, sessionID string) {
		dir := filepath.Join(vault, testTasksDir)
		Expect(os.MkdirAll(dir, 0o750)).To(BeNil())
		Expect(os.WriteFile(
			filepath.Join(dir, string(name)+".md"),
			[]byte(taskFile(sessionID)),
			0o600,
		)).To(BeNil())
	}

	// writePage puts a task file carrying exactly these frontmatter lines in the vault.
	writePage := func(name pkg.TaskName, frontmatter ...string) {
		dir := filepath.Join(vault, testTasksDir)
		Expect(os.MkdirAll(dir, 0o750)).To(BeNil())
		Expect(os.WriteFile(
			filepath.Join(dir, string(name)+".md"),
			[]byte(pageFile(frontmatter...)),
			0o600,
		)).To(BeNil())
	}

	// writeGoal puts a goal file in the vault's goals directory, so a task's `goals:` link has
	// something to resolve to.
	writeGoal := func(name string, launcher string) {
		dir := filepath.Join(vault, testGoalsDir)
		Expect(os.MkdirAll(dir, 0o750)).To(BeNil())
		frontmatter := []string{"page_type: goal"}
		if launcher != "" {
			frontmatter = append(frontmatter, "launcher: "+launcher)
		}
		Expect(os.WriteFile(
			filepath.Join(dir, name+".md"),
			[]byte(pageFile(frontmatter...)),
			0o600,
		)).To(BeNil())
	}

	// writeLauncher creates an executable file BESIDE THE VAULT LAUNCHER, which is where the cc-*
	// scripts really live. That is what makes the two spellings of one launcher testable: the
	// absolute path is what a `launcher:` may carry, and the bare name is what resolves there.
	writeLauncher := func(name string) string {
		path := filepath.Join(filepath.Dir(testClaudeScript), name)
		Expect(os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755)).To(BeNil())
		return path
	}

	Describe("Create", func() {
		It("opens a tab and records a running local worker", func() {
			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Worker.Task).To(Equal(task))
			Expect(response.Worker.WorkerID).NotTo(BeEmpty())
			Expect(response.Worker.Backend).To(Equal(pkg.BackendLocal))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateRunning))
			Expect(response.Worker.PaneID).To(Equal("pane-1001"))
			Expect(client.spawns).To(HaveLen(1))
		})

		// ⚠️ THESE MUST FAIL WITHOUT THE GUARD: each drives a launcher that dies on its own.
		Describe("a launcher that exits before its worker starts", func() {
			It("fails the create rather than reporting a running worker", func() {
				// Dies before the guard's first look: the launcher that never started at all — a
				// bad path, a missing binary, or `claude` unresolvable mid-update.
				client.paneDeathAfter = time.Nanosecond

				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("pane exited immediately"))
				// ⚠️ DISTINCT FROM THE SPAWN FAILURE, AND ASSERTED SO. A caller that cannot tell
				// "no tab was opened" from "the tab died at once" cannot decide whether a retry is
				// safe, and the two want opposite answers.
				Expect(err.Error()).NotTo(ContainSubstring("spawn worker tab for task"))
				Expect(response).To(BeNil())
				// ⚠️ ASSERTED BECAUSE A CUT OF THIS FIX LEAKED IT: this branch reads the LIVENESS
				// list, so a pane missing here is HELD — a real tab, unreachable by `Stop`.
				Expect(client.killed).To(HaveLen(1))
				Expect(client.killed[0].paneID).To(Equal("pane-1001"))
			})

			It("leaves no worker in the store, so a retry is not refused as a duplicate", func() {
				client.paneDeathAfter = time.Nanosecond

				_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
				Expect(err).NotTo(BeNil())

				// ⚠️ READ FROM THE STORE DIRECTLY, NOT FROM list. list reconciles dead panes away
				// on every call, so it answers "no worker" whether or not the guard ran — a
				// criterion resting on it could never fail.
				workers, listErr := store.List(ctx)
				Expect(listErr).To(BeNil())
				Expect(workers).To(BeEmpty())
			})

			It("catches a launcher that dies partway through the wait, not only at once", func() {
				// Dies inside the floor, so a single immediate read would pass it.
				//
				// ⚠️ HALF THE FLOOR, NOT THREE QUARTERS: ticks are floor/10 apart, so 3/4 leaves a
				// 10 ms window and a scheduling delay would fail a correct implementation.
				client.paneDeathAfter = testLivenessFloor / 2

				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("pane exited immediately"))
				Expect(response).To(BeNil())
			})

			It("still records a worker whose pane outlives the floor", func() {
				// ⚠️ THE CONTROL, AND IT IS NOT OPTIONAL. A guard that failed every create would
				// pass all three specs above and break every worker; without this one the suite
				// would read as green on a backend that never starts anything.
				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).To(BeNil())
				Expect(response.Worker.State).To(Equal(pkg.WorkerStateRunning))
				Expect(response.Worker.PaneID).To(Equal("pane-1001"))
				workers, listErr := store.List(ctx)
				Expect(listErr).To(BeNil())
				Expect(workers).To(HaveLen(1))
			})
		})

		// ⚠️ THE GUARD'S OWN ERROR PATH IS THE ONE THAT CAN ORPHAN A PANE. A failed listing is NOT
		// a dead pane — the guard reaches no verdict — so returning without closing leaves a live
		// tab no caller was told about, unreachable by `stop`, and duplicated by any retry.
		It(
			"closes the pane when the liveness check fails, rather than leaving it unrecorded",
			func() {
				client.paneErrAfterSpawn = errWezterm

				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).NotTo(BeNil())
				Expect(response).To(BeNil())
				Expect(client.killed).To(HaveLen(1))
				Expect(client.killed[0].paneID).To(Equal("pane-1001"))
				workers, listErr := store.List(ctx)
				Expect(listErr).To(BeNil())
				Expect(workers).To(BeEmpty())
			},
		)

		// ⚠️ THE SHIPPED FLOOR IS EXERCISED, NOT MERELY RESTATED: asserting the constant's own
		// value could not fail while it was unchanged. This runs the real default against a
		// launcher dying at ~1.2 s — the case SC1 names — and is the one spec paying that floor.
		It("catches a launcher that dies a second after spawn, at the shipped floor", func() {
			unconfigured := pkg.NewLocalBackend(client, failing, pkg.LocalConfig{
				VaultDir:     vault,
				ClaudeScript: testClaudeScript,
			})
			client.paneDeathAfter = 1200 * time.Millisecond

			response, err := unconfigured.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("pane exited immediately"))
			Expect(response).To(BeNil())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		It("reports no A2A URL and no token, because a tab worker has neither", func() {
			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			// ⚠️ Both are empty by construction, not by failure: a tab worker serves no A2A
			// endpoint, so the pane id is the handle a caller gets instead.
			Expect(response.Worker.A2AURL).To(BeEmpty())
			Expect(response.Token).To(BeEmpty())
			Expect(response.Worker.PaneID).NotTo(BeEmpty())
		})

		// The whole command line is asserted, escaping included.
		//
		// ⚠️ THE TASK NAME IN THE PROMPT IS DOUBLE-QUOTED, NOT SINGLE-QUOTED. The outer quoting
		// already makes the whole prompt ONE argv element by the time the launcher sees it, so
		// the inner quoting is prompt TEXT and never reaches a shell. The fleet's convention for
		// that text is double quotes — the supervisor spawns a tab worker with
		// `/vault-cli:work-on-task "<task>"` — so a worker opened here is started exactly like
		// one the supervisor opened.
		It("spawns the vault launcher with the tab name and the work prompt", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].inner).To(Equal(
				"cd '" + vault + "' && exec '" + testClaudeScript + "'" +
					" -n '" + testTabNamePrefix + string(task) + "'" +
					` '/vault-cli:work-on-task "local-backend-task"'`,
			))
		})

		// ⚠️ THIS IS WHAT THE OLD SINGLE-QUOTED FORM GOT WRONG. shellQuote's output is not
		// shell-evaluated inside the prompt, so `'it'\''s'` travelled to the launcher literally
		// and the task name arrived mangled. Double quoting leaves the apostrophe alone; the
		// `'\''` in the expectation below is the OUTER quoting, which is shell-evaluated and
		// correct.
		It("keeps an apostrophe in the task name intact in the work prompt", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: "don't-break"})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].inner).To(ContainSubstring(
				`'/vault-cli:work-on-task "don'\''t-break"'`,
			))
		})

		// The values are asserted IN the command line, and that is deliberate rather than an
		// oversight: `wezterm cli spawn` has no environment flag, so the command line is the
		// only channel a tab worker's environment has. A caller must therefore not pass this
		// backend a value it would not accept appearing in `ps` — see tabCommand's own comment.
		It(
			"exports the caller's environment before the cd, so a profile cannot overwrite it",
			func() {
				_, err := backend.Create(ctx, pkg.CreateRequest{
					Task: task,
					Env: map[string]string{
						"POD_ATTENTION_STORE_URL": "http://localhost:18081",
						"POD_ATTENTION_TOKEN":     "a-worker-token",
					},
				})

				Expect(err).To(BeNil())
				inner := client.spawns[0].inner
				Expect(inner).To(ContainSubstring(
					"export POD_ATTENTION_STORE_URL='http://localhost:18081'; ",
				))
				Expect(inner).To(ContainSubstring("export POD_ATTENTION_TOKEN='a-worker-token'; "))
				// The exports go first: `bash -lc` sources the profile files before the rest of the
				// command line runs, so an export after the cd is the one a profile default wins.
				Expect(strings.Index(inner, "export POD_ATTENTION_TOKEN")).To(BeNumerically(
					"<",
					strings.Index(inner, "cd "),
				))
			},
		)

		It("orders the exported names, so the command line is deterministic", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{
				Task: task,
				Env:  map[string]string{"ZULU": "1", "ALPHA": "2", "MIKE": "3"},
			})

			Expect(err).To(BeNil())
			inner := client.spawns[0].inner
			Expect(strings.Index(inner, "ALPHA")).To(BeNumerically(
				"<",
				strings.Index(inner, "MIKE"),
			))
			Expect(strings.Index(inner, "MIKE")).To(BeNumerically(
				"<",
				strings.Index(inner, "ZULU"),
			))
		})

		// The docker backend receives each name as one argv element, so a name is only ever a
		// name there. This backend renders the same entries into a shell command line, where a
		// name carrying a space or a semicolon is syntax rather than a variable.
		It("refuses an env name that is not a shell variable name", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{
				Task: task,
				Env:  map[string]string{"X; rm -rf /": "1"},
			})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("not a valid shell variable name"))
			Expect(client.spawns).To(BeEmpty())
		})

		It("refuses a caller-supplied name the controller sets itself", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{
				Task: task,
				Env:  map[string]string{"AGENT_TYPE": "headless"},
			})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("AGENT_TYPE"))
			Expect(client.spawns).To(BeEmpty())
		})

		// ⚠️ THE TASK NAME BECOMES A PATH COMPONENT, so a name that would leave the tasks
		// directory is refused before it can be read as one — otherwise a caller could name
		// `../../../../Users/bborbe/.claude/settings` and be handed that file's session id.
		It("refuses a task name that would become a path outside the tasks directory", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{
				Task: "../../../../Users/bborbe/.claude/settings",
			})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("not a valid task name"))
			Expect(client.spawns).To(BeEmpty())
		})

		It("refuses a task name containing a backslash or a bare dot-dot", func() {
			for _, name := range []pkg.TaskName{`..\..\settings`, "a..b"} {
				_, err := backend.Create(ctx, pkg.CreateRequest{Task: name})

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("not a valid task name"))
			}
			Expect(client.spawns).To(BeEmpty())
		})

		It("refuses an empty task name", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: "   "})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("must not be empty"))
			Expect(client.spawns).To(BeEmpty())
		})

		// The GUI is unreachable — it is not running, or its socket cannot be resolved. Nothing
		// can be spawned and nothing may be recorded.
		It("fails when the wezterm gui cannot be reached", func() {
			client.paneErr = errWezterm

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		// ⚠️ THE CREATE-TIME READ IS A BASELINE, NOT THIS WORKER'S SESSION. `claude_session_id`
		// is a TASK-scoped frontmatter field: the vault stamps it when it is empty and leaves it
		// alone on every later binding, so after a worker is stopped and another created for the
		// same task the file still names the STOPPED worker's session — and nothing rewrites it.
		// Reporting that value would hand the caller a handle that resolves to a session that
		// ended, which is worse than reporting none.
		It(
			"does not report a session id the task already carried before the worker existed",
			func() {
				writeTask(task, "9f2c4a11-1111-2222-3333-444455556666")

				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).To(BeNil())
				Expect(response.Worker.SessionID).To(BeEmpty())
				Expect(response.Worker.PaneID).NotTo(BeEmpty())
			},
		)

		// ⚠️ THE BASELINE IS READ BEFORE THE TAB IS OPENED, AND THIS IS WHAT PINS IT. The fake's
		// spawn writes the worker's own session id into the task file — which is what really
		// happens, a moment later — so a baseline read AFTER the spawn would swallow it: the value
		// would equal the baseline, and refreshSessionIDs never accepts a value equal to the
		// baseline, so the worker's own id would be the one id it never reports. Read before, the
		// id differs from the baseline and is reported normally.
		It(
			"reads the baseline before the tab exists, so the worker's own id is not swallowed",
			func() {
				client.onSpawn = func() { writeTask(task, "session-of-this-worker") }

				_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
				Expect(err).To(BeNil())
				workers, listErr := store.List(ctx)
				Expect(listErr).To(BeNil())
				Expect(workers).To(HaveLen(1))
				Expect(workers[0].CreateSessionID).To(BeEmpty())
				Expect(workers[0].SessionID).To(BeEmpty())

				response, err := backend.List(ctx)

				Expect(err).To(BeNil())
				Expect(response.Workers).To(HaveLen(1))
				Expect(response.Workers[0].SessionID).To(Equal("session-of-this-worker"))
			},
		)

		// The reachable path from the backend's own recovery advice — "stop that worker and
		// create another" — which under the old behaviour recorded the stopped worker's session
		// on its replacement, permanently, because a non-empty id was never re-read.
		It("does not carry a stopped worker's session id over to its replacement", func() {
			first, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			writeTask(task, "session-of-the-first-worker")
			refreshed, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(refreshed.Workers[0].SessionID).To(Equal("session-of-the-first-worker"))

			_, err = backend.Stop(ctx, pkg.StopRequest{WorkerID: first.Worker.WorkerID})
			Expect(err).To(BeNil())
			second, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			Expect(second.Worker.SessionID).To(BeEmpty())

			listed, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(listed.Workers).To(HaveLen(1))
			Expect(listed.Workers[0].WorkerID).To(Equal(second.Worker.WorkerID))
			// ⚠️ THE ASSERTION THAT MATTERS: the file still names the stopped worker's session,
			// and it must not be reported as the new worker's.
			Expect(listed.Workers[0].SessionID).To(BeEmpty())
		})

		It(
			"reports an empty session id rather than inventing one, and still opens the tab",
			func() {
				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).To(BeNil())
				Expect(response.Worker.SessionID).To(BeEmpty())
				Expect(response.Worker.PaneID).NotTo(BeEmpty())
			},
		)

		It("refuses a task that already has a worker, naming that worker's id", func() {
			first, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			// ⚠️ THE ID IS THE ASSERTION THAT MATTERS: it is the only handle a caller gets on a
			// worker it cannot authenticate to, and `stop` resolves ownership from that id alone.
			Expect(err).To(MatchError(ContainSubstring(string(first.Worker.WorkerID))))
			Expect(err).To(MatchError(ContainSubstring(string(task))))
			Expect(response).To(BeNil())
			// A duplicate create is a +0 delta: no second tab was opened.
			Expect(client.spawns).To(HaveLen(1))
		})

		It("does not dedup a different task onto an existing worker", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: "another-task"})

			Expect(err).To(BeNil())
			Expect(response.Worker.Task).To(Equal(pkg.TaskName("another-task")))
			Expect(client.spawns).To(HaveLen(2))
		})

		// The pane dying on its own must not refuse every later create for its task: a caller
		// whose worker was lost would otherwise be told to stop a worker whose pane is already
		// gone, and be stuck.
		It("opens a tab for a task whose earlier worker's pane is gone", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			client.panes = nil

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Worker.PaneID).To(Equal("pane-1002"))
			Expect(client.spawns).To(HaveLen(2))
		})

		It("records nothing when the spawn fails", func() {
			client.spawnErr = errWezterm

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		It("reports an error when the gui identity cannot be resolved", func() {
			client.guiErr = errWezterm

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		// ⚠️ THE WINDOW IS PART OF THE SPAWN, SO IT IS ASSERTED RATHER THAN ASSUMED. Nothing
		// else in the suite reads it: passing the empty string where the configured window
		// belongs — or dropping the flag entirely — would otherwise pass every other spec.
		//
		// The role map is the empty one, so no role resolves and the CONFIGURED window is what the
		// spawn carries. That is the fallback path rather than the ordinary one — see the Role
		// resolution specs for the window a resolved role produces.
		It("opens the tab in the configured window", func() {
			windowed := pkg.NewLocalBackend(client, failing, pkg.LocalConfig{
				VaultDir:      vault,
				ClaudeScript:  testClaudeScript,
				WindowID:      "7",
				LivenessFloor: testLivenessFloor,
				RoleMapPath:   roleMapPath,
			})

			_, err := windowed.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns).To(HaveLen(1))
			Expect(client.spawns[0].windowID).To(Equal("7"))
			// ⚠️ THE TAB IS OPENED IN THE GUI THE WORKER IS RECORDED UNDER. Resolving the
			// identity twice — once to reconcile, once inside the spawn — is what lets a GUI
			// restart in between record a pane under an identity that never opened it.
			Expect(client.spawns[0].guiID).To(Equal(testGuiID))
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(HaveLen(1))
			Expect(workers[0].GuiID).To(Equal(testGuiID))
		})

		// ⚠️ A FAILED WRITE AFTER A SUCCESSFUL SPAWN IS THE ONE PATH THAT CAN ORPHAN A LIVE TAB.
		// The pane is already open, and the store is the only thing that makes it stoppable —
		// Stop refuses an id the store does not hold — so leaving it there would produce a live
		// tab no caller was told about, and a retry of the same create would pass the dedup and
		// open a SECOND one. The pane is closed again, and named in the error either way.
		It("closes the tab again and names the pane when the worker cannot be recorded", func() {
			failing.saveErr = errStore

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(err.Error()).To(ContainSubstring("pane-1001"))
			Expect(client.killed).To(Equal([]killCall{{guiID: testGuiID, paneID: "pane-1001"}}))
			Expect(client.panes).To(BeEmpty())
		})

		It("names the pane as left open when closing it fails too", func() {
			failing.saveErr = errStore
			client.killErr = errWezterm

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			// The operator can close it by hand from this: the error says which pane, and says
			// that the controller could not.
			Expect(err.Error()).To(ContainSubstring("pane-1001"))
			Expect(err.Error()).To(ContainSubstring("left open"))
		})

		It("reports an error when the store cannot be read", func() {
			failing.listErr = errStore

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		// The task name reaches the work prompt between double quotes, so a quote in it would
		// break the prompt's own quoting; a control character would split the tab bar entry.
		It("refuses a task name containing a double quote or a control character", func() {
			for _, name := range []pkg.TaskName{`a"b`, "a\nb", "a\tb"} {
				_, err := backend.Create(ctx, pkg.CreateRequest{Task: name})

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("not a valid task name"))
			}
			Expect(client.spawns).To(BeEmpty())
		})
	})

	// ⚠️ THE LAUNCHER PRECEDENCE IS task → goal → vault `claude_script`, AND IT IS THE FLEET'S OWN
	// RULE (claude-supervisor `docs/fleet-surface.md` § Spawn a worker, item 8) RATHER THAN ONE THIS
	// BACKEND INVENTED. `spawn_agent` honours it; this backend used to exec the vault default for
	// every tab, so `/supervisor:open` — whose create branch routes an absent `target:` here —
	// silently opened a task on the wrong script.
	Describe("Launcher resolution", func() {
		It("runs the vault launcher when neither the task nor a goal names one", func() {
			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(testClaudeScript))
			Expect(response.LauncherSource).To(Equal("vault"))
			Expect(client.spawns[0].inner).To(ContainSubstring("exec '" + testClaudeScript + "'"))
		})

		It("runs the launcher the task names, and reports task as the source", func() {
			launcher := writeLauncher("cc-private-claude")
			writePage(task, "launcher: "+launcher)

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(launcher))
			Expect(response.LauncherSource).To(Equal("task"))
			Expect(client.spawns[0].inner).To(ContainSubstring("exec '" + launcher + "'"))
		})

		// The bare-name rule: the cc-* scripts live side by side, so a name resolves beside the
		// vault's own launcher rather than on PATH.
		It("resolves a bare launcher name beside the vault launcher", func() {
			writeLauncher("cc-private-claude")
			writePage(task, "launcher: cc-private-claude")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(
				filepath.Join(filepath.Dir(testClaudeScript), "cc-private-claude"),
			))
			Expect(response.LauncherSource).To(Equal("task"))
		})

		// ⚠️ THE STAT AND THE EXEC MUST NAME THE SAME FILE, AND THIS SPEC IS WHAT PINS IT. tabCommand
		// builds `cd <vaultDir> && exec <launcher>`, so the TAB resolves a relative launcher against
		// the VAULT directory, while the executability check resolves the same string against the
		// CONTROLLER PROCESS's working directory. A launcher written relative to the vault — which is
		// exactly what a path-qualified value is — was therefore stat'ed at a path the exec never ran:
		// refused for a file the exec would have found, or, the other way round, admitted for one it
		// would not. The launcher here exists ONLY under the vault, so without the fix the check fails
		// with "is not an executable file" while the tab command would have worked.
		It(
			"resolves a relative launcher against the vault directory, where the tab execs it",
			func() {
				Expect(os.WriteFile(
					filepath.Join(vault, "cc-relative"),
					[]byte("#!/bin/sh\n"),
					0o755,
				)).To(BeNil())
				writePage(task, "launcher: ./cc-relative")

				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).To(BeNil())
				Expect(response.Launcher).To(Equal(filepath.Join(vault, "cc-relative")))
				Expect(response.LauncherSource).To(Equal("task"))
				// The tab execs the same absolute path the check stat'ed, so the two cannot drift apart.
				Expect(client.spawns[0].inner).To(ContainSubstring(
					"exec '" + filepath.Join(vault, "cc-relative") + "'",
				))
			},
		)

		It("runs the launcher a goal names when the task names none", func() {
			launcher := writeLauncher("cc-private-claude")
			writeGoal("A Goal With A Launcher", launcher)
			writePage(task, "goals:", "    - '[[A Goal With A Launcher]]'")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(launcher))
			Expect(response.LauncherSource).To(Equal("goal"))
			Expect(client.spawns[0].inner).To(ContainSubstring("exec '" + launcher + "'"))
		})

		// ⚠️ A PATH-QUALIFIED GOAL LINK IS A REAL VAULT SHAPE, NOT A TYPO: 11 live tasks in `25 Tasks`
		// carry one — `goals: - "[[24 Goals/Build Security Review Agent]]"` and
		// `[[25 Tasks/Return to Live Trading]]` among them, measured 2026-10-18. It resolves against
		// the VAULT ROOT, so a rule that refused the separator made the goal rule skip and the worker
		// open on the vault default: the exact silent drop this feature exists to remove.
		It("resolves a path-qualified goal link against the vault root", func() {
			launcher := writeLauncher("cc-private-claude")
			writeGoal("A Goal With A Launcher", launcher)
			writePage(task, "goals:", "    - '[[24 Goals/A Goal With A Launcher]]'")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(launcher))
			Expect(response.LauncherSource).To(Equal("goal"))
		})

		// The other live shape points into `25 Tasks`, and it resolves by the same rule: the path is
		// relative to the vault root, whichever numbered folder it names.
		It("resolves a path-qualified goal link that points into the tasks directory", func() {
			launcher := writeLauncher("cc-private-claude")
			writePage(pkg.TaskName("Return to Live Trading"), "launcher: "+launcher)
			writePage(task, "goals:", "    - '[[25 Tasks/Return to Live Trading]]'")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(launcher))
			Expect(response.LauncherSource).To(Equal("goal"))
		})

		// The inline flow form is what 420 files in the live vault carry, and a goal name can
		// contain a comma — so this is the case a split-on-comma reader would get wrong.
		It("reads an inline goals list whose entry contains a comma", func() {
			launcher := writeLauncher("cc-private-claude")
			writeGoal("Supervisor Routes Workers to local, docker or k8s", launcher)
			writePage(task, "goals: ['[[Supervisor Routes Workers to local, docker or k8s]]']")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(launcher))
			Expect(response.LauncherSource).To(Equal("goal"))
		})

		// ⚠️ THE SCALAR SHAPE IS IN THE LIVE VAULT TOO — `goals: '[[Restore Trading Profitability
		// 2026Q2]]'`, two files measured 2026-10-18 — and a reader that required a LIST dropped the
		// goal entirely, falling through to the vault launcher for a task that did name one.
		It("reads a scalar goals value as a one-entry list", func() {
			launcher := writeLauncher("cc-private-claude")
			writeGoal("A Goal With A Launcher", launcher)
			writePage(task, "goals: '[[A Goal With A Launcher]]'")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(launcher))
			Expect(response.LauncherSource).To(Equal("goal"))
		})

		// ⚠️ A TASK NAMING ITS OWN LAUNCHER NEVER READS ITS GOALS, so a goal that cannot be
		// resolved cannot refuse a spawn the precedence says should open.
		//
		// ⚠️ THE GOALS HERE NAME TWO DIFFERENT LAUNCHERS, AND THAT IS WHAT MAKES THE SPEC
		// DISCRIMINATING. Naming one that merely does not exist proves nothing: a reader that read
		// the goals first would skip the missing file and still answer with the task's own launcher,
		// so the spec passed either way. Two goals naming different launchers is the input a
		// read-the-goals-first path REFUSES — "different launchers" — so only a code path that
		// honours the task's own launcher without consulting them can succeed here.
		It("does not read the goals when the task names its own launcher", func() {
			launcher := writeLauncher("cc-private-claude")
			writeGoal("Goal A", writeLauncher("cc-a"))
			writeGoal("Goal B", writeLauncher("cc-b"))
			writePage(
				task,
				"launcher: "+launcher,
				"goals:",
				"    - '[[Goal A]]'",
				"    - '[[Goal B]]'",
			)

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(launcher))
			Expect(response.LauncherSource).To(Equal("task"))
		})

		// A stale goal link is a normal vault state, and refusing on one would stop every task
		// carrying it from opening at all.
		It(
			"skips a goal link that does not resolve and falls through to the vault launcher",
			func() {
				writePage(task, "goals:", "    - '[[A Goal That Does Not Exist]]'")

				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).To(BeNil())
				Expect(response.Launcher).To(Equal(testClaudeScript))
				Expect(response.LauncherSource).To(Equal("vault"))
			},
		)

		// ⚠️ REFUSED, NOT RESOLVED BY FRONTMATTER ORDER. List order is not a precedence anyone
		// chose, so picking the first would be a silent guess.
		It("refuses two goals naming different launchers", func() {
			first := writeLauncher("cc-a")
			second := writeLauncher("cc-b")
			writeGoal("Goal A", first)
			writeGoal("Goal B", second)
			writePage(task, "goals:", "    - '[[Goal A]]'", "    - '[[Goal B]]'")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("different launchers"))
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		// Two goals naming the SAME launcher is one launcher, not a conflict.
		It("accepts two goals naming the same launcher", func() {
			launcher := writeLauncher("cc-private-claude")
			writeGoal("Goal A", launcher)
			writeGoal("Goal B", launcher)
			writePage(task, "goals:", "    - '[[Goal A]]'", "    - '[[Goal B]]'")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(launcher))
			Expect(response.LauncherSource).To(Equal("goal"))
		})

		// ⚠️ THE SHAPE GUARD IS A TRUST BOUNDARY: the value comes from frontmatter, which
		// supervised workers can write, and it is exec'd inside `bash -lc`.
		It("refuses a launcher carrying shell syntax", func() {
			for _, value := range []string{
				"cc-private; rm -rf /",
				"cc-private && curl evil",
				"$(whoami)",
				"cc private",
			} {
				writePage(task, "launcher: "+value)

				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("not a plain script name or path"))
				Expect(response).To(BeNil())
			}
			Expect(client.spawns).To(BeEmpty())
		})

		// ⚠️ THE GUARD APPLIES TO A GOAL'S LAUNCHER TOO, AND A GOAL IS THE EASIER PLACE TO MISS IT.
		// The value is read out of a DIFFERENT file from the one the caller named, so it is the
		// default route for any task that declares no launcher of its own — and goalLauncher
		// shape-checks each value BEFORE counting, so an unsafe one is refused for what it IS rather
		// than losing a count comparison to a sibling goal.
		It("refuses a goal whose launcher carries shell syntax", func() {
			writeGoal("A Goal With An Unsafe Launcher", "cc-private; rm -rf /")
			writePage(task, "goals:", "    - '[[A Goal With An Unsafe Launcher]]'")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("not a plain script name or path"))
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		It("refuses a launcher carrying a parent path segment", func() {
			writePage(task, "launcher: ../../bin/sh")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("path segment"))
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		// ⚠️ SC2, AND THE REFUSAL HAPPENS BEFORE ANY TAB IS OPENED. `wezterm cli spawn` reports
		// success for a tab whose `exec` then fails, so without this check a typo produces a
		// recorded worker whose pane is already gone — a create that reads as successful and a
		// worker that does not exist.
		It("refuses a launcher that is not an executable file, opening no tab", func() {
			writePage(task, "launcher: cc-private-claud")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("is not an executable file"))
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		It("refuses a launcher that is a directory", func() {
			writePage(task, "launcher: "+GinkgoT().TempDir())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("is not an executable file"))
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		It("refuses a launcher that exists but is not executable", func() {
			path := filepath.Join(GinkgoT().TempDir(), "cc-not-executable")
			Expect(os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644)).To(BeNil())
			writePage(task, "launcher: "+path)

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("is not an executable file"))
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		// ⚠️ AN UNREADABLE TASK FILE DEGRADES TO THE VAULT LAUNCHER RATHER THAN REFUSING, which is
		// the behaviour that existed before the field was honoured and the same treatment the
		// session-id read already gives it. A task file is not required for a tab to open.
		It("falls back to the vault launcher when the task file cannot be read", func() {
			response, err := backend.Create(ctx, pkg.CreateRequest{Task: "no-such-task"})

			Expect(err).To(BeNil())
			Expect(response.Launcher).To(Equal(testClaudeScript))
			Expect(response.LauncherSource).To(Equal("vault"))
		})
	})

	// ⚠️ THE ROLE DECIDES THE WINDOW AND THE CHIP, AND BOTH ARE PER-CREATE. A single configured
	// window cannot serve three roles: measured 2026-10-17, two creates fifteen minutes apart landed
	// in windows 2 and 0, both for `role: agent` rows, so placement was luck.
	Describe("Role resolution", func() {
		writeAgentMap := func() {
			Expect(os.WriteFile(roleMapPath, []byte(`{
				"Managers": {"chip": "orange", "window_id": 0},
				"Direct": {"chip": "cyan", "window_id": 1},
				"Agents": {"chip": "pink", "window_id": 2}
			}`), 0o600)).To(BeNil())
		}

		It("opens the tab in the role's window and applies the role's chip", func() {
			writeAgentMap()

			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].windowID).To(Equal("2"))
			Expect(client.sent).To(HaveLen(1))
			Expect(client.sent[0].text).To(Equal("/color pink"))
			// ⚠️ THE MARKER IS WHAT MAKES THE SEND A CONFIRMED ONE. A call site that dropped it
			// would report a colour as applied on the strength of an exit code that says only that
			// keystrokes reached the pty.
			Expect(client.sent[0].marker).To(Equal("Session color set to"))
			Expect(client.sent[0].paneID).To(Equal("pane-1001"))
			Expect(client.sent[0].guiID).To(Equal(testGuiID))
		})

		It("opens a manager's tab in the Managers window with the Managers chip", func() {
			writeAgentMap()
			writePage(task, "role: manager")

			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].windowID).To(Equal("0"))
			Expect(client.sent[0].text).To(Equal("/color orange"))
		})

		// The map's own vocabulary is not the task's: a window named "Direct" serves "human".
		It("opens a human's tab in the Direct window with the Direct chip", func() {
			writeAgentMap()
			writePage(task, "role: human")

			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].windowID).To(Equal("1"))
			Expect(client.sent[0].text).To(Equal("/color cyan"))
		})

		// ⚠️ AN ABSENT ROLE IS `agent`, which is what keeps every task that has never declared one
		// on the behaviour it had before.
		It("treats a task with no role as agent", func() {
			writeAgentMap()
			writePage(task, "status: in_progress")

			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].windowID).To(Equal("2"))
			Expect(client.sent[0].text).To(Equal("/color pink"))
		})

		// ⚠️ AN UNKNOWN ROLE IS REFUSED RATHER THAN DEFAULTED: a worker opened in the wrong window
		// with the wrong colour is discovered only by noticing it.
		It("refuses a role that is not one of the three", func() {
			writeAgentMap()
			writePage(task, "role: worker")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("is not a role"))
			Expect(response).To(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		// ⚠️ AN UNUSABLE MAP IS DEGRADED, NOT REFUSED — and the fallback is the CONFIGURED window,
		// not "no preference". Reading an unresolved role as no preference is what lands a tab in
		// whatever window the spawning process happens to be associated with.
		It("falls back to the configured window when the role map is missing", func() {
			Expect(os.Remove(roleMapPath)).To(BeNil())
			// ⚠️ testLivenessFloor IS CONFIGURED HERE LIKE EVERY OTHER SPEC. Leaving it out does not
			// fail — it silently pays the shipped 1500ms default, so this one spec costs the suite
			// more than the rest of the file combined.
			windowed := pkg.NewLocalBackend(client, failing, pkg.LocalConfig{
				VaultDir:      vault,
				ClaudeScript:  testClaudeScript,
				WindowID:      "7",
				LivenessFloor: testLivenessFloor,
				RoleMapPath:   roleMapPath,
			})

			response, err := windowed.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.LauncherSource).To(Equal("vault"))
			Expect(client.spawns[0].windowID).To(Equal("7"))
			// No colour is sent at all: a wrong role signal is worse than a missing one.
			Expect(client.sent).To(BeEmpty())
		})

		It("falls back when the role map is not valid json", func() {
			Expect(os.WriteFile(roleMapPath, []byte("not json"), 0o600)).To(BeNil())

			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].windowID).To(BeEmpty())
			Expect(client.sent).To(BeEmpty())
		})

		// A map entry that publishes no window is not an answer: reading a missing `window_id` as
		// window 0 would land the worker in the Managers window.
		It("falls back when the role's entry carries no window id", func() {
			Expect(
				os.WriteFile(roleMapPath, []byte(`{"Agents": {"chip": "pink"}}`), 0o600),
			).To(BeNil())

			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].windowID).To(BeEmpty())
			Expect(client.sent).To(BeEmpty())
		})

		// A null chip is a real answer — the live map's `Hold` entry carries one — and it means
		// send nothing rather than send a default.
		It("applies no colour when the role's entry publishes a null chip", func() {
			Expect(os.WriteFile(roleMapPath, []byte(
				`{"Agents": {"chip": null, "window_id": 2}}`,
			), 0o600)).To(BeNil())

			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].windowID).To(Equal("2"))
			Expect(client.sent).To(BeEmpty())
		})

		// ⚠️ THE CHIP IS TYPED INTO A LIVE PANE, SO A VALUE THAT IS NOT A PLAIN COLOUR NAME IS
		// REFUSED RATHER THAN SENT. The send appends a trailing `\r`, so a chip carrying a newline
		// would submit a second line the caller never wrote. The worker is still recorded — the
		// colour is cosmetic — but nothing is typed.
		It("sends no colour when the role map's chip is not a plain colour name", func() {
			Expect(os.WriteFile(roleMapPath, []byte(
				`{"Agents": {"chip": "pink\nrm -rf /", "window_id": 2}}`,
			), 0o600)).To(BeNil())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Worker.PaneID).To(Equal("pane-1001"))
			Expect(client.sent).To(BeEmpty())
		})

		// ⚠️ THE COLOUR STEP IS BOUNDED, AND THIS IS WHERE THE BOUND IS VISIBLE WITHOUT WAITING IT OUT.
		// Create applies the colour while holding the backend's mutex, and the send's own polls can
		// sum to ~11s, so a cosmetic step could otherwise serialize every other call. The fake records
		// the deadline the caller handed it; a call site that dropped the deadline would record zero.
		It("bounds the colour send with a deadline of its own", func() {
			writeAgentMap()

			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.sent).To(HaveLen(1))
			Expect(client.sent[0].deadline).To(BeNumerically(">", 0))
			Expect(client.sent[0].deadline).To(BeNumerically("<=", 10*time.Second))
		})

		// ⚠️ A COLOUR THAT FAILS TO APPLY IS LOGGED AND NON-FATAL: the worker still has its task,
		// so failing the create over a cosmetic difference would orphan a live pane the caller was
		// never told about.
		It("still records the worker when the colour cannot be applied", func() {
			writeAgentMap()
			client.sendErr = errWezterm

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Worker.PaneID).To(Equal("pane-1001"))
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(HaveLen(1))
		})

		// The colour is applied after the store write, which is what makes the pane stoppable. A
		// failed write must therefore leave no colour behind on a pane that is about to be killed.
		It("sends no colour when the worker cannot be recorded", func() {
			writeAgentMap()
			failing.saveErr = errStore

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.sent).To(BeEmpty())
		})
	})

	Describe("List", func() {
		It("returns the recorded worker as a running local worker", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(HaveLen(1))
			Expect(response.Workers[0].WorkerID).To(Equal(created.Worker.WorkerID))
			Expect(response.Workers[0].PaneID).To(Equal(created.Worker.PaneID))
			Expect(response.Workers[0].Backend).To(Equal(pkg.BackendLocal))
			Expect(response.Workers[0].State).To(Equal(pkg.WorkerStateRunning))
		})

		It("returns nothing when no worker was ever created", func() {
			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(BeEmpty())
		})

		// A pane killed by hand, or lost with the GUI, is not a worker any more — and the
		// store is rewritten without it, so the record converges on what is running.
		It("drops a worker whose pane is gone, from the reply and from the store", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			client.panes = nil

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(BeEmpty())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		It("keeps a worker whose pane is still open, and leaves the store alone", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(HaveLen(1))
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(HaveLen(1))
			Expect(workers[0].WorkerID).To(Equal(created.Worker.WorkerID))
		})

		// ⚠️ LIST IS WHERE THE SESSION ID BECOMES AVAILABLE, NOT CREATE. The binding is written
		// by the worker's own session, which starts after the spawn returns, so create records
		// what it can read at that instant — usually nothing — and list re-reads the task file
		// and persists what it finds. This is the contract the README states.
		It("fills in a session id that was not readable at create time, and persists it", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			Expect(created.Worker.SessionID).To(BeEmpty())

			writeTask(task, "9f2c4a11-1111-2222-3333-444455556666")
			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(HaveLen(1))
			Expect(response.Workers[0].SessionID).To(Equal("9f2c4a11-1111-2222-3333-444455556666"))
			// Persisted, not merely reported: the next list must not have to read the file again.
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(HaveLen(1))
			Expect(workers[0].SessionID).To(Equal("9f2c4a11-1111-2222-3333-444455556666"))
		})

		// ⚠️ THE BASELINE IS COMPARED AGAINST, NOT REMEMBERED AS THE ANSWER. A task file that
		// already named an earlier session when the worker started, and is then rewritten with a
		// different one, has changed since the worker began — so the new value is this worker's
		// own binding and is reported.
		It(
			"reports a session id that differs from the one the task carried at create time",
			func() {
				writeTask(task, "session-of-an-earlier-worker")
				_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
				Expect(err).To(BeNil())
				writeTask(task, "9f2c4a11-1111-2222-3333-444455556666")

				response, err := backend.List(ctx)

				Expect(err).To(BeNil())
				Expect(response.Workers).To(HaveLen(1))
				Expect(
					response.Workers[0].SessionID,
				).To(Equal("9f2c4a11-1111-2222-3333-444455556666"))
			},
		)

		// An unresolvable id stays empty rather than being invented — a handle that resolves to
		// nothing while looking like one is worse than an absent one.
		It("leaves the session id empty when the task file still records none", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(HaveLen(1))
			Expect(response.Workers[0].SessionID).To(BeEmpty())
		})

		// ⚠️ A PANE ID IS A PER-MUX COUNTER THAT RESTARTS WITH THE MUX, so an entry stored before
		// a GUI restart can carry an id that names a live pane of the NEW GUI. The recorded GUI
		// id is what stops that entry from being reported as a worker.
		//
		// The entry is erased here because its GUI is PROVABLY gone: the restarted GUI removed
		// the socket it was recorded under, and no file was created for it. That absence is the
		// only thing that justifies an erase — see the spec below, where a different GUI is live
		// and the entry must survive it.
		It("drops a worker whose GUI restarted, even when its pane id is live again", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			// The new GUI reuses the same pane id — exactly the collision the GUI id catches.
			client.guiID = testGuiIDAfterRestart
			client.panes = []string{created.Worker.PaneID}
			// The GUI's socket is gone, which is the fact that makes the entry provably dead.
			_, statErr := os.Stat(testGuiID)
			Expect(statErr).To(HaveOccurred())

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(BeEmpty())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		// ⚠️ A SECOND, STILL-RUNNING GUI MUST NOT COST THE FIRST ONE ITS RECORDS — NOR ITS
		// WORKER'S ID. With a second GUI live the resolved identity flips, and under the old rule
		// every stored worker was judged dead on that basis alone and erased, irreversibly, while
		// its tab kept running. Both halves of "that GUI is still running" hold here — its socket
		// is on disk and the pid in its name is alive — so the entry is kept.
		//
		// ⚠️ AND IT IS REPORTED. Withholding it looked like the careful answer and was the
		// harmful one: the entry stays in the store, so create still refuses that task, while the
		// id that would clear it appears in no reply and a backend failure reaches a caller with
		// no detail — a dead end until the other GUI exits. Reporting it makes the id obtainable,
		// and stop closes such a worker through its own socket, so it is actionable too.
		It("keeps and reports a worker whose GUI is still running beside the current one", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			Expect(os.WriteFile(testGuiID, nil, 0o600)).To(BeNil())
			client.guiID = testGuiIDAfterRestart

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(HaveLen(1))
			Expect(response.Workers[0].WorkerID).To(Equal(created.Worker.WorkerID))
			Expect(response.Workers[0].PaneID).To(Equal(created.Worker.PaneID))
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(HaveLen(1))
			Expect(workers[0].WorkerID).To(Equal(created.Worker.WorkerID))
		})

		// The recovery the reporting above exists to make possible: the id from list is the one
		// stop takes, and the pane it names is closed through the other GUI's own socket.
		It("stops the worker of another live GUI using the id that list reported", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			Expect(os.WriteFile(testGuiID, nil, 0o600)).To(BeNil())
			client.guiID = testGuiIDAfterRestart

			listed, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(listed.Workers).To(HaveLen(1))

			response, err := backend.Stop(
				ctx,
				pkg.StopRequest{WorkerID: listed.Workers[0].WorkerID},
			)

			Expect(err).To(BeNil())
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
			Expect(client.killed).To(Equal([]killCall{{
				guiID:  testGuiID,
				paneID: created.Worker.PaneID,
			}}))
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		// ⚠️ THE OTHER HALF OF THAT RULE, AND THE ONE THAT STOPS A STALE SOCKET WEDGING A TASK.
		// A GUI that dies UNCLEANLY leaves its socket on disk, so existence alone would keep its
		// entries alive forever: never reported by list, because they belong to another GUI, and
		// still seen by create's dedup, which refuses every later create for their tasks while
		// the id that would clear them is reachable from nowhere a caller can look. The dead pid
		// in the socket's own name is what says that GUI is gone.
		It("erases a worker whose GUI socket was left behind by a dead process", func() {
			// A worker recorded under a GUI that has since died UNCLEANLY: the kernel left the
			// socket file on disk, and the pid in its name is no longer a live process.
			Expect(os.WriteFile(testGuiIDDeadProcess, nil, 0o600)).To(BeNil())
			Expect(store.Save(ctx, []pkg.LocalWorker{{
				WorkerID: "w1",
				Task:     task,
				PaneID:   "1001",
				GuiID:    testGuiIDDeadProcess,
			}})).To(BeNil())
			client.guiID = testGuiIDAfterRestart

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(BeEmpty())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		// The other half of that rule: an entry kept because its GUI might still be running is
		// still an entry the dedup sees, so a second worker for the same task is refused rather
		// than opened alongside a pane this controller cannot check.
		It(
			"still refuses a second worker for a task whose worker belongs to another live GUI",
			func() {
				_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
				Expect(err).To(BeNil())
				Expect(os.WriteFile(testGuiID, nil, 0o600)).To(BeNil())
				client.guiID = testGuiIDAfterRestart

				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).To(MatchError(ContainSubstring(string(task))))
				Expect(response).To(BeNil())
				Expect(client.spawns).To(HaveLen(1))
			},
		)

		// ⚠️ AN ERROR, NOT AN EMPTY LIST: with no GUI there is nothing to reconcile against, and
		// an empty reply would report "no workers" for what is really "cannot tell".
		It(
			"reports an error rather than no workers when the wezterm gui cannot be reached",
			func() {
				_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
				Expect(err).To(BeNil())
				client.paneErr = errWezterm

				response, err := backend.List(ctx)

				Expect(err).NotTo(BeNil())
				Expect(response).To(BeNil())
			},
		)

		// ⚠️ A FAILED BOOKKEEPING WRITE MUST NOT HIDE AN ANSWER THE CALL ALREADY HAS. The
		// reconcile decided what is running; the write only brings the store into line with it,
		// and the next call retries it. Returning the error would report "cannot tell you" for a
		// question this call has just answered — on the fleet's polling path.
		It("still answers when the reconciled store cannot be written", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			client.panes = nil
			failing.saveErr = errStore

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(BeEmpty())
		})

		It("reports an error when the gui identity cannot be resolved", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			client.guiErr = errWezterm

			response, err := backend.List(ctx)

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
		})

		It("reports an error when the store cannot be read", func() {
			failing.listErr = errStore

			response, err := backend.List(ctx)

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
		})
	})

	Describe("Stop", func() {
		It("kills the pane and removes the entry", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).To(BeNil())
			Expect(response.Worker.WorkerID).To(Equal(created.Worker.WorkerID))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
			Expect(client.killed).To(Equal([]killCall{{
				guiID:  testGuiID,
				paneID: created.Worker.PaneID,
			}}))
			Expect(client.panes).To(BeEmpty())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		It("refuses a worker id the controller does not own, killing nothing", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: "not-ours"})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.killed).To(BeEmpty())
		})

		It("refuses when the store holds no worker at all", func() {
			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: "not-ours"})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.killed).To(BeEmpty())
		})

		// A worker whose pane died on its own is still the controller's to remove, and there is
		// nothing left to kill — refusing here would leave its task uncreatable and its record
		// behind.
		It("removes the entry of a worker whose pane is already gone", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			client.panes = nil

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).To(BeNil())
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
			Expect(client.killed).To(BeEmpty())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		// ⚠️ THE SPEC THAT CATCHES THE TAB LEAK: a held pane still exists and `kill-pane` still
		// closes it, so it must be KILLED — unlike the spec above, where nothing is there to close.
		It("still kills a held pane, rather than leaving its tab open", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			// Held from here on: the pane still exists, but its process is gone.
			client.paneDeathAfter = time.Nanosecond
			Expect(client.panes).NotTo(BeEmpty())

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).To(BeNil())
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
			Expect(client.killed).To(HaveLen(1))
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		// The kill is the one step that can fail after ownership has been resolved, and the entry
		// must survive it: the pane is still open, so dropping the record would orphan a live
		// worker that no caller is told about.
		It("fails and keeps the entry when the pane cannot be killed", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			client.killErr = errWezterm

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(HaveLen(1))
		})

		// ⚠️ A PANE THAT DIED BETWEEN THE RECONCILE AND THE KILL IS NOT A FAILURE. wezterm
		// reports killing a pane that is no longer there as an error, and returning it would
		// fail a stop whose whole purpose — that pane being gone — is already met, while leaving
		// the entry behind for a retry. The pane list is re-read to tell that case from a kill
		// that genuinely failed.
		It("treats a pane that died before the kill as already stopped", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			client.killErr = errWezterm
			client.panesAfterFailedKill = []string{}

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).To(BeNil())
			Expect(response.Worker.WorkerID).To(Equal(created.Worker.WorkerID))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		It("reports an error when the pane list cannot be read", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			client.paneErr = errWezterm

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.killed).To(BeEmpty())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(HaveLen(1))
		})

		It("reports an error when the gui identity cannot be resolved", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			client.guiErr = errWezterm

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.killed).To(BeEmpty())
		})

		// ⚠️ THE PANE ID ALONE MUST NOT DECIDE WHAT TO KILL. After a GUI restart the mux is new
		// and pane ids restart, so a stale entry's id can name a live pane this controller never
		// opened — killing on the id alone would close a stranger's tab.
		// A RESTART is the case where the GUI is provably gone: its socket went with it, so the
		// panes went with it too and there is nothing left to kill. The entry is dropped, and
		// that is what keeps "stop that worker and create another" a recovery a caller can
		// complete — a pane whose GUI no longer exists cannot be killed by anyone.
		It(
			"drops the entry of a worker whose GUI restarted, killing nothing",
			func() {
				created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
				Expect(err).To(BeNil())
				client.guiID = testGuiIDAfterRestart
				client.panes = []string{created.Worker.PaneID}
				// The old GUI's socket is gone, which is what makes it provably a restart.
				_, statErr := os.Stat(testGuiID)
				Expect(statErr).To(HaveOccurred())

				response, err := backend.Stop(
					ctx,
					pkg.StopRequest{WorkerID: created.Worker.WorkerID},
				)

				Expect(err).To(BeNil())
				Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
				Expect(client.killed).To(BeEmpty())
				workers, listErr := store.List(ctx)
				Expect(listErr).To(BeNil())
				Expect(workers).To(BeEmpty())
			},
		)

		// ⚠️ A WORKER OF A SECOND, STILL-RUNNING GUI MUST BE STOPPED, NOT UNTRACKED. Its pane is
		// invisible to this controller's GUI, so it cannot be checked — but it can be reached:
		// the recorded GUI id IS that GUI's socket, and the kill has to be addressed to it. The
		// alternative, removing the record without closing the pane, leaves a live tab that
		// nothing names any more and no later call can clean up.
		It("kills a worker of another live GUI through that GUI's own socket", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			// The first GUI is still running: its socket is on disk and its pid is alive.
			Expect(os.WriteFile(testGuiID, nil, 0o600)).To(BeNil())
			client.guiID = testGuiIDAfterRestart

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).To(BeNil())
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
			Expect(client.killed).To(Equal([]killCall{{
				guiID:  testGuiID,
				paneID: created.Worker.PaneID,
			}}))
			Expect(client.panes).To(BeEmpty())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		It("keeps the entry when a worker of another live GUI cannot be killed", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			Expect(os.WriteFile(testGuiID, nil, 0o600)).To(BeNil())
			client.guiID = testGuiIDAfterRestart
			client.killErr = errWezterm

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(HaveLen(1))
		})
	})

	Describe("WorkerStore", func() {
		It("reads a store that does not exist as an empty store", func() {
			missing := pkg.NewJSONWorkerStore(filepath.Join(GinkgoT().TempDir(), "absent.json"))

			workers, err := missing.List(ctx)

			Expect(err).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

		It("writes the store 0600 in a 0750 directory", func() {
			dir := GinkgoT().TempDir()
			path := filepath.Join(dir, "state", "local-workers.json")
			Expect(pkg.NewJSONWorkerStore(path).Save(ctx, []pkg.LocalWorker{{
				WorkerID: "abc",
				Task:     task,
				PaneID:   "1001",
			}})).To(BeNil())

			info, err := os.Stat(path)
			Expect(err).To(BeNil())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
			dirInfo, err := os.Stat(filepath.Dir(path))
			Expect(err).To(BeNil())
			Expect(dirInfo.Mode().Perm()).To(Equal(os.FileMode(0o750)))
		})

		It("round-trips a worker, including the empty session id", func() {
			path := filepath.Join(GinkgoT().TempDir(), "local-workers.json")
			store := pkg.NewJSONWorkerStore(path)
			Expect(store.Save(ctx, []pkg.LocalWorker{{
				WorkerID: "abc",
				Task:     task,
				PaneID:   "1001",
			}})).To(BeNil())

			workers, err := store.List(ctx)

			Expect(err).To(BeNil())
			Expect(workers).To(HaveLen(1))
			Expect(workers[0].WorkerID).To(Equal(pkg.WorkerID("abc")))
			Expect(workers[0].PaneID).To(Equal("1001"))
			Expect(workers[0].SessionID).To(BeEmpty())
		})

		// The create-time snapshot has to survive the round trip: it is compared against on every
		// later list, so a value that did not persist would let a stale id be accepted again.
		It("round-trips the session id the task carried at create time", func() {
			path := filepath.Join(GinkgoT().TempDir(), "local-workers.json")
			store := pkg.NewJSONWorkerStore(path)
			Expect(store.Save(ctx, []pkg.LocalWorker{{
				WorkerID:        "abc",
				Task:            task,
				PaneID:          "1001",
				SessionID:       "session-of-this-worker",
				CreateSessionID: "session-of-an-earlier-worker",
			}})).To(BeNil())

			workers, err := store.List(ctx)

			Expect(err).To(BeNil())
			Expect(workers).To(HaveLen(1))
			Expect(workers[0].SessionID).To(Equal("session-of-this-worker"))
			Expect(workers[0].CreateSessionID).To(Equal("session-of-an-earlier-worker"))
		})

	})
})
