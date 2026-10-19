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

	vaultconfig "github.com/bborbe/vault-cli/pkg/config"
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
			ids, err := parsePaneIDs(
				context.Background(),
				`[{"pane_id":3,"cwd":"file:///vault"},{"pane_id":17,"cwd":"file:///vault"}]`,
			)

			Expect(err).To(BeNil())
			Expect(ids).To(Equal([]string{"3", "17"}))
		})

		// ⚠️ THE TWO LISTS ANSWER DIFFERENT QUESTIONS AND THESE SPECS PIN THE DIFFERENCE. A pane
		// whose launcher died is HELD: it still exists — so parsePaneIDs keeps it, and `Stop` can
		// still close it — but it has no process, so parseLivePaneIDs drops it and the create
		// guard fails. Collapsing the two makes `Stop` skip its kill and leak the tab, so a spec
		// that only exercises one of them is not enough.
		It("keeps a held pane in the existence list, so it can still be closed", func() {
			ids, err := parsePaneIDs(
				context.Background(),
				`[{"pane_id":3,"cwd":"file:///vault"},{"pane_id":17,"cwd":""}]`,
			)

			Expect(err).To(BeNil())
			Expect(ids).To(Equal([]string{"3", "17"}))
		})

		// ⚠️ THIS IS THE SPEC THAT WOULD HAVE CAUGHT v0.12.2's DEFECT. `exit_behavior = "Hold"`
		// keeps a dead launcher's pane OPEN, so a check on pane existence alone reports it as a
		// healthy worker — measured 2026-10-18, a stub exiting 127 left `cwd: ""` and the guard
		// passed the create.
		It("drops a held pane from the liveness list, so the guard fails the create", func() {
			ids, err := parseLivePaneIDs(
				context.Background(),
				`[{"pane_id":3,"cwd":"file:///vault"},{"pane_id":17,"cwd":""}]`,
			)

			Expect(err).To(BeNil())
			Expect(ids).To(Equal([]string{"3"}))
		})

		// ⚠️ ABSENT IS NOT EMPTY, AND THE DESTRUCTIVE DIRECTION IS THE ONE TO FAIL ON. Reading an
		// absent cwd as empty would mark every worker dead the moment wezterm renamed the field.
		It(
			"fails the liveness read when an entry carries no cwd, rather than reading it as empty",
			func() {
				_, err := parseLivePaneIDs(context.Background(), `[{"pane_id":3}]`)

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("cwd"))
			},
		)

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
				"/vault",
				"/bin/cc",
				map[string]string{"B_TOKEN": "s3cr3t", "A_URL": "http://x"},
				"My Task",
				"",
			)

			Expect(command).To(Equal(
				"export A_URL='http://x'; export B_TOKEN='s3cr3t'; " +
					"cd '/vault' && exec '/bin/cc' -n '⚙ My Task'" +
					` '/vault-cli:work-on-task "My Task"'`,
			))
		})

		It("renders no export at all when the caller supplied no environment", func() {
			command := tabCommand("/vault", "/bin/cc", nil, "t", "")

			Expect(command).To(HavePrefix("cd '/vault'"))
		})

		// ⚠️ THE LAUNCHER IS THE ONE IT WAS HANDED, NOT THE CONFIGURED ONE, AND THIS IS THE WHOLE
		// POINT OF THE CHANGE: the vault launcher used to be read from the config in here, which is
		// how every tab came up on the vault default however the task was declared.
		It("execs the launcher it was handed rather than a configured one", func() {
			command := tabCommand("/vault", "/scripts/cc-private-claude", nil, "t", "")

			Expect(command).To(ContainSubstring("exec '/scripts/cc-private-claude'"))
			Expect(command).NotTo(ContainSubstring("cc-private'"))
		})

		It("expands a leading ~ in the vault directory", func() {
			home, err := os.UserHomeDir()
			Expect(err).To(BeNil())

			command := tabCommand("~/vault", "/bin/cc", nil, "t", "")

			Expect(command).To(ContainSubstring("cd '" + filepath.Join(home, "vault") + "'"))
		})
		// The first step leads, a blank line separates it, and the work prompt follows — all of it
		// one single-quoted argv element, so a quote inside the first step is quoted, not syntax.
		It("prepends the first step to the work prompt, separated by a blank line", func() {
			command := tabCommand("/vault", "/bin/cc", nil, "t", "read the guide's")

			Expect(command).To(HaveSuffix(
				` 'read the guide'\''s` + "\n\n" + `/vault-cli:work-on-task "t"'`,
			))
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

	Describe("frontmatterBlock", func() {
		It("returns the lines between the delimiters", func() {
			block, found := frontmatterBlock("---\nlauncher: cc\nrole: agent\n---\n\nBody\n")

			Expect(found).To(BeTrue())
			Expect(block).To(Equal("launcher: cc\nrole: agent"))
		})

		It("returns the first block only", func() {
			block, found := frontmatterBlock("---\na: 1\n---\n\nb: 2\n---\n")

			Expect(found).To(BeTrue())
			Expect(block).To(Equal("a: 1"))
		})

		// ⚠️ AN UNTERMINATED BLOCK IS NOT ONE. Reading the rest of the page as YAML would invent
		// fields out of body text — a task body quoting `launcher:` in prose would become a
		// launcher.
		It("reports no block when the delimiter is never closed", func() {
			_, found := frontmatterBlock("---\nlauncher: cc\n\nBody\n")

			Expect(found).To(BeFalse())
		})

		It("reports no block when the page does not start with one", func() {
			_, found := frontmatterBlock("Body\n---\nlauncher: cc\n---\n")

			Expect(found).To(BeFalse())
			_, found = frontmatterBlock("")

			Expect(found).To(BeFalse())
		})
	})

	Describe("parseTaskFrontmatter", func() {
		// ⚠️ ALL THREE `goals:` SHAPES ARE IN THE LIVE VAULT, and this is the trio of specs that keeps
		// them readable: 2367 files carry the block form, 420 the inline one, and two a bare scalar.
		It("reads the block goals form", func() {
			content := "---\ngoals:\n    - '[[Goal A]]'\n    - '[[Goal B]]'\n---\n"

			frontmatter, err := parseTaskFrontmatter(context.Background(), content)

			Expect(err).To(BeNil())
			Expect(frontmatter.Goals).To(Equal([]string{"Goal A", "Goal B"}))
		})

		// ⚠️ THE COMMA IS THE REASON THIS IS PARSED AS YAML. A goal name can contain one
		// ("Supervisor Routes Workers to local, docker or k8s Per Task"), so a reader that split
		// the inline form on its commas would cut the name in two and look up a goal that does not
		// exist.
		It("reads an inline goals form whose entry contains a comma", func() {
			content := "---\ngoals: ['[[Routes Workers to local, docker or k8s]]']\n---\n"

			frontmatter, err := parseTaskFrontmatter(context.Background(), content)

			Expect(err).To(BeNil())
			Expect(frontmatter.Goals).To(Equal([]string{"Routes Workers to local, docker or k8s"}))
		})

		It("reads an empty inline goals list as no goals", func() {
			frontmatter, err := parseTaskFrontmatter(context.Background(), "---\ngoals: []\n---\n")

			Expect(err).To(BeNil())
			Expect(frontmatter.Goals).To(BeEmpty())
		})

		// ⚠️ THE SCALAR FORM IS THE THIRD SHAPE, AND IT IS A REAL ONE: two live files write
		// `goals: '[[Restore Trading Profitability 2026Q2]]'` rather than a list, measured
		// 2026-10-18. Requiring []any dropped the goal, and a task that names a goal but resolves as
		// though it named none falls through to the vault launcher.
		It("reads a scalar goals value as a one-entry list", func() {
			content := "---\ngoals: '[[Restore Trading Profitability 2026Q2]]'\n---\n"

			frontmatter, err := parseTaskFrontmatter(context.Background(), content)

			Expect(err).To(BeNil())
			Expect(frontmatter.Goals).To(Equal([]string{"Restore Trading Profitability 2026Q2"}))
		})

		It("reads a bare scalar goals value as a one-entry list", func() {
			frontmatter, err := parseTaskFrontmatter(
				context.Background(),
				"---\ngoals: A Goal\n---\n",
			)

			Expect(err).To(BeNil())
			Expect(frontmatter.Goals).To(Equal([]string{"A Goal"}))
		})

		// An empty scalar is not a goal: it degrades to "no entries" like the other malformed shapes
		// rather than to an entry that names nothing.
		It("reads an empty scalar goals value as no goals", func() {
			frontmatter, err := parseTaskFrontmatter(context.Background(), "---\ngoals: ''\n---\n")

			Expect(err).To(BeNil())
			Expect(frontmatter.Goals).To(BeEmpty())
		})

		It("reads the launcher and the role", func() {
			content := "---\nlauncher: cc-private-claude\nrole: manager\n---\n"

			frontmatter, err := parseTaskFrontmatter(context.Background(), content)

			Expect(err).To(BeNil())
			Expect(frontmatter.Launcher).To(Equal("cc-private-claude"))
			Expect(frontmatter.Role).To(Equal("manager"))
		})

		It("unquotes a quoted launcher", func() {
			frontmatter, err := parseTaskFrontmatter(
				context.Background(),
				"---\nlauncher: \"cc-private-claude\"\n---\n",
			)

			Expect(err).To(BeNil())
			Expect(frontmatter.Launcher).To(Equal("cc-private-claude"))
		})

		It("leaves every field empty when the block carries none of them", func() {
			frontmatter, err := parseTaskFrontmatter(
				context.Background(),
				"---\nstatus: in_progress\n---\n\nlauncher: cc\n",
			)

			Expect(err).To(BeNil())
			Expect(frontmatter).To(Equal(taskFrontmatter{}))
		})

		It("leaves every field empty when there is no frontmatter at all", func() {
			frontmatter, err := parseTaskFrontmatter(context.Background(), "launcher: cc\n")

			Expect(err).To(BeNil())
			Expect(frontmatter).To(Equal(taskFrontmatter{}))
		})

		// A malformed block is an error rather than a silent zero value, so the caller can log
		// what went wrong while still degrading to the documented defaults.
		It("fails on a frontmatter block that is not valid yaml", func() {
			_, err := parseTaskFrontmatter(context.Background(), "---\nlauncher: [unclosed\n---\n")

			Expect(err).NotTo(BeNil())
		})
	})

	Describe("trimWikilink", func() {
		It("strips the brackets a wikilink carries", func() {
			Expect(trimWikilink("[[Goal A]]")).To(Equal("Goal A"))
			Expect(trimWikilink("  [[Goal A]]  ")).To(Equal("Goal A"))
		})

		It("leaves a bare name alone", func() {
			Expect(trimWikilink("Goal A")).To(Equal("Goal A"))
		})
	})

	Describe("launcherPath", func() {
		// The cc-* scripts live side by side, so a bare name resolves beside the vault's own
		// launcher rather than on PATH.
		It("resolves a bare name beside the vault launcher", func() {
			path, err := launcherPath(
				context.Background(),
				"/scripts/cc-private",
				"cc-private-claude",
			)

			Expect(err).To(BeNil())
			Expect(path).To(Equal("/scripts/cc-private-claude"))
		})

		It("uses a value containing a slash as given", func() {
			path, err := launcherPath(context.Background(), "/scripts/cc-private", "/other/cc")

			Expect(err).To(BeNil())
			Expect(path).To(Equal("/other/cc"))
		})

		// A vault launcher with no directory is a bare name itself, and joining against "." would
		// produce "./cc-private-claude" — a path that happens to work and does not say what it is.
		It("leaves a bare name alone when the vault launcher is bare too", func() {
			path, err := launcherPath(context.Background(), "cc-private", "cc-private-claude")

			Expect(err).To(BeNil())
			Expect(path).To(Equal("cc-private-claude"))
		})

		// ⚠️ THE SHAPE GUARD IS A TRUST BOUNDARY: frontmatter is worker-writable and the value is
		// exec'd inside `bash -lc`, so anything a shell could read as syntax is refused rather
		// than quoted.
		It("refuses a value carrying shell syntax", func() {
			for _, value := range []string{
				"cc; rm -rf /",
				"cc && curl evil",
				"$(whoami)",
				"cc private",
				"cc\nrm",
				"~/cc",
			} {
				_, err := launcherPath(context.Background(), "/scripts/cc-private", value)

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("not a plain script name or path"))
			}
		})

		// ⚠️ THE REGEX DOES NOT COVER THIS: `..` is built from characters safeLauncher allows, so
		// the segment check is what stops a value escaping the launcher directory.
		It("refuses a parent path segment", func() {
			for _, value := range []string{"../cc", "../../bin/sh", "/a/../b"} {
				_, err := launcherPath(context.Background(), "/scripts/cc-private", value)

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("path segment"))
			}
		})

		// ⚠️ A SINGLE DOT IS NOT A PARENT REFERENCE, AND `./cc` NAMES A FILE RATHER THAN A DIRECTORY —
		// the earlier caption here claimed the executability check then refused it for being a
		// directory, and that was simply wrong: `./cc` is a path to a file in the launcher directory,
		// and what the check does with it depends on the file, not on the `.`. It is accepted here,
		// and it stays relative at this layer — chooseLauncher is what resolves it against the vault
		// directory, because that is where the tab's `exec` resolves it.
		It("accepts a single dot segment, which is a file path and not a parent reference", func() {
			path, err := launcherPath(context.Background(), "/scripts/cc-private", "./cc")

			Expect(err).To(BeNil())
			Expect(path).To(Equal("./cc"))
		})
	})

	Describe("requireExecutableLauncher", func() {
		It("accepts an executable file", func() {
			path := filepath.Join(GinkgoT().TempDir(), "cc")
			Expect(os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755)).To(BeNil())

			Expect(requireExecutableLauncher(context.Background(), path)).To(BeNil())
		})

		It("refuses a file that exists but is not executable", func() {
			path := filepath.Join(GinkgoT().TempDir(), "cc")
			Expect(os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644)).To(BeNil())

			err := requireExecutableLauncher(context.Background(), path)

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("is not an executable file"))
		})

		// ⚠️ A DIRECTORY IS REFUSED EVEN THOUGH ITS MODE CARRIES AN EXECUTE BIT, which is the case
		// a mode check alone would admit.
		It("refuses a directory", func() {
			err := requireExecutableLauncher(context.Background(), GinkgoT().TempDir())

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("is not an executable file"))
		})

		It("refuses a path that does not exist", func() {
			err := requireExecutableLauncher(
				context.Background(),
				filepath.Join(GinkgoT().TempDir(), "absent"),
			)

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("is not an executable file"))
		})
	})

	Describe("readGoalLauncher", func() {
		// writeGoalPage puts a goal file at <vault>/<dir>/<name>.md carrying the launcher line.
		writeGoalPage := func(vault string, dir string, name string, launcher string) {
			Expect(os.MkdirAll(filepath.Join(vault, dir), 0o750)).To(BeNil())
			Expect(os.WriteFile(
				filepath.Join(vault, dir, name+".md"),
				[]byte("---\nlauncher: "+launcher+"\n---\n"),
				0o600,
			)).To(BeNil())
		}

		// The two resolution bases are the whole point of this function's name validation: a bare
		// name is a goal in the goals directory, and a name containing a `/` is a PATH-QUALIFIED
		// wikilink relative to the vault root.
		It("resolves a bare name against the goals directory", func() {
			vault := GinkgoT().TempDir()
			writeGoalPage(vault, "24 Goals", "A Goal", "/scripts/cc-goal")

			launcher, err := readGoalLauncher(context.Background(), vault, "24 Goals", "A Goal")

			Expect(err).To(BeNil())
			Expect(launcher).To(Equal("/scripts/cc-goal"))
		})

		// ⚠️ THE SHAPE 11 LIVE TASKS CARRY: `[[24 Goals/Build Security Review Agent]]`. The name
		// already names the goals directory, so prefixing goalsDir again would look for
		// `24 Goals/24 Goals/…` and find nothing — the same silent drop, one layer down.
		It("resolves a path-qualified name against the vault root", func() {
			vault := GinkgoT().TempDir()
			writeGoalPage(vault, "24 Goals", "A Goal", "/scripts/cc-goal")

			launcher, err := readGoalLauncher(
				context.Background(),
				vault,
				"24 Goals",
				"24 Goals/A Goal",
			)

			Expect(err).To(BeNil())
			Expect(launcher).To(Equal("/scripts/cc-goal"))
		})

		// The other live shape points into the tasks directory — `[[25 Tasks/Return to Live Trading]]`
		// — and it resolves by the same rule: relative to the vault root, whichever folder it names.
		It("resolves a path-qualified name that points outside the goals directory", func() {
			vault := GinkgoT().TempDir()
			writeGoalPage(vault, "25 Tasks", "Return to Live Trading", "/scripts/cc-task")

			launcher, err := readGoalLauncher(
				context.Background(),
				vault,
				"24 Goals",
				"25 Tasks/Return to Live Trading",
			)

			Expect(err).To(BeNil())
			Expect(launcher).To(Equal("/scripts/cc-task"))
		})

		It("answers empty when the goal records no launcher", func() {
			vault := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(vault, "24 Goals"), 0o750)).To(BeNil())
			Expect(os.WriteFile(
				filepath.Join(vault, "24 Goals", "Bare.md"),
				[]byte("---\npage_type: goal\n---\n"),
				0o600,
			)).To(BeNil())

			launcher, err := readGoalLauncher(context.Background(), vault, "24 Goals", "Bare")

			Expect(err).To(BeNil())
			Expect(launcher).To(BeEmpty())
		})

		// ⚠️ ALLOWING THE SEPARATOR DID NOT ALLOW LEAVING THE VAULT. The name becomes a path, so the
		// escapes a path admits — a `..` segment, an absolute path, a backslash — are still refused,
		// and the empty name with them.
		It("refuses a name that escapes the vault or names an absolute path", func() {
			vault := GinkgoT().TempDir()
			for _, name := range []string{
				"../outside",
				"24 Goals/../../outside",
				"24 Goals/..",
				"/etc/passwd",
				`24 Goals\A Goal`,
				"",
			} {
				_, err := readGoalLauncher(context.Background(), vault, "24 Goals", name)

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("not a valid goal name"))
			}
		})

		It("fails when the goal file does not exist", func() {
			_, err := readGoalLauncher(
				context.Background(),
				GinkgoT().TempDir(),
				"24 Goals",
				"Absent",
			)

			Expect(err).NotTo(BeNil())
			Expect(err.Error()).To(ContainSubstring("read goal file"))
		})
	})

	Describe("goalsDirForVault", func() {
		It("matches the vault by path and returns its configured goals directory", func() {
			vaults := []*vaultconfig.Vault{
				{Path: "/vaults/personal", GoalsDir: "24 Goals"},
				{Path: "/vaults/family", GoalsDir: "23 Goals"},
			}

			dir, err := goalsDirForVault(context.Background(), vaults, "/vaults/family")

			Expect(err).To(BeNil())
			Expect(dir).To(Equal("23 Goals"))
		})

		// ⚠️ MATCHED ON PATH, NEVER ON NAME, because this backend is configured with a directory
		// and never learns a vault name. A name match would be the obvious wrong field.
		It("does not match on the vault name", func() {
			vaults := []*vaultconfig.Vault{{Name: "family", Path: "/vaults/family"}}

			_, err := goalsDirForVault(context.Background(), vaults, "family")

			Expect(err).NotTo(BeNil())
		})

		// A trailing slash and a `~` are both spellings of the same directory, and a create must
		// not lose the goal rule to either.
		It("tolerates a trailing slash and expands a leading ~", func() {
			home, err := os.UserHomeDir()
			Expect(err).To(BeNil())
			vaults := []*vaultconfig.Vault{
				{Path: filepath.Join(home, "vaults/personal"), GoalsDir: "24 Goals"},
			}

			dir, err := goalsDirForVault(context.Background(), vaults, "~/vaults/personal/")

			Expect(err).To(BeNil())
			Expect(dir).To(Equal("24 Goals"))
		})

		It("fails when no entry names the vault", func() {
			_, err := goalsDirForVault(
				context.Background(),
				[]*vaultconfig.Vault{{Path: "/vaults/other"}},
				"/vaults/personal",
			)

			Expect(err).NotTo(BeNil())
		})
	})

	Describe("goalsDir", func() {
		// ⚠️ THE FALLBACK IS THE POINT: an unresolvable goals directory must cost the goal rule,
		// never a create. A wrong one is not a visible failure — it just means the worker opens on
		// the vault default, which is the silent drop this change exists to remove.
		It("falls back to the default when no vault-cli entry names the vault", func() {
			absent := filepath.Join(GinkgoT().TempDir(), "absent")

			Expect(goalsDirFor(context.Background(), absent)).To(Equal(DefaultLocalGoalsDir))
		})
	})

	Describe("vaultDirHasDir", func() {
		// ⚠️ THIS IS THE SEAM THAT CATCHES A CONFIGURED GOALS DIRECTORY WHICH IS SIMPLY NOT THERE.
		// vault-cli's GetGoalsDir answers its own default of `Goals` when an entry declares none, and
		// a numbered-folder vault has no such directory — so the goal rule would find nothing and
		// fall through to the vault launcher with no warning that anything was wrong.
		It("reports a directory that exists", func() {
			vault := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(vault, "24 Goals"), 0o750)).To(BeNil())

			Expect(vaultDirHasDir(vault, "24 Goals")).To(BeTrue())
		})

		It("reports a directory that does not exist", func() {
			Expect(vaultDirHasDir(GinkgoT().TempDir(), "Goals")).To(BeFalse())
		})

		// A FILE is not a directory, so a `Goals` path that happens to be a file must not satisfy the
		// check and send the goal rule looking inside it.
		It("reports a path that is a file rather than a directory", func() {
			vault := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(vault, "Goals"), []byte("x"), 0o600)).To(BeNil())

			Expect(vaultDirHasDir(vault, "Goals")).To(BeFalse())
		})

		// A `~` is a spelling of the same directory, so the check must expand it rather than stat a
		// literal `~` — which is not a path any vault lives at.
		It("expands a leading ~ in the vault directory", func() {
			// `.` under the home directory IS the home directory, which exists.
			Expect(vaultDirHasDir("~", ".")).To(BeTrue())
		})
	})

	Describe("validateRole", func() {
		It("accepts the three roles the fleet uses", func() {
			for _, role := range []string{"manager", "agent", "human"} {
				value, err := validateRole(context.Background(), role)

				Expect(err).To(BeNil())
				Expect(value).To(Equal(role))
			}
		})

		// ⚠️ AN ABSENT ROLE IS `agent`, WHICH IS WHAT KEEPS EVERY TASK THAT HAS NEVER DECLARED ONE
		// ON THE BEHAVIOUR IT HAD BEFORE.
		It("defaults an absent role to agent", func() {
			for _, role := range []string{"", "   "} {
				value, err := validateRole(context.Background(), role)

				Expect(err).To(BeNil())
				Expect(value).To(Equal("agent"))
			}
		})

		// ⚠️ REFUSED RATHER THAN DEFAULTED: a worker opened in the wrong window with the wrong
		// colour is discovered only by noticing it.
		It("refuses a role that is not one of the three", func() {
			for _, role := range []string{"worker", "Agent", "hold", "agents", "manager,agent"} {
				_, err := validateRole(context.Background(), role)

				Expect(err).NotTo(BeNil())
				Expect(err.Error()).To(ContainSubstring("is not a role"))
			}
		})

		// The comparison is case-SENSITIVE and the value is TRIMMED, which is the supervisor's own
		// rule (server/role-map.mjs trims, then matches against the three literals). A padded role
		// is therefore a real role and not a typo.
		It("trims surrounding whitespace before matching", func() {
			value, err := validateRole(context.Background(), "  manager  ")

			Expect(err).To(BeNil())
			Expect(value).To(Equal("manager"))
		})
	})

	Describe("roleFromMap", func() {
		// The two vocabularies are deliberately different: the window is named for its purpose.
		It("maps a role to its window's chip and id", func() {
			orange, cyan, pink := "orange", "cyan", "pink"
			zero, one, two := 0, 1, 2
			entries := roleMap{
				"Managers": {Chip: &orange, WindowID: &zero},
				"Direct":   {Chip: &cyan, WindowID: &one},
				"Agents":   {Chip: &pink, WindowID: &two},
			}

			Expect(roleFromMap("manager", entries)).To(Equal(
				resolvedRole{Role: "manager", Chip: "orange", WindowID: "0", Resolved: true},
			))
			Expect(roleFromMap("agent", entries)).To(Equal(
				resolvedRole{Role: "agent", Chip: "pink", WindowID: "2", Resolved: true},
			))
			Expect(roleFromMap("human", entries)).To(Equal(
				resolvedRole{Role: "human", Chip: "cyan", WindowID: "1", Resolved: true},
			))
		})

		// ⚠️ A NULL CHIP IS A REAL ANSWER, NOT AN ABSENT ONE. The live map publishes
		// `"Hold": {"chip": null}`; reading that as "no chip published" is what makes the caller
		// send nothing rather than a default colour.
		It("resolves the window with no chip when the entry publishes a null chip", func() {
			two := 2
			entries := roleMap{"Agents": {Chip: nil, WindowID: &two}}

			Expect(roleFromMap("agent", entries)).To(Equal(
				resolvedRole{Role: "agent", WindowID: "2", Resolved: true},
			))
		})

		// ⚠️ AN ABSENT window_id MUST NOT READ AS WINDOW 0, WHICH IS THE MANAGERS WINDOW.
		It("reports unresolved when the entry carries no window id", func() {
			pink := "pink"
			entries := roleMap{"Agents": {Chip: &pink}}

			Expect(roleFromMap("agent", entries)).To(Equal(resolvedRole{Role: "agent"}))
		})

		It("reports unresolved when the map has no entry for the window", func() {
			Expect(roleFromMap("agent", roleMap{})).To(Equal(resolvedRole{Role: "agent"}))
		})
	})

	Describe("readRoleMap", func() {
		It("reads the chip and window id of each entry", func() {
			path := filepath.Join(GinkgoT().TempDir(), "wezterm-role-map.json")
			Expect(os.WriteFile(path, []byte(
				`{"Agents": {"chip": "pink", "window_id": 2}, "Hold": {"chip": null, "window_id": 3}}`,
			), 0o600)).To(BeNil())

			entries, err := readRoleMap(context.Background(), path)

			Expect(err).To(BeNil())
			Expect(entries).To(HaveLen(2))
			Expect(*entries["Agents"].Chip).To(Equal("pink"))
			Expect(*entries["Agents"].WindowID).To(Equal(2))
			Expect(entries["Hold"].Chip).To(BeNil())
			Expect(*entries["Hold"].WindowID).To(Equal(3))
		})

		It("fails on a file that is not the role map", func() {
			path := filepath.Join(GinkgoT().TempDir(), "wezterm-role-map.json")
			Expect(os.WriteFile(path, []byte("not json"), 0o600)).To(BeNil())

			_, err := readRoleMap(context.Background(), path)

			Expect(err).NotTo(BeNil())
		})

		It("fails on a role map that does not exist", func() {
			_, err := readRoleMap(
				context.Background(),
				filepath.Join(GinkgoT().TempDir(), "absent.json"),
			)

			Expect(err).NotTo(BeNil())
		})
	})

	Describe("composerEmpty", func() {
		// ⚠️ THE GLYPH ALONE IS NOT READINESS. The TUI paints the composer with a placeholder
		// suggestion before it will honour a submit, and a message sent into that phase is accepted
		// and then swallowed.
		It("reports a drawn, empty composer as ready", func() {
			Expect(composerEmpty("some output\n❯ \n")).To(BeTrue())
			Expect(composerEmpty("❯")).To(BeTrue())
		})

		It("reports a composer holding text as not ready", func() {
			Expect(composerEmpty("❯ hello")).To(BeFalse())
			Expect(composerEmpty("❯ Try \"fix lint errors\"")).To(BeFalse())
		})

		It("reports a pane with no composer at all as not ready", func() {
			Expect(composerEmpty("")).To(BeFalse())
			Expect(composerEmpty("no prompt here\n")).To(BeFalse())
		})

		// The composer is the LAST line the glyph starts: every echoed submitted prompt above it
		// carries the same glyph, so position — not the glyph alone — identifies it.
		It("reads the composer as the last prompt line", func() {
			Expect(composerLine("❯ first\noutput\n❯ second")).To(Equal("❯ second"))
			Expect(composerLine("no prompt")).To(BeEmpty())
		})
	})
})
