# Small-Production Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make jgateway safe to run as the public edge of a small production
app: a Valkey-shared rate limit, built-in TLS, CORS, JWT cookies with CSRF
protection, edge-header hardening, honest metrics, and a compose deployment.

**Architecture:** Each feature is a middleware or a small package wired in
`cmd/gateway`, per AGENTS.md. Valkey sits behind the existing
`ratelimit.Store` interface, wrapped in a fallback store. TLS lives in
`internal/tlsx`. The process runs up to three `http.Server`s (public, admin,
HTTP redirect/ACME) that shut down together.

**Tech Stack:** Go 1.25 stdlib, `github.com/valkey-io/valkey-go v1.0.79`,
`golang.org/x/crypto/acme/autocert` (x/crypto v0.59.0), existing
`golang-jwt/v5`, `prometheus/client_golang`, `yaml.v3`.

**Spec:** `docs/superpowers/specs/2026-11-19-small-production-design.md`

## Global Constraints

- New dependencies allowed: `github.com/valkey-io/valkey-go` and
  `golang.org/x/crypto` only. Anything else comes from the stdlib.
- Valkey image: `valkey/valkey:8.1.1` (compose, CI and local tests).
- Valkey key format: `jgw:rl:<route path_prefix>:<first 32 hex chars of sha256(key)>`.
- Valkey defaults:
  - `valkey.timeout` 50ms;
  - fallback cool-down 5s;
  - fallback log at most once per 10s.
- Admin listener defaults to `127.0.0.1:9090`; compose uses `:9090` and
  never publishes it.
- TLS: `MinVersion` TLS 1.2, HTTP/2 on. File reload checks at most every
  30s, during handshakes, with no goroutine.
- Container ports `:8443` (TLS) and `:8080` (HTTP), published as
  `443:8443` and `80:8080`.
- Identity headers are `X-Auth-Subject` and `X-Auth-Method`. Every
  client-supplied `X-Auth-*` header is stripped on every route.
- Unnamed API key subject: `key-` + the first 8 hex chars of `sha256(key)`.
- Config decoding uses `yaml.Decoder.KnownFields(true)`.
- Every task follows TDD: write the failing test, watch it fail, implement,
  watch it pass, commit. `go test -race ./...` and `gofmt -l .` stay clean
  after every task.
- Every exported identifier has a doc comment. Errors wrap with `%w`.
- Commit messages carry no attribution trailer.

## Review Focus

1. **Empty config file.** It should give a clear "no upstreams…"
   validation error, not a panic or a bare `EOF`.
   → Task 1, `TestLoad_EmptyFileFailsValidation`.
2. **`strip_prefix` with an encoded path** (`/api/users/a%2Fb`). The
   upstream should receive `/a%2Fb` with the encoding intact, not `/a/b`.
   → Task 2, `TestReverseProxy_StripPrefixKeepsEncoding`.
3. **Valkey down when the gateway starts, back later.** The gateway should
   start, use memory limits, then switch to Valkey without a restart.
   → Task 5, `TestValkeyStore_ConnectsLazily`.
4. **Redirect listener with an unexpected `Host`.** A host outside the
   ACME domains, or a missing `Host`, should get 400 instead of a redirect
   to an arbitrary host. → Task 11, `TestRedirectHandler`.
5. **`ORDERS_API_KEYS` with spaces or trailing commas**
   (`"a:k1, b:k2,"`). Entries should be trimmed and empty ones ignored.
   → Task 3, `TestParseAPIKeys`.

---

## File Structure

| File | Responsibility |
|---|---|
| `cmd/gateway/main.go` | flags (`-healthcheck`), load config, `run` (start/stop servers) |
| `cmd/gateway/server.go` | `newServers`: builds public/admin/HTTP servers, shared breakers, Valkey store |
| `cmd/gateway/routes.go` | `buildRouteHandler`: per-route middleware chain |
| `internal/config/config.go` | schema, strict load, validation (existing file grows) |
| `internal/config/tls.go`, `valkey.go`, `cors.go` | new config blocks + their validation |
| `internal/httpx/clientip.go` | `ClientIP` (moved from ratelimit) |
| `internal/proxy/proxy.go` | segment-boundary router, `Rewrite`, strip prefix, forwarded headers |
| `internal/middleware/sanitize/` | strips client `X-Auth-*` |
| `internal/middleware/auth/` | identity headers, named API keys, JWT cookie + CSRF |
| `internal/middleware/ratelimit/store.go` | `Store` interface (ctx + error), `MemoryStore` |
| `internal/middleware/ratelimit/valkey.go` | `ValkeyStore` (Lua token bucket, lazy connect) |
| `internal/middleware/ratelimit/fallback.go` | `FallbackStore` |
| `internal/middleware/cors/` | CORS middleware |
| `internal/middleware/circuitbreaker/`, `retry/` | `OnStateChange` / `OnRetry` hooks |
| `internal/middleware/metrics/metrics.go` | registers all collectors, adds new ones |
| `internal/tlsx/` | `FileReloader`, `RedirectHandler`, `ServerTLS` |
| `deploy/docker-compose.yml`, `deploy/config.prod.yaml` | single-host deployment |
| `Dockerfile`, `.github/workflows/ci.yml`, `deploy/k8s/gateway.yaml`, docs | deployment + docs |

