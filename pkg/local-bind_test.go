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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/mocks"
	"github.com/bborbe/a2a-worker-controller/pkg"
)

const (
	// testBindTimeout is the session-registry poll window the specs drive the backend with, far
	// below the shipped DefaultLocalBindTimeout: every create that registers no session pays the
	// window in full.
	testBindTimeout = 30 * time.Millisecond
	// testDefaultVaultName is the vault-cli name the fake resolver gives the configured vault.
	testDefaultVaultName = "default-vault"
)

// The bind seams every local-backend spec runs against, reset per spec by resetBindSeams.
//
// ⚠️ THEY ARE PACKAGE-LEVEL SO EVERY SPEC FILE SHARES ONE HERMETIC DEFAULT. A spec left on the real
// seams would poll the operator's ~/.claude/sessions, read the installed vault-cli config, and — on
// a registry hit — write into a real vault through the real vault-cli.
var (
	testRegistryDir string
	testResolver    *mocks.PkgLocalVaultResolver
	testVaultCLI    *mocks.PkgVaultCLI
)

// resetBindSeams gives a spec an empty registry, a resolver that names the configured vault
// testDefaultVaultName and knows no other, and a vault-cli that records and succeeds.
func resetBindSeams() {
	testRegistryDir = GinkgoT().TempDir()
	testResolver = &mocks.PkgLocalVaultResolver{}
	testResolver.ResolveDirCalls(func(_ context.Context, dir string) (pkg.LocalVault, error) {
		return pkg.LocalVault{Name: testDefaultVaultName, Dir: dir}, nil
	})
	testResolver.ResolveNameReturns(pkg.LocalVault{}, errors.New("no such vault"))
	testVaultCLI = &mocks.PkgVaultCLI{}
}

// bindSeams wires the current bind seams, and the short bind window, into a backend config.
func bindSeams(config pkg.LocalConfig) pkg.LocalConfig {
	config.SessionRegistryDir = testRegistryDir
	config.BindTimeout = testBindTimeout
	config.VaultResolver = testResolver
	config.VaultCLI = testVaultCLI
	return config
}

