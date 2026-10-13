// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/pkg"
)

// errDaemon stands in for a docker CLI failure.
var errDaemon = errors.New("docker daemon unavailable")

// The label keys and the controller label's identity value, repeated here rather than read
// from the package so a rename of a key is a test failure rather than a test that follows
// the code.
const (
	testControllerLabel      = "com.bborbe.a2a-worker-controller"
	testControllerLabelValue = "true"
	testWorkerIDLabel        = "com.bborbe.a2a-worker-controller.worker-id"
	testTaskLabel            = "com.bborbe.a2a-worker-controller.task"
	testTokenEnvName         = "INTERACTIVE_AUTH_TOKEN"
)

// fakeDockerClient is the injected DockerClient seam. It records every invocation's argv and
// extra environment and answers from canned output, so a spec drives the real backend
// through the real code path without a daemon.
type fakeDockerClient struct {
	stdout   []string
	errs     []error
	args     [][]string
	extraEnv [][]string
}

func (f *fakeDockerClient) Run(
	_ context.Context,
	args []string,
	extraEnv []string,
) (string, error) {
	index := len(f.args)
	f.args = append(f.args, args)
	f.extraEnv = append(f.extraEnv, extraEnv)
	if index < len(f.errs) && f.errs[index] != nil {
		return "", f.errs[index]
	}
	if index < len(f.stdout) {
		return f.stdout[index], nil
	}
	return "", nil
}

// fakePortProber is the injected PortProber seam: a fixed set of ports is reported taken and
// every probe is recorded.
type fakePortProber struct {
	taken map[int]bool
	asked []int
}

func (f *fakePortProber) IsFree(_ context.Context, port int) bool {
	f.asked = append(f.asked, port)
	return !f.taken[port]
}

// psLine renders one docker ps line in the shape the backend's --format asks for.
func psLine(name string, workerID pkg.WorkerID, task pkg.TaskName, ports string) string {
	return name + "\t" + string(workerID) + "\t" + string(task) + "\t" + ports
}

// flagValues returns every value that follows flag in args.
func flagValues(args []string, flag string) []string {
	values := make([]string, 0)
	for index := 0; index < len(args)-1; index++ {
		if args[index] == flag {
			values = append(values, args[index+1])
		}
	}
	return values
}

// containsBareEnv reports whether args names a container environment variable without a
// value, i.e. a `-e NAME` with no `=`.
func containsBareEnv(args []string, name string) bool {
	for index := 0; index < len(args)-1; index++ {
		if args[index] == "-e" && args[index+1] == name {
			return true
		}
	}
	return false
}

// runCallIndex returns the index of the docker run invocation, or -1 when none was made.
func runCallIndex(calls [][]string) int {
	for index, args := range calls {
		if len(args) > 0 && args[0] == "run" {
			return index
		}
	}
	return -1
}