Delete: `internal/discovery/` (unused).

---

### Task 1: Strict config, dead fields removed, `cmd/gateway` split

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `config.example.yaml`, `README.md`, `docs/configuration.md` (drop `idempotent_only`)
- Delete: `internal/discovery/discovery.go`
- Create (pure move): `cmd/gateway/server.go` (`newServer`, `readinessCheck`), `cmd/gateway/routes.go` (`buildRouteHandler`, `splitNonEmpty`)

**Interfaces:**
- Produces: `config.Load(path string) (*Config, error)` rejects unknown keys.
  `RetryConfig` loses `IdempotentOnly`; `RateLimitConfig` loses `Window`.

- [ ] **Step 1: Write failing tests** in `internal/config/config_test.go`:
  - `TestLoad_UnknownFieldFails`: table with two YAMLs, a valid base plus
    `retry: {enabled: false, idempotent_only: true}` under the upstream,
    and the base with `requst_timeout: 1s` under `server`. `Load` must
    error, and the error must contain `idempotent_only` /
    `requst_timeout` respectively.
  - `TestLoad_EmptyFileFailsValidation`: empty file. The error must
    contain `at least one upstream`.
- [ ] **Step 2: Run** `go test ./internal/config/ -run 'UnknownField|EmptyFile'`. Expected: FAIL (no error returned / EOF error).
- [ ] **Step 3: Implement.**
  - Decode with `yaml.NewDecoder(bytes.NewReader(data))` and
    `dec.KnownFields(true)`, treating `io.EOF` as an empty document.
  - Remove both dead fields and `internal/discovery`.
  - Remove `idempotent_only` from `config.example.yaml`, the README
    snippet and the docs.
  - Move the `cmd/gateway` functions into `server.go`/`routes.go`
    unchanged.
- [ ] **Step 4: Run** `go test -race ./...`. Expected: PASS (includes `TestLoad_ExampleConfig`).
- [ ] **Step 5: Commit** `Reject unknown config keys; remove dead fields and unused discovery package`

### Task 2: Segment-boundary routing, prefix strip, gateway-owned forwarded headers

**Files:**
- Create: `internal/httpx/clientip.go`; move `internal/middleware/ratelimit/clientip_test.go` → `internal/httpx/clientip_test.go`
- Modify: `internal/middleware/ratelimit/ratelimit.go` (call `httpx.ClientIP`; delete local copy)
- Modify: `internal/proxy/proxy.go`, `internal/proxy/proxy_test.go`
- Modify: `internal/config/config.go` (`Route.StripPrefix bool \`yaml:"strip_prefix"\``, `path_prefix` must start with `/`)
- Modify: `cmd/gateway/routes.go`, `cmd/gateway/server.go`

