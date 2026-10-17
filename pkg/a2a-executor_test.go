// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bborbe/a2a-worker-controller/mocks"
	"github.com/bborbe/a2a-worker-controller/pkg"
)

// run drains the executor's event sequence, so a spec can assert on the events and the
// errors a call produced rather than on the iterator itself.
func run(
	executor a2asrv.AgentExecutor,
	ctx context.Context,
	execCtx *a2asrv.ExecutorContext,
) ([]a2a.Event, []error) {
	events := make([]a2a.Event, 0)
	errs := make([]error, 0)
	for event, err := range executor.Execute(ctx, execCtx) {
		// A refusal yields a nil event beside its error; only real events are collected
		// so a spec can assert that a refused call produced no event at all.
		if event != nil {
			events = append(events, event)
		}
		errs = append(errs, err)
	}
	return events, errs
}

// execContextFor builds the executor context one control call arrives in: a message whose
// data part carries the request, and the task and context ids the SDK would have generated.
func execContextFor(request pkg.ControlRequest) *a2asrv.ExecutorContext {
	return &a2asrv.ExecutorContext{
		Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(request)),
		TaskID:    a2a.NewTaskID(),
		ContextID: a2a.NewContextID(),
	}
}

// responseOf returns the ControlResponse the executor emitted as its artifact, or nil when
// it emitted none.
func responseOf(events []a2a.Event) *pkg.ControlResponse {
	for _, event := range events {
		artifact, ok := event.(*a2a.TaskArtifactUpdateEvent)
		if !ok {
			continue
		}
		for _, part := range artifact.Artifact.Parts {
			raw, err := json.Marshal(part.Data())
			if err != nil {
				continue
			}
			var response pkg.ControlResponse
			if err := json.Unmarshal(raw, &response); err != nil {
				continue
			}
			return &response
		}
	}
	return nil
}

// terminalState returns the state of the last status event the executor emitted.
func terminalState(events []a2a.Event) a2a.TaskState {
	for index := len(events) - 1; index >= 0; index-- {
		status, ok := events[index].(*a2a.TaskStatusUpdateEvent)
		if ok {
			return status.Status.State
		}
	}
	return a2a.TaskStateUnspecified
}

