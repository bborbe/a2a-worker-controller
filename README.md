# a2a-worker-controller

Stateless A2A worker controller — `create` / `list` / `stop` over A2A, one isolated worker
per task, with Docker and k3s backends behind a single backend-agnostic seam.

The controller **creates** workers and **owns** them by label. It is never in the message
path between a caller and a worker: `create` returns the worker's own A2A URL, and callers
talk to that URL directly.

## Install

```bash
go install github.com/bborbe/a2a-worker-controller@latest
```

## Configure

Required — the process refuses to start without these:

| Flag | Env | Meaning |
|---|---|---|
| `-listen` | `LISTEN` | address to listen on, e.g. `:8080` |
| `-sentry-dsn` | `SENTRY_DSN` | Sentry DSN |
| `-a2a-public-url` | `A2A_PUBLIC_URL` | externally reachable endpoint the Agent Card advertises |
| `-a2a-auth-token` | `A2A_AUTH_TOKEN` | bearer token every gated request must present |
| `-worker-image` | `WORKER_IMAGE` | image every created worker container runs |

Optional, with defaults:

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-backend` | `BACKEND` | `docker` | which backend serves workers — `docker`, `k8s` or `local` |
| `-a2a-agent-name` | `A2A_AGENT_NAME` | `a2a-worker-controller` | name the Agent Card advertises |
| `-sentry-proxy` | `SENTRY_PROXY` | — | Sentry proxy URL |
| `-build-git-version` | `BUILD_GIT_VERSION` | `dev` | reported build version |
| `-build-git-commit` | `BUILD_GIT_COMMIT` | `none` | reported build commit |
| `-local-vault` | `LOCAL_VAULT` | — | **local backend, required**: vault root the workers' task files live in |
| `-local-claude-script` | `LOCAL_CLAUDE_SCRIPT` | — | **local backend, required**: the vault's `claude_script` launcher every worker tab runs |
| `-local-window-id` | `LOCAL_WINDOW_ID` | — | local backend: WezTerm window id every worker tab opens in; empty lands the tab in the window the spawning process is associated with |
| `-local-wezterm` | `LOCAL_WEZTERM` | the app bundle's binary | local backend: absolute path to the `wezterm` binary |
| `-local-state-file` | `LOCAL_STATE_FILE` | `~/.local/state/a2a-worker-controller/local-workers.json` | local backend: the ownership store |

The local backend's two required settings are checked only when it is the backend selected; a
controller serving containers starts without them.

⚠️ **`-local-wezterm` is an absolute path and a bare `wezterm` will not work.** Under launchd
`wezterm` is not on `PATH` — it exists only inside the app bundle — and Go resolves the binary
against the *parent's* `PATH`, never against the child environment, so prepending to the
child's `PATH` cannot repair it. The default is `/Applications/WezTerm.app/Contents/MacOS/wezterm`.

`WORKER_IMAGE` is **required and has no default on purpose** — an untagged reference such as
`bborbe/agent-claude` resolves to `:latest`, which need not exist, and the failure surfaces
only later as a `docker run` exit 125.

### The workers' model route

`create` gives every worker three model variables. Two are **compile-time constants**, not
configuration:

| Variable | Source | Value |
|---|---|---|
| `ANTHROPIC_BASE_URL` | constant `routerBaseURL` (`pkg/docker.go:72`) | `http://host.docker.internal:8788` — the host router as seen from inside a container |
| `ANTHROPIC_MODEL` | constant `DefaultAnthropicModel` (`pkg/docker.go:56`) | `MiniMax-M2.7-highspeed` |
| `ANTHROPIC_AUTH_TOKEN` | **the controller's own environment** | passed **bare**, so the value never appears in argv or in `ps` |

Only the token is configurable, and only by being present in the controller's environment
when it starts.

⚠️ **A controller started without `ANTHROPIC_AUTH_TOKEN` in its environment produces workers
that start, answer the Agent Card and run their session cache, but reply
`Not logged in · Please run /login` to every turn.** Nothing else fails, so the symptom looks
like a worker bug rather than a missing variable on the controller.

## Run