var _ = Describe("LocalBackend session binding", func() {
	var (
		ctx         context.Context
		client      *fakeWeztermClient
		vault       string
		registryDir string
		resolver    *mocks.PkgLocalVaultResolver
		vaultCLI    *mocks.PkgVaultCLI
		backend     pkg.WorkerBackend
	)

	const task = pkg.TaskName("local-backend-task")

	// The same hermetic setup as the LocalBackend specs: a temporary vault, launcher, GUI socket,
	// store, empty role map, registry directory, and faked vault resolver and vault-cli — so no
	// spec here reads the operator's sessions or vault-cli config, or writes into a real vault.
	BeforeEach(func() {
		ctx = context.Background()
		vault = GinkgoT().TempDir()
		guiID := filepath.Join(GinkgoT().TempDir(), "gui-sock-"+strconv.Itoa(os.Getpid()))
		testClaudeScript = filepath.Join(GinkgoT().TempDir(), "cc-private")
		Expect(os.WriteFile(testClaudeScript, []byte("#!/bin/sh\n"), 0o755)).To(BeNil())
		client = &fakeWeztermClient{guiID: guiID}
		roleMapPath := filepath.Join(GinkgoT().TempDir(), "wezterm-role-map.json")
		Expect(os.WriteFile(roleMapPath, []byte("{}"), 0o600)).To(BeNil())
		resetBindSeams()
		registryDir, resolver, vaultCLI = testRegistryDir, testResolver, testVaultCLI
		backend = pkg.NewLocalBackend(
			client,
			pkg.NewJSONWorkerStore(filepath.Join(GinkgoT().TempDir(), "local-workers.json")),
			bindSeams(pkg.LocalConfig{
				VaultDir:      vault,
				ClaudeScript:  testClaudeScript,
				LivenessFloor: testLivenessFloor,
				RoleMapPath:   roleMapPath,
			}),
		)
	})

	writeTask := func(name pkg.TaskName, sessionID string) {
		dir := filepath.Join(vault, testTasksDir)
		Expect(os.MkdirAll(dir, 0o750)).To(BeNil())
		Expect(os.WriteFile(
			filepath.Join(dir, string(name)+".md"),
			[]byte(taskFile(sessionID)),
			0o600,
		)).To(BeNil())
	}

	// The session binding: the join from a tab to the session it started, through the session
	// registry by tab name, and the fleet's ownership rule applied through vault-cli. Each of these
	// is behaviour v0.14.1 did not have — it left a tab worker's session id to the worker's own
	// session, read only from the configured vault.
	Describe("Session binding", func() {
		const (
			otherVaultName = "other-vault"
			otherTasksDir  = "24 Tasks"
			newSession     = "11111111-2222-3333-4444-555555555555"
		)
		var otherVault string

		// register puts a session registry entry on disk, in Claude Code's own field names.
		register := func(file string, sessionID string, name string) {
			Expect(os.WriteFile(
				filepath.Join(registryDir, file),
				[]byte(`{"pid": `+strconv.Itoa(os.Getpid())+`, "sessionId": "`+sessionID+
					`", "name": "`+name+`"}`),
				0o600,
			)).To(BeNil())
		}

		// otherTaskPath is the task file's path in the non-default vault.
		otherTaskPath := func() string {
			return filepath.Join(otherVault, otherTasksDir, string(task)+".md")
		}

		writeOtherTask := func(sessionID string) {
			Expect(os.MkdirAll(filepath.Join(otherVault, otherTasksDir), 0o750)).To(BeNil())
			Expect(os.WriteFile(otherTaskPath(), []byte(taskFile(sessionID)), 0o600)).To(BeNil())
		}

		BeforeEach(func() {
			otherVault = GinkgoT().TempDir()
			resolver.ResolveNameStub = func(_ context.Context, name string) (pkg.LocalVault, error) {
				if name != otherVaultName {
					return pkg.LocalVault{}, errors.New("no such vault")
				}
				return pkg.LocalVault{
					Name:         otherVaultName,
					Dir:          otherVault,
					TasksDir:     otherTasksDir,
					ClaudeScript: testClaudeScript,
				}, nil
			}
			// The tab's session registers as soon as the pane exists, which is what the real one
			// does about a second later.
			client.onSpawn = func() { register("4242.json", newSession, testTabNamePrefix+string(task)) }
			// The fake vault-cli performs the stamp the real one would, so the spec can assert the
			// task file itself rather than only the argv.
			vaultCLI.RunStub = func(_ context.Context, args ...string) error {
				if len(args) > 1 && args[1] == "set" {
					return os.WriteFile(otherTaskPath(), []byte(taskFile(args[4])), 0o600)
				}
				return nil
			}
		})

		It("stamps the session into the task file of the vault the create named", func() {
			writeOtherTask("")

			response, err := backend.Create(
				ctx,
				pkg.CreateRequest{Task: task, Vault: otherVaultName},
			)

			Expect(err).To(BeNil())
			Expect(response.Bind).NotTo(BeNil())
			Expect(response.Bind.Action).To(Equal("stamped"))
			Expect(response.Bind.Error).To(BeEmpty())
			Expect(response.Worker.SessionID).To(Equal(newSession))
			Expect(client.spawns[0].inner).To(HavePrefix("cd '" + otherVault + "' && exec "))
			Expect(vaultCLI.RunCallCount()).To(Equal(1))
			_, args := vaultCLI.RunArgsForCall(0)
			Expect(args).To(Equal([]string{
				"task", "set", string(task), "claude_session_id", newSession,
				"--vault", otherVaultName,
			}))
			content, err := os.ReadFile(otherTaskPath())
			Expect(err).To(BeNil())
			Expect(string(content)).To(ContainSubstring("claude_session_id: " + newSession))
		})

		It("keeps reporting the bound session from list, reading the worker's own vault", func() {
			writeOtherTask("")
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task, Vault: otherVaultName})
			Expect(err).To(BeNil())

			listed, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(listed.Workers).To(HaveLen(1))
			Expect(listed.Workers[0].SessionID).To(Equal(newSession))
		})

		It(
			"leaves an existing owner alone and appends the session to the metrics sessions",
			func() {
				writeOtherTask("earlier-session")

				response, err := backend.Create(
					ctx,
					pkg.CreateRequest{Task: task, Vault: otherVaultName},
				)

				Expect(err).To(BeNil())
				Expect(response.Bind).NotTo(BeNil())
				Expect(response.Bind.Action).To(Equal("appended"))
				Expect(vaultCLI.RunCallCount()).To(Equal(1))
				_, args := vaultCLI.RunArgsForCall(0)
				Expect(args).To(Equal([]string{
					"task", "append-metrics-session", string(task), newSession,
					"--vault", otherVaultName,
				}))
				content, err := os.ReadFile(otherTaskPath())
				Expect(err).To(BeNil())
				Expect(string(content)).To(ContainSubstring("claude_session_id: earlier-session"))
				Expect(string(content)).NotTo(ContainSubstring(newSession))
			},
		)

		It(
			"binds through the configured vault's own vault-cli name when no vault is named",
			func() {
				writeTask(task, "")
				vaultCLI.RunStub = nil

				response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

				Expect(err).To(BeNil())
				Expect(response.Bind).NotTo(BeNil())
				Expect(response.Bind.Action).To(Equal("stamped"))
				_, args := vaultCLI.RunArgsForCall(0)
				Expect(args).To(ContainElements("--vault", testDefaultVaultName))
				Expect(client.spawns[0].inner).To(HavePrefix("cd '" + vault + "' && exec "))
			},
		)

		It("reports no bind and still succeeds when no session registers in the window", func() {
			writeOtherTask("")
			client.onSpawn = nil

			response, err := backend.Create(
				ctx,
				pkg.CreateRequest{Task: task, Vault: otherVaultName},
			)

			Expect(err).To(BeNil())
			Expect(response.Bind).To(BeNil())
			Expect(response.Worker.SessionID).To(BeEmpty())
			Expect(vaultCLI.RunCallCount()).To(Equal(0))
		})

		// ⚠️ AN EARLIER, STILL-LIVE SESSION OF THE SAME TASK ANSWERS TO THE SAME TAB NAME, and a
		// poll that matched it would bind a stranger's session to this worker.
		It("never binds a session that held the tab name before the spawn", func() {
			writeOtherTask("")
			register("1000.json", "session-of-an-earlier-tab", testTabNamePrefix+string(task))
			client.onSpawn = nil

			response, err := backend.Create(
				ctx,
				pkg.CreateRequest{Task: task, Vault: otherVaultName},
			)

			Expect(err).To(BeNil())
			Expect(response.Bind).To(BeNil())
			Expect(vaultCLI.RunCallCount()).To(Equal(0))
		})

		It("reports a failed vault-cli write on the bind without failing the create", func() {
			writeOtherTask("")
			vaultCLI.RunStub = nil
			vaultCLI.RunReturns(errors.New("vault-cli exploded"))

			response, err := backend.Create(
				ctx,
				pkg.CreateRequest{Task: task, Vault: otherVaultName},
			)

			Expect(err).To(BeNil())
			Expect(response.Bind).NotTo(BeNil())
			Expect(response.Bind.Action).To(BeEmpty())
			Expect(response.Bind.Error).To(ContainSubstring("vault-cli exploded"))
		})

		It("refuses an unknown vault before any tab is opened", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task, Vault: "no-such-vault"})

			Expect(err).NotTo(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})

		It("refuses a vault name shaped like a flag before any tab is opened", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task, Vault: "--config"})

			Expect(err).NotTo(BeNil())
			Expect(client.spawns).To(BeEmpty())
			Expect(resolver.ResolveNameCallCount()).To(Equal(0))
		})

		It("starts the worker with the first step ahead of the work prompt", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task, FirstStep: "step one"})

			Expect(err).To(BeNil())
			Expect(client.spawns[0].inner).To(HaveSuffix(
				"'step one\n\n" + `/vault-cli:work-on-task "local-backend-task"'`,
			))
		})

		It("refuses a first step carrying a NUL byte", func() {
			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task, FirstStep: "a\x00b"})

			Expect(err).NotTo(BeNil())
			Expect(client.spawns).To(BeEmpty())
		})
	})

})
