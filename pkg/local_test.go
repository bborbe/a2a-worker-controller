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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// The vault layout and the tab-name prefix, repeated here rather than read from the package
// so a rename is a test failure rather than a test that follows the code.
const (
	testTasksDir      = "25 Tasks"
	testTabNamePrefix = "⚙ "
	testClaudeScript  = "/Users/bborbe/Documents/workspaces/scripts/cc-private"
	// testGuiID stands in for the live GUI socket's path, which carries the GUI process's pid.
	// testGuiIDAfterRestart is what the same field reads after the GUI has been restarted: a
	// different socket, and therefore a different identity for the panes behind it.
	testGuiID             = "/Users/bborbe/.local/share/wezterm/gui-sock-1000"
	testGuiIDAfterRestart = "/Users/bborbe/.local/share/wezterm/gui-sock-2000"
)

// errWezterm stands in for a wezterm CLI failure.
var errWezterm = errors.New("wezterm unavailable")

// spawnCall is one recorded spawn: the window it was asked for and the command line it was
// handed.
type spawnCall struct {
	windowID string
	inner    string
}

// fakeWeztermClient is the injected WeztermClient seam. It records every spawn and every kill
// and answers with the panes it holds, so a spec drives the real backend through the real code
// path without a GUI, a socket or a pane.
//
// A spawn adds its pane to the held panes, because that is what really happens: the pane it
// returns is open by definition. A spec that wants a pane to have gone sets panes itself, or
// kills it.
type fakeWeztermClient struct {
	panes    []string
	spawns   []spawnCall
	killed   []string
	guiID    string
	spawnErr error
	paneErr  error
	killErr  error
}

func (f *fakeWeztermClient) Spawn(
	_ context.Context,
	windowID string,
	inner string,
) (string, error) {
	f.spawns = append(f.spawns, spawnCall{windowID: windowID, inner: inner})
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	paneID := "pane-" + strconv.Itoa(1000+len(f.spawns))
	f.panes = append(f.panes, paneID)
	return paneID, nil
}

func (f *fakeWeztermClient) PaneIDs(_ context.Context) ([]string, error) {
	if f.paneErr != nil {
		return nil, f.paneErr
	}
	return f.panes, nil
}

// GuiID answers with the GUI instance the fake is standing in for. It is a settable field, so a
// spec can restart the GUI mid-test by changing it — which is the whole reason the backend
// records it.
func (f *fakeWeztermClient) GuiID(_ context.Context) (string, error) {
	return f.guiID, nil
}

func (f *fakeWeztermClient) KillPane(_ context.Context, paneID string) error {
	if f.killErr != nil {
		return f.killErr
	}
	f.killed = append(f.killed, paneID)
	panes := make([]string, 0, len(f.panes))
	for _, pane := range f.panes {
		if pane != paneID {
			panes = append(panes, pane)
		}
	}
	f.panes = panes
	return nil
}

// taskFile renders a task file with the given frontmatter, in the shape the backend reads.
func taskFile(sessionID string) string {
	content := "---\nstatus: in_progress\n"
	if sessionID != "" {
		content += "claude_session_id: " + sessionID + "\n"
	}
	return content + "---\n\nTags: [[Task]]\n"
}

var _ = Describe("LocalBackend", func() {
	var (
		ctx     context.Context
		client  *fakeWeztermClient
		store   pkg.WorkerStore
		vault   string
		backend pkg.WorkerBackend
	)

	const task = pkg.TaskName("local-backend-task")

	BeforeEach(func() {
		ctx = context.Background()
		vault = GinkgoT().TempDir()
		client = &fakeWeztermClient{guiID: testGuiID}
		store = pkg.NewJSONWorkerStore(filepath.Join(GinkgoT().TempDir(), "local-workers.json"))
		backend = pkg.NewLocalBackend(client, store, pkg.LocalConfig{
			VaultDir:     vault,
			ClaudeScript: testClaudeScript,
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

		It("records the session the task is bound to", func() {
			writeTask(task, "9f2c4a11-1111-2222-3333-444455556666")

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Worker.SessionID).To(Equal("9f2c4a11-1111-2222-3333-444455556666"))
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
		It("drops a worker whose GUI restarted, even when its pane id is live again", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())
			// The new GUI reuses the same pane id — exactly the collision the GUI id catches.
			client.guiID = testGuiIDAfterRestart
			client.panes = []string{created.Worker.PaneID}

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(BeEmpty())
			workers, listErr := store.List(ctx)
			Expect(listErr).To(BeNil())
			Expect(workers).To(BeEmpty())
		})

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
	})

	Describe("Stop", func() {
		It("kills the pane and removes the entry", func() {
			created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
			Expect(err).To(BeNil())

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: created.Worker.WorkerID})

			Expect(err).To(BeNil())
			Expect(response.Worker.WorkerID).To(Equal(created.Worker.WorkerID))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
			Expect(client.killed).To(Equal([]string{created.Worker.PaneID}))
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

		// ⚠️ THE PANE ID ALONE MUST NOT DECIDE WHAT TO KILL. After a GUI restart the mux is new
		// and pane ids restart, so a stale entry's id can name a live pane this controller never
		// opened — killing on the id alone would close a stranger's tab.
		It(
			"does not kill a pane that belongs to a restarted GUI, and still drops the entry",
			func() {
				created, err := backend.Create(ctx, pkg.CreateRequest{Task: task})
				Expect(err).To(BeNil())
				client.guiID = testGuiIDAfterRestart
				client.panes = []string{created.Worker.PaneID}

				response, err := backend.Stop(
					ctx,
					pkg.StopRequest{WorkerID: created.Worker.WorkerID},
				)

				Expect(err).To(BeNil())
				Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
				Expect(client.killed).To(BeEmpty())
			},
		)
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
	})
})