**Interfaces:**
- Produces:
  - `httpx.ClientIP(r *http.Request, trusted []netip.Prefix) string`
    (same behaviour as today's `ratelimit.ClientIP`).
  - `proxy.Options{StripPrefix string; TrustedProxies []netip.Prefix}`.
  - `proxy.NewReverseProxy(up *Upstream, opts Options) *httputil.ReverseProxy`.

- [ ] **Step 1: Write failing tests** in `internal/proxy/proxy_test.go`:
  - `TestRouter_SegmentBoundary`: table, with routes `/api/users` and `/`
    registered.
    - `/api/users` → users;
    - `/api/users/42` → users;
    - `/api/users-admin` → root;
    - `/api/usersX` → root.
    - A route `/static/` matches `/static/x`.
  - `TestReverseProxy_StripPrefix`: strip `/api/users`, so
    `/api/users/42?x=1` reaches upstream as `/42?x=1`, and `/api/users`
    reaches upstream as `/`.
  - `TestReverseProxy_StripPrefixKeepsEncoding`: `/api/users/a%2Fb` → the
    upstream sees `RequestURI` `/a%2Fb`.
  - `TestReverseProxy_UpstreamBasePath`: upstream URL `<backend>/v1`, so
    `/api/x` becomes `/v1/api/x`.
  - `TestReverseProxy_ForwardedHeadersRebuilt`: the client sends
    `X-Forwarded-For: 6.6.6.6`, `X-Forwarded-Host: evil` and
    `Forwarded: for=6.6.6.6` from `RemoteAddr` `1.2.3.4:5`, with no
    trusted proxies. The upstream must see:
    - `X-Forwarded-For` == `1.2.3.4`;
    - `X-Forwarded-Host` == the request `Host`;
    - `X-Forwarded-Proto` == `http`;
    - no `Forwarded` header.
- [ ] **Step 2: Run** `go test ./internal/proxy/`. Expected: FAIL / compile error on `Options`.
- [ ] **Step 3: Implement.**
  - **Router match rule:**
    `path == prefix || strings.HasSuffix(prefix, "/") && strings.HasPrefix(path, prefix) || strings.HasPrefix(path, prefix+"/")`.
  - **`Rewrite` steps, in order:**
    1. Strip `opts.StripPrefix` from both `Out.URL.Path` and
       `Out.URL.RawPath` (when set), using `"/"` if the result is empty.
    2. `pr.SetURL(up.Target)`.
    3. Set `X-Forwarded-For` (`httpx.ClientIP(pr.In, opts.TrustedProxies)`),
       `X-Forwarded-Host` (`pr.In.Host`) and `X-Forwarded-Proto`
       (`https` iff `pr.In.TLS != nil`).
    4. Keep the empty-`User-Agent` behaviour.
  - Keep `ErrorHandler` as is.
  - Wire `Options` in `routes.go` from `routeCfg.StripPrefix` and the
    parsed trusted proxies.
- [ ] **Step 4: Run** `go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `Route on path segments, add strip_prefix, rebuild forwarded headers`

### Task 3: Trusted identity headers and named API keys

**Files:**
- Create: `internal/middleware/sanitize/sanitize.go`, `sanitize_test.go`
- Modify: `internal/middleware/auth/auth.go`, `auth_test.go`
- Modify: `cmd/gateway/server.go` (gateway-wide chain becomes recovery → timeout → bodylimit → sanitize), `cmd/gateway/routes.go`
- Modify: `cmd/gateway/integration_test.go` / `hardening_integration_test.go`, `.env.example`, `docs/security.md`, `docs/configuration.md`

**Interfaces:**
- Produces:
  - `sanitize.New() middleware.Middleware`.
  - `auth.HeaderSubject = "X-Auth-Subject"`, `auth.HeaderMethod = "X-Auth-Method"`.
  - `auth.APIKey{Name, Key string}`.
  - `auth.ParseAPIKeys(s string) []APIKey`.
  - `auth.APIKeyConfig{Keys []APIKey; HeaderName string}`, replacing
    `ValidKeys`.

- [ ] **Step 1: Write failing tests.**
  - **`TestSanitize_StripsClientAuthHeaders`:** a request with
    `X-Auth-Subject`, `x-auth-method`, `X-Auth-Anything` and
    `X-Other: keep`. `next` sees none of the `X-Auth-*` headers, and
    `X-Other` is still present.
  - **`TestParseAPIKeys` table:**
    - `"billing:s1,reports:s2"` → names `billing`/`reports`;
    - `"a:k1, b:k2,"` → 2 keys, trimmed;
    - `"bare"` → name `key-` + the first 8 hex of `sha256("bare")`;
    - `""` → empty.
  - **`TestAPIKey_SubjectIsNameNotKey`:** `next` sees
    `IdentityFromContext` Subject `billing`, `X-Auth-Subject: billing` and
    `X-Auth-Method: api_key`. No header carries the value `s1`.
  - **`TestJWT_SetsIdentityHeaders`:** `X-Auth-Subject: user-1`,
    `X-Auth-Method: jwt`.
  - **Integration `TestGateway_ClientCannotForgeIdentity`:** a route with
    no auth, where the client sends `X-Auth-Subject: admin`. The upstream
    sees no `X-Auth-Subject`.
- [ ] **Step 2: Run** `go test ./internal/middleware/... ./cmd/...`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `sanitize` deletes every header whose canonical key starts with
    `X-Auth-`.
  - Auth sets both identity headers on `r.Header` on success. API-key
    matching stays constant-time over all keys.
  - `.env.example`: `ORDERS_API_KEYS=billing-svc:change-me-1,reports:change-me-2`.
- [ ] **Step 4: Run** `go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `Forward trusted identity headers; name API keys so secrets never leave the gateway`

### Task 4: `Store` takes context and returns errors; hashed, namespaced keys; reject hook

**Files:**
- Modify: `internal/middleware/ratelimit/store.go`, `ratelimit.go`, all ratelimit tests
- Modify: `cmd/gateway/routes.go`, `internal/middleware/metrics/metrics.go`

**Interfaces:**
- Produces:
  - `Store.Allow(ctx context.Context, key string, rps float64, burst int, now time.Time) (bool, error)`.
  - `ratelimit.Config` gains `Route string` and `OnReject func()`.
  - `ratelimit.StoreKey(route, key string) string` returns
    `"jgw:rl:" + route + ":" + hex(sha256(key))[:32]`.

- [ ] **Step 1: Write failing tests** in `ratelimit_test.go`, using a
  `recordingStore` fake that records keys:
  - `TestNew_StoreKeyIsHashedAndNamespaced`: route `/api` with an API-key
    identity `secret-key`. The recorded key equals
    `StoreKey("/api", "secret-key")`, starts with `jgw:rl:/api:`, and
    doesn't contain `secret-key`.
  - `TestNew_OnRejectCalledOn429`: burst 1, two requests → `OnReject`
    called exactly once.
  - `TestNew_StoreErrorFailsClosed503`: the store returns an error →
    503, and `next` is not called.
- [ ] **Step 2: Run** `go test ./internal/middleware/ratelimit/`. Expected: compile failure, then FAIL.
- [ ] **Step 3: Implement.**
  - The middleware calls `store.Allow(r.Context(), StoreKey(cfg.Route, key), …)`.
  - On error it answers `503 rate limit unavailable`. That path is only
    reachable without `FallbackStore`.
  - `MemoryStore` gets the new signature and always returns a nil error.
  - Wire `OnReject` to `metrics.RateLimitRejections.WithLabelValues(route).Inc`.
- [ ] **Step 4: Run** `go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `Rate-limit store takes context and reports errors; hash and namespace keys`

### Task 5: `ValkeyStore`

**Files:**
- Create: `internal/middleware/ratelimit/valkey.go`, `valkey_test.go`
- Modify: `go.mod`, `go.sum` (`go get github.com/valkey-io/valkey-go@v1.0.79`)

**Interfaces:**
- Consumes: `Store` (Task 4).
- Produces:
  - `ratelimit.NewValkeyStore(connect func() (valkey.Client, error)) *ValkeyStore`.
  - `(*ValkeyStore).Allow(...)`, which implements `Store` and ignores
    `now`.
  - `(*ValkeyStore).Ping(ctx context.Context) error`.
  - `(*ValkeyStore).Close()`.
  - Test helper `valkeyAddr(t) string`, which calls `t.Skip` when
    `VALKEY_ADDR` is unset.

- [ ] **Step 1: Write failing tests**, using real Valkey and a unique key
  per test (`t.Name()` plus a random suffix):
  - `TestValkeyStore_BurstThenBlocks`: rps 1, burst 3. Three calls are
    allowed and the fourth is denied.
  - `TestValkeyStore_Refills`: rps 10, burst 1. Allowed, then denied;
    after `time.Sleep(150ms)`, allowed again.
  - `TestValkeyStore_KeyExpires`: after one call, `PTTL` on the key is
    > 0 and <= 1000+100ms (burst 1, rps 10).
  - `TestValkeyStore_SharedAcrossClients`: two stores, each with its own
    client, burst 1. The first allows and the second denies.
  - `TestValkeyStore_ConnectsLazily`: `connect` fails the first time, so
    `Allow` returns an error. The second time it succeeds and `Allow`
    returns true. `connect` is called exactly twice; this test runs
    without Valkey by failing both times and asserting the error, and
    with Valkey for the success leg.
- [ ] **Step 2: Run** `docker run -d --rm -p 6379:6379 --name jgw-valkey valkey/valkey:8.1.1`, then `VALKEY_ADDR=localhost:6379 go test ./internal/middleware/ratelimit/ -run Valkey`. Expected: compile failure, then FAIL.
- [ ] **Step 3: Implement.**
  - `connect` is called under a mutex until it succeeds; the client is
    cached; failures return the error, wrapped.
  - The script is a `valkey.NewLuaScript` package var, run with
    `Exec(ctx, client, []string{key}, []string{rps, burst})`.
  - Script body (fixed by the spec: Valkey `TIME`, hash fields `tokens`
    and `ts`, PEXPIRE at the full-refill time + 1s):
    ```lua
    local t = redis.call('TIME')
    local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
    local rps, burst = tonumber(ARGV[1]), tonumber(ARGV[2])
    local v = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
    local tokens, ts = tonumber(v[1]), tonumber(v[2])
    if tokens == nil then tokens, ts = burst, now end
    tokens = math.min(burst, tokens + math.max(0, now - ts) / 1000 * rps)
    local allowed = 0
    if tokens >= 1 then tokens = tokens - 1; allowed = 1 end
    redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', now)
    redis.call('PEXPIRE', KEYS[1], math.ceil(burst / rps * 1000) + 1000)
    return allowed
    ```
  - `Ping` sends a `PING` command.
- [ ] **Step 4: Run** the Step 2 command, plus `go test ./internal/middleware/ratelimit/` without `VALKEY_ADDR` (the Valkey tests skip). Expected: PASS both.
- [ ] **Step 5: Commit** `Add Valkey rate-limit store`

### Task 6: `FallbackStore`

**Files:**
- Create: `internal/middleware/ratelimit/fallback.go`, `fallback_test.go`

**Interfaces:**
- Produces:
  - `ratelimit.FallbackConfig{Timeout, Cooldown time.Duration; OnFallback func(); Logger *slog.Logger}`.
    The defaults apply when a field is zero: `Timeout` 50ms, `Cooldown`
    5s, logging at most once per 10s.
  - `ratelimit.NewFallbackStore(primary, fallback Store, cfg FallbackConfig) *FallbackStore`,
    which implements `Store` and never returns an error.

- [ ] **Step 1: Write failing tests**, with fake stores:
  - `TestFallback_PrimaryHealthyUsesPrimary`: the fallback is never
    called.
  - `TestFallback_PrimaryErrorAnswersFromFallback`: the result comes from
    the fallback, and `OnFallback` is called once.
  - `TestFallback_CooldownSkipsPrimary`: cool-down 50ms. After one
    primary error, the next call within 50ms doesn't touch the primary.
    After 60ms, the primary is tried again.
  - `TestFallback_SlowPrimaryTimesOut`: the primary blocks until its ctx
    is done, with timeout 20ms. The call returns within 100ms using the
    fallback.
  - `TestFallback_LogsAtMostOncePer10s`: 5 failures in a row → exactly 1
    log line in a `bytes.Buffer` slog handler.
- [ ] **Step 2: Run** `go test ./internal/middleware/ratelimit/ -run Fallback`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - The primary is called with `context.WithTimeout(ctx, Timeout)`.
  - The `downUntil` and `lastLog` times are guarded by a mutex.
- [ ] **Step 4: Run** `go test -race ./internal/middleware/ratelimit/`. Expected: PASS.
- [ ] **Step 5: Commit** `Fall back to in-memory limits when the shared store fails`

### Task 7: Valkey config and wiring

**Files:**
- Create: `internal/config/valkey.go` + tests in `internal/config/valkey_test.go`
- Modify: `internal/config/config.go`, `cmd/gateway/server.go`, `cmd/gateway/routes.go`
- Modify: `internal/middleware/metrics/metrics.go` (add `StoreFallbacks`)
- Create: `cmd/gateway/valkey_integration_test.go`

**Interfaces:**
- Consumes: Tasks 4–6.
- Produces:
  - `config.ValkeyConfig{Addr, UsernameEnv, PasswordEnv string; DB int; TLS bool; Timeout time.Duration}`,
    as `Config.Valkey *ValkeyConfig \`yaml:"valkey"\``.
  - `metrics.StoreFallbacks`: a counter, `gateway_rate_limit_store_fallback_total`.
  - All rate-limited routes share one `FallbackStore` when `valkey` is
    set. Without it, each route gets its own `MemoryStore`, as today.

- [ ] **Step 1: Write failing tests.**
  - `TestValidate_Valkey` table:
    - `addr` must be `host:port`;
    - a `password_env` that's set must exist in the environment;
    - a negative `timeout` is an error;
    - a zero `timeout` becomes 50ms after `Load`.
  - Integration `TestGateway_ValkeyDownFallsBackToMemory`: Valkey at
    `127.0.0.1:1`, route rate-limited with burst 2 by IP. The responses
    are 200, 200, 429, and `/readyz` reports `valkey` with a non-`ok`
    status. This uses the public listener; Task 12 moves `/readyz` to the
    admin listener and updates this test.
  - Integration `TestGateway_TwoGatewaysShareValkeyLimit` (skipped without
    `VALKEY_ADDR`): two gateways on the same Valkey, burst 1. The first
    gateway returns 200 and the second returns 429 for the same client.
- [ ] **Step 2: Run** `go test ./internal/config/ ./cmd/...`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - The `connect` func builds `valkey.NewClient(valkey.ClientOption{…})`
    with these options:
    - `InitAddress: []string{Addr}`;
    - `Username` / `Password` from env;
    - `SelectDB: DB`;
    - `TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}` when `TLS`
      is set;
    - `DisableCache: true`.
  - Startup calls `Ping` once with a 1s timeout. On failure it logs
    `"valkey unreachable at startup, using in-memory limits until it recovers"`.
  - Register `health` check `valkey` = `store.Ping`.
  - Wire `OnFallback` → `metrics.StoreFallbacks.Inc`.
  - Close the store on shutdown.
  - Add the `valkey:` block, commented, to `config.example.yaml` and
    `VALKEY_PASSWORD` to `.env.example`.
- [ ] **Step 4: Run** `VALKEY_ADDR=localhost:6379 go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `Wire Valkey-backed rate limiting with in-memory fallback`

### Task 8: One breaker per upstream; breaker and retry metrics recorded

**Files:**
- Modify: `internal/middleware/circuitbreaker/circuitbreaker.go` + test
- Modify: `internal/middleware/retry/retry.go` + test
- Modify: `internal/middleware/metrics/metrics.go` (register `RetryAttempts`)
- Modify: `cmd/gateway/server.go`, `routes.go`, `cmd/gateway/integration_test.go`

**Interfaces:**
- Produces:
  - `circuitbreaker.Config.OnStateChange func(from, to State)`, called
    *after* the mutex is released.
  - `retry.Config.OnRetry func()`, called once per retry attempt (not the
    first try).
  - `buildRouteHandler` receives the upstream's shared
    `*circuitbreaker.Breaker` (nil when it's disabled) instead of building
    one.

- [ ] **Step 1: Write failing tests.**
  - `TestBreaker_OnStateChangeSequence`: threshold 1, probe-after 20ms.
    The recorded transitions are `closed→open`, then `open→half_open`
    after 25ms on the next request, then `half_open→closed` after a
    success.
  - `TestRetry_OnRetryCountsRetries`: 502, 502, 200 → `OnRetry` called 2
    times.
  - Integration `TestGateway_BreakerSharedAcrossRoutes`: two routes, `/a`
    and `/b`, point at one always-500 upstream with threshold 2. Two
    failures on `/a` → `/b` returns 503 `upstream circuit open`. The
    `gateway_circuit_breaker_state{upstream="backend"}` gauge reads 2
    (`prometheus/testutil.ToFloat64`).
- [ ] **Step 2: Run** `go test ./internal/middleware/... ./cmd/...`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Initialise the gauge to 0 for each upstream with a breaker.
  - `OnStateChange` sets the gauge: closed=0, half_open=1, open=2.
  - `OnRetry` → `metrics.RetryAttempts.WithLabelValues(route).Inc`.
- [ ] **Step 4: Run** `go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `Share one circuit breaker per upstream; record breaker and retry metrics`

### Task 9: CORS

**Files:**
- Create: `internal/middleware/cors/cors.go`, `cors_test.go`
- Create: `internal/config/cors.go` (`CORSConfig` + validation), tests in `internal/config/cors_test.go`
- Modify: `internal/config/config.go` (`Route.CORS CORSConfig \`yaml:"cors"\``), `cmd/gateway/routes.go`

**Interfaces:**
- Produces:
  - `cors.Config{AllowedOrigins, AllowedMethods, AllowedHeaders, ExposeHeaders []string; AllowCredentials bool; MaxAge time.Duration}`.
  - `cors.New(cfg Config) middleware.Middleware`.
  - `config.CORSConfig` has the same fields plus `Enabled`. Its YAML keys
    are those in spec §3.
  - When empty, methods default to `GET, HEAD, POST, PUT, PATCH, DELETE`
    and headers to `Authorization, Content-Type`.

- [ ] **Step 1: Write failing tests**, table `TestCORS`, with origin
  `https://app.example.com` allowed.
  - **Preflight:**
    - allowed → 204, with `Access-Control-Allow-Origin` = the origin,
      `-Allow-Methods` listing the configured methods, `-Allow-Headers`,
      `-Max-Age: 600` and `Vary: Origin`; `next` is not called.
    - disallowed origin → 403, `next` not called.
    - disallowed method (`Access-Control-Request-Method: TRACE`) → 403.
  - **Actual requests:**
    - allowed → `next` called, `ACAO` = the origin,
      `Access-Control-Expose-Headers: X-Request-Id`, `Vary: Origin`.
    - disallowed → `next` called, no `ACAO`.
  - `["*"]` without credentials → `ACAO: *`. With credentials and the
    exact origin → `Access-Control-Allow-Credentials: true`.
  - **Upstream sets `Access-Control-Allow-Origin: *`** → the response
    carries only the gateway's value.
  - **`OPTIONS` without `Access-Control-Request-Method`** → passed to
    `next`.
  - **`TestCORS_StreamingAndHijackPassThrough`:**
    `http.NewResponseController(w).Flush()` inside `next` succeeds.
  - **`TestValidate_CORS`:**
    - `*` + `allow_credentials` is an error;
    - an origin `app.example.com` (no scheme) is an error;
    - an origin with a path is an error.
  - **Integration `TestGateway_CORSPreflightBeforeAuth`:** a JWT route
    with CORS. A preflight without a token → 204, not 401.
- [ ] **Step 2: Run** `go test ./internal/middleware/cors/ ./internal/config/ ./cmd/...`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - Response wrapper: on the first `WriteHeader`/`Write`, delete upstream
    `Access-Control-*` headers, then set the gateway's.
  - Implement `Unwrap` and `FlushError` like `statusrec`.
  - Chain position: after logging/metrics, before auth.
- [ ] **Step 4: Run** `go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `Add per-route CORS that answers preflights before auth`

### Task 10: JWT from cookie with CSRF protection

**Files:**
- Modify: `internal/middleware/auth/auth.go`, `jwt_test.go`
- Modify: `internal/config/config.go` (`AuthConfig.JWTCookie \`yaml:"jwt_cookie"\`` + validation), `config_hardening_test.go`
- Modify: `cmd/gateway/routes.go`

