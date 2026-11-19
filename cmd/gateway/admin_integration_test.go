package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Health, readiness and metrics must not be reachable on the public
// (internet-facing) listener, only on the admin one.
func TestGateway_AdminEndpointsOnlyOnAdminListener(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	public, admin := startGatewayWithAdmin(t, gatewayConfig(backend.URL)) // only route: /api

	if got := getStatus(t, public.URL+"/api/x"); got != http.StatusOK {
		t.Fatalf("public /api/x = %d, want 200", got)
	}
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		if got := getStatus(t, public.URL+p); got != http.StatusNotFound {
			t.Errorf("public %s = %d, want 404", p, got)
		}
	}

	if got := getStatus(t, admin.URL+"/healthz"); got != http.StatusOK {
		t.Errorf("admin /healthz = %d, want 200", got)
	}
	resp, err := http.Get(admin.URL + "/readyz")
	if err != nil {
		t.Fatalf("admin /readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("admin /readyz = %d %q, want 200 JSON", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, err = http.Get(admin.URL + "/metrics")
	if err != nil {
		t.Fatalf("admin /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "gateway_requests_total") {
		t.Error("admin /metrics does not expose gateway_requests_total")
	}
}

// "/api/x/../../admin" must not be routed by its "/api" prefix and then
// resolved by the upstream to a different path: the public listener
// cleans paths first (a 307 redirect to the clean form).
func TestGateway_PublicPathsAreCleaned(t *testing.T) {
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer backend.Close()
	public, _ := startGatewayWithAdmin(t, gatewayConfig(backend.URL))

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, public.URL+"/api/x/../../admin", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "/admin" {
		t.Errorf("got %d Location %q, want 307 to /admin (ServeMux path cleaning keeps the method)", resp.StatusCode, resp.Header.Get("Location"))
	}
	if hits != 0 {
		t.Errorf("upstream hit %d times for an unclean path, want 0", hits)
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ok.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	tests := []struct {
		name, url string
		want      int
	}{
		{"healthy", ok.URL, 0},
		{"unhealthy", failing.URL, 1},
		{"unreachable", "http://127.0.0.1:1/healthz", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := healthcheck(tt.url, defaultHealthcheckTimeout); got != tt.want {
				t.Errorf("healthcheck(%s) = %d, want %d", tt.url, got, tt.want)
			}
		})
	}
}
