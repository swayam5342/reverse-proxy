# Deployment

## Docker image

[`Dockerfile`](../Dockerfile) is a multi-stage build:

1. `golang:1.25-alpine` compiles a static binary (`CGO_ENABLED=0`,
   `-trimpath -ldflags="-s -w"`).
2. The final image is `gcr.io/distroless/static-debian12:nonroot` — no
   shell, no package manager, runs as the `nonroot` user. Only the compiled
   binary and a default config are copied in.

```sh
docker build -t jgateway .
docker run -p 8080:8080 --env-file .env jgateway
```

The image bakes in `config.example.yaml` as `/app/config.yaml` — override
by mounting your own file at that path, or by pointing `GATEWAY_CONFIG` at
a different mounted path.

## TLS

The gateway itself terminates plaintext HTTP. In every deployment, either
put TLS termination in front of it (an LB, ingress controller, or sidecar)
or add termination before going anywhere near production traffic — never
serve plaintext directly to the internet. This is a checklist item in
[`AGENTS.md`](../AGENTS.md) precisely because it's easy to forget when a
local `docker run` "just works" over HTTP.

## Kubernetes

[`deploy/k8s/gateway.yaml`](../deploy/k8s/gateway.yaml) has a `Deployment` +
`Service`. Notable pieces:

- **Readiness/liveness probes** point at `/readyz` and `/healthz`
  respectively (see [`docs/observability.md`](observability.md) for why
  they're separate).
- **`preStop` sleep + `terminationGracePeriodSeconds: 40`** — on `SIGTERM`
  the gateway starts draining in-flight requests
  (`server.Shutdown(ctx)` in `cmd/gateway/main.go`) using
  `server.shutdown_timeout` from config. The `preStop` hook gives the
  endpoint controller time to remove the pod from Service routing *before*
  the process starts shutting down, and `terminationGracePeriodSeconds`
  must exceed `preStop sleep + shutdown_timeout` or Kubernetes will `SIGKILL`
  mid-drain. If you change `shutdown_timeout` in config, update both here.
- **Resource requests/limits** (`250m`/`128Mi` request, `1`/`256Mi` limit)
  are a starting point sized for the demo config's 2-upstream, in-memory
  rate limiter — re-baseline against your actual traffic with the load test
  below before trusting them in production.
- Secrets are injected via `envFrom: secretRef: gateway-secrets` — populate
  that Secret with the same variables listed in
  [`.env.example`](../.env.example) (`JWT_SECRET`, `ORDERS_API_KEYS`,
  `ORDERS_SIGNING_SECRET`, etc. — whichever your `config.yaml` actually
  references).

```sh
kubectl apply -f deploy/k8s/gateway.yaml
```

## Rolling deploys

Graceful shutdown (above) plus a readiness probe that fails fast on a
genuinely broken instance is what makes rolling deploys safe: Kubernetes
won't route new traffic to a pod until `/readyz` passes, and won't kill a
pod's connections until it's had `shutdown_timeout` to drain. If you
increase `replicas` or add upstreams, re-run the load test in
[`docs/observability.md`](observability.md#load-testing-baseline) — new
upstream count changes per-pod connection pool sizing
(`MaxIdleConnsPerHost` in `internal/proxy/proxy.go`).

## Scaling the rate limiter

The in-memory `ratelimit.Store` (`internal/middleware/ratelimit/store.go`)
is per-process. With `replicas: 3`, a client's effective rate limit is
roughly 3x the configured value, since each pod tracks its own buckets
independently. This is fine for a coarse anti-abuse limit; if you need an
exact shared limit across replicas, the `Store` interface is the extension
point for a Redis-backed implementation (tracked as a planned improvement,
not yet built).
