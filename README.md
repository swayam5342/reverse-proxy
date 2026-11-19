<div align="center">

<img src="docs/icon.svg" width="88" height="88" alt="jgateway">

# jgateway

**A dumb core, smart middleware.**

`jgateway` is a reverse proxy and API gateway written in **Go**, with a **composable middleware pipeline**. It sits in front of backend services and handles the cross-cutting concerns: **auth, rate limiting, request signing, circuit breaking, retries and observability**. The core proxy stays simple; adding a feature means writing a new middleware, not touching the core.

[![Go](https://img.shields.io/badge/Go-1.25%2B-4cb3f0?style=flat-square&logo=go&logoColor=white&labelColor=0d3a55)](go.mod)
[![CI](https://img.shields.io/github/actions/workflow/status/swayam5342/reverse-proxy/ci.yml?branch=main&style=flat-square&logo=githubactions&logoColor=white&labelColor=0d3a55&color=4cb3f0&label=CI)](.github/workflows/ci.yml)
[![Docker](https://img.shields.io/badge/image-distroless%2C%20non--root-4cb3f0?style=flat-square&logo=docker&logoColor=white&labelColor=0d3a55)](#option-b-docker)
[![Metrics](https://img.shields.io/badge/metrics-Prometheus-4cb3f0?style=flat-square&logo=prometheus&logoColor=white&labelColor=0d3a55)](docs/observability.md)

[Architecture](docs/architecture.md) &nbsp;&middot;&nbsp; [Configuration](docs/configuration.md) &nbsp;&middot;&nbsp; [Security](docs/security.md) &nbsp;&middot;&nbsp; [Observability](docs/observability.md) &nbsp;&middot;&nbsp; [Deployment](docs/deployment.md)

</div>

<br>

## Table of Contents

| | |
| :--- | :--- |
| **Overview** | [Features](#features) &middot; [Endpoints](#endpoints) |
| **Setup** | [Prerequisites](#prerequisites) &middot; [Running the Gateway](#running-the-gateway) &middot; [Option A: Locally](#option-a-locally) &middot; [Option B: Docker](#option-b-docker) &middot; [Configuration](#configuration) |
| **Reference** | [Deployment](#deployment) &middot; [Documentation](#documentation) &middot; [Non-Goals](#non-goals) &middot; [License](#license) |
| **Testing** | [Verification & Testing](#verification--testing) |

<br>

## Features

| Feature | What it means |
| :--- | :--- |
| **Data-driven routing** | Longest-prefix path matching to any number of upstreams, each with its own dial and response timeouts. |
| **Auth** | JWT (HMAC) or API key verification. Fails closed on any error. |
| **Rate limiting** | Token bucket, keyed by authenticated identity rather than naive client IP, to resist spoofing. |
| **Request signing** | HMAC-SHA256 verification with replay and clock-skew protection. |
| **Resilience** | Idempotency-aware retries with jittered exponential backoff, and a per-upstream circuit breaker (closed → open → half-open). |
| **Observability** | Structured JSON logging, Prometheus metrics, and separate liveness and readiness endpoints. |
| **Operational safety** | Fail-fast config validation at startup, graceful shutdown that drains in-flight requests, and panic recovery so a bug in one middleware can't take down unrelated requests. |

See [`docs/architecture.md`](docs/architecture.md) for how these fit together and why the middleware order is fixed.

<br>

## Endpoints

| Route | Purpose |
| :--- | :--- |
| `/healthz` | Liveness: the process is up. Returns `200` and `ok`. |
| `/readyz` | Readiness: whether it can actually serve. Returns JSON. |
| `/metrics` | Prometheus metrics. |
| Your `routes[].path_prefix` | Proxied to the matching upstream through that route's middleware chain. |

<br>

## Prerequisites

| Requirement | When |
| :--- | :--- |
| **Go 1.25+** | To build or run locally. |
| **Docker** | For the image, or the end-to-end example in [`examples/`](examples/README.md). |

<br>

## Running the Gateway

### Option A: Locally

**1. Copy the example config and secrets**

```sh
cp config.example.yaml config.yaml
cp .env.example .env
```

**2. Edit them for your upstreams and secrets, then load the environment**

```sh
export $(grep -v '^#' .env | xargs)   # or use your secrets manager
```

> [!IMPORTANT]
> `.env.example` ships `change-me` values for `JWT_SECRET`, `ORDERS_API_KEYS` and `ORDERS_SIGNING_SECRET`. Replace them before exposing the gateway, and never commit the real `.env`.

**3. Run it**

```sh
go run ./cmd/gateway
```

It listens on `:8080` by default (`GATEWAY_ADDR`, or `server.addr` in the config).

### Option B: Docker

```sh
docker build -t jgateway .
docker run -p 8080:8080 --env-file .env jgateway
```

The image is a multi-stage build producing a minimal distroless binary that runs as a non-root user (see [`Dockerfile`](Dockerfile)).

> [!TIP]
> To see the whole pipeline running against real backends, [`examples/README.md`](examples/README.md) has a Docker Compose setup with three demo services covering every middleware.

<br>

## Configuration

Routes, upstreams and their middleware are data, not code. [`docs/configuration.md`](docs/configuration.md) is the full YAML and env reference.

| Variable | Example | Purpose |
| :--- | :--- | :--- |
| `GATEWAY_CONFIG` | `config.yaml` | Path to the YAML config. |
| `GATEWAY_ADDR` | `:8080` | Listen address; overrides `server.addr`. |
| `JWT_SECRET` | `change-me` | Secret for JWT routes (named by `auth.jwt_secret_env`). |
| `ORDERS_API_KEYS` | `change-me-key-1,change-me-key-2` | API keys for an `api_key` route (named by `auth.api_keys_env`). |
| `ORDERS_SIGNING_SECRET` | `change-me` | HMAC secret for a signed route. |

<details>
<summary><b>An upstream and a route</b> (from <code>config.example.yaml</code>)</summary>

```yaml
upstreams:
  - name: users-api
    url: "http://users-api.internal:8081"
    dial_timeout: 2s
    request_timeout: 2s
    circuit_breaker:
      enabled: true
      failure_threshold: 5
      half_open_probe_after: 30s
      half_open_max_requests: 1
    retry:
      enabled: true
      max_attempts: 3
      base_backoff: 100ms
      max_backoff: 2s

routes:
  - path_prefix: "/api/users"
    upstream: users-api
    auth:
      enabled: true
      mode: jwt
      jwt_secret_env: JWT_SECRET
    rate_limit:
      enabled: true
      requests_per_second: 50
      burst: 100
      key_strategy: api_key
```

</details>

<br>

## Deployment

Kubernetes manifests are in [`deploy/k8s`](deploy/k8s) and a k6 load test script is in [`deploy/loadtest.js`](deploy/loadtest.js). [`docs/deployment.md`](docs/deployment.md) covers graceful shutdown and resource sizing.

<br>

## Documentation

| Doc | What it covers |
| :--- | :--- |
| [`docs/architecture.md`](docs/architecture.md) | Package layout, request lifecycle, middleware ordering and why it's fixed that way. |
| [`docs/configuration.md`](docs/configuration.md) | Full YAML and env reference. |
| [`docs/security.md`](docs/security.md) | Threat model and the security decisions in auth, signing and rate limiting. |
| [`docs/observability.md`](docs/observability.md) | Logs, metrics, health checks, and what to alert on. |
| [`docs/deployment.md`](docs/deployment.md) | Docker and Kubernetes, graceful shutdown, resource sizing. |
| [`AGENTS.md`](AGENTS.md) | The production-readiness checklist this project is built and reviewed against. |

<br>

## Non-Goals

| Not | Why |
| :--- | :--- |
| **A service mesh** | No sidecar injection, no mTLS between every service. |
| **A UI or dashboard** | Metrics go to Prometheus and Grafana. |
| **gRPC proxying** | HTTP/REST only in v1, until the core is solid. |

<br>

## License

> [!NOTE]
> No license file yet. Treat the code as all rights reserved until one is added.

<br>

## Verification & Testing

```sh
go build ./...
go vet ./...
go test ./... -race
```

CI also runs `gofmt -l .`, `golangci-lint` and `govulncheck` (see [`.github/workflows`](.github/workflows)).
