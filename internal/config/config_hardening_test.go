package config

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func baseConfig() Config {
	return Config{
		Server:    Default().Server,
		Upstreams: []Upstream{{Name: "a", URL: "http://x", DialTimeout: time.Second, RequestTimeout: time.Second}},
		Routes:    []Route{{PathPrefix: "/x", Upstream: "a"}},
	}
}

func TestDefault_HardeningDefaults(t *testing.T) {
	s := Default().Server
	if s.RequestTimeout <= 0 || s.RequestTimeout >= s.WriteTimeout {
		t.Errorf("default request_timeout %v must be > 0 and < write_timeout %v", s.RequestTimeout, s.WriteTimeout)
	}
	if s.UpgradeTimeout <= 0 {
		t.Errorf("default upgrade_timeout %v must be > 0", s.UpgradeTimeout)
	}
	if s.MaxBodyBytes != 10<<20 {
		t.Errorf("default max_body_bytes = %d, want 10MiB", s.MaxBodyBytes)
	}
}

func TestValidate_Hardening(t *testing.T) {
	t.Setenv("JWT_SECRET", "s")
	t.Setenv("JWT_PUBLIC_KEY", "pem")

	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // "" = valid
	}{
		{"defaults valid", func(*Config) {}, ""},
		{"path_prefix must start with a slash", func(c *Config) { c.Routes[0].PathPrefix = "api" }, "routes[0].path_prefix must start with \"/\""},
		{"request_timeout must be set", func(c *Config) { c.Server.RequestTimeout = 0 }, "server.request_timeout must be > 0"},
		{
			"request_timeout must leave room to write the 504",
			func(c *Config) { c.Server.RequestTimeout = c.Server.WriteTimeout },
			"server.request_timeout",
		},
		{
			"upstream timeout longer than overall deadline never applies",
			func(c *Config) { c.Upstreams[0].RequestTimeout = c.Server.RequestTimeout + time.Second },
			"upstreams[0].request_timeout",
		},
		{"upgrade_timeout must be set", func(c *Config) { c.Server.UpgradeTimeout = 0 }, "server.upgrade_timeout"},
		{"negative body limit", func(c *Config) { c.Server.MaxBodyBytes = -1 }, "server.max_body_bytes"},
		{"bad trusted proxy", func(c *Config) { c.Server.TrustedProxies = []string{"10.0.0.0/33"} }, "server.trusted_proxies"},
		{"trusted proxies valid", func(c *Config) { c.Server.TrustedProxies = []string{"10.0.0.0/8", "192.168.1.5", "fd00::/8"} }, ""},
		{
			"jwt with public key env",
			func(c *Config) {
				c.Routes[0].Auth = AuthConfig{Enabled: true, Mode: "jwt", JWTPublicKeyEnv: "JWT_PUBLIC_KEY"}
			},
			"",
		},
		{
			"jwt needs a key",
			func(c *Config) { c.Routes[0].Auth = AuthConfig{Enabled: true, Mode: "jwt"} },
			"exactly one of jwt_secret_env or jwt_public_key_env",
		},
		{
			"jwt key sources are exclusive",
			func(c *Config) {
				c.Routes[0].Auth = AuthConfig{Enabled: true, Mode: "jwt", JWTSecretEnv: "JWT_SECRET", JWTPublicKeyEnv: "JWT_PUBLIC_KEY"}
			},
			"exactly one of jwt_secret_env or jwt_public_key_env",
		},
		{
			"jwt public key env must be set",
			func(c *Config) {
				c.Routes[0].Auth = AuthConfig{Enabled: true, Mode: "jwt", JWTPublicKeyEnv: "UNSET_PUBLIC_KEY"}
			},
			`"UNSET_PUBLIC_KEY"`,
		},
		{
			"negative leeway",
			func(c *Config) {
				c.Routes[0].Auth = AuthConfig{Enabled: true, Mode: "jwt", JWTSecretEnv: "JWT_SECRET", Leeway: -time.Second}
			},
			"auth.leeway",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies([]string{"10.0.0.0/8", "192.168.1.5", "::1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.1.5/32"),
		netip.MustParsePrefix("::1/128"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// The shipped example must always load, so it can't drift out of date.
func TestLoad_ExampleConfig(t *testing.T) {
	t.Setenv("JWT_SECRET", "x")
	t.Setenv("ORDERS_API_KEYS", "k")
	t.Setenv("ORDERS_SIGNING_SECRET", "x")
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}
	if cfg.Server.RequestTimeout != 9*time.Second || cfg.Server.MaxBodyBytes != 10<<20 {
		t.Errorf("unexpected server config: %+v", cfg.Server)
	}
}

func TestValidate_AdminAddr(t *testing.T) {
	cfg := baseConfig()
	cfg.Server.AdminAddr = cfg.Server.Addr
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "server.admin_addr") {
		t.Errorf("error = %v, want one about server.admin_addr equal to server.addr", err)
	}
	cfg.Server.AdminAddr = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "server.admin_addr") {
		t.Errorf("error = %v, want one about empty server.admin_addr", err)
	}
}

// Containers need the admin listener reachable by probes/Prometheus
// without editing the baked-in config.
func TestLoad_AdminAddrEnvOverride(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_ADDR", ":9191")
	cfg, err := Load(writeConfig(t, `
upstreams:
  - name: backend
    url: "http://localhost:9000"
    dial_timeout: 2s
    request_timeout: 5s
routes:
  - path_prefix: "/api"
    upstream: backend
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.AdminAddr != ":9191" {
		t.Errorf("admin_addr = %q, want :9191 from GATEWAY_ADMIN_ADDR", cfg.Server.AdminAddr)
	}
	if Default().Server.AdminAddr != "127.0.0.1:9090" {
		t.Errorf("default admin_addr = %q, want 127.0.0.1:9090", Default().Server.AdminAddr)
	}
}
