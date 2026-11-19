package tlsx

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/swayam5342/jgateway/internal/config"
)

// fileCheckInterval is how often (at most) FileReloader looks for renewed
// certificate files.
const fileCheckInterval = 30 * time.Second

// ServerTLS returns the TLS config for the public listener and the handler
// for the plaintext listener, for a TLS-enabled cfg.
//
//   - acme: certificates come from Let's Encrypt (or DirectoryURL) via
//     autocert, restricted to cfg.ACME.Domains and cached in CacheDir.
//     Both TLS-ALPN-01 (on the TLS listener) and HTTP-01 (on the plaintext
//     listener) challenges are answered; other plaintext requests redirect
//     to HTTPS.
//   - files: the pair at cfg.Files is served and reloaded when renewed;
//     onReloadErr (optional) reports failed reloads.
//
// Both use TLS 1.2 minimum and offer HTTP/2.
func ServerTLS(cfg config.TLSConfig, onReloadErr func(error)) (*tls.Config, http.Handler, error) {
	switch cfg.Mode {
	case config.TLSModeACME:
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.ACME.Domains...),
			Cache:      autocert.DirCache(cfg.ACME.CacheDir),
			Email:      cfg.ACME.Email,
		}
		if cfg.ACME.DirectoryURL != "" {
			m.Client = &acme.Client{DirectoryURL: cfg.ACME.DirectoryURL}
		}
		tlsCfg := m.TLSConfig()
		tlsCfg.MinVersion = tls.VersionTLS12
		return tlsCfg, m.HTTPHandler(RedirectHandler(cfg.ACME.Domains)), nil
	case config.TLSModeFiles:
		r, err := NewFileReloader(cfg.Files.CertFile, cfg.Files.KeyFile, fileCheckInterval, onReloadErr)
		if err != nil {
			return nil, nil, err
		}
		return &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: r.GetCertificate,
			NextProtos:     []string{"h2", "http/1.1"},
		}, RedirectHandler(nil), nil
	default:
		return nil, nil, fmt.Errorf("tlsx: TLS mode %q has no TLS config", cfg.Mode)
	}
}
