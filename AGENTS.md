This file orients any coding agent (or future you) working on this repo. Read it before writing code. It describes what the project is, how it's structured, and the bar for "production-ready" that every PR should meet.

Project Summary

A reverse proxy / API gateway in Go with a composable middleware pipeline. Sits in front of backend services and handles cross-cutting concerns: auth, rate limiting, request signing, circuit breaking, retries, observability.

Design principle: the core proxy is dumb; the middleware is where behavior lives. Adding a feature should mean writing a new middleware, not touching the core.

Architecture
cmd/
  gateway/              # main.go — wiring only, no logic
internal/
  config/               # env parsing, validation, fail-fast on bad config
  proxy/                # core reverse proxy: routing table, director, transport
  middleware/
    auth/               # JWT / API key verification
    ratelimit/          # token bucket / sliding window, pluggable store (memory, redis)
    signing/            # HMAC request signing/verification
    circuitbreaker/      # per-upstream breaker (open/half-open/closed)
    retry/               # backoff + retry policy per route
    logging/             # structured request logging
    metrics/             # Prometheus middleware
  discovery/            # static config or service discovery (Consul/DNS) for upstreams
  health/               # /healthz, /readyz handlers
pkg/                    # only if something here is meant for external import
  middleware/           # the Middleware interface + chaining helper — this is the reusable core
migrations/             # if rate-limit / auth state needs persistent storage
deploy/                 # Dockerfile, k8s manifests / systemd units
Middleware contract
go
type Middleware func(http.Handler) http.Handler

func Chain(h http.Handler, mws ...Middleware) http.Handler {
    for i := len(mws) - 1; i >= 0; i-- {
        h = mws[i](h)
    }
    return h
}

Every middleware:

Takes config via constructor injection, not globals (func NewRateLimiter(cfg RateLimitConfig, store Store) Middleware).
Is independently unit-testable with httptest.NewRecorder() — no need to spin up the full gateway.
Must be safe to reorder relative to most other middleware; document any ordering dependency explicitly (e.g., auth must run before rate-limiting-by-user-id).
Never panics — recovers internally or lets a top-level recovery middleware handle it, but a bug in one middleware must not take down unrelated requests.

Routing config (which middleware chain applies to which upstream/path) should be data-driven — a config file or struct, not a hardcoded if chain in main.go.

Production-readiness checklist

A feature isn't done until these are true. Treat this as the Definition of Done for any PR.

Correctness & resilience
 Context propagation: every request's context.Context carries cancellation and deadline through the whole middleware chain and into the upstream call.
 Timeouts set at every layer: client read/write timeouts, upstream dial/response timeouts, and an overall request deadline. No unbounded waits.
 Circuit breaker per upstream, with configurable failure threshold and half-open probe interval. Don't let one dead backend take down the gateway.
 Retries are idempotency-aware — only retry safe methods (GET, or POST explicitly marked idempotent) and always with jittered exponential backoff, never a tight loop.
 Graceful shutdown: on SIGTERM, stop accepting new connections, drain in-flight requests within a deadline, then exit. Use http.Server.Shutdown(ctx).
 No goroutine leaks — every goroutine spawned per-request must exit when the request context is done. Verify with go test -race and a leak detector (e.g. goleak) in tests.
Security
 TLS termination (or clearly documented that it's offloaded to a sidecar/LB) — never serve plaintext in prod config.
 Auth middleware fails closed: any error in token validation is a 401, never a silent pass-through.
 Request signing/verification uses constant-time comparison (hmac.Equal, subtle.ConstantTimeCompare) — never == on secrets or signatures.
 Rate limiting keys are chosen carefully (IP alone is spoofable behind a proxy — use X-Forwarded-For only if you control/trust the upstream LB, otherwise a combination of API key + IP).
 All secrets (signing keys, upstream credentials) come from env vars or a secrets manager — never hardcoded, never logged.
 Input validation on all proxied paths/headers you interpret (host header injection, header smuggling) — don't blindly forward attacker-controlled headers into internal-only fields.
 Dependency scanning (govulncheck) in CI.
Observability
 Structured logging (slog) with request ID, upstream, status code, latency on every request — one line per request minimum.
 Prometheus metrics: request count/latency histograms labeled by route+status, circuit breaker state per upstream, rate-limit rejections, retry counts.
 Distributed tracing hooks (OpenTelemetry) if this will sit in a multi-service chain — at minimum, propagate/generate a request ID header.
 /healthz (liveness — is the process alive) and /readyz (readiness — can it actually serve, e.g. are upstream connections healthy) are separate endpoints.
Config & deployment
 Config loaded once at startup, validated (required fields, valid URLs, sane timeout ranges), and the process fails fast with a clear error on invalid config — never starts in a half-broken state.
 Config is environment-based (12-factor) with a documented schema (a config.example.yaml or .env.example checked into the repo).
 Dockerfile is multi-stage, produces a minimal final image (distroless or scratch + CA certs), runs as non-root.
 Resource limits documented/set (CPU/memory) for the deployment target.
 Rolling deploy safe: the gateway should handle in-flight requests during a deploy without dropping them (this is why graceful shutdown above matters).
Testing
 Table-driven unit tests per middleware.
 Integration test that spins up the full chain against a fake upstream (httptest.Server) and asserts end-to-end behavior (auth rejects bad token, rate limiter returns 429 after N requests, circuit breaker opens after failures).
 Load test (even a simple k6 or hey script) documented with expected p50/p99 latency and max RPS on your dev machine, as a baseline for regressions.
 -race clean in CI, always.
Tooling / CI
 golangci-lint, go vet, gofmt -l ., govulncheck all run in CI and block merge.
 Go version pinned in go.mod and CI matrix.
 Pre-commit or CI step that fails on TODO/FIXME left in security-sensitive middleware (auth, signing) without a linked issue.
Coding conventions
internal/ for everything not meant to be imported elsewhere; only promote to pkg/ if there's a real external consumer.
Errors: wrap with %w, define sentinel errors for expected failure modes (ErrUpstreamUnavailable, ErrRateLimited), check with errors.Is/errors.As.
No global mutable state. Config and dependencies are passed in via constructors.
Prefer the stdlib (net/http, slog, context) over frameworks — this is infrastructure code; minimize dependencies. Pull in a router (chi is a reasonable choice) only if net/http's ServeMux genuinely isn't enough for your routing needs.
Every exported type/function has a doc comment.


Non-goals (for now)
Not building a full service mesh — no sidecar injection, no mTLS between every service.
Not building a UI/dashboard — config is file/env based; metrics go to Prometheus/Grafana, not a custom panel.
Not supporting gRPC proxying in v1 — HTTP/REST only until the core is solid.