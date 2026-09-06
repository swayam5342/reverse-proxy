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

- **JWT**: only `HS256`/`HS384`/`HS512` are accepted
  (`jwt.WithValidMethods`), which also blocks the classic
  "alg: none" / algorithm-confusion attack against `jwt.Parse`'s keyfunc.
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
- `ip` reads `X-Forwarded-For` **only if** `TrustForwardedFor` is explicitly
  set, and even then takes only the leftmost entry (the convention for
  "closest-to-client" when the LB itself appends). If you're not certain
  your load balancer overwrites (not appends to) that header, do not set
  `TrustForwardedFor: true` — it becomes a free rate-limit bypass.
- If the configured strategy can't resolve a key (e.g. `api_key_ip` with no
  authenticated identity), the request is rejected with `429` rather than
  falling through unbucketed — fail closed, same principle as auth.

**Recommendation:** default to `api_key` or `api_key_ip` on any route with
auth enabled. Reserve `ip` for genuinely anonymous routes sitting behind a
load balancer you control.

## Secrets

Every secret (JWT signing key, API keys, HMAC signing key) is configured in
YAML as an *env var name*, never a literal value — see
[`docs/configuration.md`](configuration.md). `config.Validate()` fails
startup if a referenced env var is unset, so a missing secret is caught
before the process ever accepts a connection, not discovered as a runtime
401 storm. Never commit a real `.env` file; `.env.example` documents the
shape only.

## Host header / header injection

`proxy.NewReverseProxy`'s director explicitly overwrites `req.Host` and
`req.URL.Host` from the configured upstream target — the inbound `Host`
header is never forwarded as-is into a field the upstream might trust for
routing or link generation.

## What's out of scope today

- No mTLS between gateway and upstreams — see
  [`docs/deployment.md`](deployment.md) for where TLS termination is
  assumed to happen.
- No WAF-style deep body inspection; signing and auth cover
  authenticity/integrity, not payload content validation (that's the
  upstream's job).
- `govulncheck` runs in CI against dependencies, but there's no automated
  secret-scanning pre-commit hook yet — review `git status`/`git diff`
  before committing config or env files.
