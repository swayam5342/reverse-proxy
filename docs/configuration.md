# Configuration reference

Config is loaded once at startup from a YAML file, validated, and the
process exits immediately with a descriptive error if anything is wrong. It
never starts half-broken (`internal/config`, `Config.Validate`).

- [`config.example.yaml`](../config.example.yaml) is an annotated template
  for local use.
- [`deploy/config.prod.yaml`](../deploy/config.prod.yaml) is a complete
  production config for the compose deployment (TLS, Valkey, CORS, cookie
  JWTs, service API keys).

Secrets are never stored in YAML. Every secret field is an *env var name*,
resolved from the environment at startup ([`.env.example`](../.env.example),
[`deploy/.env.example`](../deploy/.env.example)).

**Unknown keys are errors.** A misspelled key (`requst_timeout`) or a removed
one stops startup with the key's name, instead of silently falling back to a
default.

## Loading

```sh
GATEWAY_CONFIG=config.yaml   # path to the YAML file; defaults to config.yaml
GATEWAY_ADDR=:8080           # overrides server.addr
GATEWAY_ADMIN_ADDR=:9090     # overrides server.admin_addr (containers, k8s)
```

`gateway -healthcheck [-healthcheck-url URL]` GETs the admin `/healthz`
(default `http://127.0.0.1:9090/healthz`) and exits 0 or 1. It exists for
container healthchecks, because the image has no shell or curl.

## `server`

