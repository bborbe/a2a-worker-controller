// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"

	"github.com/bborbe/errors"
)

// The backend names. They live here and nowhere else: this file is the one place above the
// seam that names a backend, so a backend becomes selectable by editing this selection
// wiring alone. The shared schema (types.go) names none of them, which is what keeps a
// backend's identity from becoming part of the control API.
const (
	// BackendDocker runs each worker as a container on the local host.
	BackendDocker Backend = "docker"
	// BackendKubernetes runs each worker as a workload in the cluster.
	BackendKubernetes Backend = "k8s"
	// BackendLocal runs each worker as a WezTerm tab on the local host, started with the
	// vault's own launcher. It is the one backend whose worker serves no A2A endpoint: a tab
	// is reached through its pane, not through a URL.
	BackendLocal Backend = "local"
)

// BackendProvider resolves a backend name to the WorkerBackend that serves it.
type BackendProvider interface {
	// Get returns the backend serving the named backend, or an error when the name is
	// not one the controller knows or its implementation is not wired.
	Get(ctx context.Context, backend Backend) (WorkerBackend, error)
}

// NewBackendProvider creates the backend selection wiring.
//
// docker, kubernetes and local are the implementations of the three backends the controller
// supports, in the order their names are declared above. Any of them may be nil while its
// implementation is not yet wired — and a backend that is not selected is passed as nil on
// purpose, because its settings exist only for it; Get then refuses that backend rather than
// handing back a nil seam that would fail on first use.
func NewBackendProvider(
	docker WorkerBackend,
	kubernetes WorkerBackend,
	local WorkerBackend,
) BackendProvider {
	return &backendProvider{docker: docker, kubernetes: kubernetes, local: local}
}

type backendProvider struct {
	docker     WorkerBackend
	kubernetes WorkerBackend
	local      WorkerBackend
}

// Get returns the WorkerBackend serving backend.
//
// The set of backends is closed and known at compile time, so this is a switch rather than
// a map registry: the compiler keeps it exhaustive, a rename propagates, and there is no
// registration order to get wrong. A name that is not in the set is refused, and so is a
// name whose implementation is not yet wired.
func (p *backendProvider) Get(ctx context.Context, backend Backend) (WorkerBackend, error) {
	var selected WorkerBackend
	switch backend {
	case BackendDocker:
		selected = p.docker
	case BackendKubernetes:
		selected = p.kubernetes
	case BackendLocal:
		selected = p.local
	default:
		return nil, errors.Errorf(ctx, "unknown backend %q", backend)
	}
	if selected == nil {
		return nil, errors.Errorf(ctx, "backend %q is not implemented", backend)
	}
	return selected, nil
}
