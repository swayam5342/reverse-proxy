package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/swayam5342/jgateway/internal/config"
)

func rateLimitedConfig(upstreamURL string, valkey *config.ValkeyConfig) *config.Config {
	cfg := gatewayConfig(upstreamURL)
	// A unique route keeps runs from sharing buckets in a real Valkey.
	cfg.Routes[0].PathPrefix = fmt.Sprintf("/rl-%d", rand.Int64())
	cfg.Routes[0].RateLimit = config.RateLimitConfig{Enabled: true, RequestsPerSecond: 0.001, Burst: 2, KeyStrategy: "ip"}
	cfg.Valkey = valkey
	return cfg
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Valkey down must degrade to per-copy limits, not an outage, and be
// visible in /readyz.
func TestGateway_ValkeyDownFallsBackToMemory(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	cfg := rateLimitedConfig(backend.URL, &config.ValkeyConfig{Addr: "127.0.0.1:1", Timeout: 50 * time.Millisecond})
	gw := startGateway(t, cfg)

	url := gw.URL + cfg.Routes[0].PathPrefix + "/x"
	for i, want := range []int{200, 200, 429} {
		if got := getStatus(t, url); got != want {
			t.Errorf("request %d = %d, want %d", i, got, want)
		}
	}

	resp, err := http.Get(gw.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Checks map[string]string `json:"checks"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if s := body.Checks["valkey"]; s == "" || s == "ok" {
		t.Errorf("checks[valkey] = %q, want the connection error", s)
	}
}

func TestGateway_TwoGatewaysShareValkeyLimit(t *testing.T) {
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		t.Skip("VALKEY_ADDR not set; skipping test that needs a real Valkey")
	}
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	cfg := rateLimitedConfig(backend.URL, &config.ValkeyConfig{Addr: addr, Timeout: 200 * time.Millisecond})
	cfg.Routes[0].RateLimit.Burst = 1
	a, b := startGateway(t, cfg), startGateway(t, cfg)

	path := cfg.Routes[0].PathPrefix + "/x"
	if got := getStatus(t, a.URL+path); got != 200 {
		t.Fatalf("gateway A = %d, want 200", got)
	}
	if got := getStatus(t, b.URL+path); got != http.StatusTooManyRequests {
		t.Errorf("gateway B = %d, want 429 (limit shared through Valkey)", got)
	}
}