| Field | Type | Meaning |
|---|---|---|
| `addr` | string | Public listener. Defaults to `:443` when TLS is on, `:8080` otherwise. |
| `admin_addr` | string | Admin listener for `/healthz`, `/readyz`, `/metrics`, which are never served on `addr`. Default `127.0.0.1:9090`. In containers use `:9090` and never publish it. Must differ from `addr` and `tls.http_addr`. |
| `read_timeout` | duration | `http.Server.ReadTimeout`. Must be > 0. |
| `write_timeout` | duration | `http.Server.WriteTimeout`. Must be > 0. |
| `idle_timeout` | duration | `http.Server.IdleTimeout`. |
| `read_header_timeout` | duration | `http.Server.ReadHeaderTimeout`. |
| `shutdown_timeout` | duration | How long all listeners drain in-flight requests on SIGTERM/SIGINT. Must be > 0. |
| `request_timeout` | duration | Overall deadline for one request through the whole chain, retries and backoff included. On expiry the client gets `504`. Must be > 0 and < `write_timeout`. |
| `upgrade_timeout` | duration | Maximum lifetime of an upgraded (WebSocket) connection, used instead of `request_timeout`. Must be > 0. |
| `max_body_bytes` | int | Larger request bodies get `413` before any middleware buffers them. `0` disables the limit. |
| `trusted_proxies` | list of CIDR or IP | Load balancers in front of the gateway. `X-Forwarded-For` is only used when the direct peer is in this list. See [security](security.md#rate-limit-keys). |

Defaults: 5s read, 10s write, 120s idle, 5s read-header, 30s shutdown, 9s
request, 1h upgrade, 10 MiB body limit, no trusted proxies.

## `tls`

| Field | Type | Meaning |
|---|---|---|
| `mode` | `off` \| `acme` \| `files` | Default `off` (plaintext, with a startup warning). |
| `http_addr` | string | Plaintext listener: ACME HTTP-01 challenges, plus 301 redirects to `https://`. It never proxies. Default `:80`. |
| `acme.domains` | list | Names to get Let's Encrypt certificates for: plain DNS names, no wildcards. Requests for other names are refused, and the redirect listener answers them with `400`. |
| `acme.email` | string | Optional contact for expiry notices. |
| `acme.cache_dir` | string | Where account keys and certificates are stored. **Must persist across restarts** (a volume), or every restart requests new certificates and you hit Let's Encrypt rate limits. Required for `acme`. |
| `acme.directory_url` | string | Optional CA override, e.g. Let's Encrypt staging (`https://acme-staging-v02.api.letsencrypt.org/directory`) while testing. |
| `files.cert_file` / `files.key_file` | string | PEM certificate chain and key. Loaded at startup (fails fast if unreadable). Reloaded within 30s of the files changing, so renewals need no restart. A broken renewal keeps the previous certificate and increments `gateway_tls_cert_reload_errors_total`. |

Both modes use TLS 1.2 or newer and HTTP/2. `acme` answers both TLS-ALPN-01
(on `addr`) and HTTP-01 (on `http_addr`), so ports 443 and 80 must reach the
gateway from the internet.

## `valkey`

When present, every rate-limited route keeps its buckets in Valkey, shared
by all gateway copies. When absent, each route uses an in-memory store per
process.

| Field | Type | Meaning |
|---|---|---|
| `addr` | `host:port` | Required. |
| `username_env` / `password_env` | string | Env var names for ACL credentials. If named, the env var must be set. |
| `db` | int | Logical database. Default 0. |
| `tls` | bool | Connect over TLS. |
| `timeout` | duration | Per-call budget. Default 50ms. |

**When Valkey is down,** limits fall back to per-process memory, so they
are less precise but still enforced, and nothing fails:
- Valkey is skipped for 5s after an error, then retried automatically.
- A warning is logged at most every 10s.
- Each decision answered from memory increments
  `gateway_rate_limit_store_fallback_total`.
- `/readyz` reports `valkey` with its error.

The gateway starts even if Valkey is unreachable.

## `upstreams[]`

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Unique identifier referenced by `routes[].upstream`. Required. |
| `url` | string | Absolute URL of the backend. A path is prepended to forwarded paths (`http://svc/v1` + `/x` → `/v1/x`). |
| `dial_timeout` | duration | TCP dial timeout. Must be > 0. |
| `request_timeout` | duration | Wait for response headers, per attempt. Must be > 0 and <= `server.request_timeout`. |
| `circuit_breaker.enabled` | bool | One breaker per upstream, shared by every route that uses it. |
| `circuit_breaker.failure_threshold` | int | Consecutive 5xx responses before opening. Required > 0 if enabled. |
| `circuit_breaker.half_open_probe_after` | duration | How long it stays open before a probe. |
| `circuit_breaker.half_open_max_requests` | int | Concurrent probes while half-open. Default 1. |
| `retry.enabled` | bool | Enable retries. |
| `retry.max_attempts` | int | Total attempts including the first. Required > 0 if enabled. |
| `retry.base_backoff` / `retry.max_backoff` | duration | Jittered exponential backoff. Defaults 50ms / 5s. |

Retries only apply to idempotency-safe requests: GET, HEAD, OPTIONS, PUT,
DELETE, or POST with an `Idempotency-Key` header. They never apply to
WebSocket upgrades.

## `routes[]`

| Field | Type | Meaning |
|---|---|---|
| `path_prefix` | string | Must start with `/`. Matches on path-segment boundaries: `/api/users` matches `/api/users` and `/api/users/42`, but not `/api/users-admin`. The longest prefix wins. |
| `strip_prefix` | bool | Remove `path_prefix` before forwarding (`/api/users/42` → `/42`). Percent-encoding is preserved. |
| `upstream` | string | Must match an `upstreams[].name`. |
| `auth.enabled` / `auth.mode` | bool / `jwt` \| `api_key` | |
| `auth.jwt_secret_env` | string | Env var holding an HMAC secret (HS256/384/512). |
| `auth.jwt_public_key_env` | string | Env var holding the issuer's PEM public key or certificate (RS*/PS*, ES* or EdDSA, depending on the key type). Exactly one of this and `jwt_secret_env`. |
| `auth.issuer` / `auth.audience` | string | If set, `iss` must equal / `aud` must contain it. |
| `auth.leeway` | duration | Clock-skew allowance for `exp`/`nbf`/`iat`. |
| `auth.jwt_cookie` | string | Read the JWT from this cookie when there's no `Authorization` header. Cookie-authenticated POST/PUT/PATCH/DELETE must pass a cross-origin check, and fail with `403` otherwise. With CORS enabled this requires `cors.allow_credentials: true` and no `*` origin. |
| `auth.api_keys_env` | string | Env var holding `name:key,name:key` (mode `api_key`). The name identifies the caller. An entry without a name is named `key-<8 hex of sha256(key)>`. |
| `cors.enabled` | bool | Enable CORS for browser frontends. |
| `cors.allowed_origins` | list | Exact `scheme://host[:port]` values, or `["*"]` (not with credentials). |
| `cors.allowed_methods` / `cors.allowed_headers` | list | Defaults: `GET, HEAD, POST, PUT, PATCH, DELETE` / `Authorization, Content-Type`. |
| `cors.expose_headers` | list | Response headers the browser may read, e.g. `X-Request-Id`. |
| `cors.allow_credentials` | bool | Needed for cookies. |
| `cors.max_age` | duration | How long browsers cache preflights. |
| `rate_limit.*` | | `requests_per_second`, `burst`, and `key_strategy` (`api_key` \| `api_key_ip` \| `ip`). See [security](security.md#rate-limit-keys). |
| `signing.*` | | `secret_env`, `header_name`, `tolerance_seconds`: HMAC request signing. |

JWTs must carry an unexpired `exp` and a non-empty `sub`.

`key_strategy: api_key` and `api_key_ip` need auth on the same route. A
missing identity fails closed with `429`.

## Validation

`Config.Validate()` reports *every* problem in one run. Checks include:
- required fields and absolute upstream URLs;
- unknown keys;
- timeouts that fit inside each other
  (`upstreams[].request_timeout` <= `server.request_timeout` <
  `server.write_timeout`);
- listener addresses that don't collide;
- parseable `trusted_proxies`, CORS origins and ACME domains;
- TLS certificate files that actually load;
- that every env var the config names is set.
