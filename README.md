# jgateway

[![CI](https://github.com/swayam5342/rever_proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/swayam5342/rever_proxy/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/swayam5342/jgateway)](https://goreportcard.com/report/github.com/swayam5342/jgateway)

A reverse proxy / API gateway in Go with a composable middleware pipeline. It
sits in front of backend services and handles cross-cutting concerns: auth,
rate limiting, request signing, circuit breaking, retries, and observability.

**Design principle:** the core proxy is dumb; the middleware is where
behavior lives. Adding a feature means writing a new middleware, not touching
the core.

## Features

- **Data-driven routing** — longest-prefix path matching to any number of
  upstreams, each with independent dial/response timeouts.
- **Auth** — JWT (HMAC) or API key verification, fails closed on any error.
- **Rate limiting** — token bucket, keyed by authenticated identity (not
  naive client IP) to resist spoofing.
- **Request signing** — HMAC-SHA256 verification with replay/clock-skew
  protection.
- **Resilience** — idempotency-aware retries with jittered exponential
  backoff, and a per-upstream circuit breaker (closed → open → half-open).
- **Observability** — structured JSON logging, Prometheus metrics, separate
  `/healthz` (liveness) and `/readyz` (readiness) endpoints.
- **Operational safety** — fail-fast config validation at startup, graceful
  shutdown that drains in-flight requests, panic recovery so a bug in one
  middleware can't take down unrelated requests.

See [`docs/architecture.md`](docs/architecture.md) for how these fit
together.

## Quickstart

```sh
cp config.example.yaml config.yaml
cp .env.example .env
# edit config.yaml and .env for your upstreams and secrets

export $(grep -v '^#' .env | xargs)   # or use your secrets manager
go run ./cmd/gateway
```

The gateway listens on `:8080` by default (`GATEWAY_ADDR` env var, or
`server.addr` in config). See [`docs/configuration.md`](docs/configuration.md)
for the full config reference.

Want to see the whole pipeline running end to end against real backends?
See [`examples/README.md`](examples/README.md) — a docker-compose setup with
three demo services covering every middleware.

## Running the tests

```sh
go build ./...
go vet ./...
go test ./... -race
```

CI additionally runs `gofmt -l .`, `golangci-lint`, and `govulncheck` — see
[`.github/workflows`](.github/workflows).

## Docker

```sh
docker build -t jgateway .
docker run -p 8080:8080 --env-file .env jgateway
```

The image is a multi-stage build producing a minimal distroless binary
running as a non-root user (see [`Dockerfile`](Dockerfile)).

## Deployment

Kubernetes manifests are in [`deploy/k8s`](deploy/k8s); a k6 load test script
is in [`deploy/loadtest.js`](deploy/loadtest.js). See
[`docs/deployment.md`](docs/deployment.md).

## Documentation

- [`docs/architecture.md`](docs/architecture.md) — package layout, request
  lifecycle, middleware ordering and why it's fixed that way.
- [`docs/configuration.md`](docs/configuration.md) — full YAML/env reference.
- [`docs/security.md`](docs/security.md) — threat model and the specific
  security decisions in auth, signing, and rate limiting.
- [`docs/observability.md`](docs/observability.md) — logs, metrics, health
  checks, and what to alert on.
- [`docs/deployment.md`](docs/deployment.md) — Docker/Kubernetes, graceful
  shutdown, resource sizing.
- [`AGENTS.md`](AGENTS.md) — the production-readiness checklist this project
  is built and reviewed against.

## Non-goals

Not a service mesh (no sidecar injection, no mTLS between every service),
not a UI/dashboard (metrics go to Prometheus/Grafana), and no gRPC proxying
in v1 — HTTP/REST only until the core is solid.

## License

No license file yet — treat as all-rights-reserved until one is added.
