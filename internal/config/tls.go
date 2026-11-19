package config

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"regexp"
)

// TLS modes.
const (
	TLSModeOff   = "off"
	TLSModeACME  = "acme"
	TLSModeFiles = "files"
)

// TLSConfig configures TLS termination. Mode "acme" obtains and renews
// certificates from Let's Encrypt automatically; "files" serves a
// certificate/key pair from disk, reloading it when the files change;
// "off" serves plaintext (local development, or TLS terminated in front).
type TLSConfig struct {
	Mode string `yaml:"mode"`
	// HTTPAddr is the plaintext listener used for ACME HTTP-01 challenges
	// and HTTP→HTTPS redirects. It never proxies. Default ":80".
	HTTPAddr string         `yaml:"http_addr"`
	ACME     ACMEConfig     `yaml:"acme"`
	Files    TLSFilesConfig `yaml:"files"`
}

// ACMEConfig configures automatic certificates.
type ACMEConfig struct {
	// Domains the gateway may obtain certificates for (exact names, no
	// wildcards). Requests for other names are refused.
	Domains []string `yaml:"domains"`
	// Email is given to the CA for expiry notices. Optional.
	Email string `yaml:"email"`
	// CacheDir stores account keys and certificates; it must persist
	// across restarts (a volume), or every restart requests new ones.
	CacheDir string `yaml:"cache_dir"`
	// DirectoryURL overrides the CA (e.g. Let's Encrypt staging). Empty
	// means Let's Encrypt production.
	DirectoryURL string `yaml:"directory_url"`
}

// TLSFilesConfig points at a PEM certificate chain and private key.
type TLSFilesConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Enabled reports whether the gateway terminates TLS itself.
func (t TLSConfig) Enabled() bool { return t.Mode != "" && t.Mode != TLSModeOff }

// hostnameRE matches a DNS name ACME can issue for: dot-separated labels,
// no scheme, port, path or wildcard.
var hostnameRE = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func (t TLSConfig) validate(serverAddr string) []string {
	var errs []string
	switch t.Mode {
	case TLSModeOff, "": // empty = off (configs built in code)
		return nil
	case TLSModeACME:
		if len(t.ACME.Domains) == 0 {
			errs = append(errs, "tls.acme.domains must list at least one domain for mode acme")
		}
		for _, d := range t.ACME.Domains {
			if !hostnameRE.MatchString(d) {
				errs = append(errs, fmt.Sprintf("tls.acme.domains %q must be a plain DNS name (no scheme, port or wildcard)", d))
			}
		}
		if t.ACME.CacheDir == "" {
			errs = append(errs, "tls.acme.cache_dir must be set for mode acme (use a persistent volume)")
		}
		if t.ACME.DirectoryURL != "" {
			if u, err := url.Parse(t.ACME.DirectoryURL); err != nil || u.Scheme != "https" || u.Host == "" {
				errs = append(errs, fmt.Sprintf("tls.acme.directory_url %q must be an https URL", t.ACME.DirectoryURL))
			}
		}
	case TLSModeFiles:
		if t.Files.CertFile == "" || t.Files.KeyFile == "" {
			errs = append(errs, "tls.files.cert_file and tls.files.key_file must be set for mode files")
		} else if _, err := tls.LoadX509KeyPair(t.Files.CertFile, t.Files.KeyFile); err != nil {
			errs = append(errs, fmt.Sprintf("tls.files: cannot load certificate/key pair: %v", err))
		}
	default:
		return []string{fmt.Sprintf("tls.mode must be one of off, acme, files; got %q", t.Mode)}
	}
	switch t.HTTPAddr {
	case "":
		errs = append(errs, "tls.http_addr must be set when TLS is on (ACME challenges and HTTP→HTTPS redirects)")
	case serverAddr:
		errs = append(errs, "tls.http_addr must differ from server.addr")
	}
	return errs
}
