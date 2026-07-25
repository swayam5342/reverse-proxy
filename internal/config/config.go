// Package config loads and validates the gateway's configuration. Config is
// loaded once at startup from a YAML file (with environment variable
// overrides for secrets), validated, and the process fails fast on any
// invalid value rather than starting in a half-broken state.
package config

import (
	"fmt"
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
}

// ServerConfig controls the listener and top-level timeouts.
type ServerConfig struct {
	Addr              string        `yaml:"addr"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
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
	Enabled        bool          `yaml:"enabled"`
	MaxAttempts    int           `yaml:"max_attempts"`
	BaseBackoff    time.Duration `yaml:"base_backoff"`
	MaxBackoff     time.Duration `yaml:"max_backoff"`
	IdempotentOnly bool          `yaml:"idempotent_only"`
}

// RateLimitConfig configures the token bucket limiter for a route.
type RateLimitConfig struct {
	Enabled           bool          `yaml:"enabled"`
	RequestsPerSecond float64       `yaml:"requests_per_second"`
	Burst             int           `yaml:"burst"`
	Window            time.Duration `yaml:"window"`
	KeyStrategy       string        `yaml:"key_strategy"` // "api_key", "api_key_ip", "ip" (untrusted unless behind trusted LB)
}

// AuthConfig configures auth for a route.
type AuthConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Mode         string `yaml:"mode"` // "jwt" or "api_key"
	JWTSecretEnv string `yaml:"jwt_secret_env"`
	APIKeysEnv   string `yaml:"api_keys_env"` // env var holding comma-separated valid keys
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
	PathPrefix string          `yaml:"path_prefix"`
	Upstream   string          `yaml:"upstream"`
	Auth       AuthConfig      `yaml:"auth"`
	RateLimit  RateLimitConfig `yaml:"rate_limit"`
	Signing    SigningConfig   `yaml:"signing"`
}

// Load reads and parses the YAML config at path, applies environment
// variable overrides, validates the result, and returns an error describing
// exactly what's wrong if validation fails. Callers should treat any error
// as fatal (fail fast on invalid config).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
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
		}
		if r.Upstream == "" {
			errs = append(errs, fmt.Sprintf("routes[%d].upstream must not be empty", i))
		} else if !names[r.Upstream] {
			errs = append(errs, fmt.Sprintf("routes[%d].upstream %q does not match any configured upstream", i, r.Upstream))
		}
		if r.Auth.Enabled {
			switch r.Auth.Mode {
			case "jwt":
				if r.Auth.JWTSecretEnv == "" {
					errs = append(errs, fmt.Sprintf("routes[%d].auth.jwt_secret_env must be set for jwt mode", i))
				} else if os.Getenv(r.Auth.JWTSecretEnv) == "" {
					errs = append(errs, fmt.Sprintf("routes[%d].auth: env var %q referenced by jwt_secret_env is not set", i, r.Auth.JWTSecretEnv))
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

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
