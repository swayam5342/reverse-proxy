package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/swayam5342/jgateway/internal/config"
	"github.com/swayam5342/jgateway/internal/middleware/metrics"
	"github.com/swayam5342/jgateway/internal/proxy"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestIntegration_AuthRejectsBadToken spins up the full chain (logging,
// metrics, auth, proxy) against a fake upstream and asserts a bad/missing
// JWT never reaches it.
func TestIntegration_AuthRejectsBadToken(t *testing.T) {
	var upstreamHits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	up, err := proxy.NewUpstream("backend", backend.URL, time.Second, time.Second)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}

	secret := "integration-secret"
	t.Setenv("JWT_SECRET", secret)

	routeCfg := config.Route{
		PathPrefix: "/api",
		Upstream:   "backend",
		Auth: config.AuthConfig{
			Enabled:      true,
			Mode:         "jwt",
			JWTSecretEnv: "JWT_SECRET",
		},
	}
	handler, err := buildRouteHandler(testLogger(), config.Default().Server, routeCfg, config.Upstream{Name: "backend"}, up)
	if err != nil {
		t.Fatalf("buildRouteHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/thing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing token, got %d", rec.Code)
	}
	if upstreamHits != 0 {
		t.Fatalf("expected upstream never called for unauthenticated request, got %d hits", upstreamHits)
	}

	// A validly signed token should pass through to the upstream.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: "user-1", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))})
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/api/thing", nil)
	req2.Header.Set("Authorization", "Bearer "+signed)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid token, got %d", rec2.Code)
	}
	if upstreamHits != 1 {
		t.Fatalf("expected exactly 1 upstream hit after valid request, got %d", upstreamHits)
	}
}

// TestIntegration_RateLimiterReturns429AfterN exercises the full chain with
// rate limiting enabled and asserts requests are rejected once the burst is
// exhausted.
func TestIntegration_RateLimiterReturns429AfterN(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	up, err := proxy.NewUpstream("backend", backend.URL, time.Second, time.Second)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}

	routeCfg := config.Route{
		PathPrefix: "/api",
		Upstream:   "backend",
		RateLimit: config.RateLimitConfig{
			Enabled:           true,
			RequestsPerSecond: 1,
			Burst:             2,
			KeyStrategy:       "ip",
		},
	}
	handler, err := buildRouteHandler(testLogger(), config.Default().Server, routeCfg, config.Upstream{Name: "backend"}, up)
	if err != nil {
		t.Fatalf("buildRouteHandler: %v", err)
	}

	newReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/thing", nil)
		r.RemoteAddr = "9.9.9.9:1234"
		return r
	}

	var lastCode int
	for i := range 3 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, newReq())
		lastCode = rec.Code
		if i < 2 && rec.Code != http.StatusOK {
			t.Fatalf("request %d within burst expected 200, got %d", i, rec.Code)
		}
	}
	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after burst exhausted, got %d", lastCode)
	}
}

// TestIntegration_CircuitBreakerOpensAfterFailures exercises the full chain
// against an always-failing upstream and asserts the breaker trips to
// reject requests with 503 instead of continuing to hammer the backend.
func TestIntegration_CircuitBreakerOpensAfterFailures(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	up, err := proxy.NewUpstream("backend", backend.URL, time.Second, time.Second)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}

	upCfg := config.Upstream{
		Name: "backend",
		CircuitBreaker: config.CircuitBreakerConfig{
			Enabled:             true,
			FailureThreshold:    2,
			HalfOpenProbeAfter:  time.Hour,
			HalfOpenMaxRequests: 1,
		},
	}
	routeCfg := config.Route{PathPrefix: "/api", Upstream: "backend"}
	handler, err := buildRouteHandler(testLogger(), config.Default().Server, routeCfg, upCfg, up)
	if err != nil {
		t.Fatalf("buildRouteHandler: %v", err)
	}

	for i := range 2 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thing", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("request %d: expected upstream 500 to pass through pre-trip, got %d", i, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thing", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 once breaker trips, got %d", rec.Code)
	}
}

// The rate-limit rejection metric is recorded (it used to be registered
// but never incremented).
func TestIntegration_RateLimitRejectionMetricRecorded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	up, err := proxy.NewUpstream("backend", backend.URL, time.Second, time.Second)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	routeCfg := config.Route{
		PathPrefix: "/metric-test",
		Upstream:   "backend",
		RateLimit:  config.RateLimitConfig{Enabled: true, RequestsPerSecond: 0.001, Burst: 1, KeyStrategy: "ip"},
	}
	handler, err := buildRouteHandler(testLogger(), config.Default().Server, routeCfg, config.Upstream{Name: "backend"}, up)
	if err != nil {
		t.Fatalf("buildRouteHandler: %v", err)
	}
	before := testutil.ToFloat64(metrics.RateLimitRejections.WithLabelValues("/metric-test"))
	for range 3 {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metric-test/x", nil))
	}
	if got := testutil.ToFloat64(metrics.RateLimitRejections.WithLabelValues("/metric-test")) - before; got != 2 {
		t.Errorf("rejections recorded = %v, want 2", got)
	}
}
