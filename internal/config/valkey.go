package config

import (
	"fmt"
	"net"
	"os"
	"time"
)

// ValkeyConfig configures the shared rate-limit store. When present, every
// rate-limited route keeps its buckets in Valkey (shared by all gateway
// copies), falling back to in-memory limits while Valkey is unreachable.
type ValkeyConfig struct {
	// Addr is host:port, e.g. "valkey:6379".
	Addr string `yaml:"addr"`
	// UsernameEnv / PasswordEnv name env vars holding ACL credentials.
	UsernameEnv string `yaml:"username_env"`
	PasswordEnv string `yaml:"password_env"`
	// DB selects the logical database.
	DB int `yaml:"db"`
	// TLS connects to Valkey over TLS.
	TLS bool `yaml:"tls"`
	// Timeout bounds each rate-limit call to Valkey. Default 50ms.
	Timeout time.Duration `yaml:"timeout"`
}

// defaultValkeyTimeout applies when valkey.timeout is left unset.
const defaultValkeyTimeout = 50 * time.Millisecond

func (v *ValkeyConfig) applyDefaults() {
	if v.Timeout == 0 {
		v.Timeout = defaultValkeyTimeout
	}
}

func (v *ValkeyConfig) validate() []string {
	var errs []string
	if host, port, err := net.SplitHostPort(v.Addr); err != nil || host == "" || port == "" {
		errs = append(errs, fmt.Sprintf("valkey.addr %q must be host:port", v.Addr))
	}
	for field, env := range map[string]string{"username_env": v.UsernameEnv, "password_env": v.PasswordEnv} {
		if env != "" && os.Getenv(env) == "" {
			errs = append(errs, fmt.Sprintf("valkey: env var %q referenced by %s is not set", env, field))
		}
	}
	if v.DB < 0 {
		errs = append(errs, "valkey.db must be >= 0")
	}
	if v.Timeout < 0 {
		errs = append(errs, "valkey.timeout must be > 0")
	}
	return errs
}