**Interfaces:**
- Consumes: `config.CORSConfig` (Task 9).
- Produces:
  - `auth.JWTConfig` gains `Cookie string` and `TrustedOrigins []string`.
  - `NewJWT` errors (wrapping `ErrInvalidJWTConfig`) if an origin is
    rejected by `http.CrossOriginProtection.AddTrustedOrigin`.

- [ ] **Step 1: Write failing tests**, `TestJWT_Cookie` table with cookie
  `session` and trusted origin `https://app.example.com`:
  - cookie token, GET → 200;
  - cookie token, POST, `Sec-Fetch-Site: cross-site`, `Origin: https://evil.example` → 403;
  - cookie token, POST, `Origin: https://app.example.com`, `Sec-Fetch-Site: same-site` → 200;
  - cookie token, POST, `Sec-Fetch-Site: same-origin` → 200;
  - header token, POST, `Sec-Fetch-Site: cross-site` → 200 (no CSRF check);
  - malformed `Authorization` plus a valid cookie → 401 (no fallback when
    the header is present);
  - neither header nor cookie → 401;
  - `Cookie` empty and only a cookie sent → 401.

  `TestValidate_JWTCookie`:
  - `jwt_cookie` + CORS enabled + `allow_credentials: false` → an error
    mentioning `allow_credentials`;
  - `jwt_cookie` + `allowed_origins: ["*"]` → an error.