var _ = Describe("A2AExecutor", func() {
	var ctx context.Context
	var backend *mocks.PkgWorkerBackend
	var executor a2asrv.AgentExecutor

	BeforeEach(func() {
		ctx = context.Background()
		backend = &mocks.PkgWorkerBackend{}
		// The mock is wired in through the real selection wiring, so the spec exercises
		// the provider as well as the executor.
		executor = pkg.NewA2AExecutor(pkg.NewBackendProvider(backend, nil, nil), pkg.BackendDocker)
	})

	Context("dispatching an operation", func() {
		It("dispatches create to the injected backend", func() {
			backend.CreateReturns(
				&pkg.CreateResponse{
					Worker: pkg.Worker{
						WorkerID: "worker-1",
						Task:     "task-1",
						A2AURL:   "http://worker-1",
						Backend:  pkg.BackendDocker,
						State:    pkg.WorkerStateRunning,
					},
					Token: "token-1",
				},
				nil,
			)

			events, errs := run(executor, ctx, execContextFor(pkg.ControlRequest{
				Operation: pkg.OperationCreate,
				Create:    &pkg.CreateRequest{Task: "task-1"},
			}))

			Expect(errs).To(HaveEach(BeNil()))
			Expect(backend.CreateCallCount()).To(Equal(1))
			_, request := backend.CreateArgsForCall(0)
			Expect(request.Task).To(Equal(pkg.TaskName("task-1")))

			response := responseOf(events)
			Expect(response).NotTo(BeNil())
			Expect(response.Operation).To(Equal(pkg.OperationCreate))
			Expect(response.Create.Worker.WorkerID).To(Equal(pkg.WorkerID("worker-1")))
			Expect(response.Create.Token).To(Equal("token-1"))
			Expect(terminalState(events)).To(Equal(a2a.TaskStateCompleted))
		})

		It("dispatches list to the injected backend", func() {
			backend.ListReturns(
				&pkg.ListResponse{
					Workers: []pkg.Worker{
						{WorkerID: "worker-1", Task: "task-1", State: pkg.WorkerStateRunning},
					},
				},
				nil,
			)

			events, errs := run(executor, ctx, execContextFor(pkg.ControlRequest{
				Operation: pkg.OperationList,
			}))

			Expect(errs).To(HaveEach(BeNil()))
			Expect(backend.ListCallCount()).To(Equal(1))

			response := responseOf(events)
			Expect(response).NotTo(BeNil())
			Expect(response.Operation).To(Equal(pkg.OperationList))
			Expect(response.List.Workers).To(HaveLen(1))
			Expect(terminalState(events)).To(Equal(a2a.TaskStateCompleted))
		})

		It("dispatches stop to the injected backend", func() {
			backend.StopReturns(
				&pkg.StopResponse{
					Worker: pkg.Worker{
						WorkerID: "worker-1",
						Task:     "task-1",
						State:    pkg.WorkerStateStopped,
					},
				},
				nil,
			)

			events, errs := run(executor, ctx, execContextFor(pkg.ControlRequest{
				Operation: pkg.OperationStop,
				Stop:      &pkg.StopRequest{WorkerID: "worker-1"},
			}))

			Expect(errs).To(HaveEach(BeNil()))
			Expect(backend.StopCallCount()).To(Equal(1))
			_, request := backend.StopArgsForCall(0)
			Expect(request.WorkerID).To(Equal(pkg.WorkerID("worker-1")))

			response := responseOf(events)
			Expect(response).NotTo(BeNil())
			Expect(response.Operation).To(Equal(pkg.OperationStop))
			Expect(response.Stop.Worker.State).To(Equal(pkg.WorkerStateStopped))
			Expect(terminalState(events)).To(Equal(a2a.TaskStateCompleted))
		})
	})

	Context("refusing a malformed request", func() {
		It("refuses an unknown operation", func() {
			events, errs := run(executor, ctx, execContextFor(pkg.ControlRequest{
				Operation: pkg.Operation("bogus"),
			}))

			Expect(errs).To(ContainElement(a2a.ErrInvalidParams))
			Expect(events).To(BeEmpty())
			Expect(backend.CreateCallCount()).To(Equal(0))
			Expect(backend.ListCallCount()).To(Equal(0))
			Expect(backend.StopCallCount()).To(Equal(0))
		})

		It("refuses a request that sets two operation-specific fields", func() {
			events, errs := run(executor, ctx, execContextFor(pkg.ControlRequest{
				Operation: pkg.OperationCreate,
				Create:    &pkg.CreateRequest{Task: "task-1"},
				Stop:      &pkg.StopRequest{WorkerID: "worker-1"},
			}))

			Expect(errs).To(ContainElement(a2a.ErrInvalidParams))
			Expect(events).To(BeEmpty())
			Expect(backend.CreateCallCount()).To(Equal(0))
		})

		It("refuses a create that sets no operation-specific field", func() {
			events, errs := run(executor, ctx, execContextFor(pkg.ControlRequest{
				Operation: pkg.OperationCreate,
			}))

			Expect(errs).To(ContainElement(a2a.ErrInvalidParams))
			Expect(events).To(BeEmpty())
			Expect(backend.CreateCallCount()).To(Equal(0))
		})

		It("refuses a list that sets an operation-specific field", func() {
			events, errs := run(executor, ctx, execContextFor(pkg.ControlRequest{
				Operation: pkg.OperationList,
				Create:    &pkg.CreateRequest{Task: "task-1"},
			}))

			Expect(errs).To(ContainElement(a2a.ErrInvalidParams))
			Expect(events).To(BeEmpty())
			Expect(backend.ListCallCount()).To(Equal(0))
		})

		It("refuses a message that carries no data part", func() {
			execCtx := &a2asrv.ExecutorContext{
				Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")),
				TaskID:    a2a.NewTaskID(),
				ContextID: a2a.NewContextID(),
			}

			events, errs := run(executor, ctx, execCtx)

			Expect(errs).To(ContainElement(a2a.ErrInvalidParams))
			Expect(events).To(BeEmpty())
		})
	})

	Context("when the selected backend is not wired", func() {
		It("reports the call as failed rather than answering", func() {
			executor = pkg.NewA2AExecutor(pkg.NewBackendProvider(nil, nil, nil), pkg.BackendDocker)

			events, errs := run(executor, ctx, execContextFor(pkg.ControlRequest{
				Operation: pkg.OperationList,
			}))

			Expect(errs).To(HaveEach(BeNil()))
			Expect(responseOf(events)).To(BeNil())
			Expect(terminalState(events)).To(Equal(a2a.TaskStateFailed))
		})
	})
})

var _ = Describe("A2A surface", func() {
	It("serves a control call over the JSON-RPC binding", func() {
		executor := pkg.NewA2AExecutor(
			pkg.NewBackendProvider(pkg.NewStubWorkerBackend(), nil, nil),
			pkg.BackendDocker,
		)
		handler := pkg.NewA2AHandler(executor)

		body := `{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":` +
			`{"messageId":"m1","role":"ROLE_USER","parts":[{"data":{"operation":"list"}}]}}}`
		req := httptest.NewRequest("POST", pkg.A2APath, strings.NewReader(body))
		resp := httptest.NewRecorder()

		handler.ServeHTTP(resp, req)

		Expect(resp.Code).To(Equal(http.StatusOK))
		Expect(resp.Body.String()).To(ContainSubstring(`"operation":"list"`))
	})

	It("serves the Agent Card at the well-known path", func() {
		handler := pkg.NewAgentCardHandler(pkg.NewAgentCard(pkg.AgentCardConfig{
			Name:      "a2a-worker-controller",
			PublicURL: "https://example.test/a2a",
		}))

		req := httptest.NewRequest("GET", pkg.AgentCardPath, nil)
		resp := httptest.NewRecorder()

		handler.ServeHTTP(resp, req)

		Expect(resp.Code).To(Equal(http.StatusOK))
		Expect(resp.Body.String()).To(ContainSubstring(`"https://example.test/a2a"`))
	})
})