```bash
export LISTEN=:8080
export SENTRY_DSN=<dsn>
export A2A_PUBLIC_URL=http://localhost:8080
export A2A_AUTH_TOKEN=<token>
export WORKER_IMAGE=bborbe/agent-claude:v0.19.1
export ANTHROPIC_AUTH_TOKEN=<router key>   # forwarded to workers; see the note above

a2a-worker-controller
```

Or via Docker:

```bash
docker run --rm -p 8080:8080 \
  -e LISTEN=:8080 \
  -e A2A_PUBLIC_URL=http://localhost:8080 \
  -e A2A_AUTH_TOKEN \
  -e WORKER_IMAGE=bborbe/agent-claude:v0.19.1 \
  bborbe/a2a-worker-controller:latest
```

## Control API

Served over **A2A** at `/a2a` — JSON-RPC 2.0, `method: "SendMessage"`, with the control
request carried in a **data** part (no `kind` discriminator). Every gated route requires
`Authorization: Bearer <A2A_AUTH_TOKEN>`.

```bash
curl -s http://localhost:8080/a2a \
  -H "Authorization: Bearer $A2A_AUTH_TOKEN" -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{
        "messageId":"1","role":"ROLE_USER",
        "parts":[{"data":{"operation":"list"}}]}}}'
```

| Operation | Request | Reply |
|---|---|---|
| `create` | `{"operation":"create","create":{"task":"<name>"}}` | `{"create":{"worker":{…},"token":"…"}}` — ⚠️ **FAILED** if the task already has a worker; see below |
| `list` | `{"operation":"list"}` | `{"list":{"workers":[…]}}` |
| `stop` | `{"operation":"stop","stop":{"worker_id":"…"}}` | `{"stop":{"worker":{…}}}` |

The control reply is at `result.task.artifacts[0].parts[0].data`. A `worker` is
`{worker_id, task, a2a_url, backend, state, pane_id, session_id}`; the `token` returned by
`create` is that worker's **own** A2A bearer token and is returned by no other operation.

⚠️ **`pane_id` and `session_id` exist for workers that have no URL.** A `local` worker is a
WezTerm tab: it serves no A2A endpoint, so `a2a_url` is **empty**, the token `create` returns
is **empty**, and the pane id is the only handle a caller has on it — it is what a supervisor
messages through, while `stop` takes the `worker_id` (see the table above). `session_id` is the
Claude Code session the tab bound to the task; it is empty until that session has bound itself,
which happens after `create` returns, so a caller that needs it reads `list` — which re-reads
each task file and reports the id once the binding has landed — rather than treating the empty
value as a failure. For a `docker` or `k8s` worker both fields are empty and `a2a_url` is set.

⚠️ **A `create` for a task that already has a worker is REFUSED, and the refusal is not a
transport failure.** Only one worker is ever started per task, and that worker's token cannot
be returned — the controller kept no copy of it — so answering with the worker would name
something no caller could authenticate to: a reply that reads as a working worker while being
unreachable. The controller therefore fails the call, and its log names the existing worker's
id.

⚠️ **That id does not travel to the caller.** A backend failure is reported as
`TASK_STATE_FAILED` with no detail attached, so a caller that must recover — stop the existing
worker, then create again — resolves the id **itself** from `list`, matching on the `task` it
asked for. ⚠️ Do **not** retry `create` unchanged on this failure: it fails identically and
forever while the worker it names is left running.

**The schema is `pkg/types.go`** — the single definition of the API, shared by the Docker
backend, the k3s sibling, and the supervisor's `spawn_agent` target. Nothing above
`pkg/backend-provider.go` may name a backend.

## Ownership and labels

Every worker container carries exactly three labels, and ownership is read from them — never
from memory, so `list` is correct after any restart:

| Label | Value |
|---|---|
| `com.bborbe.a2a-worker-controller` | fixed marker |
| `com.bborbe.a2a-worker-controller.worker-id` | the worker id |
| `com.bborbe.a2a-worker-controller.task` | the task the worker serves |

A container without the marker label is never listed and never stopped. No credential is ever
written to a label.

## Development

`example.env` holds the local development defaults. `make precommit` runs format, generate,
test and lint.

## License

BSD-2-Clause. See [LICENSE](LICENSE).
