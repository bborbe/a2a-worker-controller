// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

import (
	"context"
	"encoding/json"
	"iter"
	"log/slog"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/bborbe/errors"
)

// a2aExecutor serves the control API over A2A. One SendMessage carries one ControlRequest
// in the message's data part and answers with one ControlResponse in an artifact.
//
// It never names a backend. It asks the injected provider for the WorkerBackend that serves
// the backend name it was constructed with, so the executor is identical for every backend
// and a new backend is selectable without touching this file.
type a2aExecutor struct {
	provider BackendProvider
	backend  Backend
}

var _ a2asrv.AgentExecutor = (*a2aExecutor)(nil)

// NewA2AExecutor creates the A2A executor for the control API.
//
// provider resolves a backend name to the WorkerBackend that serves it; backend is the name
// the controller was configured with. Both are injected — the executor reads no global and
// no environment — so the same executor serves any backend, and a test drives it with a
// stub through the real selection wiring.
func NewA2AExecutor(provider BackendProvider, backend Backend) a2asrv.AgentExecutor {
	return &a2aExecutor{provider: provider, backend: backend}
}

// Execute runs one control-API call and yields the events that describe it: the submitted
// task, the ControlResponse as an artifact, and the terminal status.
//
// A request that does not carry exactly one well-formed operation is refused with
// a2a.ErrInvalidParams before any task is created. A backend failure yields a failed
// status: the backend's error text is logged and never returned to the caller.
func (e *a2aExecutor) Execute(
	ctx context.Context,
	execCtx *a2asrv.ExecutorContext,
) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		request, err := controlRequestFromMessage(ctx, execCtx.Message)
		if err != nil {
			slog.Warn("a2a control request refused", "error", err)
			yield(nil, a2a.ErrInvalidParams)
			return
		}

		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}

		response, err := e.dispatch(ctx, request)
		if err != nil {
			slog.Warn("a2a control call failed", "error", err)
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, nil), nil)
			return
		}

		if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewDataPart(response)), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

// Cancel yields the cancelled status the SDK's default cancellation shape uses. A control
// call is a single request and response with no live turn to interrupt, so this is a total
// function that matches the interface without claiming cancellation semantics this service
// does not have.
func (e *a2aExecutor) Cancel(
	_ context.Context,
	execCtx *a2asrv.ExecutorContext,
) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// dispatch resolves the backend and runs the one operation the request names. The request
// has already been validated, so each operation's own field is present.
func (e *a2aExecutor) dispatch(
	ctx context.Context,
	request *ControlRequest,
) (*ControlResponse, error) {
	backend, err := e.provider.Get(ctx, e.backend)
	if err != nil {
		return nil, errors.Wrap(ctx, err, "resolve backend failed")
	}

	switch request.Operation {
	case OperationCreate:
		response, err := backend.Create(ctx, *request.Create)
		if err != nil {
			return nil, errors.Wrap(ctx, err, "create worker failed")
		}
		return &ControlResponse{Operation: OperationCreate, Create: response}, nil
	case OperationList:
		response, err := backend.List(ctx)
		if err != nil {
			return nil, errors.Wrap(ctx, err, "list workers failed")
		}
		return &ControlResponse{Operation: OperationList, List: response}, nil
	case OperationStop:
		response, err := backend.Stop(ctx, *request.Stop)
		if err != nil {
			return nil, errors.Wrap(ctx, err, "stop worker failed")
		}
		return &ControlResponse{Operation: OperationStop, Stop: response}, nil
	default:
		return nil, errors.Errorf(ctx, "unknown operation %q", request.Operation)
	}
}

// controlRequestFromMessage reads the ControlRequest from the message's first data part and
// validates it. A message with no data part, a data part that does not decode as a
// ControlRequest, an unknown operation, and a request that sets no operation-specific field
// or more than one are all refused rather than guessed at.
func controlRequestFromMessage(
	ctx context.Context,
	message *a2a.Message,
) (*ControlRequest, error) {
	if message == nil {
		return nil, errors.New(ctx, "message is empty")
	}
	for _, part := range message.Parts {
		if part == nil || part.Data() == nil {
			continue
		}
		// A data part decoded from the wire holds a generic map, and one built in
		// process holds the caller's own value, so the request is decoded from its
		// JSON form either way.
		raw, err := json.Marshal(part.Data())
		if err != nil {
			return nil, errors.Wrap(ctx, err, "marshal data part failed")
		}
		var request ControlRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, errors.Wrap(ctx, err, "decode control request failed")
		}
		if err := validateControlRequest(ctx, &request); err != nil {
			return nil, err
		}
		return &request, nil
	}
	return nil, errors.New(ctx, "message carries no data part")
}

// validateControlRequest enforces the schema's exactly-one rule: exactly one
// operation-specific field is set and it matches Operation. list sets none, and an
// operation this schema does not define is refused.
func validateControlRequest(ctx context.Context, request *ControlRequest) error {
	switch request.Operation {
	case OperationCreate:
		if request.Create == nil || request.Stop != nil {
			return errors.Errorf(ctx, "operation %q requires create and no stop", request.Operation)
		}
	case OperationList:
		if request.Create != nil || request.Stop != nil {
			return errors.Errorf(
				ctx,
				"operation %q sets no operation-specific field",
				request.Operation,
			)
		}
	case OperationStop:
		if request.Stop == nil || request.Create != nil {
			return errors.Errorf(ctx, "operation %q requires stop and no create", request.Operation)
		}
	default:
		return errors.Errorf(ctx, "unknown operation %q", request.Operation)
	}
	return nil
}