- [ ] **Step 2: Run** `go test ./internal/middleware/auth/ ./internal/config/`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - The token comes from the header when present. Otherwise it comes
    from the cookie, and then `cop.Check(r)` must pass or the request
    gets 403 `cross-origin request rejected`.
  - `routes.go` sets `TrustedOrigins` from `routeCfg.CORS.AllowedOrigins`
    when CORS is enabled.
  - Never log the cookie.
- [ ] **Step 4: Run** `go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `Accept JWT from a cookie with cross-origin protection`

### Task 11: TLS (ACME and files) plus the HTTP redirect listener

**Files:**
- Create: `internal/tlsx/reload.go`, `redirect.go`, `server.go` + `_test.go` for each
- Create: `internal/config/tls.go` + `tls_test.go`; modify `config.go` (`Config.TLS TLSConfig \`yaml:"tls"\``; `Addr` default from TLS mode)
- Modify: `internal/middleware/metrics/metrics.go` (add `TLSCertReloadErrors`)
- Modify: `cmd/gateway/server.go`, `cmd/gateway/main.go`
- Modify: `go.mod`/`go.sum` (`go get golang.org/x/crypto@v0.59.0`)

**Interfaces:**
- Produces:
  - **Config types:**
    - `config.TLSConfig{Mode, HTTPAddr string; ACME ACMEConfig; Files TLSFilesConfig}`.
    - `ACMEConfig{Domains []string; Email, CacheDir, DirectoryURL string}`.
    - `TLSFilesConfig{CertFile, KeyFile string}`.
    - `Mode` is one of `off|acme|files`, default `off`. `HTTPAddr`
      defaults to `:80`.
  - **`Load` default:** if YAML leaves `server.addr` unset, it becomes
    `:443` when TLS is on and `:8080` otherwise.
  - **`tlsx.NewFileReloader(certFile, keyFile string, checkEvery time.Duration, onErr func(error)) (*FileReloader, error)`**
    returns an error if the initial load fails.
    `(*FileReloader).GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)`.
  - **`tlsx.RedirectHandler(allowedHosts []string) http.Handler`:**
    returns 301 to `https://<host without port><RequestURI>`, and 400 if
    the host is missing or not allowed. An empty list allows any host.
  - **`tlsx.ServerTLS(cfg config.TLSConfig, onReloadErr func(error)) (*tls.Config, http.Handler, error)`**
    returns the TLS config for the public server and the handler for the
    HTTP listener (`autocert.Manager.HTTPHandler(RedirectHandler(domains))`
    for acme, `RedirectHandler(nil)` for files).

