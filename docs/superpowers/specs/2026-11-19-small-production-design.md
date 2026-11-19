# Small-production readiness: Valkey, TLS, CORS, and edge hardening

Date: 2026-11-19 · Branch: `hardening/production-fixes` (builds on `717f668`)

## Intent

Make jgateway something the owner can put in front of a real, small
production application and trust. The gateway is the public edge:

- **Callers:** a browser frontend (needs CORS) and other services (API
  keys / request signing).
- **Placement:** runs on the same host(s) as the backend services, with no
  load balancer in front. The gateway terminates TLS itself.
- **Valkey is essential:** it is the shared rate-limit store so more than
  one gateway copy can run with one set of limits.

Success: the owner can `docker compose up` on a server with a domain
name, get HTTPS automatically, point the frontend at it, and have limits,
auth, and observability behave correctly without reading the source.

## Goals

1. Valkey-backed rate limiting shared across gateway copies.
2. Built-in TLS, from automatic Let's Encrypt or from certificate files.
3. Configurable CORS per route.
4. Close the remaining edge-security gaps from the review (route
   boundaries, forwarded headers, identity forwarding).
5. Make the existing features honest (breaker per upstream, metrics that
   are actually recorded, no silently ignored config).
6. A ready-to-run single-host deployment (docker compose + Valkey).

## Non-goals

- Load balancing across multiple instances of one backend (one URL per
  upstream stays).
- Config hot reload; JWKS URL fetching; issuing or refreshing session
  cookies (the gateway only reads them).
- gRPC, service mesh, UI (unchanged AGENTS.md non-goals).
- A Kubernetes-first setup. The k8s manifest stays, updated for the new
  admin port, but compose is the primary path.

## Anti-goals (would make this a failure even if it works)

- A Valkey hiccup taking routes down.
- Metrics, readiness, or admin endpoints reachable from the internet.
- Backends trusting a header a client could have forged.
- Secrets (API keys, private keys) appearing in Valkey key names, logs,
  or forwarded headers.
- A typo in config silently ignored.

## Decisions made with the owner

- **TLS:** both sources, chosen in config (`tls.mode: acme | files`).
- **Valkey outage:** fall back to per-copy in-memory limits, log + count
  it, switch back automatically.
- **CORS:** configurable (per route).
- **Browser auth:** not decided yet, so support both. The JWT comes from
  `Authorization: Bearer` first, and falls back to a named cookie.

## Approaches considered

**TLS certificates.** (a) automatic Let's Encrypt only: least work for a
public domain, but useless when certs come from elsewhere (Cloudflare
origin certs, internal CA). (b) files only: no new dependency, but the
owner runs certbot and renewals themselves. (c) **both, chosen in config:
picked.** It costs one extra code path and covers both cases.

**Valkey outage.** (a) **in-memory fallback: picked**, because limits keep
working (less precisely) and nothing goes down. (b) 503: strict, but turns
Valkey into a single point of failure for every limited route.
(c) fail open: no outage, but no protection either.

**Valkey client (my call).** `github.com/valkey-io/valkey-go`, the
official Valkey client, over `go-redis`, since Valkey is the named target.

## Design

### 1. Valkey rate-limit store

- **Interface change:**
  `Store.Allow(ctx, key string, rps float64, burst int, now time.Time) (bool, error)`.
  `MemoryStore` never errors.
