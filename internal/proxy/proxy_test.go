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
	rp := NewReverseProxy(up, Options{})

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
	rp := NewReverseProxy(up, Options{})

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for dead upstream, got %d", rec.Code)
	}
}

func TestRouter_SegmentBoundary(t *testing.T) {
	mk := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Handler", name) })
	}
	rt := NewRouter([]Route{
		{PathPrefix: "/api/users", Handler: mk("users")},
		{PathPrefix: "/static/", Handler: mk("static")},
		{PathPrefix: "/", Handler: mk("root")},
	})
	tests := []struct{ path, want string }{
		{"/api/users", "users"},
		{"/api/users/42", "users"},
		{"/api/users-admin", "root"},
		{"/api/usersX", "root"},
		{"/static/x", "static"},
		{"/other", "root"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if got := rec.Header().Get("X-Handler"); got != tt.want {
				t.Errorf("%s routed to %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// seen is what the echo backend observed.
type seen struct {
	requestURI string
	header     http.Header
}

func echoBackend(t *testing.T) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requestURI = r.RequestURI
		s.header = r.Header.Clone()
	}))
	t.Cleanup(backend.Close)
	return backend, s
}

func proxyTo(t *testing.T, target string, opts Options) http.Handler {
	t.Helper()
	up, err := NewUpstream("backend", target, time.Second, time.Second)
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}
	return NewReverseProxy(up, opts)
}

func TestReverseProxy_StripPrefix(t *testing.T) {
	backend, s := echoBackend(t)
	rp := proxyTo(t, backend.URL, Options{StripPrefix: "/api/users"})
	for in, want := range map[string]string{"/api/users/42?x=1": "/42?x=1", "/api/users": "/"} {
		rp.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, in, nil))
		if s.requestURI != want {
			t.Errorf("%s reached upstream as %q, want %q", in, s.requestURI, want)
		}
	}
}

func TestReverseProxy_StripPrefixKeepsEncoding(t *testing.T) {
	backend, s := echoBackend(t)
	rp := proxyTo(t, backend.URL, Options{StripPrefix: "/api/users"})
	rp.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/users/a%2Fb", nil))
	if s.requestURI != "/a%2Fb" {
		t.Errorf("upstream RequestURI = %q, want /a%%2Fb", s.requestURI)
	}
}

func TestReverseProxy_UpstreamBasePath(t *testing.T) {
	backend, s := echoBackend(t)
	rp := proxyTo(t, backend.URL+"/v1", Options{})
	rp.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if s.requestURI != "/v1/api/x" {
		t.Errorf("upstream RequestURI = %q, want /v1/api/x", s.requestURI)
	}
}

// Client-supplied forwarding headers must never reach backends: they'd
// let any client claim an arbitrary IP or host.
func TestReverseProxy_ForwardedHeadersRebuilt(t *testing.T) {
	backend, s := echoBackend(t)
	rp := proxyTo(t, backend.URL, Options{})

	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/x", nil)
	req.RemoteAddr = "1.2.3.4:5"
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Host", "evil")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Forwarded", "for=6.6.6.6")
	rp.ServeHTTP(httptest.NewRecorder(), req)

	checks := map[string]string{
		"X-Forwarded-For":   "1.2.3.4",
		"X-Forwarded-Host":  "api.example.com",
		"X-Forwarded-Proto": "http",
		"Forwarded":         "",
	}
	for h, want := range checks {
		if got := s.header.Get(h); got != want {
			t.Errorf("upstream %s = %q, want %q", h, got, want)
		}
	}
}