- [ ] **Step 1: Write failing tests.**
  - **`TestFileReloader_ReloadsChangedCert`:** generate cert A (serial 1),
    start a `tls.Listen` with `GetCertificate`, and handshake to see
    serial 1. Overwrite with cert B (serial 2), set the mtime +1s, and
    with `checkEvery` 10ms a handshake after 20ms sees serial 2.
  - **`TestFileReloader_BadNewFilesKeepOldCert`:** write garbage → still
    serial 1, and `onErr` is called.
  - **`TestNewFileReloader_MissingFileErrors`.**
  - **`TestRedirectHandler`** table, with allowed `api.example.com`:
    - `http://api.example.com:8080/a?b=1` → 301, `Location: https://api.example.com/a?b=1`;
    - host `evil.example` → 400;
    - empty host → 400;
    - allowed list empty → any host redirects.
  - **`TestServerTLS_ACME`:** domains `api.example.com` and a temp
    `cache_dir`. The returned `NextProtos` contain `acme-tls/1`,
    `MinVersion` == TLS 1.2, and the HTTP handler answers `GET /x` (host
    `api.example.com`) with 301.
  - **`TestValidate_TLS`** table:
    - an unknown mode is an error;
    - acme with no domains is an error;
    - acme with no `cache_dir` is an error;
    - files with a missing file is an error;
    - files with a cert/key pair that doesn't parse is an error;
    - `mode: off` is valid.
  - **Integration `TestGateway_TLSFilesMode`:**
    - Files mode with a test cert. An HTTPS GET with the cert in a
      `RootCAs` pool returns 200 from the upstream, and the upstream sees
      `X-Forwarded-Proto: https`.
    - A plain GET to the HTTP listener → 301 to `https://`.
