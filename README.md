# a2a-worker-controller

Stateless A2A worker controller — create / list / stop over A2A, one isolated worker per task, Docker and k3s backends.

## Install

```bash
go install github.com/bborbe/a2a-worker-controller@latest
```

## Usage

```bash
a2a-worker-controller \
  -listen=:8080 \
  -kafka-brokers=localhost:9092 \
  -datadir=data \
  -sentry-dsn=<dsn>
```

Or via Docker:

```bash
docker run --rm -p 8080:8080 bborbe/a2a-worker-controller:latest
```

See `example.env` for the development defaults.

## License

BSD-2-Clause. See [LICENSE](LICENSE).
