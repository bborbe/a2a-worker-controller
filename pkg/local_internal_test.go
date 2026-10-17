// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// ⚠️ THIS FILE IS `package pkg`, NOT `package pkg_test`, AND THAT IS THE WHOLE POINT. Every
// helper it covers is unexported and sits behind no seam: the exported backend takes an injected
// WeztermClient, so a spec that drove the backend would exercise the fake and never this code.
// These are exactly the paths that differ against a REAL wezterm — the argv it is handed, the
// environment the child is given, the socket replacement, the pane-list parser, the prompt text —
// and they were the least covered part of the backend. Go compiles a package's internal and
// external test files into one binary, so the suite bootstrap in pkg_suite_test.go runs these
// specs alongside the external ones.
package pkg

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// deadPID is a pid no process holds, so a socket named for it reads as a dead GUI. It is far
// above macOS's own pid limit, so the probe is refused rather than delivered to a stranger.
//
// It is repeated in the external test file rather than shared, because that file is a separate
// package and repeating a fixture is what makes a rename here a failure there rather than a
// silent follow.
const deadPID = "4194304"

var _ = Describe("local backend internals", func() {
	Describe("spawnArgs", func() {
		// The tab's command line is the last argv element, after a `--` separator. That shape is
		// what describeArgs relies on to keep a caller's environment out of an error message.
		It("mirrors the supervisor's tab spawn, with the command line after a separator", func() {
			Expect(spawnArgs("", `/vault-cli:work-on-task "x"`)).To(Equal(
				[]string{"cli", "spawn", "--", "bash", "-lc", `/vault-cli:work-on-task "x"`},
			))
		})

		It("adds the window flag only when a window was configured", func() {
			Expect(spawnArgs("3", "inner")).To(Equal(
				[]string{"cli", "spawn", "--window-id", "3", "--", "bash", "-lc", "inner"},
			))
			// Whitespace is not a window id, so it must not become a flag argument.
			Expect(spawnArgs("   ", "inner")).To(Equal(
				[]string{"cli", "spawn", "--", "bash", "-lc", "inner"},
			))
		})
	})

	Describe("describeArgs", func() {
		// ⚠️ THIS IS THE REDACTION THAT KEEPS A CALLER'S ENVIRONMENT OUT OF THE LOG AND SENTRY.
		// On the spawn path the element after `--` is the tab's command line, and that command
		// line is where the caller's environment values travel.
		It("drops everything after the -- separator", func() {
			args := []string{
				"cli", "spawn", "--", "bash", "-lc", "export TOKEN='s3cr3t'; exec cc",
			}

			Expect(describeArgs(args)).To(Equal("cli spawn"))
			Expect(describeArgs(args)).NotTo(ContainSubstring("s3cr3t"))
		})

		It("keeps every argument when there is no separator", func() {
			Expect(describeArgs([]string{"cli", "list", "--format", "json"})).
				To(Equal("cli list --format json"))
			Expect(describeArgs([]string{"cli", "kill-pane", "--pane-id", "7"})).
				To(Equal("cli kill-pane --pane-id 7"))
		})
	})

	Describe("firstLine", func() {
		// ⚠️ A SIZE BOUND, NOT A REDACTION, and the two are deliberately separate concerns:
		// describeArgs drops the caller's environment, while this keeps wezterm's own
		// diagnostic — which is not caller-controlled — from being persisted in full by the
		// log and by Sentry.
		It("keeps only the first non-empty line", func() {
			Expect(firstLine("window not found\nstack line\n")).To(Equal("window not found"))
			Expect(firstLine("\n\n  leading blanks  \nsecond\n")).To(Equal("leading blanks"))
		})

		It("returns the empty string when there is nothing to keep", func() {
			Expect(firstLine("")).To(Equal(""))
			Expect(firstLine("\n \n\t\n")).To(Equal(""))
		})
	})

	Describe("envWithSocket", func() {
		// ⚠️ LOAD-BEARING, AND THE REASON THE RULE EXISTS: a duplicate key leaves the FIRST entry
		// winning in the child's own getenv, so appending would keep a stale socket — the mux
		// server, not the GUI — and discard the resolved one.
		It("drops an inherited WEZTERM_UNIX_SOCKET instead of appending a second one", func() {
			env := envWithSocket(
				[]string{"PATH=/bin", "WEZTERM_UNIX_SOCKET=/stale/sock", "HOME=/home/u"},
				"/gui/sock",
			)

			Expect(env).To(Equal(
				[]string{"PATH=/bin", "HOME=/home/u", "WEZTERM_UNIX_SOCKET=/gui/sock"},
			))
			Expect(strings.Count(strings.Join(env, "\n"), "WEZTERM_UNIX_SOCKET=")).To(Equal(1))
		})

		It("adds the socket when the environment carries none", func() {
			Expect(envWithSocket([]string{"PATH=/bin"}, "/gui/sock")).To(Equal(
				[]string{"PATH=/bin", "WEZTERM_UNIX_SOCKET=/gui/sock"},
			))
		})
	})

	Describe("parsePaneIDs", func() {
		It("reads pane ids out of the json list and renders them as strings", func() {
			ids, err := parsePaneIDs(context.Background(), `[{"pane_id":3},{"pane_id":17}]`)

			Expect(err).To(BeNil())
			Expect(ids).To(Equal([]string{"3", "17"}))
		})

		It("returns an empty list for a gui with no panes", func() {
			ids, err := parsePaneIDs(context.Background(), `[]`)

			Expect(err).To(BeNil())
			Expect(ids).To(BeEmpty())
		})

		It("fails on output that is not the pane list", func() {
			_, err := parsePaneIDs(context.Background(), "no panes here")

			Expect(err).NotTo(BeNil())
		})

		// ⚠️ LOUDLY, BECAUSE THE ALTERNATIVE IS SILENT AND DESTRUCTIVE. Reading an absent field as
		// the number 0 yields a pane list that does not contain any real pane, and List reads "no
		// matching pane" as "dead" — so a wezterm that renamed this field would erase every
		// stored worker rather than failing. An empty array is still fine: a GUI with no panes
		// really has none.
		It("fails when an entry carries no pane_id, rather than reading it as zero", func() {
			_, err := parsePaneIDs(context.Background(), `[{"window_id":1}]`)

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("pane_id"))
		})
	})

	Describe("shellQuote", func() {
		It("survives a value that contains a single quote", func() {
			Expect(shellQuote("it's")).To(Equal(`'it'\''s'`))
		})
	})

	Describe("workPrompt", func() {
		// The fleet convention: the supervisor spawns a tab worker with the task name in DOUBLE
		// quotes. This is prompt text, never shell-evaluated — tabCommand quotes the whole
		// prompt as one unit.
		It("double-quotes the task name, which is the fleet convention", func() {
			Expect(workPrompt("My Task")).To(Equal(`/vault-cli:work-on-task "My Task"`))
		})

		It("leaves an apostrophe in the task name alone", func() {
			Expect(workPrompt("don't-break")).To(Equal(`/vault-cli:work-on-task "don't-break"`))
		})
	})

	Describe("tabCommand", func() {
		// ⚠️ THE EXPORTS COME FIRST, BEFORE THE cd. `bash -lc` sources the profile files before
		// the rest of the command line runs, so an export placed after the cd is the one a
		// profile default overwrites.
		It("exports the caller's environment before the cd, then execs the launcher", func() {
			command := tabCommand(
				LocalConfig{VaultDir: "/vault", ClaudeScript: "/bin/cc"},
				map[string]string{"B_TOKEN": "s3cr3t", "A_URL": "http://x"},
				"My Task",
			)

			Expect(command).To(Equal(
				"export A_URL='http://x'; export B_TOKEN='s3cr3t'; " +
					"cd '/vault' && exec '/bin/cc' -n '⚙ My Task'" +
					` '/vault-cli:work-on-task "My Task"'`,
			))
		})

		It("renders no export at all when the caller supplied no environment", func() {
			command := tabCommand(
				LocalConfig{VaultDir: "/vault", ClaudeScript: "/bin/cc"},
				nil,
				"t",
			)

			Expect(command).To(HavePrefix("cd '/vault'"))
		})
	})

	Describe("validateTaskName", func() {
		It("accepts an ordinary task name", func() {
			Expect(validateTaskName(context.Background(), "Add a Local Backend")).To(BeNil())
		})

		It("refuses a name that is empty or only whitespace", func() {
			Expect(validateTaskName(context.Background(), "")).NotTo(BeNil())
			Expect(validateTaskName(context.Background(), "  ")).NotTo(BeNil())
		})

		It("refuses a name carrying a path separator or a parent reference", func() {
			for _, name := range []TaskName{"a/b", `a\b`, "../x", "a..b"} {
				Expect(validateTaskName(context.Background(), name)).NotTo(BeNil())
			}
		})

		// ⚠️ THE DOUBLE QUOTE IS REFUSED BECAUSE OF THE WORK PROMPT, WHICH IS NOT A SHELL BUT
		// STILL HAS QUOTING. workPrompt renders the name between double quotes, so `a"b` produces
		// `/vault-cli:work-on-task "a"b"` — a prompt whose quoting no longer says what it means.
		It("refuses a name carrying a double quote", func() {
			Expect(validateTaskName(context.Background(), `a"b`)).NotTo(BeNil())
			Expect(validateTaskName(context.Background(), `"`)).NotTo(BeNil())
		})

		// A control character is refused for the prompt and for the tab bar: a newline in a tab
		// name splits the tab bar entry rather than being shown in it.
		It("refuses a name carrying a control character", func() {
			Expect(validateTaskName(context.Background(), "a\nb")).NotTo(BeNil())
			Expect(validateTaskName(context.Background(), "a\tb")).NotTo(BeNil())
		})

		// The quoting story is otherwise complete, so the ordinary punctuation a task name really
		// carries must keep working — an apostrophe is prompt text and never reaches a shell.
		It("accepts an apostrophe, a colon and a dash", func() {
			Expect(validateTaskName(context.Background(), "don't-break: it's fine")).To(BeNil())
		})
	})

	Describe("reconcileWorkers", func() {
		It("keeps a worker whose pane is live in the same GUI", func() {
			workers := []LocalWorker{{WorkerID: "w1", PaneID: "1", GuiID: "gui-a"}}

			Expect(reconcileWorkers(workers, map[string]bool{"1": true}, "gui-a")).
				To(Equal(workers))
		})

		It("drops a worker whose pane is gone", func() {
			workers := []LocalWorker{{WorkerID: "w1", PaneID: "1", GuiID: "gui-a"}}

			Expect(reconcileWorkers(workers, map[string]bool{}, "gui-a")).To(BeEmpty())
		})

		// ⚠️ THE NEW GUI REUSES THE PANE ID AND THE ENTRY MUST STILL GO: that id now names a pane
		// this controller never opened, so reporting it would be a phantom worker. It goes
		// because the GUI it was recorded under is PROVABLY gone — its socket is not on disk.
		It("drops a worker whose GUI socket is gone and whose pane id is live again", func() {
			workers := []LocalWorker{{
				WorkerID: "w1",
				PaneID:   "1",
				GuiID:    filepath.Join(GinkgoT().TempDir(), "gui-sock-1000"),
			}}

			Expect(reconcileWorkers(workers, map[string]bool{"1": true}, "gui-b")).To(BeEmpty())
		})

		// ⚠️ A SECOND GUI RUNNING ALONGSIDE MUST NOT COST THE FIRST ONE ITS RECORD. The pane is
		// invisible from here, so nothing says the worker is dead — and this function's answer is
		// what List persists, so dropping it would erase the only record naming a live pane. Its
		// socket being on disk AND its pid being alive is what says that GUI is still there.
		It("keeps a worker whose GUI is still running", func() {
			socket := filepath.Join(GinkgoT().TempDir(), "gui-sock-"+strconv.Itoa(os.Getpid()))
			Expect(os.WriteFile(socket, nil, 0o600)).To(BeNil())
			workers := []LocalWorker{{WorkerID: "w1", PaneID: "1", GuiID: socket}}

			Expect(reconcileWorkers(workers, map[string]bool{"1": true}, "gui-b")).
				To(Equal(workers))
		})

		// ⚠️ THE SOCKET FILE ALONE IS NOT ENOUGH, AND THIS IS THE CASE THAT PROVES IT. A GUI that
		// died uncleanly leaves its socket on disk; treating that as "still running" would keep
		// its entries alive forever — out of every list reply, and still refusing create for
		// their tasks. The dead pid in the socket's own name is the fact that closes that.
		It("drops a worker whose GUI socket was left behind by a dead process", func() {
			socket := filepath.Join(GinkgoT().TempDir(), "gui-sock-"+deadPID)
			Expect(os.WriteFile(socket, nil, 0o600)).To(BeNil())
			workers := []LocalWorker{{WorkerID: "w1", PaneID: "1", GuiID: socket}}

			Expect(reconcileWorkers(workers, map[string]bool{"1": true}, "gui-b")).To(BeEmpty())
		})

		It("drops a worker recorded with no GUI identity at all", func() {
			workers := []LocalWorker{{WorkerID: "w1", PaneID: "1"}}

			Expect(reconcileWorkers(workers, map[string]bool{"1": true}, "gui-b")).To(BeEmpty())
		})
	})

	Describe("guiAlive", func() {
		// Both halves are asserted separately, because either one alone is a wrong answer:
		// existence alone keeps a dead GUI's records, and liveness alone cannot be read from a
		// socket that is gone.
		It("reports a GUI whose socket is on disk and whose pid is alive", func() {
			socket := filepath.Join(GinkgoT().TempDir(), "gui-sock-"+strconv.Itoa(os.Getpid()))
			Expect(os.WriteFile(socket, nil, 0o600)).To(BeNil())

			Expect(guiAlive(socket)).To(BeTrue())
		})

		It("reports a GUI whose socket was left behind by a dead process as gone", func() {
			socket := filepath.Join(GinkgoT().TempDir(), "gui-sock-"+deadPID)
			Expect(os.WriteFile(socket, nil, 0o600)).To(BeNil())

			Expect(guiAlive(socket)).To(BeFalse())
		})

		It("reports a GUI with no socket as gone, whatever its pid", func() {
			Expect(guiAlive(
				filepath.Join(GinkgoT().TempDir(), "gui-sock-"+strconv.Itoa(os.Getpid())),
			)).To(BeFalse())
		})

		It("reports a path that names no GUI socket as gone", func() {
			Expect(guiAlive("")).To(BeFalse())
			Expect(guiAlive(GinkgoT().TempDir())).To(BeFalse())
			Expect(guiAlive(filepath.Join(GinkgoT().TempDir(), "sock"))).To(BeFalse())
		})
	})

	Describe("liveGuiSocket", func() {
		// ⚠️ THE DIRECTORY IS A PARAMETER SO THIS RULE IS TESTABLE WITHOUT A RUNNING GUI. The
		// selection is load-bearing — it decides which GUI every call talks to — and it is the
		// one part of the backend that a fake client cannot stand in for, because the fake is
		// the thing being replaced.
		It("picks the live socket and ignores one whose process is gone", func() {
			dir := GinkgoT().TempDir()
			live := filepath.Join(dir, "gui-sock-"+strconv.Itoa(os.Getpid()))
			dead := filepath.Join(dir, "gui-sock-4194304")
			Expect(os.WriteFile(live, nil, 0o600)).To(BeNil())
			Expect(os.WriteFile(dead, nil, 0o600)).To(BeNil())
			// The dead one is the NEWER file, so a selection that ignored liveness would pick it.
			Expect(os.Chtimes(live, time.Unix(0, 0), time.Unix(0, 0))).To(BeNil())

			socket, err := liveGuiSocket(context.Background(), dir)

			Expect(err).To(BeNil())
			Expect(socket).To(Equal(live))
		})

		It("ignores files that are not per-GUI sockets", func() {
			dir := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(dir, "sock"), nil, 0o600)).To(BeNil())
			Expect(os.WriteFile(filepath.Join(dir, "gui-sock-abc"), nil, 0o600)).To(BeNil())

			_, err := liveGuiSocket(context.Background(), dir)

			Expect(err).NotTo(BeNil())
		})

		It("fails rather than falling back when no live socket exists", func() {
			_, err := liveGuiSocket(context.Background(), GinkgoT().TempDir())

			Expect(err).NotTo(BeNil())
		})

		It("fails when the directory cannot be read", func() {
			_, err := liveGuiSocket(
				context.Background(),
				filepath.Join(GinkgoT().TempDir(), "absent"),
			)

			Expect(err).NotTo(BeNil())
		})
	})

	Describe("processAlive", func() {
		It("reads this process's own pid as alive", func() {
			Expect(processAlive(strconv.Itoa(os.Getpid()))).To(BeTrue())
		})

		It("reads an unused pid as dead", func() {
			Expect(processAlive("4194304")).To(BeFalse())
		})

		It("reads a value that is not a pid as dead", func() {
			Expect(processAlive("")).To(BeFalse())
			Expect(processAlive("abc")).To(BeFalse())
			Expect(processAlive("0")).To(BeFalse())
			Expect(processAlive("-1")).To(BeFalse())
		})
	})

	Describe("frontmatterSessionID", func() {
		It("reads the session id out of the leading frontmatter block", func() {
			content := "---\nstatus: in_progress\nclaude_session_id: abc\n---\n"

			Expect(frontmatterSessionID(content)).To(Equal("abc"))
		})

		It("unquotes a quoted value", func() {
			content := "---\nclaude_session_id: \"abc\"\n---\n"

			Expect(frontmatterSessionID(content)).To(Equal("abc"))
		})

		// Only the first block counts: after the closing delimiter a `key: value` line is body
		// text, and reading one there would invent a binding.
		It("ignores the key when it appears only in the body", func() {
			content := "---\nstatus: x\n---\n\nclaude_session_id: abc\n"

			Expect(frontmatterSessionID(content)).To(BeEmpty())
		})

		It("returns empty when there is no frontmatter block at all", func() {
			Expect(frontmatterSessionID("claude_session_id: abc\n")).To(BeEmpty())
			Expect(frontmatterSessionID("")).To(BeEmpty())
		})
	})
})
