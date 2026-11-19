// Package config loads and validates the gateway's configuration. Config is
// loaded once at startup from a YAML file (with environment variable
// overrides for secrets), validated, and the process fails fast on any
// invalid value rather than starting in a half-broken state.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root gateway configuration.
type Config struct {
	Server    ServerConfig `yaml:"server"`
	Upstreams []Upstream   `yaml:"upstreams"`
	Routes    []Route      `yaml:"routes"`
	// Valkey, if set, makes rate limits shared across gateway copies.
	Valkey *ValkeyConfig `yaml:"valkey"`
}

// ServerConfig controls the listener and top-level timeouts.
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`

	// RequestTimeout is the overall deadline for one request through the
	// whole chain, retries and backoff included. Must be shorter than
	// WriteTimeout so the resulting 504 can still be written.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// UpgradeTimeout is the maximum lifetime of an upgraded (WebSocket)
	// connection; it replaces read/write timeouts for those connections.
	UpgradeTimeout time.Duration `yaml:"upgrade_timeout"`
	// MaxBodyBytes caps request body size (413 above it). 0 disables the cap.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// TrustedProxies lists CIDRs (or bare IPs) of load balancers in front of
	// the gateway. X-Forwarded-For is only honored from these peers.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

// Upstream is a backend service the gateway can proxy to.
type Upstream struct {
	Name           string        `yaml:"name"`
	URL            string        `yaml:"url"`
	DialTimeout    time.Duration `yaml:"dial_timeout"`
	RequestTimeout time.Duration `yaml:"request_timeout"`

	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	Retry          RetryConfig          `yaml:"retry"`
}

// CircuitBreakerConfig configures the per-upstream breaker.
type CircuitBreakerConfig struct {
	Enabled             bool          `yaml:"enabled"`
	FailureThreshold    int           `yaml:"failure_threshold"`
	HalfOpenProbeAfter  time.Duration `yaml:"half_open_probe_after"`
	HalfOpenMaxRequests int           `yaml:"half_open_max_requests"`
}

// RetryConfig configures retry behavior for a route/upstream.
type RetryConfig struct {
	Enabled     bool          `yaml:"enabled"`
	MaxAttempts int           `yaml:"max_attempts"`
	BaseBackoff time.Duration `yaml:"base_backoff"`
	MaxBackoff  time.Duration `yaml:"max_backoff"`
}

// RateLimitConfig configures the token bucket limiter for a route.
type RateLimitConfig struct {
	Enabled           bool    `yaml:"enabled"`
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
	KeyStrategy       string  `yaml:"key_strategy"` // "api_key", "api_key_ip", "ip" (untrusted unless behind trusted LB)
}

// AuthConfig configures auth for a route.
type AuthConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Mode         string `yaml:"mode"`           // "jwt" or "api_key"
	JWTSecretEnv string `yaml:"jwt_secret_env"` // env var holding an HMAC secret
	// JWTPublicKeyEnv names an env var holding a PEM public key or
	// certificate (RS*/PS*/ES*/EdDSA). Exactly one of JWTSecretEnv and
	// JWTPublicKeyEnv must be set for jwt mode.
	JWTPublicKeyEnv string `yaml:"jwt_public_key_env"`
	// Issuer and Audience, if set, must match the token's iss / aud claims.
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`
	// Leeway tolerates issuer/gateway clock skew on exp/nbf/iat.
	Leeway     time.Duration `yaml:"leeway"`
	APIKeysEnv string        `yaml:"api_keys_env"` // env var holding comma-separated valid keys
}

// SigningConfig configures HMAC request signing verification for a route.
type SigningConfig struct {
	Enabled      bool   `yaml:"enabled"`
	SecretEnv    string `yaml:"secret_env"`
	HeaderName   string `yaml:"header_name"`
	ToleranceSec int64  `yaml:"tolerance_seconds"`
}

// Route maps a path prefix to an upstream and the middleware chain applied
// to it. This is intentionally data-driven, per AGENTS.md, rather than a
// hardcoded if-chain in main.go.
type Route struct {
	PathPrefix string `yaml:"path_prefix"`
	// StripPrefix removes PathPrefix from the path before forwarding, so
	// "/api/users/42" reaches the upstream as "/42".
	StripPrefix bool            `yaml:"strip_prefix"`
	Upstream    string          `yaml:"upstream"`
	Auth        AuthConfig      `yaml:"auth"`
	RateLimit   RateLimitConfig `yaml:"rate_limit"`
	Signing     SigningConfig   `yaml:"signing"`
}

