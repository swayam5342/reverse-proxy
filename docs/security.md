# Security

This document covers the specific security decisions in the gateway's
middleware, and the threat each one addresses. For the broader
production-readiness bar this project is held to, see the Security section
of [`AGENTS.md`](../AGENTS.md).

## Auth fails closed

Both auth modes (`internal/middleware/auth`) return `401` on *any* error
path — missing header, malformed token, expired token, wrong signature,
unrecognized key. There is no code path where a validation error results in
the request being let through. If you add a new auth mode, preserve this:
an `if err != nil { return }` before the success path, never the inverse.

- **JWT**: the accepted algorithms are pinned to the configured key's
  type (`jwt.WithValidMethods`): a shared secret allows only
  `HS256`/`HS384`/`HS512`; an RSA public key only `RS*`/`PS*`; ECDSA only
  `ES*`; Ed25519 only `EdDSA`. This blocks "alg: none" and the classic
  RS256→HS256 confusion attack (HMAC-signing a token with the public key as
  the secret). Tokens must also carry an unexpired `exp` (a token without
  one would never expire) and a non-empty `sub`, and must match
  `auth.issuer` / `auth.audience` when those are configured. A key that
  fails to parse stops the gateway at startup.
- **API key**: compared with `crypto/subtle.ConstantTimeCompare`, not `==`,
  so response timing can't be used to binary-search a valid key
  byte-by-byte.

## Constant-time comparisons everywhere a secret is checked

- API key comparison: `subtle.ConstantTimeCompare` (`auth.go`).
- HMAC signature comparison: `hmac.Equal` (`signing.go`), never a byte-slice
  `==` or manual loop that could short-circuit.

## Request signing

`internal/middleware/signing` verifies `method + "\n" + path + "\n" +
timestamp + "\n" + body` as a single HMAC-SHA256 payload. Binding the method
and path into the signature means a validly-signed request for one route
can't be replayed against a different route or with a different verb. The
timestamp plus a tolerance window (default 5 minutes) bounds replay: a
captured request/signature pair stops working once the tolerance elapses,
independent of any other rate limiting.

## Rate limit keys

Naive IP-based rate limiting is spoofable in two ways: an attacker behind a
shared NAT/proxy shares a bucket with legitimate users, and `X-Forwarded-For`
is attacker-controlled unless something trustworthy sets it.

`internal/middleware/ratelimit` reflects this directly in its `KeyStrategy`:

- `api_key` / `api_key_ip` key on the identity the auth middleware attached
  to the request context — spoofing this means having a valid credential,
  which is a much smaller attack surface.
- `ip` uses `ratelimit.ClientIP`. If the direct peer is **not** listed in
  `server.trusted_proxies`, the peer address is the client and
  `X-Forwarded-For` is ignored entirely. If it is, `X-Forwarded-For` is
  read **from the right**, the end trusted proxies append to, skipping
  trusted hops; the first untrusted address is the client. Entries to the
  left of that were written by the client and are never used, so
  prepending fake addresses doesn't buy a fresh bucket. (Taking the
  *leftmost* entry, a common mistake, is a free rate-limit bypass.)
- Behind a load balancer, list its address range in
  `server.trusted_proxies`. Otherwise every request appears to come from
  the LB and all clients share one bucket.
- If the configured strategy can't resolve a key (e.g. `api_key_ip` with no
  authenticated identity), the request is rejected with `429` rather than
  falling through unbucketed — fail closed, same principle as auth.

**Recommendation:** default to `api_key` or `api_key_ip` on any route with
auth enabled. Reserve `ip` for genuinely anonymous routes sitting behind a
load balancer you control.

## Request body size

`server.max_body_bytes` (default 10 MiB) is enforced by
`internal/middleware/bodylimit`, outside every route chain: requests
declaring a larger `Content-Length` get `413` immediately, and chunked
bodies fail with `413` once they cross the limit. Signing verification and
retry both read the whole body into memory, so without this cap a handful
of large uploads could exhaust the pod's memory.

## Secrets

Every secret (JWT signing key, API keys, HMAC signing key) is configured in
YAML as an *env var name*, never a literal value — see
[`docs/configuration.md`](configuration.md). `config.Validate()` fails
startup if a referenced env var is unset, so a missing secret is caught
before the process ever accepts a connection, not discovered as a runtime
401 storm. Never commit a real `.env` file; `.env.example` documents the
shape only.

## Headers backends can trust

The gateway owns every header a backend might use for a security decision.
Whatever a client sends under these names never reaches a backend.

- **`X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `Forwarded`.**
  `proxy.NewReverseProxy` uses `httputil.ReverseProxy.Rewrite`, which drops
  the client's copies. The gateway then sets them itself:
  - `X-Forwarded-For` is the client IP resolved by `httpx.ClientIP`;
  - `X-Forwarded-Host` is the original `Host`;
  - `X-Forwarded-Proto` is `https` when the gateway terminated TLS.

  The outbound `Host` is the upstream's own.
- **`X-Auth-Subject`, `X-Auth-Method`** (and anything else starting with
  `X-Auth-`). The gateway-wide `sanitize` middleware strips client-supplied
  copies on every route, including routes without auth. After successful
  authentication the auth middleware sets the subject (the JWT `sub`, or
  the API key's *name*) and the method (`jwt` or `api_key`). A backend can
  authorize on them, provided it is only reachable through the gateway.

## API keys are named; raw keys never leave the gateway

`auth.api_keys_env` holds `name:key` pairs. The name is the caller's
identity everywhere: the request context, `X-Auth-Subject`, the rate-limit
key and logs. The key itself is only ever compared, in constant time.
Rate-limit keys are additionally hashed (`jgw:rl:<route>:<sha256 prefix>`),
so Valkey never holds API-key names or client IPs in clear text.

## Cookie JWTs and CSRF

With `auth.jwt_cookie`, a browser's session cookie can carry the JWT.
Browsers attach cookies to cross-site requests on their own, so a
cookie-authenticated POST, PUT, PATCH or DELETE must also pass Go's
`http.CrossOriginProtection`.

- It passes if `Sec-Fetch-Site` is `same-origin`, or if `Origin` is one of
  the route's `cors.allowed_origins`. Otherwise the request gets `403`.
- Requests that use the `Authorization` header skip the check, because a
  foreign site can't make a browser attach that header.
- A malformed `Authorization` header never falls back to the cookie.

## Path cleaning

The public listener cleans paths before routing (`ServeMux` redirects
`/api/x/../../admin` to `/admin`). A path can therefore never match one
route's prefix, and its auth and limits, and then resolve upstream to a
different path. Route prefixes match on segment boundaries, so `/api/users`
doesn't match `/api/users-admin`.

## Admin endpoints are not public

`/healthz`, `/readyz` and `/metrics` are only served on `server.admin_addr`
(default `127.0.0.1:9090`). Readiness bodies reveal upstream addresses and
errors, and metrics reveal traffic patterns, so neither belongs on the
internet. Don't publish the admin port.

## What's out of scope today

- No mTLS between gateway and upstreams. The gateway terminates TLS for
  clients (see [`docs/deployment.md`](deployment.md)); upstream traffic is
  expected to stay on the host or a private network.
- No WAF-style deep body inspection; signing and auth cover
  authenticity/integrity, not payload content validation (that's the
  upstream's job).
- `govulncheck` runs in CI against dependencies, but there's no automated
  secret-scanning pre-commit hook yet — review `git status`/`git diff`
  before committing config or env files.
