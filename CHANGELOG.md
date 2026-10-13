# Changelog

All notable changes to this project will be documented in this file.

Please choose versions by [Semantic Versioning](http://semver.org/).

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