- [ ] **Step 2: Run** `go test ./internal/tlsx/ ./internal/config/ ./cmd/...`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - **autocert manager:**
    - `Prompt: autocert.AcceptTOS`;
    - `HostPolicy: autocert.HostWhitelist(domains...)`;
    - `Cache: autocert.DirCache(cacheDir)`;
    - `Email`;
    - `Client: &acme.Client{DirectoryURL: …}` only when it's set.
  - Set `MinVersion` on the manager's `TLSConfig()`.
  - Files mode sets `NextProtos: []string{"h2", "http/1.1"}`.
  - `main` uses `ListenAndServeTLS("", "")` for the public server when
    TLS is on, and starts the HTTP listener with read-header 5s, read 5s,
    write 10s and idle 60s timeouts.
  - Log a warning at startup when `mode: off`.
  - Reload errors → `metrics.TLSCertReloadErrors.Inc` and a log line.
- [ ] **Step 4: Run** `go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `Terminate TLS via Let's Encrypt or reloadable cert files`

### Task 12: Admin listener, `-healthcheck`, coordinated shutdown

**Files:**
- Modify: `internal/config/config.go` (`ServerConfig.AdminAddr \`yaml:"admin_addr"\``, default `127.0.0.1:9090`; validation: non-empty, differs from `addr` and `tls.http_addr`)
- Modify: `cmd/gateway/server.go`, `main.go`, `hardening_integration_test.go` (helpers use the new struct)
- Create: `cmd/gateway/healthcheck.go`, `healthcheck_test.go`

**Interfaces:**
- Produces:
  - `type servers struct { Public, Admin, HTTP *http.Server; TLS bool; Close func() }`
    (`HTTP` is nil when TLS is off).
  - `newServers(cfg *config.Config, logger *slog.Logger) (*servers, error)`
    replaces `newServer`.
  - `healthcheck(url string, timeout time.Duration) int`.
  - The flags are `-healthcheck` and
    `-healthcheck-url` (default `http://127.0.0.1:9090/healthz`).

