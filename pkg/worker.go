// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import "context"

//counterfeiter:generate -o ../mocks/pkg-worker-backend.go --fake-name PkgWorkerBackend . WorkerBackend

// WorkerBackend is the single seam every backend implements. It is expressible with the
// control-API schema alone: it takes the schema's request shapes and returns its response
// shapes, so nothing above the seam names a backend and a new backend needs no change to
// the executor or to the HTTP surface.
//
// ⚠️ THE SCHEMA HALF OF THAT IS NO LONGER UNCONDITIONAL. Adding a backend whose workers are
// not reachable at a URL — the local backend, whose workers are WezTerm tabs — widened
// Worker with pane_id and session_id (types.go), because a tab has no A2A URL and the pane is
// the only handle a caller gets on it. That widening was deliberate and is recorded there;
// what still holds is the narrower rule, that a backend's IDENTITY never reaches this schema.
// A backend whose workers answer at their own URL still needs no schema change at all.
type WorkerBackend interface {
	// Create starts one worker serving the requested task and returns it together with
	// its own bearer token. ⚠️ A task that already has a worker is REFUSED, naming that
	// worker's id, rather than answered: only one worker is ever started per task, and the
	// existing worker's token cannot be returned because the backend kept no copy of it —
	// so a success reply would name a worker no caller could authenticate to, which reads
	// as a working worker while being unreachable. The caller's recovery is to stop the
	// named worker and create again.
	Create(ctx context.Context, req CreateRequest) (*CreateResponse, error)

	// List returns every worker the backend owns. It is rebuilt from the backend's own
	// resource state on every call, never from controller memory, so a controller
	// restart loses nothing.
	List(ctx context.Context) (*ListResponse, error)

	// Stop stops and removes the named worker and returns it.
	Stop(ctx context.Context, req StopRequest) (*StopResponse, error)
}
