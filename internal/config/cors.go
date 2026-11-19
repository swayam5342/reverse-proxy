package config

import (
	"fmt"
	"net/url"
	"time"
)

// CORSConfig configures CORS for a route (see internal/middleware/cors).
// Origins are exact "scheme://host[:port]" values, or "*".
type CORSConfig struct {
	Enabled          bool          `yaml:"enabled"`
	AllowedOrigins   []string      `yaml:"allowed_origins"`
	AllowedMethods   []string      `yaml:"allowed_methods"`
	AllowedHeaders   []string      `yaml:"allowed_headers"`
	ExposeHeaders    []string      `yaml:"expose_headers"`
	AllowCredentials bool          `yaml:"allow_credentials"`
	MaxAge           time.Duration `yaml:"max_age"`
}

func (c CORSConfig) validate(i int) []string {
	if !c.Enabled {
		return nil
	}
	var errs []string
	if len(c.AllowedOrigins) == 0 {
		errs = append(errs, fmt.Sprintf("routes[%d].cors.allowed_origins must list at least one origin when cors is enabled", i))
	}
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			if c.AllowCredentials {
				errs = append(errs, fmt.Sprintf("routes[%d].cors: allowed_origins \"*\" cannot be combined with allow_credentials: true; list the exact origins", i))
			}
			continue
		}
		if !validOrigin(o) {
			errs = append(errs, fmt.Sprintf("routes[%d].cors.allowed_origins %q must be scheme://host[:port] with no path", i, o))
		}
	}
	if c.MaxAge < 0 {
		errs = append(errs, fmt.Sprintf("routes[%d].cors.max_age must be >= 0", i))
	}
	return errs
}

// validOrigin reports whether o is an origin exactly as browsers send it in
// the Origin header: http(s)://host[:port], nothing else.
func validOrigin(o string) bool {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil
}
