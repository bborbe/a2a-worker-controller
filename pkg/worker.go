// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import "context"

//counterfeiter:generate -o ../mocks/pkg-worker-backend.go --fake-name PkgWorkerBackend . WorkerBackend

// WorkerBackend is the single seam every backend implements. It is expressible with the
// control-API schema alone: it takes the schema's request shapes and returns its response
// shapes, so nothing above the seam names a backend and a new backend needs no change to
// the schema, to the executor or to the HTTP surface.
type WorkerBackend interface {
	// Create starts one worker serving the requested task and returns it together with
	// its own bearer token. A task that already has a worker returns the existing one
	// rather than starting a second.
	Create(ctx context.Context, req CreateRequest) (*CreateResponse, error)

	// List returns every worker the backend owns. It is rebuilt from the backend's own
	// resource state on every call, never from controller memory, so a controller
	// restart loses nothing.
	List(ctx context.Context) (*ListResponse, error)

	// Stop stops and removes the named worker and returns it.
	Stop(ctx context.Context, req StopRequest) (*StopResponse, error)
}