- **`ValkeyStore`:** one Lua script, loaded once and invoked by SHA
  (valkey-go's `valkey.NewLuaScript`). It implements the same token bucket
  as `MemoryStore`, atomically, in a hash `{tokens, ts}`. It uses Valkey's
  own `TIME` so gateway copies with skewed clocks agree. It sets
  `PEXPIRE` to the full-refill time plus 1s, so idle keys expire by
  themselves (the Valkey-side equivalent of the memory sweep).
- **Key namespacing and secrecy:** the middleware builds the key as
  `jgw:rl:<route>:<sha256(key)[:32 hex]>`. Routes stay independent, as
  they are today with one MemoryStore per route, and raw API keys or IPs
  never appear in Valkey key names.
- **Fallback (`FallbackStore`):** wraps the Valkey store and an in-memory
  store. Each Valkey call gets its own timeout (`valkey.timeout`, default
  50ms). On error it answers from the memory store, logs at most once per
  10s, increments `gateway_rate_limit_store_fallback_total`, and skips
  Valkey for a 5s cool-down, so an outage doesn't add the timeout to every
  request. After the cool-down it tries Valkey again.
- **Config:** a top-level `valkey:` block. When it's present, every
  rate-limited route uses it:
  ```yaml
  valkey:
    addr: "valkey:6379"
    username_env: ""          # optional, ACL user
    password_env: VALKEY_PASSWORD
    db: 0
    tls: false
    timeout: 50ms
  ```
  Startup pings Valkey and logs a warning if it can't connect; it does not
  refuse to start, which matches the fallback decision.
- **Readiness:** `/readyz` reports `valkey: ok | <error>` in its body,
  informational only, like upstreams.

### 2. TLS

```yaml
tls:
  mode: acme            # off | acme | files
  http_addr: ":80"      # HTTP listener: ACME HTTP-01 + redirect to HTTPS
  acme:
    domains: ["api.example.com"]
    email: "ops@example.com"
    cache_dir: "/var/lib/jgateway/acme"
    directory_url: ""   # empty = Let's Encrypt production; set for staging
  files:
    cert_file: /etc/jgateway/tls/fullchain.pem
    key_file:  /etc/jgateway/tls/privkey.pem
```

- `acme` uses `golang.org/x/crypto/acme/autocert` with `HostPolicy`
  locked to `domains`, a `DirCache`, and TLS-ALPN-01 on the TLS listener
  plus HTTP-01 on `http_addr`. Copies on one host share the cache through
  a shared volume.
- `files` uses a `GetCertificate` that re-reads the pair when either
  file's mtime changes. It checks at most every 30s, during handshakes,
  with no background goroutine. A failed reload keeps the last good
  certificate and logs it.
- Both modes use `MinVersion: TLS 1.2` and HTTP/2 enabled.
  `server.addr` defaults to `:443` when TLS is on.
- The `http_addr` listener serves only ACME challenges (acme mode) and
  301 redirects to `https://host/path`. It never proxies.
- `mode: off` is allowed (local dev, or a TLS-terminating proxy in front)
  and logged as a warning at startup.

### 3. CORS (per route)

```yaml
cors:
  enabled: true
  allowed_origins: ["https://app.example.com"]   # exact match, or ["*"]
  allowed_methods: [GET, POST, PUT, DELETE]
  allowed_headers: [Authorization, Content-Type, Idempotency-Key]
  expose_headers: [X-Request-Id]
  allow_credentials: false
  max_age: 10m
```

- New `internal/middleware/cors`. It sits in the route chain after
  logging/metrics and **before auth** (cookie CSRF checks reuse its
  origin list, see 3b), so preflights are observed but
  never rejected for missing credentials.
- **Preflight** (`OPTIONS` + `Origin` + `Access-Control-Request-Method`):
  an allowed origin gets 204 with CORS headers; a disallowed origin or
  method gets 403. It is never forwarded upstream.
- **Actual requests:** an allowed origin gets the CORS headers plus
  `Vary: Origin`. A disallowed origin passes through without CORS headers
  (the browser blocks it; server-to-server callers are unaffected).
  `Access-Control-*` headers set by the upstream are removed, so the
  gateway is the single source.
- **Validation:** `*` together with `allow_credentials: true` is rejected,
  and origins must be `scheme://host[:port]`.

### 3b. JWT from a cookie, with CSRF protection

```yaml
auth:
  mode: jwt
  jwt_cookie: "session"   # optional; read when there's no Authorization header
```

- The JWT middleware looks for `Authorization: Bearer` first. Only if that
  header is absent does it read the `jwt_cookie` cookie. All the same
  validation applies (signature, `exp`, `sub`, issuer, audience).
- **CSRF:** browsers attach cookies automatically, so a token that came
  from the cookie must also pass the standard library's
  `http.CrossOriginProtection` (Go 1.25). It is checked through its
  `Check` method.
  - It rejects cross-origin non-safe requests (POST, PUT, DELETE, PATCH)
    with 403, using `Sec-Fetch-Site` with an `Origin` fallback.
  - The trusted origins are the route's `cors.allowed_origins`.
  - Bearer-header requests skip this check, because browsers never attach
    that header on their own.
- **Validation:**
  - `jwt_cookie` on a route with CORS enabled requires
    `allow_credentials: true`, or the browser won't send the cookie
    cross-origin. The error message says so.
  - `jwt_cookie` also forbids `allowed_origins: ["*"]`.
- The cookie value is never logged.

### 4. Edge hardening

- **Route matching on segment boundaries:** prefix `/api/users` matches
  `/api/users` and `/api/users/...`, not `/api/users-admin`. A prefix
  ending in `/`, or `/` itself, matches as written.
- **Optional prefix strip:** `strip_prefix: true` per route forwards
  `/api/users/42` as `/42` (empty becomes `/`), handling `Path` and
  `RawPath`. Signing still verifies the public path, because the strip
  happens in the proxy, last.
- **Forwarded headers:** switch the proxy from `Director` to `Rewrite`.
  `SetURL` also honours a base path in an upstream URL, which is ignored
  today. Client-supplied `Forwarded` and `X-Forwarded-*` headers are
  dropped and rebuilt:
  - `X-Forwarded-For` = the resolved client IP (`httpx.ClientIP`, moved
    out of ratelimit).
  - `X-Forwarded-Proto` = `https` or `http`.
  - `X-Forwarded-Host` = the original `Host`.
- **Identity to backends:** a gateway-wide middleware strips any
  client-supplied `X-Auth-*` header on every route. After successful auth,
  the auth middleware sets `X-Auth-Subject` and `X-Auth-Method`, so
  backends can trust them.
- **API key names:** `API_KEYS` entries become `name:key` (for example
  `billing-svc:s3cr3t,reports:abc`). The subject is the name, so the raw
  key is never forwarded, logged, or used as a limit key. Entries without
  a name still work, with subject `key-<first 8 hex of sha256(key)>`.

### 5. Honest features

- **One breaker per upstream,** shared by every route to it (built once in
  `newServer`).
- **Metrics actually recorded,** through callbacks in each middleware's
  config (`OnStateChange`, `OnReject`, `OnRetry`), wired in `main`:
  - breaker state gauge;
  - rate-limit rejections;
  - retry attempts (the counter is also registered, which it isn't today);
  - new: `gateway_rate_limit_store_fallback_total`;
  - new: `gateway_tls_cert_reload_errors_total`.
- **Strict config parsing:** YAML is decoded with `KnownFields(true)`, so
  typos and removed fields fail at startup. The dead fields
  `retry.idempotent_only` and `rate_limit.window` are removed. This is
  deliberately breaking.
- **Remove the unused `internal/discovery` package.**

### 6. Admin listener

`server.admin_addr` (default `127.0.0.1:9090`) serves `/healthz`,
`/readyz` and `/metrics`. The public listener serves only proxied routes.
In compose it's set to `:9090`, so other containers such as Prometheus
can reach it on the compose network, but the port is never published to
the host. *(My call.)*

### 7. Deployment

- **`deploy/docker-compose.yml`:**
  - `gateway`, built from the Dockerfile. Inside the container it listens
    on unprivileged ports (`:8443` TLS, `:8080` HTTP, running as
    non-root), and compose publishes them as `443:8443` and `80:8080`.
    Redirects drop the port, so clients land on 443. *(My call.)*
  - `valkey` (`valkey/valkey:8.1.1`, `requirepass` from env, no
    persistence, not published);
  - a named volume for the ACME cache, mounted config and secrets via
    `.env`;
  - `mem_limit` / `cpus` set and `restart: unless-stopped`.
- **Healthcheck:** a `gateway -healthcheck` flag GETs the admin
  `/healthz` and exits 0/1. This is needed because distroless has no
  shell or curl; compose's `healthcheck` uses it.
- **Image:** the Dockerfile adds `/var/lib/jgateway` owned by `nonroot`
  for the ACME cache. distroless `static` already includes CA certs.
- **Docs:** `docs/deployment.md` gets a "single server with compose"
  walkthrough (DNS, `.env`, `docker compose up`, verify).

### 8. Testing

- TDD as in the previous change: unit tests per middleware, then
  full-server integration tests.
- **Valkey tests against a real Valkey:**
  - Gated by `VALKEY_ADDR`, and skipped with a message when it's unset.
  - Locally: `docker run -p 6379:6379 valkey/valkey:8.1.1`.
  - CI adds a `valkey` service container and sets `VALKEY_ADDR`.
- **Fallback logic** is tested with a fake failing store, so no Valkey is
  needed for those tests.
- **TLS tests:**
  - files mode: certs generated in the test, the cert swapped on disk,
    the reload observed over a real TLS handshake;
  - redirect listener;
  - ACME: wiring and config validation only, against no real CA.
- **Before done:**
  - `go test -race ./...` with `VALKEY_ADDR` set, so the Valkey tests run;
  - `gofmt`, `go vet`, `golangci-lint`, `govulncheck`.

## Left to the builder

- Exact file layout inside the new packages.
- Log wording.
- Exact metric help strings.
- The order of commits.

## Breaking changes (to call out in the PR)

- Unknown or removed config keys now fail startup (`idempotent_only`,
  `window`).
- `Store` interface signature.
- Forwarded `X-Forwarded-*` and `X-Auth-*` headers are now gateway-owned.
- `/healthz`, `/readyz` and `/metrics` move to the admin listener.
- Route matching respects path segments (`/api/users` no longer matches
  `/api/users-admin`).
