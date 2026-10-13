# Changelog

All notable changes to this project will be documented in this file.

Please choose versions by [Semantic Versioning](http://semver.org/).

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
