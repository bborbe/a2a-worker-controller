# Changelog

All notable changes to this project will be documented in this file.

Please choose versions by [Semantic Versioning](http://semver.org/).

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
