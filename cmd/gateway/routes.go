package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/swayam5342/jgateway/internal/config"
	"github.com/swayam5342/jgateway/internal/middleware/auth"
	"github.com/swayam5342/jgateway/internal/middleware/circuitbreaker"
	"github.com/swayam5342/jgateway/internal/middleware/logging"
	"github.com/swayam5342/jgateway/internal/middleware/metrics"
	"github.com/swayam5342/jgateway/internal/middleware/ratelimit"
	"github.com/swayam5342/jgateway/internal/middleware/retry"
	"github.com/swayam5342/jgateway/internal/middleware/signing"
	"github.com/swayam5342/jgateway/internal/proxy"
	pkgmw "github.com/swayam5342/jgateway/pkg/middleware"
)

// buildRouteHandler composes the middleware chain for a single route. Order
// is deliberate and documented per AGENTS.md: logging/metrics wrap
// everything for full observability, auth runs before rate limiting so
// limiter keys can use the authenticated identity, signing verifies the
// payload before it's retried/forwarded, and the circuit breaker sits
// innermost so it trips on real upstream failures only.
func buildRouteHandler(logger *slog.Logger, srv config.ServerConfig, routeCfg config.Route, upCfg config.Upstream, up *proxy.Upstream) (http.Handler, error) {
	// Validated by config.Validate, so this can't fail here.
	trusted, err := config.ParseTrustedProxies(srv.TrustedProxies)
	if err != nil {
		return nil, err
	}
	proxyOpts := proxy.Options{TrustedProxies: trusted}
	if routeCfg.StripPrefix {
		proxyOpts.StripPrefix = routeCfg.PathPrefix
	}
	reverseProxy := proxy.NewReverseProxy(up, proxyOpts)

	var mws []pkgmw.Middleware
	mws = append(mws, logging.New(logger, routeCfg.PathPrefix))
	mws = append(mws, metrics.New(routeCfg.PathPrefix))

	if routeCfg.Auth.Enabled {
		switch routeCfg.Auth.Mode {
		case "jwt":
			jwtCfg := auth.JWTConfig{
				Issuer:   routeCfg.Auth.Issuer,
				Audience: routeCfg.Auth.Audience,
				Leeway:   routeCfg.Auth.Leeway,
			}
			if routeCfg.Auth.JWTSecretEnv != "" {
				jwtCfg.Secret = []byte(os.Getenv(routeCfg.Auth.JWTSecretEnv))
			} else {
				jwtCfg.PublicKeyPEM = []byte(os.Getenv(routeCfg.Auth.JWTPublicKeyEnv))
			}
			mw, err := auth.NewJWT(jwtCfg)
			if err != nil {
				return nil, fmt.Errorf("route %q: %w", routeCfg.PathPrefix, err)
			}
			mws = append(mws, mw)
		case "api_key":
			keys := auth.ParseAPIKeys(os.Getenv(routeCfg.Auth.APIKeysEnv))
			mws = append(mws, auth.NewAPIKey(auth.APIKeyConfig{Keys: keys}))
		}
	}

	if routeCfg.RateLimit.Enabled {
		mws = append(mws, ratelimit.New(ratelimit.Config{
			RequestsPerSecond: routeCfg.RateLimit.RequestsPerSecond,
			Burst:             routeCfg.RateLimit.Burst,
			Strategy:          ratelimit.KeyStrategy(routeCfg.RateLimit.KeyStrategy),
			TrustedProxies:    trusted,
		}, ratelimit.NewMemoryStore()))
	}

	if routeCfg.Signing.Enabled {
		secret := []byte(os.Getenv(routeCfg.Signing.SecretEnv))
		mws = append(mws, signing.New(signing.Config{
			Secret:          secret,
			SignatureHeader: routeCfg.Signing.HeaderName,
			Tolerance:       time.Duration(routeCfg.Signing.ToleranceSec) * time.Second,
		}))
	}

	if upCfg.Retry.Enabled {
		mws = append(mws, retry.New(retry.Config{
			MaxAttempts: upCfg.Retry.MaxAttempts,
			BaseBackoff: upCfg.Retry.BaseBackoff,
			MaxBackoff:  upCfg.Retry.MaxBackoff,
		}))
	}

	if upCfg.CircuitBreaker.Enabled {
		br := circuitbreaker.New(circuitbreaker.Config{
			FailureThreshold:    upCfg.CircuitBreaker.FailureThreshold,
			HalfOpenProbeAfter:  upCfg.CircuitBreaker.HalfOpenProbeAfter,
			HalfOpenMaxRequests: upCfg.CircuitBreaker.HalfOpenMaxRequests,
		})
		mws = append(mws, circuitbreaker.Middleware(br))
	}

	return pkgmw.Chain(reverseProxy, mws...), nil
}