var _ = Describe("DockerBackend", func() {
	var ctx context.Context

	const (
		name     = "a2a-worker-abc123def456"
		workerID = pkg.WorkerID("abc123def456")
		task     = pkg.TaskName("docker-backend-task")
		ports    = "0.0.0.0:9093->9090/tcp, :::9093->9090/tcp"
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("Create", func() {
		It("starts a container for a task the daemon does not know", func() {
			client := &fakeDockerClient{stdout: []string{"", ""}}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Worker.Task).To(Equal(task))
			Expect(response.Worker.Backend).To(Equal(pkg.BackendDocker))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateRunning))
			Expect(response.Worker.WorkerID).NotTo(BeEmpty())
			Expect(response.Worker.A2AURL).To(Equal("http://localhost:9093/a2a"))
			Expect(response.Token).NotTo(BeEmpty())
		})

		It("lists first so a create dedups against the daemon", func() {
			client := &fakeDockerClient{stdout: []string{"", ""}}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			_, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(client.args).To(HaveLen(2))
			Expect(client.args[0]).To(ContainElement("ps"))
			Expect(client.args[0]).To(ContainElement("label=" + testControllerLabel))
		})

		It("labels the container with the controller identity, the worker id and the task", func() {
			client := &fakeDockerClient{stdout: []string{"", ""}}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			runIndex := runCallIndex(client.args)
			Expect(runIndex).NotTo(Equal(-1))
			Expect(flagValues(client.args[runIndex], "--label")).To(ConsistOf(
				testControllerLabel+"="+testControllerLabelValue,
				testWorkerIDLabel+"="+string(response.Worker.WorkerID),
				testTaskLabel+"="+string(task),
			))
		})

		It("keeps the token out of every argument and out of every label", func() {
			client := &fakeDockerClient{stdout: []string{"", ""}}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			runIndex := runCallIndex(client.args)
			Expect(runIndex).NotTo(Equal(-1))
			run := client.args[runIndex]

			// The whole argv is credential-free, so it is safe for the error path to print.
			Expect(strings.Join(run, " ")).NotTo(ContainSubstring(response.Token))
			Expect(
				flagValues(run, "--label"),
			).NotTo(ContainElement(ContainSubstring(response.Token)))
			// And no label key is credential-shaped.
			for _, label := range flagValues(run, "--label") {
				Expect(strings.SplitN(label, "=", 2)[0]).NotTo(MatchRegexp(`(?i)token|auth|secret`))
			}
		})

		It("passes the token through the environment, named bare on the command line", func() {
			client := &fakeDockerClient{stdout: []string{"", ""}}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			runIndex := runCallIndex(client.args)
			Expect(runIndex).NotTo(Equal(-1))

			// Bare: no `=value` anywhere on the command line.
			Expect(containsBareEnv(client.args[runIndex], testTokenEnvName)).To(BeTrue())
			Expect(strings.Join(client.args[runIndex], " ")).
				NotTo(ContainSubstring(testTokenEnvName + "="))
			// The value travels in the child's environment instead.
			Expect(client.extraEnv[runIndex]).
				To(Equal([]string{testTokenEnvName + "=" + response.Token}))
		})

		It("publishes on the first free host port, skipping the ones already bound", func() {
			client := &fakeDockerClient{stdout: []string{"", ""}}
			prober := &fakePortProber{taken: map[int]bool{9093: true, 9094: true}}
			backend := pkg.NewDockerBackend(client, prober, pkg.DefaultDockerConfig())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(prober.asked).To(Equal([]int{9093, 9094, 9095}))
			Expect(response.Worker.A2AURL).To(Equal("http://localhost:9095/a2a"))
			Expect(flagValues(client.args[runCallIndex(client.args)], "-p")).
				To(Equal([]string{"9095:9090"}))
		})

		It("refuses to create when no host port in the range is free", func() {
			client := &fakeDockerClient{stdout: []string{""}}
			prober := &fakePortProber{taken: map[int]bool{9093: true, 9094: true, 9095: true}}
			backend := pkg.NewDockerBackend(client, prober, pkg.DockerConfig{
				Image:         pkg.DefaultDockerImage,
				AgentNameBase: pkg.DefaultAgentNameBase,
				Model:         pkg.DefaultAnthropicModel,
				AllowedTools:  pkg.DefaultAllowedTools,
				PortStart:     9093,
				PortEnd:       9095,
			})

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(runCallIndex(client.args)).To(Equal(-1))
		})

		It("returns the existing worker for a task that already has one", func() {
			client := &fakeDockerClient{
				stdout: []string{psLine(name, workerID, task, ports)},
			}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Worker.WorkerID).To(Equal(workerID))
			Expect(response.Worker.A2AURL).To(Equal("http://localhost:9093/a2a"))
			// A duplicate create is a +0 delta: nothing was started.
			Expect(client.args).To(HaveLen(1))
			Expect(runCallIndex(client.args)).To(Equal(-1))
		})

		It("returns no token for an existing worker, because none was kept", func() {
			client := &fakeDockerClient{
				stdout: []string{psLine(name, workerID, task, ports)},
			}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).To(BeNil())
			Expect(response.Token).To(BeEmpty())
		})

		It("does not dedup a different task onto an existing worker", func() {
			client := &fakeDockerClient{
				stdout: []string{psLine(name, workerID, task, ports), ""},
			}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: "another-task"})

			Expect(err).To(BeNil())
			Expect(response.Worker.Task).To(Equal(pkg.TaskName("another-task")))
			Expect(response.Worker.WorkerID).NotTo(Equal(workerID))
			Expect(runCallIndex(client.args)).NotTo(Equal(-1))
		})

		It("wraps a daemon failure rather than reporting a worker", func() {
			client := &fakeDockerClient{
				stdout: []string{"", ""},
				errs:   []error{nil, errDaemon},
			}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Create(ctx, pkg.CreateRequest{Task: task})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
		})
	})

	Describe("List", func() {
		It("returns every labelled container as a running worker", func() {
			client := &fakeDockerClient{
				stdout: []string{
					psLine(name, workerID, task, ports) + "\n" +
						psLine("a2a-worker-ffff", "ffff", "second-task",
							"0.0.0.0:9094->9090/tcp"),
				},
			}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(HaveLen(2))
			Expect(response.Workers[0].WorkerID).To(Equal(workerID))
			Expect(response.Workers[0].Task).To(Equal(task))
			Expect(response.Workers[0].A2AURL).To(Equal("http://localhost:9093/a2a"))
			Expect(response.Workers[0].Backend).To(Equal(pkg.BackendDocker))
			Expect(response.Workers[0].State).To(Equal(pkg.WorkerStateRunning))
			Expect(response.Workers[1].A2AURL).To(Equal("http://localhost:9094/a2a"))
		})

		It("filters on the controller label and reads nothing else", func() {
			client := &fakeDockerClient{stdout: []string{""}}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			_, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(client.args).To(HaveLen(1))
			Expect(client.args[0]).To(ContainElement("label=" + testControllerLabel))
		})

		It("rebuilds from the daemon on every call, caching nothing", func() {
			client := &fakeDockerClient{
				stdout: []string{psLine(name, workerID, task, ports), ""},
			}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			first, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(first.Workers).To(HaveLen(1))

			second, err := backend.List(ctx)
			Expect(err).To(BeNil())
			Expect(second.Workers).To(BeEmpty())
			Expect(client.args).To(HaveLen(2))
		})

		It("skips a line that is not a fully labelled container", func() {
			client := &fakeDockerClient{
				stdout: []string{
					psLine(name, workerID, task, ports) + "\n" +
						"a2a-worker-broken\t\t\tsome-ports\n" +
						"a2a-worker-short\tfff\n",
				},
			}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.List(ctx)

			Expect(err).To(BeNil())
			Expect(response.Workers).To(HaveLen(1))
			Expect(response.Workers[0].WorkerID).To(Equal(workerID))
		})
	})

	Describe("Stop", func() {
		It("stops and removes the container the worker id names", func() {
			client := &fakeDockerClient{
				stdout: []string{psLine(name, workerID, task, ports), "", ""},
			}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: workerID})

			Expect(err).To(BeNil())
			Expect(response.Worker.WorkerID).To(Equal(workerID))
			Expect(response.Worker.State).To(Equal(pkg.WorkerStateStopped))
			Expect(client.args).To(HaveLen(3))
			Expect(client.args[1]).To(Equal([]string{"stop", name}))
			Expect(client.args[2]).To(Equal([]string{"rm", name}))
		})

		It("refuses a worker id the controller does not own, touching nothing", func() {
			client := &fakeDockerClient{
				stdout: []string{psLine(name, workerID, task, ports)},
			}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: "not-ours"})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.args).To(HaveLen(1))
		})

		It("refuses when no labelled container exists at all", func() {
			client := &fakeDockerClient{stdout: []string{""}}
			backend := pkg.NewDockerBackend(client, &fakePortProber{}, pkg.DefaultDockerConfig())

			response, err := backend.Stop(ctx, pkg.StopRequest{WorkerID: workerID})

			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
			Expect(client.args).To(HaveLen(1))
		})
	})
})
