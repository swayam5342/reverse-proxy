package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swayam5342/jgateway/internal/tlsx/tlsxtest"
)

func TestValidate_TLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := tlsxtest.WriteCert(t, dir, 1)
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	acme := ACMEConfig{Domains: []string{"api.example.com"}, CacheDir: dir}

	tests := []struct {
		name    string
		tls     TLSConfig
		wantErr string
	}{
		{"off", TLSConfig{Mode: "off"}, ""},
		{"acme valid", TLSConfig{Mode: "acme", HTTPAddr: ":80", ACME: acme}, ""},
		{"files valid", TLSConfig{Mode: "files", HTTPAddr: ":80", Files: TLSFilesConfig{CertFile: certFile, KeyFile: keyFile}}, ""},
		{"unknown mode", TLSConfig{Mode: "auto"}, "tls.mode"},
		{"acme without domains", TLSConfig{Mode: "acme", HTTPAddr: ":80", ACME: ACMEConfig{CacheDir: dir}}, "tls.acme.domains"},
		{"acme domain with scheme", TLSConfig{Mode: "acme", HTTPAddr: ":80", ACME: ACMEConfig{Domains: []string{"https://api.example.com"}, CacheDir: dir}}, "tls.acme.domains"},
		{"acme wildcard domain", TLSConfig{Mode: "acme", HTTPAddr: ":80", ACME: ACMEConfig{Domains: []string{"*.example.com"}, CacheDir: dir}}, "tls.acme.domains"},
		{"acme without cache_dir", TLSConfig{Mode: "acme", HTTPAddr: ":80", ACME: ACMEConfig{Domains: []string{"api.example.com"}}}, "tls.acme.cache_dir"},
		{"files missing", TLSConfig{Mode: "files", HTTPAddr: ":80", Files: TLSFilesConfig{CertFile: filepath.Join(dir, "nope.pem"), KeyFile: keyFile}}, "tls.files"},
		{"files unparseable", TLSConfig{Mode: "files", HTTPAddr: ":80", Files: TLSFilesConfig{CertFile: garbage, KeyFile: keyFile}}, "tls.files"},
		{"http_addr required", TLSConfig{Mode: "acme", ACME: acme}, "tls.http_addr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.TLS = tt.tls
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

func TestLoad_AddrDefaultsFollowTLSMode(t *testing.T) {
	dir := t.TempDir()
	base := `
upstreams:
  - name: backend
    url: "http://localhost:9000"
    dial_timeout: 2s
    request_timeout: 5s
routes:
  - path_prefix: "/api"
    upstream: backend
`
	plain, err := Load(writeConfig(t, base))
	if err != nil {
		t.Fatalf("Load plain: %v", err)
	}
	if plain.Server.Addr != ":8080" || plain.TLS.Mode != "off" {
		t.Errorf("plain: addr=%q mode=%q, want :8080/off", plain.Server.Addr, plain.TLS.Mode)
	}

	withTLS, err := Load(writeConfig(t, base+`
tls:
  mode: acme
  acme:
    domains: ["api.example.com"]
    cache_dir: "`+filepath.ToSlash(dir)+`"
`))
	if err != nil {
		t.Fatalf("Load tls: %v", err)
	}
	if withTLS.Server.Addr != ":443" || withTLS.TLS.HTTPAddr != ":80" {
		t.Errorf("tls: addr=%q http_addr=%q, want :443 and :80", withTLS.Server.Addr, withTLS.TLS.HTTPAddr)
	}
}
