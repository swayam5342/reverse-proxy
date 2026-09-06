# Observability

## Logging

`internal/middleware/logging` emits one structured `slog` line per request,
per route, via the JSON handler configured in `cmd/gateway/main.go`
(`slog.NewJSONHandler(os.Stdout, nil)`). At minimum every line carries
request path, upstream, status code, and latency, so a single log line
answers "what happened to this request."

The top-level `internal/middleware/recovery` middleware logs a separate
`"panic recovered"` entry — with the panic value, request path, and full
stack trace — whenever a handler panics, before responding `500`. This is
the first place to look if you see `500`s with no matching upstream error.

## Metrics

`GET /metrics` serves the default Prometheus registry
(`internal/middleware/metrics`). Collectors:

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `gateway_requests_total` | counter | `route`, `method`, `status` | Traffic volume and error rate per route. |
| `gateway_request_duration_seconds` | histogram | `route`, `status` | Latency distribution (p50/p95/p99 via `histogram_quantile`). |
| `gateway_circuit_breaker_state` | gauge | `upstream` | 0=closed, 1=half_open, 2=open. Alert on sustained `2`. |
| `gateway_rate_limit_rejections_total` | counter | `route` | 429 volume — spikes indicate either abuse or a limit set too tight. |
| `gateway_retry_attempts_total` | counter | `route` | Retry volume — sustained non-zero means an upstream is flaky, not down. |

Suggested alerts:

- `gateway_circuit_breaker_state == 2` for more than a minute on any
  upstream — a backend is down long enough to matter.
- Error-rate ratio from `gateway_requests_total{status=~"5.."}` /
  `gateway_requests_total` per route, over your normal SLO window.
- p99 from `gateway_request_duration_seconds` exceeding your route's
  documented baseline (see the load-test note below).

## Health checks

Two separate endpoints, deliberately not conflated
(`internal/health/health.go`):

- **`/healthz`** (liveness) — `200` as long as the process can serve HTTP at
  all. Never depends on upstream state. An orchestrator killing/restarting
  the gateway because one backend is unreachable would make an outage
  worse, not better — liveness must not reflect that.
- **`/readyz`** (readiness) — `200` only if every registered upstream
  readiness check passes (a TCP dial in `cmd/gateway/main.go`'s
  `readinessCheck`); otherwise `503` with a JSON body naming which upstream
  failed. Use this as the load balancer / k8s readiness probe so traffic
  stops routing to an instance that can't currently reach its backends.

## Tracing

Not wired up yet. The logging middleware is the natural place to add a
request-ID header (generate if absent, propagate if present) as the minimum
viable correlation mechanism across a multi-service chain; full
OpenTelemetry spans would build on top of that. Tracked as a documented gap
in [`AGENTS.md`](../AGENTS.md)'s Observability checklist.

## Load testing baseline

[`deploy/loadtest.js`](../deploy/loadtest.js) is a k6 script. Run it against
a local instance and record p50/p99 and max RPS as your regression baseline
before making changes to the middleware chain:

```sh
k6 run deploy/loadtest.js
```
