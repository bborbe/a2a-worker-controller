// Copyright (c) 2026 Benjamin Borbe All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkg

// WorkerID identifies one worker the controller created. It is the value a caller passes
// back to stop a worker and the value list reports, so it stays stable across a controller
// restart: it is read back from the worker's own labels rather than minted per request and
// held in memory.
type WorkerID string

// TaskName is the vault task a worker serves. It names the work, not the worker — a task
// has at most one worker, and a second create for a task that already has one is REFUSED,
// naming that worker, rather than answered with it.
type TaskName string

// Backend names the implementation that serves workers. Its value space belongs to the
// backend implementations and to the selection wiring, never to this schema: a constant
// naming a backend here would put that backend's identity above the seam this package
// exists to hold, and a second backend would have to edit the shared schema to become
// selectable.
type Backend string

// WorkerState is what list reports about a worker's lifecycle.
type WorkerState string

const (
	// WorkerStateRunning means the worker exists and is reachable: at its own A2A URL for a
	// backend whose workers serve one, or through its pane for a tab worker, which serves none.
	WorkerStateRunning WorkerState = "running"
	// WorkerStateStopped means the worker no longer exists. It appears only in the reply to
	// the stop that removed it; list never returns it.
	WorkerStateStopped WorkerState = "stopped"
	// WorkerStateUnknown means the worker's backing resource exists but the controller
	// cannot classify it, so a caller must not read it as running.
	WorkerStateUnknown WorkerState = "unknown"
)

// Operation names one control-API operation. It travels as a plain string so a caller
// written against this schema needs no shared Go type to address the controller.
type Operation string

const (
	// OperationCreate asks the controller for a worker serving one task.
	OperationCreate Operation = "create"
	// OperationList asks the controller for every worker it owns.
	OperationList Operation = "list"
	// OperationStop asks the controller to stop and remove one worker.
	OperationStop Operation = "stop"
)

// Worker is one worker as the control API reports it. It deliberately carries no token: a
// worker's token is returned once by create and never again, so no listing, no log and no
// restart can leak one.
//
// ⚠️ PaneID AND SessionID ARE A DELIBERATE WIDENING OF THIS SHARED SCHEMA. What this file
// keeps out is a backend's IDENTITY: no backend is named here, so a second backend does not
// have to edit the schema to become selectable. These two fields are not that — they record
// how a worker is reached when it has no URL. A worker that runs in a WezTerm tab serves no
// A2A endpoint, so its pane is the only handle a caller has on it, and without these fields
// there would be nowhere for that handle to travel. A caller reads them when a2a_url is
// empty and ignores them otherwise; both are empty for a backend whose workers answer at
// their own URL.
type Worker struct {
	WorkerID WorkerID    `json:"worker_id"`
	Task     TaskName    `json:"task"`
	A2AURL   string      `json:"a2a_url"`
	Backend  Backend     `json:"backend"`
	State    WorkerState `json:"state"`
	// PaneID is the WezTerm pane the worker runs in. Empty unless the worker runs in a tab.
	PaneID string `json:"pane_id"`
	// SessionID is the Claude Code session the worker's tab is bound to. Empty unless the
	// worker runs in a tab, and empty there too until the session's own task binding lands.
	SessionID string `json:"session_id"`
}

// ControlRequest is one control-API call. Exactly one operation-specific field is set and
// it matches Operation; a request that sets none, or more than one, is refused rather than
// guessed at.
type ControlRequest struct {
	Operation Operation `json:"operation"`
	// Create is set when Operation is OperationCreate.
	Create *CreateRequest `json:"create,omitempty"`
	// Stop is set when Operation is OperationStop.
	Stop *StopRequest `json:"stop,omitempty"`
}

// Mount is one filesystem path a caller asks the controller to make reachable inside the
// worker. It is DECLARED rather than passed through as a raw docker `-v` argument, because a
// raw argument would let a caller mount any path on the host and create is a public surface.
//
// ⚠️ `Volume` EXISTS ONLY TO BE REFUSED, and that is deliberate rather than vestigial. A
// named docker volume is not a host path, so the allowlist below cannot govern it: a caller
// free to name one could mount another workload's data volume. Refusing it by name — rather
// than by omitting the field — is what turns a silently-ignored key into an error that names
// the thing the caller asked for. Go's decoder drops unknown keys, so an omitted field would
// have accepted `{"volume": "…"}` and mounted nothing, which reads as success.
type Mount struct {
	// HostPath is the absolute host path to expose. It must fall inside the controller's
	// configured allowlist, or the create is refused.
	HostPath string `json:"host_path,omitempty"`
	// Volume is a named docker volume. Refused, always, in this revision.
	Volume string `json:"volume,omitempty"`
	// ContainerPath is where the path appears inside the worker. Required.
	ContainerPath string `json:"container_path"`
	// ReadOnly asks for the mount to be read-only. Defaults to false, so a caller that wants
	// a read-only mount must say so.
	ReadOnly bool `json:"read_only,omitempty"`
}

// Secret is one credential a caller asks the controller to resolve and deliver to the worker.
//
// ⚠️ THE VALUE NEVER TRAVELS IN THE REQUEST. The caller names a TeamVault key; the controller
// resolves it with its own credential and hands the worker the resolved value through a
// channel the request and the container's spec both stay clear of. That is the whole point of
// the shape: a caller that could send the value could also log it, and create is a public
// surface whose requests are recorded.
type Secret struct {
	// Name is the key the worker reads the secret under. Required.
	Name string `json:"name"`
	// TeamVaultKey is the TeamVault lookup key the controller resolves. A short identifier
	// rather than a secret, which is what makes naming it here safe.
	TeamVaultKey string `json:"teamvault_key"`
}