var _ = Describe("StubWorkerBackend", func() {
	var ctx context.Context
	var stub *pkg.StubWorkerBackend
	var executor a2asrv.AgentExecutor

	BeforeEach(func() {
		ctx = context.Background()
		stub = pkg.NewStubWorkerBackend()
		// The stub is selected through the wiring alone: the executor and the provider
		// are the production ones and neither knows it holds a stub.
		executor = pkg.NewA2AExecutor(pkg.NewBackendProvider(stub, nil, nil), pkg.BackendDocker)
	})

	It("round-trips create, list and stop", func() {
		createEvents, createErrs := run(executor, ctx, execContextFor(pkg.ControlRequest{
			Operation: pkg.OperationCreate,
			Create:    &pkg.CreateRequest{Task: "task-1"},
		}))
		Expect(createErrs).To(HaveEach(BeNil()))
		created := responseOf(createEvents)
		Expect(created).NotTo(BeNil())
		Expect(created.Create.Worker.Task).To(Equal(pkg.TaskName("task-1")))
		Expect(created.Create.Worker.WorkerID).NotTo(BeEmpty())
		Expect(created.Create.Token).NotTo(BeEmpty())

		listEvents, listErrs := run(executor, ctx, execContextFor(pkg.ControlRequest{
			Operation: pkg.OperationList,
		}))
		Expect(listErrs).To(HaveEach(BeNil()))
		listed := responseOf(listEvents)
		Expect(listed).NotTo(BeNil())
		Expect(listed.List.Workers).To(HaveLen(1))
		Expect(listed.List.Workers[0].WorkerID).To(Equal(created.Create.Worker.WorkerID))

		stopEvents, stopErrs := run(executor, ctx, execContextFor(pkg.ControlRequest{
			Operation: pkg.OperationStop,
			Stop:      &pkg.StopRequest{WorkerID: created.Create.Worker.WorkerID},
		}))
		Expect(stopErrs).To(HaveEach(BeNil()))
		stopped := responseOf(stopEvents)
		Expect(stopped).NotTo(BeNil())
		Expect(stopped.Stop.Worker.WorkerID).To(Equal(created.Create.Worker.WorkerID))
		Expect(stopped.Stop.Worker.State).To(Equal(pkg.WorkerStateStopped))

		emptyEvents, emptyErrs := run(executor, ctx, execContextFor(pkg.ControlRequest{
			Operation: pkg.OperationList,
		}))
		Expect(emptyErrs).To(HaveEach(BeNil()))
		Expect(responseOf(emptyEvents).List.Workers).To(BeEmpty())
	})

	It("refuses a task that already has a worker, and starts no second one", func() {
		firstEvents, firstErrs := run(executor, ctx, execContextFor(pkg.ControlRequest{
			Operation: pkg.OperationCreate,
			Create:    &pkg.CreateRequest{Task: "task-1"},
		}))
		Expect(firstErrs).To(HaveEach(BeNil()))
		Expect(responseOf(firstEvents).Create.Worker.WorkerID).NotTo(BeEmpty())

		// ⚠️ THE REFUSAL REACHES THE CALLER AS A BARE FAILED STATUS, WITH NO DETAIL. The executor
		// logs the backend's error and yields `TaskStateFailed` with a nil message, so the worker
		// id the refusal names is NOT readable from this reply — which is exactly why the
		// supervisor resolves it from `list` instead. This spec pins that shape, because the
		// recovery on the other side is built on it.
		secondEvents, secondErrs := run(executor, ctx, execContextFor(pkg.ControlRequest{
			Operation: pkg.OperationCreate,
			Create:    &pkg.CreateRequest{Task: "task-1"},
		}))
		Expect(secondErrs).To(HaveEach(BeNil()))
		Expect(terminalState(secondEvents)).To(Equal(a2a.TaskStateFailed))

		// ⚠️ One worker per task is the invariant the refusal protects, and it holds precisely
		// because the refusal starts nothing.
		listEvents, _ := run(executor, ctx, execContextFor(pkg.ControlRequest{
			Operation: pkg.OperationList,
		}))
		Expect(responseOf(listEvents).List.Workers).To(HaveLen(1))
	})

	It("refuses to stop a worker it does not hold", func() {
		events, errs := run(executor, ctx, execContextFor(pkg.ControlRequest{
			Operation: pkg.OperationStop,
			Stop:      &pkg.StopRequest{WorkerID: "missing"},
		}))

		Expect(errs).To(HaveEach(BeNil()))
		Expect(responseOf(events)).To(BeNil())
		Expect(terminalState(events)).To(Equal(a2a.TaskStateFailed))
	})
})
