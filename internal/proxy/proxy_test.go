package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRouter_LongestPrefixWins(t *testing.T) {
	mkHandler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Handler", name)
		})
	}

	rt := NewRouter([]Route{
		{PathPrefix: "/api", Handler: mkHandler("api")},
		{PathPrefix: "/api/v2", Handler: mkHandler("api-v2")},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v2/thing", nil)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Handler"); got != "api-v2" {
		t.Errorf("expected longest-prefix route api-v2, got %q", got)
	}
}

func TestRouter_NoMatch404(t *testing.T) {
	rt := NewRouter([]Route{{PathPrefix: "/api", Handler: http.NotFoundHandler()}})
	req := httptest.NewRequest(http.MethodGet, "/other", nil)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestReverseProxy_ForwardsRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	up, err := NewUpstream("backend", backend.URL, time.Second, time.Second)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	rp := NewReverseProxy(up)

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-Got-Path"); got != "/hello" {
		t.Errorf("expected backend to receive /hello, got %q", got)
	}
}

func TestReverseProxy_UpstreamDown_502(t *testing.T) {
	up, err := NewUpstream("backend", "http://127.0.0.1:1", time.Millisecond*100, time.Millisecond*100)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	rp := NewReverseProxy(up)

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for dead upstream, got %d", rec.Code)
	}
}
