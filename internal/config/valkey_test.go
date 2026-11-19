package config

import (
	"strings"
	"testing"
	"time"
)

func TestValidate_Valkey(t *testing.T) {
	t.Setenv("VALKEY_PASSWORD", "pw")
	tests := []struct {
		name    string
		v       ValkeyConfig
		wantErr string
	}{
		{"valid", ValkeyConfig{Addr: "valkey:6379", PasswordEnv: "VALKEY_PASSWORD", Timeout: 50 * time.Millisecond}, ""},
		{"missing addr", ValkeyConfig{Timeout: time.Millisecond}, "valkey.addr"},
		{"addr without port", ValkeyConfig{Addr: "valkey", Timeout: time.Millisecond}, "valkey.addr"},
		{"password env unset", ValkeyConfig{Addr: "valkey:6379", PasswordEnv: "UNSET_VALKEY_PW", Timeout: time.Millisecond}, `"UNSET_VALKEY_PW"`},
		{"username env unset", ValkeyConfig{Addr: "valkey:6379", UsernameEnv: "UNSET_VALKEY_USER", Timeout: time.Millisecond}, `"UNSET_VALKEY_USER"`},
		{"negative timeout", ValkeyConfig{Addr: "valkey:6379", Timeout: -time.Millisecond}, "valkey.timeout"},
		{"negative db", ValkeyConfig{Addr: "valkey:6379", DB: -1, Timeout: time.Millisecond}, "valkey.db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			v := tt.v
			cfg.Valkey = &v
			err := cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_ValkeyTimeoutDefaults(t *testing.T) {
	path := writeConfig(t, `
upstreams:
  - name: backend
    url: "http://localhost:9000"
    dial_timeout: 2s
    request_timeout: 5s
routes:
  - path_prefix: "/api"
    upstream: backend
valkey:
  addr: "valkey:6379"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Valkey == nil || cfg.Valkey.Timeout != 50*time.Millisecond {
		t.Errorf("valkey = %+v, want timeout defaulted to 50ms", cfg.Valkey)
	}
}