// CreateRequest asks for a worker serving one task.
type CreateRequest struct {
	Task TaskName `json:"task"`
	// Env carries extra environment for the worker, keyed by variable name. It is how a
	// caller hands a worker the settings its own tooling needs — the attention store's URL
	// and token, for instance.
	//
	// ⚠️ Every entry travels through the child environment, never in argv, because the
	// controller cannot tell a credential from a plain setting and create is a public
	// surface. A name colliding with one of the controller's own settings is refused rather
	// than merged: silently overriding AGENT_TYPE would start a worker outside the gated
	// service path, which is the setting its permission gate depends on.
	Env map[string]string `json:"env,omitempty"`
	// Mounts are the host paths the caller asks the controller to make reachable. Each is
	// checked against the controller's allowlist before any container is created.
	Mounts []Mount `json:"mounts,omitempty"`
	// Secrets are the credentials the caller asks the controller to resolve and deliver.
	// Only the TeamVault KEY travels here; the value is resolved controller-side.
	Secrets []Secret `json:"secrets,omitempty"`
	// Vault names the vault-cli vault the task lives in. Empty means the backend's own
	// configured vault, which is what every create meant before the field existed. Only the
	// local backend reads it: a named vault decides which task file the launcher and role are
	// read from, the directory the tab starts in, and the vault the session binding is written
	// to. An unknown name is refused before any worker is started.
	Vault string `json:"vault,omitempty"`
	// FirstStep is a message the worker is told BEFORE its work prompt, prepended to it as
	// `<first_step>\n\n<work prompt>` — the supervisor's `--first-step`. Empty sends the work
	// prompt alone. Only the local backend reads it.
	FirstStep string `json:"first_step,omitempty"`
}

// Bind reports what a create did to bind the worker's session to its task, mirroring the
// supervisor's spawn reply.
//
// ⚠️ A NIL Bind ON THE REPLY IS NOT A FAILURE, AND IT IS DISTINCT FROM A Bind CARRYING AN ERROR.
// nil means no session registered under the worker's tab name within the bind window, so there
// was nothing to bind; a non-nil Bind with Error set means a session was found and the write
// failed. Neither fails the create, because the worker is already running by then.
type Bind struct {
	// Action is `stamped` when the task had no claude_session_id and this session became its
	// owner, or `appended` when the task already had one, which was left alone, and this session
	// was appended to its metrics sessions. Empty when Error is set.
	Action string `json:"action,omitempty"`
	// Error names why a found session could not be bound.
	Error string `json:"error,omitempty"`
}

// CreateResponse is the reply to a create. It is the only place a worker's token is ever
// returned: the controller keeps no copy it depends on, so a caller that loses this reply
// stops the worker and creates another rather than asking the controller to repeat it.
//
// ⚠️ Launcher AND LauncherSource ARE A DELIBERATE WIDENING OF THIS SHARED SCHEMA, on the same
// terms as Worker's PaneID and SessionID. What this file keeps out is a backend's IDENTITY — no
// backend is named here, so a second backend does not have to edit the schema to become
// selectable — and neither field is that: a launcher is how a worker was STARTED, which any
// backend that runs a command can answer, and a caller that asked for a worker on a task's own
// `launcher:` otherwise has no way to tell which script it got without opening a pane and reading
// a process table. They are reported by create rather than on Worker because a launcher is a
// create-time decision: nothing later re-resolves it, and a list of running workers answers a
// different question.
type CreateResponse struct {
	Worker Worker `json:"worker"`
	// Token is the worker's own bearer credential. It must never be logged, echoed into a
	// transcript, or written to a file.
	Token string `json:"token"`
	// Launcher is the script the worker was started with. Empty for a backend that starts no
	// command — a container or a pod runs an image, not a launcher.
	Launcher string `json:"launcher"`
	// LauncherSource names the rule that chose Launcher: `task` when the task's own `launcher:`
	// frontmatter won, `goal` when exactly one of its goals named one, and `vault` when neither did
	// and the vault's own claude_script was used. It is what makes a silently-ignored field
	// visible from the reply, rather than only from a worker that came up on the wrong model.
	LauncherSource string `json:"launcher_source"`
	// Bind is the session binding's outcome — see Bind for what nil means. Always nil for a
	// backend that starts no Claude Code session of its own on this host.
	Bind *Bind `json:"bind,omitempty"`
}

// ListResponse is the reply to a list.
type ListResponse struct {
	Workers []Worker `json:"workers"`
}

// StopRequest names the worker to stop and remove.
type StopRequest struct {
	WorkerID WorkerID `json:"worker_id"`
}

// StopResponse is the reply to a stop. It carries the stopped worker so a caller can
// confirm which one the controller acted on.
type StopResponse struct {
	Worker Worker `json:"worker"`
}

// ControlResponse is the controller's reply to one ControlRequest. Exactly one
// operation-specific field is set and it matches Operation.
type ControlResponse struct {
	Operation Operation       `json:"operation"`
	Create    *CreateResponse `json:"create,omitempty"`
	List      *ListResponse   `json:"list,omitempty"`
	Stop      *StopResponse   `json:"stop,omitempty"`
}
