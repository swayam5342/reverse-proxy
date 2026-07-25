// Command gateway wires the reverse proxy, its middleware chains, and the
// HTTP server together from config. It contains no proxying/middleware
// logic itself — that lives in internal/proxy and internal/middleware/*, per
// AGENTS.md's "core proxy is dumb" design principle.
package main

import (
	"context"
	"errors"
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
	"github.com/swayam5342/jgateway/internal/middleware/circuitbreaker"
	"github.com/swayam5342/jgateway/internal/middleware/logging"
	"github.com/swayam5342/jgateway/internal/middleware/metrics"
	"github.com/swayam5342/jgateway/internal/middleware/ratelimit"
	"github.com/swayam5342/jgateway/internal/middleware/recovery"
	"github.com/swayam5342/jgateway/internal/middleware/retry"
	"github.com/swayam5342/jgateway/internal/middleware/signing"
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
	upstreams := make(map[string]*proxy.Upstream, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		up, err := proxy.NewUpstream(u.Name, u.URL, u.DialTimeout, u.RequestTimeout)
		if err != nil {
			return err
		}
		upstreams[u.Name] = up
	}

	healthHandler := health.NewHandler()
	for _, u := range cfg.Upstreams {
		target := u.URL
		healthHandler.Register(u.Name, readinessCheck(target))
	}

	routes := make([]proxy.Route, 0, len(cfg.Routes))
	for _, routeCfg := range cfg.Routes {
		up, ok := upstreams[routeCfg.Upstream]
		if !ok {
			return errNoSuchUpstream(routeCfg.Upstream)
		}

		var upCfg config.Upstream
		for _, u := range cfg.Upstreams {
			if u.Name == routeCfg.Upstream {
				upCfg = u
				break
			}
		}

		handler := buildRouteHandler(logger, routeCfg, upCfg, up)
		routes = append(routes, proxy.Route{PathPrefix: routeCfg.PathPrefix, Handler: handler})
	}

	router := proxy.NewRouter(routes)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler.Liveness)
	mux.HandleFunc("/readyz", healthHandler.Readiness)
	mux.Handle("/metrics", metrics.Handler())
	mux.Handle("/", pkgmw.Chain(router, recovery.New(logger)))

	server := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           mux,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
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

// buildRouteHandler composes the middleware chain for a single route. Order
// is deliberate and documented per AGENTS.md: logging/metrics wrap
// everything for full observability, auth runs before rate limiting so
// limiter keys can use the authenticated identity, signing verifies the
// payload before it's retried/forwarded, and the circuit breaker sits
// innermost so it trips on real upstream failures only.
func buildRouteHandler(logger *slog.Logger, routeCfg config.Route, upCfg config.Upstream, up *proxy.Upstream) http.Handler {
	reverseProxy := proxy.NewReverseProxy(up)

	var mws []pkgmw.Middleware
	mws = append(mws, logging.New(logger, routeCfg.PathPrefix))
	mws = append(mws, metrics.New(routeCfg.PathPrefix))

	if routeCfg.Auth.Enabled {
		switch routeCfg.Auth.Mode {
		case "jwt":
			secret := []byte(os.Getenv(routeCfg.Auth.JWTSecretEnv))
			mws = append(mws, auth.NewJWT(auth.JWTConfig{Secret: secret}))
		case "api_key":
			keys := splitNonEmpty(os.Getenv(routeCfg.Auth.APIKeysEnv), ",")
			mws = append(mws, auth.NewAPIKey(auth.APIKeyConfig{ValidKeys: keys}))
		}
	}

	if routeCfg.RateLimit.Enabled {
		mws = append(mws, ratelimit.New(ratelimit.Config{
			RequestsPerSecond: routeCfg.RateLimit.RequestsPerSecond,
			Burst:             routeCfg.RateLimit.Burst,
			Strategy:          ratelimit.KeyStrategy(routeCfg.RateLimit.KeyStrategy),
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

	return pkgmw.Chain(reverseProxy, mws...)
}

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
	return func() error {
		conn, err := net.DialTimeout("tcp", host, 2*time.Second)
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
