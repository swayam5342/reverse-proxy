// Command gateway wires the reverse proxy, its middleware chains, and the
// HTTP server together from config. It contains no proxying/middleware
// logic itself — that lives in internal/proxy and internal/middleware/*, per
// AGENTS.md's "core proxy is dumb" design principle.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/swayam5342/jgateway/internal/config"
	"github.com/swayam5342/jgateway/internal/health"
	"github.com/swayam5342/jgateway/internal/middleware/auth"
	"github.com/swayam5342/jgateway/internal/middleware/bodylimit"
	"github.com/swayam5342/jgateway/internal/middleware/circuitbreaker"
	"github.com/swayam5342/jgateway/internal/middleware/logging"
	"github.com/swayam5342/jgateway/internal/middleware/metrics"
	"github.com/swayam5342/jgateway/internal/middleware/ratelimit"
	"github.com/swayam5342/jgateway/internal/middleware/recovery"
	"github.com/swayam5342/jgateway/internal/middleware/retry"
	"github.com/swayam5342/jgateway/internal/middleware/signing"
	"github.com/swayam5342/jgateway/internal/middleware/timeout"
	"github.com/swayam5342/jgateway/internal/proxy"
	pkgmw "github.com/swayam5342/jgateway/pkg/middleware"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	configPath := os.Getenv("GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("failed to load config, refusing to start", "error", err)
		os.Exit(1)
	}

	if err := run(cfg, logger); err != nil {
		logger.Error("gateway exited with error", "error", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config, logger *slog.Logger) error {
	server, err := newServer(cfg, logger)
	if err != nil {
		return err
	}

	serverErrCh := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", cfg.Server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serverErrCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutdown signal received, draining in-flight requests",
		"timeout", cfg.Server.ShutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("gateway shut down cleanly")
	return nil
}

// newServer builds the fully wired *http.Server (routes, middleware chains,
// health and metrics endpoints, timeouts) from a validated config, without
// starting it.
func newServer(cfg *config.Config, logger *slog.Logger) (*http.Server, error) {
	upstreams := make(map[string]*proxy.Upstream, len(cfg.Upstreams))
	upstreamCfgs := make(map[string]config.Upstream, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		up, err := proxy.NewUpstream(u.Name, u.URL, u.DialTimeout, u.RequestTimeout)
		if err != nil {
			return nil, err
		}
		upstreams[u.Name] = up
		upstreamCfgs[u.Name] = u
	}

	healthHandler := health.NewHandler(2 * time.Second)
	for _, u := range cfg.Upstreams {
		healthHandler.Register(u.Name, readinessCheck(u.URL))
	}

	routes := make([]proxy.Route, 0, len(cfg.Routes))
	for _, routeCfg := range cfg.Routes {
		up, ok := upstreams[routeCfg.Upstream]
		if !ok {
			return nil, errNoSuchUpstream(routeCfg.Upstream)
		}
		handler, err := buildRouteHandler(logger, cfg.Server, routeCfg, upstreamCfgs[routeCfg.Upstream], up)
		if err != nil {
			return nil, err
		}
		routes = append(routes, proxy.Route{PathPrefix: routeCfg.PathPrefix, Handler: handler})
	}

	router := proxy.NewRouter(routes)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler.Liveness)
	mux.HandleFunc("/readyz", healthHandler.Readiness)
	mux.Handle("/metrics", metrics.Handler())
	// Gateway-wide middleware, outermost first: recovery catches panics
	// from everything below; timeout puts the overall deadline on the
	// context before anything else runs; bodylimit caps bodies before any
	// route middleware (signing, retry) buffers them.
	mux.Handle("/", pkgmw.Chain(router,
		recovery.New(logger),
		timeout.New(timeout.Config{
			RequestTimeout: cfg.Server.RequestTimeout,
			UpgradeTimeout: cfg.Server.UpgradeTimeout,
		}),
		bodylimit.New(cfg.Server.MaxBodyBytes),
	))

	return &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           mux,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
	}, nil
}

// buildRouteHandler composes the middleware chain for a single route. Order
// is deliberate and documented per AGENTS.md: logging/metrics wrap
// everything for full observability, auth runs before rate limiting so
// limiter keys can use the authenticated identity, signing verifies the
// payload before it's retried/forwarded, and the circuit breaker sits
// innermost so it trips on real upstream failures only.
func buildRouteHandler(logger *slog.Logger, srv config.ServerConfig, routeCfg config.Route, upCfg config.Upstream, up *proxy.Upstream) (http.Handler, error) {
	reverseProxy := proxy.NewReverseProxy(up)

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
			keys := splitNonEmpty(os.Getenv(routeCfg.Auth.APIKeysEnv), ",")
			mws = append(mws, auth.NewAPIKey(auth.APIKeyConfig{ValidKeys: keys}))
		}
	}

	if routeCfg.RateLimit.Enabled {
		// Validated by config.Validate, so this can't fail here.
		trusted, err := config.ParseTrustedProxies(srv.TrustedProxies)
		if err != nil {
			return nil, err
		}
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

// readinessCheck reports whether a TCP connection to the upstream can be
// opened. Used for the informational upstream status in /readyz only.
func readinessCheck(rawURL string) health.Checker {
	// Upstream URLs are validated at config load time (config.Validate), so
	// a parse failure here can't happen in practice.
	target, _ := url.Parse(rawURL)
	host := target.Host
	if target.Port() == "" {
		port := "80"
		if target.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(target.Hostname(), port)
	}
	return func(ctx context.Context) error {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", host)
		if err != nil {
			return err
		}
		return conn.Close()
	}
}

func errNoSuchUpstream(name string) error {
	return errors.New("main: route references unknown upstream " + name)
}

func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(s, sep) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
