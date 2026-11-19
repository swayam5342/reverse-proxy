package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/swayam5342/jgateway/internal/config"
	"github.com/swayam5342/jgateway/internal/tlsx/tlsxtest"
)

func servedSerial(t *testing.T, r *FileReloader) int64 {
	t.Helper()
	cert, err := r.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf.SerialNumber.Int64()
}

func bumpMtime(t *testing.T, files ...string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	for _, f := range files {
		if err := os.Chtimes(f, future, future); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
}

// Renewed certificates (certbot, cert-manager) are picked up without a
// restart.
func TestFileReloader_ReloadsChangedCert(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := tlsxtest.WriteCert(t, dir, 1)
	r, err := NewFileReloader(certFile, keyFile, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewFileReloader: %v", err)
	}
	if s := servedSerial(t, r); s != 1 {
		t.Fatalf("serial = %d, want 1", s)
	}

	tlsxtest.WriteCert(t, dir, 2)
	bumpMtime(t, certFile, keyFile)
	time.Sleep(20 * time.Millisecond)
	if s := servedSerial(t, r); s != 2 {
		t.Errorf("serial after renewal = %d, want 2", s)
	}
}

// A half-written or broken renewal must not take TLS down.
func TestFileReloader_BadNewFilesKeepOldCert(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := tlsxtest.WriteCert(t, dir, 1)
	var reloadErrs int
	r, err := NewFileReloader(certFile, keyFile, 10*time.Millisecond, func(error) { reloadErrs++ })
	if err != nil {
		t.Fatalf("NewFileReloader: %v", err)
	}

	if err := os.WriteFile(certFile, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	bumpMtime(t, certFile)
	time.Sleep(20 * time.Millisecond)
	if s := servedSerial(t, r); s != 1 {
		t.Errorf("serial = %d, want the old cert (1) kept", s)
	}
	if reloadErrs == 0 {
		t.Error("onErr not called for a failed reload")
	}
}

func TestNewFileReloader_MissingFileErrors(t *testing.T) {
	if _, err := NewFileReloader("/does/not/exist.pem", "/does/not/exist.key", time.Second, nil); err == nil {
		t.Error("expected an error for missing files")
	}
}

func TestRedirectHandler(t *testing.T) {
	tests := []struct {
		name, url, host string
		allowed         []string
		wantStatus      int
		wantLocation    string
	}{
		{"allowed host, port dropped", "http://api.example.com:8080/a?b=1", "api.example.com:8080", []string{"api.example.com"}, http.StatusMovedPermanently, "https://api.example.com/a?b=1"},
		{"host matched case-insensitively", "http://API.example.com/", "API.example.com", []string{"api.example.com"}, http.StatusMovedPermanently, "https://api.example.com/"},
		{"unknown host rejected", "http://evil.example/", "evil.example", []string{"api.example.com"}, http.StatusBadRequest, ""},
		{"missing host rejected", "http://api.example.com/", "", []string{"api.example.com"}, http.StatusBadRequest, ""},
		{"no allow-list redirects any host", "http://other.example/x", "other.example", nil, http.StatusMovedPermanently, "https://other.example/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			RedirectHandler(tt.allowed).ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Location"); got != tt.wantLocation {
				t.Errorf("Location = %q, want %q", got, tt.wantLocation)
			}
		})
	}
}

func TestServerTLS_ACME(t *testing.T) {
	tlsCfg, httpHandler, err := ServerTLS(config.TLSConfig{
		Mode: "acme",
		ACME: config.ACMEConfig{Domains: []string{"api.example.com"}, CacheDir: t.TempDir()},
	}, nil)
	if err != nil {
		t.Fatalf("ServerTLS: %v", err)
	}
	if !slices.Contains(tlsCfg.NextProtos, "acme-tls/1") {
		t.Errorf("NextProtos = %v, want acme-tls/1 for TLS-ALPN-01", tlsCfg.NextProtos)
	}
	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", tlsCfg.MinVersion)
	}
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/x", nil)
	rec := httptest.NewRecorder()
	httpHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("HTTP handler status = %d, want 301 redirect", rec.Code)
	}
}

func TestServerTLS_Files(t *testing.T) {
	certFile, keyFile, _ := tlsxtest.WriteCert(t, t.TempDir(), 7)
	tlsCfg, _, err := ServerTLS(config.TLSConfig{
		Mode:  "files",
		Files: config.TLSFilesConfig{CertFile: certFile, KeyFile: keyFile},
	}, nil)
	if err != nil {
		t.Fatalf("ServerTLS: %v", err)
	}
	if tlsCfg.GetCertificate == nil || tlsCfg.MinVersion != tls.VersionTLS12 || !slices.Contains(tlsCfg.NextProtos, "h2") {
		t.Errorf("files TLS config incomplete: GetCertificate set=%v MinVersion=%x NextProtos=%v",
			tlsCfg.GetCertificate != nil, tlsCfg.MinVersion, tlsCfg.NextProtos)
	}
}
