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
	spawnErr error
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
	return f.panes, nil
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
		client = &fakeWeztermClient{}
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

		// The whole command line is asserted, escaping included: the work prompt carries its
		// own single quotes around the task name, so the outer quoting has to escape them —
		// `'\''` is how a single quote survives inside a single-quoted shell word. A launcher
		// that received the prompt unquoted would read the task name as a separate argument.
		It("spawns the vault launcher with the tab name and the work prompt", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].inner).To(Equal(
				"cd '" + vault + "' && exec '" + testClaudeScript + "'" +
					" -n '" + testTabNamePrefix + string(task) + "'" +
					` '/vault-cli:work-on-task '\''local-backend-task'\'''`,
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
