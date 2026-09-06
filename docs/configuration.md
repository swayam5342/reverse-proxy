# Configuration reference

Config is loaded once at startup from a YAML file (`config.example.yaml` is
the annotated template), validated, and the process exits immediately with a
descriptive error if anything is wrong — it never starts half-broken
(`internal/config/config.go`, `Config.Validate`).

Secrets are never stored in the YAML file itself — every secret field is an
*env var name*, resolved from the environment at startup (`.env.example`
lists every variable the example config references).

## Loading

```sh
GATEWAY_CONFIG=config.yaml   # path to the YAML file; defaults to config.yaml
GATEWAY_ADDR=:8080           # overrides server.addr if set
```

## `server`

| Field | Type | Meaning |
|---|---|---|
| `addr` | string | Listen address, e.g. `:8080`. Required. |
| `read_timeout` | duration | `http.Server.ReadTimeout`. Must be > 0. |
| `write_timeout` | duration | `http.Server.WriteTimeout`. Must be > 0. |
| `idle_timeout` | duration | `http.Server.IdleTimeout`. |
| `read_header_timeout` | duration | `http.Server.ReadHeaderTimeout`. |
| `shutdown_timeout` | duration | How long `Shutdown` waits for in-flight requests to drain on SIGTERM/SIGINT before giving up. Must be > 0. |

Defaults (`config.Default()`, used for any field left unset): 5s read, 10s
write, 120s idle, 5s read-header, 30s shutdown.

## `upstreams[]`

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Unique identifier referenced by `routes[].upstream`. Required. |
| `url` | string | Absolute URL of the backend, e.g. `http://orders-api.internal:8082`. Required, must parse with scheme + host. |
| `dial_timeout` | duration | TCP dial timeout for this upstream's transport. Must be > 0. |
| `request_timeout` | duration | Response-header timeout and overall `http.Client.Timeout` for this upstream. Must be > 0. |
| `circuit_breaker.enabled` | bool | Enable the per-upstream breaker. |
| `circuit_breaker.failure_threshold` | int | Consecutive 5xx responses (in closed state) before tripping open. Required > 0 if enabled. |
| `circuit_breaker.half_open_probe_after` | duration | How long the breaker stays open before allowing a probe request. |
| `circuit_breaker.half_open_max_requests` | int | Concurrent probe requests allowed while half-open. Defaults to 1. |
| `retry.enabled` | bool | Enable retries for requests to this upstream. |
| `retry.max_attempts` | int | Total attempts including the first (e.g. 3 = up to 2 retries). Required > 0 if enabled. |
| `retry.base_backoff` | duration | Base of the exponential backoff. Defaults to 50ms if unset. |
| `retry.max_backoff` | duration | Backoff ceiling. Defaults to 5s if unset. |

Retries only ever apply to idempotency-safe requests: GET, HEAD, OPTIONS,
PUT, DELETE, or POST carrying an `Idempotency-Key` header — this isn't
configurable per route, it's a hard rule in `internal/middleware/retry`.

## `routes[]`

| Field | Type | Meaning |
|---|---|---|
| `path_prefix` | string | Longest-prefix match against the incoming request path. Required. |
| `upstream` | string | Must match an `upstreams[].name`. Required. |
| `auth.enabled` | bool | Enable auth for this route. |
| `auth.mode` | `jwt` \| `api_key` | Required if `auth.enabled`. |
| `auth.jwt_secret_env` | string | Env var name holding the HMAC signing secret (mode `jwt`). Must be set and non-empty at load time. |
| `auth.api_keys_env` | string | Env var name holding comma-separated valid keys (mode `api_key`). Must be set and non-empty at load time. |
| `rate_limit.enabled` | bool | Enable the token-bucket limiter for this route. |
| `rate_limit.requests_per_second` | float | Sustained rate. Required > 0 if enabled. |
| `rate_limit.burst` | int | Bucket size. Required > 0 if enabled. |
| `rate_limit.key_strategy` | `api_key` \| `api_key_ip` \| `ip` | See [`docs/security.md`](security.md#rate-limit-keys) before choosing `ip`. |
| `signing.enabled` | bool | Enable HMAC signature verification for this route. |
| `signing.secret_env` | string | Env var name holding the shared HMAC secret. Must be set and non-empty at load time. |
| `signing.header_name` | string | Header carrying the hex-encoded signature, e.g. `X-Signature`. Required. |
| `signing.tolerance_seconds` | int64 | Max allowed drift between request timestamp and server clock. Defaults to 300s if 0. |

`key_strategy: api_key` and `api_key_ip` require `auth.enabled: true` on the
same route — they key on the identity the auth middleware attaches to the
request context (`auth.IdentityFromContext`), and a missing identity fails
the request closed (429) rather than falling back to an unkeyed bucket.

## Validation

`Config.Validate()` collects *every* problem before returning, so a single
run tells you everything wrong rather than one error per fix-and-rerun
cycle. Checked at minimum: required fields, absolute-URL parsing on
upstream URLs, positive timeouts, route→upstream references resolving,
and (for anything referencing an env var) that the env var is actually set
and non-empty — a config that names `JWT_SECRET_ENV: JWT_SECRET` but never
had `JWT_SECRET` exported fails at startup, not at the first request.

## Full example

See [`config.example.yaml`](../config.example.yaml) for a complete two-route
example (JWT + per-key rate limiting on one route; API key + HMAC signing +
identity-and-IP rate limiting on the other), and
[`.env.example`](../.env.example) for the matching env vars.
