# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
    # Writable state directory for the ACME certificate cache (distroless
    # has no shell, so it's created here and copied in with ownership).
    && mkdir -p /out/var/lib/jgateway/acme

FROM gcr.io/distroless/static-debian12:nonroot AS run
WORKDIR /app
COPY --from=build /out/gateway /app/gateway
COPY --from=build --chown=nonroot:nonroot /out/var/lib/jgateway /var/lib/jgateway
COPY config.example.yaml /app/config.yaml
USER nonroot:nonroot
# 8443 TLS / 8080 plaintext (or the only port with TLS off) / 9090 admin.
# Publish 8443 and 8080 only; never publish 9090.
EXPOSE 8443 8080 9090
ENTRYPOINT ["/app/gateway"]
