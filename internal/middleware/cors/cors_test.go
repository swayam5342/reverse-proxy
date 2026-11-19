package cors

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const app = "https://app.example.com"

func baseCfg() Config {
	return Config{
		AllowedOrigins: []string{app},
		AllowedMethods: []string{"GET", "POST"},
		AllowedHeaders: []string{"Authorization", "Content-Type"},
		ExposeHeaders:  []string{"X-Request-Id"},
		MaxAge:         10 * time.Minute,
	}
}

func TestCORS(t *testing.T) {
	tests := []struct {
		name       string
		cfg        func(*Config)
		method     string
		origin     string
		reqMethod  string // Access-Control-Request-Method; non-empty = preflight
		upstream   http.Header
		wantStatus int
		wantNext   bool
		wantHdr    map[string]string // "" = must be absent
	}{
		{
			name: "preflight allowed", method: http.MethodOptions, origin: app, reqMethod: "POST",
			wantStatus: http.StatusNoContent, wantNext: false,
			wantHdr: map[string]string{
				"Access-Control-Allow-Origin":  app,
				"Access-Control-Allow-Methods": "GET, POST",
				"Access-Control-Allow-Headers": "Authorization, Content-Type",
				"Access-Control-Max-Age":       "600",
				"Vary":                         "Origin",
			},
		},
		{name: "preflight disallowed origin", method: http.MethodOptions, origin: "https://evil.example", reqMethod: "POST", wantStatus: http.StatusForbidden, wantNext: false},
		{name: "preflight disallowed method", method: http.MethodOptions, origin: app, reqMethod: "TRACE", wantStatus: http.StatusForbidden, wantNext: false},
		{
			name: "actual allowed", method: http.MethodGet, origin: app, wantStatus: http.StatusOK, wantNext: true,
			wantHdr: map[string]string{
				"Access-Control-Allow-Origin":   app,
				"Access-Control-Expose-Headers": "X-Request-Id",
				"Vary":                          "Origin",
			},
		},
		{
			name: "actual disallowed origin passes without CORS headers", method: http.MethodGet, origin: "https://evil.example",
			wantStatus: http.StatusOK, wantNext: true,
			wantHdr: map[string]string{"Access-Control-Allow-Origin": ""},
		},
		{
			name: "wildcard without credentials", cfg: func(c *Config) { c.AllowedOrigins = []string{"*"} },
			method: http.MethodGet, origin: "https://anyone.example", wantStatus: http.StatusOK, wantNext: true,
			wantHdr: map[string]string{"Access-Control-Allow-Origin": "*", "Access-Control-Allow-Credentials": ""},
		},
		{
			name: "credentials echo exact origin", cfg: func(c *Config) { c.AllowCredentials = true },
			method: http.MethodGet, origin: app, wantStatus: http.StatusOK, wantNext: true,
			wantHdr: map[string]string{"Access-Control-Allow-Origin": app, "Access-Control-Allow-Credentials": "true"},
		},
		{
			name: "upstream CORS headers replaced by the gateway", method: http.MethodGet, origin: app,
			upstream:   http.Header{"Access-Control-Allow-Origin": {"*"}, "Access-Control-Allow-Credentials": {"true"}},
			wantStatus: http.StatusOK, wantNext: true,
			wantHdr: map[string]string{"Access-Control-Allow-Origin": app, "Access-Control-Allow-Credentials": ""},
		},
		{name: "OPTIONS without request-method is not a preflight", method: http.MethodOptions, origin: app, wantStatus: http.StatusOK, wantNext: true},
		{name: "no Origin is not a CORS request", method: http.MethodGet, wantStatus: http.StatusOK, wantNext: true, wantHdr: map[string]string{"Access-Control-Allow-Origin": "", "Vary": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseCfg()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			nextCalled := false
			h := New(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				maps.Copy(w.Header(), tt.upstream)
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(tt.method, "/", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.reqMethod != "" {
				req.Header.Set("Access-Control-Request-Method", tt.reqMethod)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if nextCalled != tt.wantNext {
				t.Errorf("next called = %v, want %v", nextCalled, tt.wantNext)
			}
			for k, want := range tt.wantHdr {
				if got := rec.Header().Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestCORS_StreamingPassThrough(t *testing.T) {
	under := httptest.NewRecorder()
	h := New(baseCfg())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("chunk"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through CORS writer: %v", err)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", app)
	h.ServeHTTP(under, req)
	if !under.Flushed || under.Header().Get("Access-Control-Allow-Origin") != app {
		t.Errorf("flushed=%v ACAO=%q; want flushed with CORS headers", under.Flushed, under.Header().Get("Access-Control-Allow-Origin"))
	}
}
