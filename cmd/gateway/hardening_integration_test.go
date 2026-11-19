package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/swayam5342/jgateway/internal/config"
	"github.com/swayam5342/jgateway/internal/middleware/metrics"
)

// gatewayConfig is a small, valid config with short timeouts so tests can
// cross them quickly: read 300ms, write 600ms, overall request deadline
// 400ms, upstream response-header timeout 300ms, 1 KiB body limit.
func gatewayConfig(upstreamURL string) *config.Config {
	cfg := config.Default()
	cfg.Server.ReadTimeout = 300 * time.Millisecond
	cfg.Server.ReadHeaderTimeout = 300 * time.Millisecond
	cfg.Server.WriteTimeout = 600 * time.Millisecond
	cfg.Server.RequestTimeout = 400 * time.Millisecond
	cfg.Server.UpgradeTimeout = 10 * time.Second
	cfg.Server.MaxBodyBytes = 1024
	cfg.Upstreams = []config.Upstream{{
		Name:           "backend",
		URL:            upstreamURL,
		DialTimeout:    200 * time.Millisecond,
		RequestTimeout: 300 * time.Millisecond,
	}}
	cfg.Routes = []config.Route{{PathPrefix: "/api", Upstream: "backend"}}
	return cfg
}

func withRetryAndBreaker(cfg *config.Config) *config.Config {
	cfg.Upstreams[0].Retry = config.RetryConfig{Enabled: true, MaxAttempts: 5, BaseBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}
	cfg.Upstreams[0].CircuitBreaker = config.CircuitBreakerConfig{Enabled: true, FailureThreshold: 100, HalfOpenProbeAfter: time.Second}
	return cfg
}

