package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_Valid(t *testing.T) {
	path := writeConfig(t, `
server:
  addr: ":9090"
  read_timeout: 5s
  write_timeout: 10s
  shutdown_timeout: 15s
upstreams:
  - name: backend
    url: "http://localhost:9000"
    dial_timeout: 2s
    request_timeout: 5s
routes:
  - path_prefix: "/api"
    upstream: backend
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error for valid config: %v", err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("Addr = %q, want :9090", cfg.Server.Addr)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "no upstreams",
			cfg:     Config{Server: Default().Server},
			wantErr: "at least one upstream",
		},
		{
			name: "bad upstream url",
			cfg: Config{
				Server:    Default().Server,
				Upstreams: []Upstream{{Name: "a", URL: "not-a-url", DialTimeout: 1, RequestTimeout: 1}},
				Routes:    []Route{{PathPrefix: "/x", Upstream: "a"}},
			},
			wantErr: "not a valid absolute URL",
		},
		{
			name: "route references unknown upstream",
			cfg: Config{
				Server:    Default().Server,
				Upstreams: []Upstream{{Name: "a", URL: "http://x", DialTimeout: 1, RequestTimeout: 1}},
				Routes:    []Route{{PathPrefix: "/x", Upstream: "missing"}},
			},
			wantErr: "does not match any configured upstream",
		},
		{
			name: "duplicate upstream names",
			cfg: Config{
				Server: Default().Server,
				Upstreams: []Upstream{
					{Name: "a", URL: "http://x", DialTimeout: 1, RequestTimeout: 1},
					{Name: "a", URL: "http://y", DialTimeout: 1, RequestTimeout: 1},
				},
				Routes: []Route{{PathPrefix: "/x", Upstream: "a"}},
			},
			wantErr: "is duplicated",
		},
		{
			name: "rate limit missing key strategy",
			cfg: Config{
				Server:    Default().Server,
				Upstreams: []Upstream{{Name: "a", URL: "http://x", DialTimeout: 1, RequestTimeout: 1}},
				Routes: []Route{{
					PathPrefix: "/x",
					Upstream:   "a",
					RateLimit:  RateLimitConfig{Enabled: true, RequestsPerSecond: 1, Burst: 1},
				}},
			},
			wantErr: "key_strategy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidate_ValidPasses(t *testing.T) {
	cfg := Config{
		Server:    Default().Server,
		Upstreams: []Upstream{{Name: "a", URL: "http://x", DialTimeout: 1, RequestTimeout: 1}},
		Routes:    []Route{{PathPrefix: "/x", Upstream: "a"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

const strictBase = `
server:
  addr: ":9090"%s
upstreams:
  - name: backend
    url: "http://localhost:9000"
    dial_timeout: 2s
    request_timeout: 5s%s
routes:
  - path_prefix: "/api"
    upstream: backend
`

// Typos and removed keys must stop startup instead of being silently
// ignored (yaml.v3 ignores unknown keys by default).
func TestLoad_UnknownFieldFails(t *testing.T) {
	tests := []struct {
		name, server, upstream, wantErr string
	}{
		{"removed retry field", "", "\n    retry: {enabled: false, idempotent_only: true}", "idempotent_only"},
		{"typo in server key", "\n  requst_timeout: 1s", "", "requst_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, fmt.Sprintf(strictBase, tt.server, tt.upstream))
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_EmptyFileFailsValidation(t *testing.T) {
	_, err := Load(writeConfig(t, ""))
	if err == nil || !strings.Contains(err.Error(), "at least one upstream") {
		t.Fatalf("error = %v, want a validation error mentioning \"at least one upstream\"", err)
	}
}
