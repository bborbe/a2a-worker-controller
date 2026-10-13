// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"sync"

	"github.com/bborbe/errors"
)

// stubBackendName is the stub's own backend identity. It is deliberately not a member of
// the closed set the selection wiring knows, so no Backend value ever resolves to the stub.
const stubBackendName Backend = "stub"

// StubWorkerBackend is a throwaway in-memory WorkerBackend. It exists for two reasons.
//
// It makes the executor testable with no container, no cluster and no credential: a test
// drives the real executor and the real selection wiring against it. And it is the second
// implementation of WorkerBackend, which is what proves the seam — the executor drives it
// through the same interface it drives a real backend through, without knowing which one it
// holds.
//
// It is absent from the selection wiring in backend-provider.go: no Backend value resolves
// to it. It is therefore reachable only by tests and by explicit construction, and can
// never serve a production request.
type StubWorkerBackend struct {
	mutex   sync.Mutex
	workers map[WorkerID]Worker
}

var _ WorkerBackend = (*StubWorkerBackend)(nil)

// NewStubWorkerBackend creates an empty in-memory backend.
func NewStubWorkerBackend() *StubWorkerBackend {
	return &StubWorkerBackend{workers: map[WorkerID]Worker{}}
}

// Create records a worker for the requested task and returns it with a token derived from
// its id. A task that already has a worker returns the existing one, matching the contract
// the real backends keep, so a repeated create never starts a second worker.
func (s *StubWorkerBackend) Create(
	ctx context.Context,
	req CreateRequest,
) (*CreateResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for _, worker := range s.workers {
		if worker.Task == req.Task {
			return &CreateResponse{Worker: worker, Token: stubToken(worker.WorkerID)}, nil
		}
	}

	worker := Worker{
		WorkerID: WorkerID(req.Task),
		Task:     req.Task,
		A2AURL:   "stub://" + string(req.Task),
		Backend:  stubBackendName,
		State:    WorkerStateRunning,
	}
	s.workers[worker.WorkerID] = worker
	return &CreateResponse{Worker: worker, Token: stubToken(worker.WorkerID)}, nil
}

// List returns every worker the stub holds, in no particular order.
func (s *StubWorkerBackend) List(ctx context.Context) (*ListResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	workers := make([]Worker, 0, len(s.workers))
	for _, worker := range s.workers {
		workers = append(workers, worker)
	}
	return &ListResponse{Workers: workers}, nil
}

// Stop removes the named worker and returns it, reporting it as stopped. A worker the stub
// does not hold is refused rather than silently ignored.
func (s *StubWorkerBackend) Stop(ctx context.Context, req StopRequest) (*StopResponse, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	worker, ok := s.workers[req.WorkerID]
	if !ok {
		return nil, errors.Errorf(ctx, "worker %q not found", req.WorkerID)
	}
	delete(s.workers, req.WorkerID)
	worker.State = WorkerStateStopped
	return &StopResponse{Worker: worker}, nil
}

// stubToken derives a worker's token from its id. It is not a credential: the stub serves
// nothing, so a fixed, non-secret string keeps a token-shaped value out of any log while
// still round-tripping through the response shape.
func stubToken(id WorkerID) string {
	return "stub-token-" + string(id)
}
