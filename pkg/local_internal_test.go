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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

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
		// this controller never opened, so reporting it would be a phantom worker.
		It("drops a worker whose pane id is live but belongs to a different GUI", func() {
			workers := []LocalWorker{{WorkerID: "w1", PaneID: "1", GuiID: "gui-a"}}

			Expect(reconcileWorkers(workers, map[string]bool{"1": true}, "gui-b")).To(BeEmpty())
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
