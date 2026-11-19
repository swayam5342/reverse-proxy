# Architecture

## Package layout

```text
cmd/gateway/            wiring only: main.go (flags, run), server.go (listeners), routes.go (route chains)
internal/
  config/                env/YAML parsing, validation, fail-fast on bad config
  httpx/                 shared helpers: client IP resolution, upgrade detection
  proxy/                 core reverse proxy: routing table, Rewrite, transport
  tlsx/                  TLS: ACME (Let's Encrypt), reloadable cert files, HTTPS redirect
  middleware/
    auth/                JWT / API key verification
    ratelimit/           token bucket; memory, Valkey and fallback stores
    cors/                per-route CORS (answers preflights before auth)
    sanitize/            strips client-supplied X-Auth-* headers
    bodylimit/           request body size cap
    timeout/             overall per-request deadline
    statusrec/           status recorder that keeps Flush/Hijack working
    signing/             HMAC request signing/verification
    circuitbreaker/      per-upstream breaker (open/half-open/closed)
    retry/               backoff + retry policy per route
    logging/             structured request logging
    metrics/             Prometheus middleware and collectors
    recovery/            top-level panic recovery
  health/                /healthz, /readyz handlers
pkg/
  middleware/            the Middleware interface + chaining helper
deploy/                  compose deployment + prod config, k8s manifests, load test
```

`internal/` holds everything not meant to be imported elsewhere. `pkg/`
holds only the `Middleware` type and `Chain` helper — the one thing an
external consumer might reasonably want to import.

## The core: dumb by design

`internal/proxy` has no knowledge of auth, rate limits, or retries. It knows
two things:

- **`Router`** — a longest-prefix-match table built once from config
  (`proxy.NewRouter`), matching on path-segment boundaries. No hardcoded if-chains; adding a route means adding a
  config entry.
- **`NewReverseProxy`** wraps `httputil.ReverseProxy` with a `Rewrite`
  function. It optionally strips the route prefix, joins the upstream's base
  path, drops client-supplied forwarding headers and sets
  `X-Forwarded-For/Host/Proto` itself.

Every upstream gets its own `http.Client` with a bounded dial timeout and
response-header timeout (`proxy.NewUpstream`) — no unbounded waits at the
transport layer, independent of whatever timeouts the route's middleware
adds.

## Middleware contract

```go
type Middleware func(http.Handler) http.Handler

func Chain(h http.Handler, mws ...Middleware) http.Handler {
    for i := len(mws) - 1; i >= 0; i-- {
        h = mws[i](h)
    }
    return h
}
```

Every middleware:

- takes its config via constructor injection (`NewRateLimiter(cfg, store)`),
  never globals;
- is unit-testable in isolation with `httptest.NewRecorder()`;
- must never panic — either it recovers internally, or it relies on the
  top-level `recovery` middleware, so one bug can't take down unrelated
  requests.

## Request lifecycle

1. The process runs up to three listeners (`newServers` in
   `cmd/gateway/server.go`):
   - **public**: the proxied routes only, over TLS when enabled;
   - **admin**: `/healthz`, `/readyz`, `/metrics`;
   - **HTTP**: ACME challenges and redirects to HTTPS.

   On the public listener a `ServeMux` cleans the path first. The request
   then goes through the gateway-wide chain
   `recovery → timeout → bodylimit → sanitize`:
   - panic recovery is outermost;
   - then the overall request deadline (`server.request_timeout`, or
     `server.upgrade_timeout` for WebSocket upgrades), so every later
     middleware and the upstream call share it;
   - then the body-size cap, so nothing downstream can buffer an unbounded
     body;
   - then removal of client-supplied `X-Auth-*` headers.
2. `Router.ServeHTTP` finds the longest matching `PathPrefix` and dispatches
   to that route's pre-built handler chain, or 404s
   (`proxy.ErrNoRouteMatch`) if nothing matches.
3. Each route's handler chain is built once at startup in
   `buildRouteHandler` (`cmd/gateway/routes.go`), in this fixed order:

   ```text
   logging → metrics → cors → auth → rate limit → signing → retry → circuit breaker → reverse proxy
   ```

   The order is deliberate, not arbitrary:

   - **logging / metrics** wrap everything so every request — including
     ones rejected by auth or rate limiting — is observed.
   - **CORS before auth**, because browsers send preflights without
     credentials. CORS answers them itself (204 or 403) and never forwards
     them.
   - **auth before rate limiting** so the limiter can key on the
     authenticated identity, not just IP.
   - **signing after auth** — verifies the payload itself once we already
     know who's calling.
   - **retry** streams responses: it only holds back retryable error
     responses (502/503/504, body capped at 64 KiB), so successful and
     streaming responses pass straight through. Upgrade requests are never
     retried.
   - **retry before circuit breaker** — a retried request that eventually
     succeeds shouldn't count as a breaker failure on every attempt; the
     breaker sees only the outcome retry settles on.
   - **circuit breaker innermost** so it trips only on real upstream
     failures, not on rejections earlier in the chain. There is one
     breaker per upstream, shared by every route that reaches it.

   Any middleware documents an ordering dependency explicitly if it has
   one (see the auth/ratelimit package docs); otherwise middleware is safe
   to reorder relative to each other.

4. The reverse proxy forwards to the upstream using that upstream's
   dedicated `http.Client`.

## Why this shape

Cross-cutting behavior (auth, limits, resilience) changes far more often
than routing/proxying logic. Keeping the core small and the middleware
independently testable means a new requirement — a new auth mode, a
different rate-limit key strategy — is additive: write a middleware,
register it in `buildRouteHandler`, add config. It never means touching the
router or the proxy transport.
