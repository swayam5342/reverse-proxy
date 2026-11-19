# Deployment

## Single server with compose

The primary deployment is one server with your application and the gateway
side by side. The gateway is the public edge: it terminates TLS with
automatic Let's Encrypt certificates, redirects HTTP to HTTPS, and shares
rate limits through Valkey.
[`deploy/docker-compose.yml`](../deploy/docker-compose.yml) runs the gateway
and Valkey, and [`deploy/config.prod.yaml`](../deploy/config.prod.yaml) is
the config.

```text
internet ──443/80──► gateway ──► your app (host or compose service)
                        │
                        └──► valkey (compose network only)
           admin :9090 (health, readiness, metrics): compose network only
```

1. **DNS.** Point an A/AAAA record (e.g. `api.example.com`) at the server.
   Ports 80 and 443 must be reachable from the internet, because Let's
   Encrypt validates through them.
2. **Config.** Edit `deploy/config.prod.yaml`:
   - set `tls.acme.domains` and `email`;
   - set the upstream `url` (`http://host.docker.internal:3000` reaches an
     app running directly on the host);
   - set `cors.allowed_origins` to your frontend's exact origin;
   - choose JWT secret or public-key auth.
3. **Secrets.** `cp deploy/.env.example deploy/.env`, then fill in
   `JWT_SECRET`, `SERVICE_API_KEYS` (`name:key,...`) and `VALKEY_PASSWORD`.
   Generate them with `openssl rand -base64 48`. `deploy/.env` is
   git-ignored.
4. **Start.** `docker compose -f deploy/docker-compose.yml up -d --build`
5. **Verify.**
   - `docker compose -f deploy/docker-compose.yml ps`: both services
     `healthy`.
   - `curl -I http://api.example.com/api/x`: `301` to `https://`.
   - `curl -i https://api.example.com/api/x`: `401` without a token, which
     shows TLS and auth are working.
   - `docker compose -f deploy/docker-compose.yml logs gateway`: one JSON
     line per request.

While testing, set `tls.acme.directory_url` to Let's Encrypt staging
(`https://acme-staging-v02.api.letsencrypt.org/directory`) to avoid
production rate limits. Remove it before going live, and remove the `acme`
volume so the staging certificates are discarded.

To use your own certificate instead (a Cloudflare origin cert, a corporate
CA), set `tls.mode: files` and mount the files read-only. Renewed files are
picked up within 30s without a restart.

**What compose sets for you:**

| Setting | Why |
|---|---|
| `443:8443`, `80:8080` | The container listens on unprivileged ports as `nonroot`. Redirects drop the port, so clients land on 443. |
| `acme` volume at `/var/lib/jgateway` | Certificates survive restarts and rebuilds. Without it, every restart re-requests certificates and runs into Let's Encrypt rate limits. |
| `healthcheck: /app/gateway -healthcheck` | The image has no shell or curl; the binary checks its own admin `/healthz`. |
| `stop_grace_period: 30s` | Longer than `server.shutdown_timeout` (25s), so in-flight requests drain before Docker kills the process. |
| `mem_limit` 256m / 128m, `cpus: 1.0` | Starting limits for a small app. Re-baseline with the load test ([observability](observability.md#load-testing-baseline)). |
| Valkey with `--requirepass`, no persistence, not published | Rate-limit state is short-lived; only the gateway can reach it. |

**If Valkey goes down,** the gateway keeps serving. Each copy enforces
limits in memory until Valkey is back, then switches back on its own.
Watch `gateway_rate_limit_store_fallback_total`.

## Docker image

[`Dockerfile`](../Dockerfile) is a multi-stage build:

1. `golang:1.26-alpine` compiles a static binary (`CGO_ENABLED=0`,
   `-trimpath -ldflags="-s -w"`).
2. The final image is `gcr.io/distroless/static-debian12:nonroot`: no
   shell, no package manager, CA certificates included, running as
   `nonroot`. It contains the binary, a default config
   (`config.example.yaml`, TLS off) and a writable `/var/lib/jgateway` for
   the ACME cache.

The image ports are `8443` (TLS), `8080` (plaintext, or the only port with
TLS off) and `9090` (admin). Never publish 9090.

## TLS

The gateway terminates TLS itself (`tls.mode: acme` or `files`, see
[configuration](configuration.md#tls)). `tls.mode: off` is only for local
development, or when something you control terminates TLS in front of the
gateway. It logs a warning at startup.

## Kubernetes

[`deploy/k8s/gateway.yaml`](../deploy/k8s/gateway.yaml) has a `Deployment`
and a `Service`:

- **Probes** hit `/healthz` and `/readyz` on the named `admin` port (9090).
  `GATEWAY_ADMIN_ADDR=":9090"` makes the admin listener reachable on the
  pod IP. The Service exposes only the public port.
- **Shutdown:** a `preStop` `sleep` action (built into Kubernetes; the image
  has no `sleep` binary) gives the endpoint controller time to remove the
  pod before draining starts.
  - `terminationGracePeriodSeconds` must exceed the preStop sleep plus
    `shutdown_timeout`.
  - In Kubernetes, terminate TLS at the ingress and set `tls.mode: off`.
- **Secrets** come from `envFrom: secretRef: gateway-secrets`, holding
  whichever env vars your config names.
- **Shared limits:** set the `valkey` block so replicas share one set of
  limits. Without it, each pod limits on its own (3 replicas ≈ 3× the
  configured rate).

```sh
kubectl apply -f deploy/k8s/gateway.yaml
```

## Rolling deploys

Graceful shutdown makes rolling deploys safe. On SIGTERM, every listener
(public, admin, HTTP redirect) stops accepting connections and drains
in-flight requests for up to `shutdown_timeout`, then the process exits.
`/readyz` never fails because an upstream is down (see
[observability](observability.md)), so a dead backend can't pull every
gateway copy out of rotation.