// Load reads and parses the YAML config at path, applies environment
// variable overrides, validates the result, and returns an error describing
// exactly what's wrong if validation fails. Callers should treat any error
// as fatal (fail fast on invalid config).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is operator-supplied config (GATEWAY_CONFIG), not request input.
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Unknown keys (typos, removed options) are an error, not silently
	// ignored: a misspelled timeout must not quietly fall back to a default.
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}

	if cfg.Valkey != nil {
		cfg.Valkey.applyDefaults()
	}

	if addr := os.Getenv("GATEWAY_ADDR"); addr != "" {
		cfg.Server.Addr = addr
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: invalid: %w", err)
	}
	return cfg, nil
}

// Default returns a Config with sane, safe timeout defaults. Values loaded
// from YAML override these field by field.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:              ":8080",
			ReadTimeout:       5 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       120 * time.Second,
			ReadHeaderTimeout: 5 * time.Second,
			ShutdownTimeout:   30 * time.Second,
			RequestTimeout:    9 * time.Second,
			UpgradeTimeout:    time.Hour,
			MaxBodyBytes:      10 << 20,
		},
	}
}

// Validate checks required fields, URL validity, and that timeouts are in
// sane ranges. It returns a single error joining every problem found so a
// user fixing config doesn't have to run it repeatedly to find each issue.
func (c *Config) Validate() error {
	var errs []string

	if c.Server.Addr == "" {
		errs = append(errs, "server.addr must not be empty")
	}
	if c.Server.ReadTimeout <= 0 {
		errs = append(errs, "server.read_timeout must be > 0")
	}
	if c.Server.WriteTimeout <= 0 {
		errs = append(errs, "server.write_timeout must be > 0")
	}
	if c.Server.ShutdownTimeout <= 0 {
		errs = append(errs, "server.shutdown_timeout must be > 0")
	}
	if c.Server.RequestTimeout <= 0 {
		errs = append(errs, "server.request_timeout must be > 0")
	} else if c.Server.WriteTimeout > 0 && c.Server.RequestTimeout >= c.Server.WriteTimeout {
		errs = append(errs, fmt.Sprintf("server.request_timeout (%v) must be < server.write_timeout (%v), or clients get a reset connection instead of a 504",
			c.Server.RequestTimeout, c.Server.WriteTimeout))
	}
	if c.Server.UpgradeTimeout <= 0 {
		errs = append(errs, "server.upgrade_timeout must be > 0")
	}
	if c.Server.MaxBodyBytes < 0 {
		errs = append(errs, "server.max_body_bytes must be >= 0 (0 disables the limit)")
	}
	if _, err := ParseTrustedProxies(c.Server.TrustedProxies); err != nil {
		errs = append(errs, err.Error())
	}

	if len(c.Upstreams) == 0 {
		errs = append(errs, "at least one upstream must be configured")
	}
	names := make(map[string]bool, len(c.Upstreams))
	for i, u := range c.Upstreams {
		if u.Name == "" {
			errs = append(errs, fmt.Sprintf("upstreams[%d].name must not be empty", i))
		} else if names[u.Name] {
			errs = append(errs, fmt.Sprintf("upstreams[%d].name %q is duplicated", i, u.Name))
		} else {
			names[u.Name] = true
		}
		parsed, err := url.Parse(u.URL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			errs = append(errs, fmt.Sprintf("upstreams[%d].url %q is not a valid absolute URL", i, u.URL))
		}
		if u.DialTimeout <= 0 {
			errs = append(errs, fmt.Sprintf("upstreams[%d].dial_timeout must be > 0", i))
		}
		if u.RequestTimeout <= 0 {
			errs = append(errs, fmt.Sprintf("upstreams[%d].request_timeout must be > 0", i))
		} else if c.Server.RequestTimeout > 0 && u.RequestTimeout > c.Server.RequestTimeout {
			errs = append(errs, fmt.Sprintf("upstreams[%d].request_timeout (%v) must be <= server.request_timeout (%v), or it can never take effect",
				i, u.RequestTimeout, c.Server.RequestTimeout))
		}
		if u.CircuitBreaker.Enabled && u.CircuitBreaker.FailureThreshold <= 0 {
			errs = append(errs, fmt.Sprintf("upstreams[%d].circuit_breaker.failure_threshold must be > 0 when enabled", i))
		}
		if u.Retry.Enabled && u.Retry.MaxAttempts <= 0 {
			errs = append(errs, fmt.Sprintf("upstreams[%d].retry.max_attempts must be > 0 when enabled", i))
		}
	}

	if len(c.Routes) == 0 {
		errs = append(errs, "at least one route must be configured")
	}
	for i, r := range c.Routes {
		if r.PathPrefix == "" {
			errs = append(errs, fmt.Sprintf("routes[%d].path_prefix must not be empty", i))
		} else if !strings.HasPrefix(r.PathPrefix, "/") {
			errs = append(errs, fmt.Sprintf("routes[%d].path_prefix must start with \"/\", got %q", i, r.PathPrefix))
		}
		if r.Upstream == "" {
			errs = append(errs, fmt.Sprintf("routes[%d].upstream must not be empty", i))
		} else if !names[r.Upstream] {
			errs = append(errs, fmt.Sprintf("routes[%d].upstream %q does not match any configured upstream", i, r.Upstream))
		}
		if r.Auth.Enabled {
			switch r.Auth.Mode {
			case "jwt":
				switch {
				case (r.Auth.JWTSecretEnv == "") == (r.Auth.JWTPublicKeyEnv == ""):
					errs = append(errs, fmt.Sprintf("routes[%d].auth: exactly one of jwt_secret_env or jwt_public_key_env must be set for jwt mode", i))
				case r.Auth.JWTSecretEnv != "" && os.Getenv(r.Auth.JWTSecretEnv) == "":
					errs = append(errs, fmt.Sprintf("routes[%d].auth: env var %q referenced by jwt_secret_env is not set", i, r.Auth.JWTSecretEnv))
				case r.Auth.JWTPublicKeyEnv != "" && os.Getenv(r.Auth.JWTPublicKeyEnv) == "":
					errs = append(errs, fmt.Sprintf("routes[%d].auth: env var %q referenced by jwt_public_key_env is not set", i, r.Auth.JWTPublicKeyEnv))
				}
				if r.Auth.Leeway < 0 {
					errs = append(errs, fmt.Sprintf("routes[%d].auth.leeway must be >= 0", i))
				}
			case "api_key":
				if r.Auth.APIKeysEnv == "" {
					errs = append(errs, fmt.Sprintf("routes[%d].auth.api_keys_env must be set for api_key mode", i))
				} else if os.Getenv(r.Auth.APIKeysEnv) == "" {
					errs = append(errs, fmt.Sprintf("routes[%d].auth: env var %q referenced by api_keys_env is not set", i, r.Auth.APIKeysEnv))
				}
			default:
				errs = append(errs, fmt.Sprintf("routes[%d].auth.mode must be \"jwt\" or \"api_key\", got %q", i, r.Auth.Mode))
			}
		}
		if r.RateLimit.Enabled {
			if r.RateLimit.RequestsPerSecond <= 0 {
				errs = append(errs, fmt.Sprintf("routes[%d].rate_limit.requests_per_second must be > 0", i))
			}
			if r.RateLimit.Burst <= 0 {
				errs = append(errs, fmt.Sprintf("routes[%d].rate_limit.burst must be > 0", i))
			}
			switch r.RateLimit.KeyStrategy {
			case "api_key", "api_key_ip", "ip":
			default:
				errs = append(errs, fmt.Sprintf("routes[%d].rate_limit.key_strategy must be one of api_key, api_key_ip, ip, got %q", i, r.RateLimit.KeyStrategy))
			}
		}
		if r.Signing.Enabled {
			if r.Signing.SecretEnv == "" {
				errs = append(errs, fmt.Sprintf("routes[%d].signing.secret_env must be set", i))
			} else if os.Getenv(r.Signing.SecretEnv) == "" {
				errs = append(errs, fmt.Sprintf("routes[%d].signing: env var %q referenced by secret_env is not set", i, r.Signing.SecretEnv))
			}
			if r.Signing.HeaderName == "" {
				errs = append(errs, fmt.Sprintf("routes[%d].signing.header_name must be set", i))
			}
		}
	}

	if c.Valkey != nil {
		errs = append(errs, c.Valkey.validate()...)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// ParseTrustedProxies parses server.trusted_proxies entries, each a CIDR
// ("10.0.0.0/8") or a bare IP (treated as a single-address prefix).
func ParseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for i, e := range entries {
		if p, err := netip.ParsePrefix(e); err == nil {
			prefixes = append(prefixes, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("server.trusted_proxies[%d] %q is not a valid CIDR or IP", i, e)
		}
		addr = addr.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return prefixes, nil
}
