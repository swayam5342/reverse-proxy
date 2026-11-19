package main

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/swayam5342/jgateway/internal/config"
	"github.com/swayam5342/jgateway/internal/tlsx/tlsxtest"
)

// Files mode end to end: HTTPS through the real public server with the
// configured certificate, and the plaintext listener redirecting to HTTPS.
func TestGateway_TLSFilesMode(t *testing.T) {
	var gotProto string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
	}))
	defer backend.Close()

	certFile, keyFile, certPEM := tlsxtest.WriteCert(t, t.TempDir(), 1)
	cfg := gatewayConfig(backend.URL)
	cfg.TLS = config.TLSConfig{
		Mode:     config.TLSModeFiles,
		HTTPAddr: "127.0.0.1:0",
		Files:    config.TLSFilesConfig{CertFile: certFile, KeyFile: keyFile},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	s, err := newServers(cfg, testLogger())
	if err != nil {
		t.Fatalf("newServers: %v", err)
	}
	t.Cleanup(s.Close)
	if !s.TLS || s.HTTP == nil {
		t.Fatalf("TLS=%v HTTP=%v, want TLS on with a plaintext listener", s.TLS, s.HTTP)
	}

	tlsLn := listen(t)
	go func() { _ = s.Public.ServeTLS(tlsLn, "", "") }()
	httpLn := listen(t)
	go func() { _ = s.HTTP.Serve(httpLn) }()
	t.Cleanup(func() { _ = s.Public.Close(); _ = s.HTTP.Close() })

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get("https://localhost:" + port(tlsLn) + "/api/x")
	if err != nil {
		t.Fatalf("HTTPS GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || gotProto != "https" {
		t.Errorf("HTTPS = %d, upstream X-Forwarded-Proto = %q; want 200 and https", resp.StatusCode, gotProto)
	}

	resp, err = client.Get("http://localhost:" + port(httpLn) + "/api/x?q=1")
	if err != nil {
		t.Fatalf("HTTP GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "https://localhost/api/x?q=1" {
		t.Errorf("HTTP = %d Location %q, want 301 to https://localhost/api/x?q=1", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func port(ln net.Listener) string {
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return p
}