- [ ] **Step 1: Write failing tests.**
  - `TestGateway_AdminEndpointsOnlyOnAdminListener`: with no `/` route,
    the public `/healthz`, `/readyz` and `/metrics` all return 404. The
    admin listener serves `/healthz` 200, `/readyz` 200 JSON, and
    `/metrics` containing `gateway_requests_total`.
  - `TestHealthcheck` table:
    - an httptest server returning 200 → 0;
    - one returning 500 → 1;
    - a closed port → 1.
  - `TestValidate_AdminAddr`: equal to `server.addr` → an error.
- [ ] **Step 2: Run** `go test ./cmd/... ./internal/config/`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `run` starts every non-nil server.
  - On a signal or the first server error, call `Shutdown` on all of them
    concurrently under one `shutdown_timeout` context, then `Close()`
    (the Valkey store).
  - `main` handles `-healthcheck` before loading config and calls
    `os.Exit(healthcheck(url, 2*time.Second))`.
  - Update the k8s probes to port 9090.
- [ ] **Step 4: Run** `go test -race ./...`. Expected: PASS.
- [ ] **Step 5: Commit** `Serve health and metrics on a separate admin listener; add -healthcheck`

### Task 13: Deployment, CI and docs

**Files:**
- Create: `deploy/docker-compose.yml`, `deploy/config.prod.yaml`, `deploy/.env.example`
- Modify: `Dockerfile`, `.github/workflows/ci.yml`, `deploy/k8s/gateway.yaml`
- Modify: `README.md`, `docs/configuration.md`, `docs/deployment.md`, `docs/security.md`, `docs/observability.md`, `docs/architecture.md`, `config.example.yaml`, `.env.example`
- Modify: `internal/config/config_hardening_test.go` (`TestLoad_ProdConfig` loads `deploy/config.prod.yaml` with env set)

- [ ] **Step 1: Write the failing test** `TestLoad_ProdConfig`. Run `go test ./internal/config/ -run ProdConfig`. Expected: FAIL (file missing).
- [ ] **Step 2: Write `deploy/config.prod.yaml`:**
  - `addr: ":8443"`, `admin_addr: ":9090"`;
  - `tls.mode: acme`, `http_addr: ":8080"`, `cache_dir: /var/lib/jgateway/acme`,
    domains `api.example.com`;
  - a `valkey` block (`addr: valkey:6379`, `password_env: VALKEY_PASSWORD`);
  - one example route with CORS and JWT (header plus `jwt_cookie`).

  Then run the test. Expected: PASS.
- [ ] **Step 3: Dockerfile.**
  - The build stage creates `/out/var/lib/jgateway`. The final stage
    copies it in with `--chown=nonroot:nonroot`.
  - `EXPOSE 8443 8080 9090`.
  - `HEALTHCHECK` isn't used by compose; compose defines its own.
- [ ] **Step 4: `docker-compose.yml`.**
  - **`gateway`:**
    - `build: ..`, `ports: ["443:8443", "80:8080"]`;
    - `env_file: .env`;
    - volumes: `./config.prod.yaml:/app/config.yaml:ro` and
      `acme:/var/lib/jgateway`;
    - `healthcheck: test: ["CMD", "/app/gateway", "-healthcheck"]`;
    - `restart: unless-stopped`, `mem_limit: 256m`, `cpus: 1.0`;
    - `depends_on: valkey` (`condition: service_healthy`).
  - **`valkey`:**
    - `image: valkey/valkey:8.1.1`;
    - `command: ["valkey-server", "--requirepass", "${VALKEY_PASSWORD}", "--appendonly", "no", "--save", ""]`;
    - healthcheck `valkey-cli -a $$VALKEY_PASSWORD ping`;
    - `mem_limit: 128m`; no published ports.
  - Volume `acme`.
- [ ] **Step 5: Verify.**
  - `docker compose -f deploy/docker-compose.yml --env-file deploy/.env.example config` exits 0.
  - `docker build -t jgateway:test .` succeeds.
  - `docker run --rm jgateway:test -healthcheck` exits 1 (nothing
    listening), which proves the flag runs in distroless.
- [ ] **Step 6: CI.**
  - Add `services.valkey` (`image: valkey/valkey:8.1.1`, `ports: ["6379:6379"]`,
    health options `--health-cmd "valkey-cli ping"`).
  - Set `env: VALKEY_ADDR: localhost:6379` on the test step.
- [ ] **Step 7: Docs.**
  - `configuration.md`: every new key from Tasks 2, 3, 7, 9, 10, 11 and 12.
  - `deployment.md`: a "Single server with compose" walkthrough (DNS
    A record → copy `deploy/.env.example` → set secrets →
    `docker compose up -d` → verify with `curl -I https://api.example.com/...`
    and `docker compose ps`), plus the Valkey-down behaviour.
  - `security.md`: forwarded headers, identity headers, named keys, CSRF.
  - `observability.md`: admin port, new metrics.
  - `architecture.md`: the updated chains.
  - README: endpoints table and env vars.
- [ ] **Step 8: Final verification.** Run:
  - `gofmt -l .` → empty;
  - `go vet ./...`;
  - `golangci-lint run ./...` → only `examples/` findings;
  - `govulncheck ./...` → no vulnerabilities;
  - `VALKEY_ADDR=localhost:6379 go test -race -count=1 ./...` → all PASS,
    with the Valkey tests reported as run, not skipped
    (`go test -v ... | grep -c SKIP` == 0 for Valkey tests).
- [ ] **Step 9: Commit** `Add compose deployment, Valkey in CI, and production docs`
