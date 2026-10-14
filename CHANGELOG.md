# Changelog

All notable changes to this project will be documented in this file.

Please choose versions by [Semantic Versioning](http://semver.org/).

## v0.9.0

- feat: a `create` request can carry environment variables for the worker it starts, so a `target: docker` worker can be handed `POD_ATTENTION_STORE_URL` / `POD_ATTENTION_TOKEN` and raise its permission gate on the attention board. `CreateRequest` gains an `Env` map, and the Docker backend forwards each entry as a **bare** `-e NAME` in argv with the value carried in the child process's environment — so no caller-supplied value, credential or not, is ever readable in `ps`. ⚠️ The uniform treatment is deliberate rather than lazy: the controller cannot tell a credential from a plain setting and `create` is a public surface, so nothing a caller passes reaches argv instead of classifying some entries as safe. ⚠️ **A name the controller sets itself is refused rather than merged**, and the load-bearing one is `AGENT_TYPE` — a caller free to overwrite it could start a worker outside the service path, where no permission decider is wired and the gate this feature exists to deliver would silently disappear. The other eight (`LISTEN`, `A2A_PUBLIC_URL`, `A2A_AGENT_NAME`, `INTERACTIVE_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_MODEL`, `ALLOWED_TOOLS`) are refused for the same reason at lower stakes, and a reserved name fails the request **before** a port or a token is minted. Keys are sorted, so an argv built from a map is deterministic and can be asserted rather than merely observed. The k8s backend is untouched: `BuildConfig` takes `(template, workerID, task)` and is never handed the new field, so its render is byte-identical by construction rather than by discipline.

## v0.8.0

- feat: let an `EGRESS_ALLOW` entry name a pod label — `namespace/key=value:port` narrows the rule to pods carrying that label, inside that namespace only. A plain `namespace:port` stays namespace-wide. Needed because on nuke `dev` port 9090 is served by the vault writers and the trading gateways alike, so `dev:9090` would let a worker reach the broker gateways; `dev/app=vault-obsidian-personal:9090` opens the personal vault writer alone. A malformed label is refused, never dropped.

## v0.7.0

- feat: add `WORKER_PRIORITY_CLASS` — when set, every k8s worker Config runs under that PriorityClass instead of the template's. On nuke each class carries its own ResourceQuota, and workers that inherited `claude-interactive` hit its 1-pod cap, which the shared `claude-interactive-0` already fills, so every worker StatefulSet failed `exceeded quota: claude-interactive`. Empty keeps the template's class.

## v0.6.1

- fix: set each k8s worker Config's `taskType` to the worker's own name instead of clearing it — the field's JSON tag has no `omitempty`, so the cleared value was sent as `""`, which the `Config` CRD rejects (`spec.taskType` must match `^[a-z0-9-]+$`); every `create` on nuke dev failed with `spec.taskType: Invalid value: ""`. The worker's own name is valid and no vault task carries it, so nothing routes to the worker

## v0.6.0

- feat: confine every k8s worker's egress with its own NetworkPolicy — on `create` the controller writes a policy owned by the worker's Config that selects the pod by `agent.benjamin-borbe.de/assignee=<worker>` (no static policy can select the workers alone, since the executor puts no Config label on the pod and selectors cannot prefix-match), declares only the Egress type so the worker's Ingress stays reachable, and allows DNS plus the in-cluster `namespace:port` destinations listed in the new `EGRESS_ALLOW` setting; the policy is written before the worker's Secret, so the pod cannot start unconfined, and `stop`'s Config delete garbage-collects it with the rest

## v0.5.0

- feat: add the k8s backend — `BACKEND=k8s` runs each worker as a per-task pod by writing, through the executor's typed clientset, one `type: service` agent `Config` the executor reconciles into a StatefulSet, cloned from a live template Config (`WORKER_TEMPLATE`, default `claude-interactive`) so the image pin and priority class stay where the chart keeps them, with the template's `trigger`, `taskType` and `taskTypes` cleared so the executor never routes vault tasks to a worker; each worker gets its own token Secret (the template Secret's keys with `INTERACTIVE_AUTH_TOKEN` replaced), ClusterIP Service and TLS Ingress on `<worker>.<INGRESS_DOMAIN>`, all owned by its Config, so `stop` is one Config delete the garbage collector completes; `list` is rebuilt from Configs carrying the controller label and never returns one the chart rendered
- chore: add `github.com/bborbe/agent-task-executor` v0.18.4 for the `Config` CRD's generated typed clientset, and `k8s.io/{api,apimachinery,client-go,apiextensions-apiserver}` v0.36.4 with v0.37.0 excluded, matching agent-task-executor — pinning `apiextensions-apiserver` without that exclude silently downgraded the executor module to v0.8.2, whose `ConfigSpec` has no `type` field, so every worker would have been cloned as a Job

## v0.4.4

- docs: replace the boilerplate README with an accurate recipe — the Usage block documented `-kafka-brokers` and `-datadir`, neither of which this controller has, and omitted every required flag; the README now lists the real configuration, the control API's request and reply shapes, the ownership labels, and the `ANTHROPIC_AUTH_TOKEN` requirement whose absence lets a worker start and serve its Agent Card while answering `Not logged in` to every turn

## v0.4.3

- fix: read published ports from the daemon before choosing one — a bind check cannot see a port a running container has published on macOS, where Docker Desktop forwards it in userspace, so the search returned its first candidate on every call and every `create` after the first collided on port 9093
- fix: serialize `create` — its dedup read and its port search both happen before the `docker run` that acts on them, so two overlapping creates could start two containers for one task or pick one port twice

## v0.4.2

- fix: make the worker image configurable and required — the backend ran a compile-time constant whose untagged reference resolves to `:latest`, a tag published nowhere for that image, so every `create` failed on docker's own resolution error after the request had already been accepted; the image now comes from configuration as `WORKER_IMAGE` and the binary refuses to start without it, since no default can resolve

## v0.4.1

- fix: register the artifact type with gob — the A2A task store copies every task through `encoding/gob` and registers only its own collection types, so an unregistered `*ControlResponse` failed the copy *after* the executor had already done its work and every control call returned `TASK_STATE_FAILED` on a running binary while the specs stayed green

## v0.4.0

- feat: require a bearer token on every gated route — the gate wraps the router, so `/a2a` and any route registered after it are covered by construction, with the five canonical admin endpoints exempt

## v0.3.0

- feat: implement the docker worker backend — `create` dedups on the task label and mints a per-worker token, `list` reads the daemon on every call, and `stop` removes only a container carrying the controller label

## v0.2.0

- feat: serve the control API over A2A — a JSON-RPC binding at `/a2a` and the Agent Card at the well-known path
- feat: define the `WorkerBackend` seam and the backend selection wiring that resolves a backend name to its implementation
- feat: add an in-memory stub backend that proves the seam without a container
- feat: add the A2A executor that dispatches `create` / `list` / `stop` to the injected backend

## v0.1.0

- feat: initial service scaffold
- feat: define the backend-independent control API (`create` / `list` / `stop`)
- refactor: drop the skeleton's Kafka and BoltDB wiring and its demo endpoints; log via slog
- fix: remove the StatefulSet and KafkaUser manifests — the controller is stateless and has no Kafka
- fix: pin Go 1.27.2 in `go.mod` and the Dockerfile so CI's `vulncheck` matches the local toolchain