// startGateway runs the real *http.Server built by newServer (with its
// read/write timeouts) on a local listener.
func startGateway(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	srv, err := newServer(cfg, testLogger())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewUnstartedServer(srv.Handler)
	ts.Config = srv
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// Fix 1: one dead upstream must not make the gateway unready.
func TestGateway_ReadyWhileUpstreamDown(t *testing.T) {
	gw := startGateway(t, gatewayConfig("http://127.0.0.1:1")) // nothing listens on port 1

	resp, err := http.Get(gw.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200 with an upstream down", resp.StatusCode)
	}
	var body struct {
		Checks map[string]string `json:"checks"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if s := body.Checks["backend"]; s == "" || s == "ok" {
		t.Errorf("checks[backend] = %q, want the dial error reported", s)
	}
}

// Fix 2: oversized bodies are rejected before they're buffered or proxied.
func TestGateway_OversizedBodyRejected(t *testing.T) {
	var hits int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer backend.Close()

	t.Setenv("SIGN_SECRET", "s")
	cfg := withRetryAndBreaker(gatewayConfig(backend.URL))
	cfg.Routes[0].Signing = config.SigningConfig{Enabled: true, SecretEnv: "SIGN_SECRET", HeaderName: "X-Signature"}
	gw := startGateway(t, cfg)

	resp, err := http.Post(gw.URL+"/api/upload", "application/octet-stream", bytes.NewReader(make([]byte, 4096)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if hits != 0 {
		t.Errorf("upstream hit %d times, want 0", hits)
	}
}

// Fix 7: retries against a hanging upstream used to run past the server's
// write timeout (5 attempts x 300ms > 600ms), so the client got a reset
// connection. The overall request deadline turns that into a clean 504.
func TestGateway_RequestDeadlineReturns504BeforeWriteTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer backend.Close()

	gw := startGateway(t, withRetryAndBreaker(gatewayConfig(backend.URL)))

	start := time.Now()
	resp, err := http.Get(gw.URL + "/api/slow")
	if err != nil {
		t.Fatalf("GET: %v (connection dropped instead of a 504)", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed >= 600*time.Millisecond {
		t.Errorf("took %v, want under the 600ms write timeout", elapsed)
	}
}

// Fix 6: WebSocket upgrades work through the full chain (logging, metrics,
// retry, circuit breaker) and survive past the server's read/write and
// request timeouts.
func TestGateway_WebSocketUpgradeThroughFullChain(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		for {
			line, err := brw.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = brw.WriteString(line)
			_ = brw.Flush()
		}
	}))
	defer backend.Close()

	gw := startGateway(t, withRetryAndBreaker(gatewayConfig(backend.URL)))

	conn, err := net.Dial("tcp", gw.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	_, _ = io.WriteString(conn, "GET /api/ws HTTP/1.1\r\nHost: gw\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", resp.StatusCode)
	}

	// Idle past read (300ms), request (400ms) and write (600ms) timeouts.
	time.Sleep(800 * time.Millisecond)

	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("write after idle: %v", err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo after idle: %v (socket was cut by a server timeout)", err)
	}
	if line != "ping\n" {
		t.Errorf("echo = %q, want ping", line)
	}
}

// Fix 2/6: streamed responses reach the client as they're produced, even
// on routes with retries enabled.
func TestGateway_StreamingResponseNotBuffered(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer backend.Close()
	defer close(release)

	gw := startGateway(t, withRetryAndBreaker(gatewayConfig(backend.URL)))

	resp, err := http.Get(gw.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if line != "data: first\n" {
			t.Errorf("first line = %q", line)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("first event not delivered while upstream still streaming (response is being buffered)")
	}
}

// Fix 4: behind a trusted proxy, clients are keyed by the address the proxy
// appended, so prepending fake X-Forwarded-For entries doesn't buy a fresh
// bucket, while distinct real clients still get their own.
func TestGateway_RateLimitByForwardedClientBehindTrustedProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()

	cfg := gatewayConfig(backend.URL)
	cfg.Server.TrustedProxies = []string{"127.0.0.1", "::1"} // the test client is the "LB"
	cfg.Routes[0].RateLimit = config.RateLimitConfig{Enabled: true, RequestsPerSecond: 0.001, Burst: 1, KeyStrategy: "ip"}
	gw := startGateway(t, cfg)

	get := func(xff string) int {
		req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/x", nil)
		req.Header.Set("X-Forwarded-For", xff)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if s := get("6.6.6.6, 1.1.1.1"); s != http.StatusOK {
		t.Fatalf("client 1.1.1.1 first request = %d, want 200", s)
	}
	if s := get("7.7.7.7, 1.1.1.1"); s != http.StatusTooManyRequests {
		t.Errorf("client 1.1.1.1 with a different spoofed prefix = %d, want 429", s)
	}
	if s := get("2.2.2.2"); s != http.StatusOK {
		t.Errorf("distinct client 2.2.2.2 = %d, want 200 (own bucket)", s)
	}
}

// Fix 5: issuer/audience from config are enforced end to end.
func TestGateway_JWTIssuerAndAudienceEnforced(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()

	t.Setenv("JWT_SECRET", "integration-secret")
	cfg := gatewayConfig(backend.URL)
	cfg.Routes[0].Auth = config.AuthConfig{
		Enabled: true, Mode: "jwt", JWTSecretEnv: "JWT_SECRET",
		Issuer: "https://issuer.example", Audience: "my-api",
	}
	gw := startGateway(t, cfg)

	call := func(iss, aud string) int {
		tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": "u1", "iss": iss, "aud": aud, "exp": time.Now().Add(time.Hour).Unix(),
		}).SignedString([]byte("integration-secret"))
		req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if s := call("https://issuer.example", "my-api"); s != http.StatusOK {
		t.Errorf("matching iss/aud = %d, want 200", s)
	}
	if s := call("https://evil.example", "my-api"); s != http.StatusUnauthorized {
		t.Errorf("wrong issuer = %d, want 401", s)
	}
	if s := call("https://issuer.example", "other"); s != http.StatusUnauthorized {
		t.Errorf("wrong audience = %d, want 401", s)
	}
}

// strip_prefix and gateway-owned forwarded headers, end to end.
func TestGateway_StripPrefixAndForwardedHeaders(t *testing.T) {
	var gotURI, gotXFF, gotXFH string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI, gotXFF, gotXFH = r.RequestURI, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Host")
	}))
	defer backend.Close()

	cfg := gatewayConfig(backend.URL)
	cfg.Routes[0].StripPrefix = true
	gw := startGateway(t, cfg)

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/items/7", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	if gotURI != "/items/7" {
		t.Errorf("upstream RequestURI = %q, want /items/7", gotURI)
	}
	if gotXFF == "6.6.6.6" || !strings.HasPrefix(gotXFF, "127.0.0.1") && gotXFF != "::1" {
		t.Errorf("upstream X-Forwarded-For = %q, want the real peer, not the spoofed value", gotXFF)
	}
	if gotXFH != strings.TrimPrefix(gw.URL, "http://") {
		t.Errorf("upstream X-Forwarded-Host = %q, want %q", gotXFH, strings.TrimPrefix(gw.URL, "http://"))
	}
}

// A client must not be able to impersonate a user by sending the identity
// headers backends trust, even on routes without auth.
func TestGateway_ClientCannotForgeIdentity(t *testing.T) {
	var gotSubject string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSubject = r.Header.Get("X-Auth-Subject")
	}))
	defer backend.Close()
	gw := startGateway(t, gatewayConfig(backend.URL))

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/x", nil)
	req.Header.Set("X-Auth-Subject", "admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if gotSubject != "" {
		t.Errorf("upstream saw forged X-Auth-Subject %q", gotSubject)
	}
}

// One dead backend trips one breaker, whichever route reached it, and the
// breaker-state metric reflects it.
func TestGateway_BreakerSharedAcrossRoutes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	cfg := gatewayConfig(backend.URL)
	cfg.Upstreams[0].Name = "shared-breaker-backend"
	cfg.Upstreams[0].CircuitBreaker = config.CircuitBreakerConfig{Enabled: true, FailureThreshold: 2, HalfOpenProbeAfter: time.Hour}
	cfg.Routes = []config.Route{
		{PathPrefix: "/a", Upstream: "shared-breaker-backend"},
		{PathPrefix: "/b", Upstream: "shared-breaker-backend"},
	}
	gw := startGateway(t, cfg)

	for range 2 {
		if got := getStatus(t, gw.URL+"/a/x"); got != http.StatusInternalServerError {
			t.Fatalf("/a = %d, want 500 before the breaker trips", got)
		}
	}
	if got := getStatus(t, gw.URL+"/b/x"); got != http.StatusServiceUnavailable {
		t.Errorf("/b = %d, want 503: the breaker opened by /a must protect /b too", got)
	}
	if v := testutil.ToFloat64(metrics.CircuitBreakerState.WithLabelValues("shared-breaker-backend")); v != 2 {
		t.Errorf("gateway_circuit_breaker_state = %v, want 2 (open)", v)
	}
}

// Browsers send preflights without credentials; a JWT route must answer
// them, not reject them with 401.
func TestGateway_CORSPreflightBeforeAuth(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	t.Setenv("JWT_SECRET", "s")
	cfg := gatewayConfig(backend.URL)
	cfg.Routes[0].Auth = config.AuthConfig{Enabled: true, Mode: "jwt", JWTSecretEnv: "JWT_SECRET"}
	cfg.Routes[0].CORS = config.CORSConfig{Enabled: true, AllowedOrigins: []string{"https://app.example.com"}}
	gw := startGateway(t, cfg)

	req, _ := http.NewRequest(http.MethodOptions, gw.URL+"/api/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Errorf("preflight = %d ACAO=%q, want 204 with the origin echoed", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}
