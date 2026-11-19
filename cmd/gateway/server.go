package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/valkey-io/valkey-go"

	"github.com/swayam5342/jgateway/internal/config"
	"github.com/swayam5342/jgateway/internal/health"
	"github.com/swayam5342/jgateway/internal/middleware/bodylimit"
	"github.com/swayam5342/jgateway/internal/middleware/metrics"
	"github.com/swayam5342/jgateway/internal/middleware/ratelimit"
	"github.com/swayam5342/jgateway/internal/middleware/recovery"
	"github.com/swayam5342/jgateway/internal/middleware/sanitize"
	"github.com/swayam5342/jgateway/internal/middleware/timeout"
	"github.com/swayam5342/jgateway/internal/proxy"
	pkgmw "github.com/swayam5342/jgateway/pkg/middleware"
)

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

	var (
		sharedStore ratelimit.Store
		closers     []func()
	)
	if cfg.Valkey != nil {
		vs := ratelimit.NewValkeyStore(valkeyConnector(*cfg.Valkey))
		pingCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := vs.Ping(pingCtx); err != nil {
			logger.Warn("valkey unreachable at startup, using in-memory limits until it recovers",
				"addr", cfg.Valkey.Addr, "error", err)
		}
		cancel()
		healthHandler.Register("valkey", vs.Ping)
		sharedStore = ratelimit.NewFallbackStore(vs, ratelimit.NewMemoryStore(), ratelimit.FallbackConfig{
			Timeout:    cfg.Valkey.Timeout,
			OnFallback: metrics.StoreFallbacks.Inc,
			Logger:     logger,
		})
		closers = append(closers, vs.Close)
	}

	routes := make([]proxy.Route, 0, len(cfg.Routes))
	for _, routeCfg := range cfg.Routes {
		up, ok := upstreams[routeCfg.Upstream]
		if !ok {
			return nil, errNoSuchUpstream(routeCfg.Upstream)
		}
		handler, err := buildRouteHandler(logger, cfg.Server, routeCfg, upstreamCfgs[routeCfg.Upstream], up, sharedStore)
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
	// route middleware (signing, retry) buffers them; sanitize drops
	// client-supplied X-Auth-* so only the gateway can set identity headers.
	mux.Handle("/", pkgmw.Chain(router,
		recovery.New(logger),
		timeout.New(timeout.Config{
			RequestTimeout: cfg.Server.RequestTimeout,
			UpgradeTimeout: cfg.Server.UpgradeTimeout,
		}),
		bodylimit.New(cfg.Server.MaxBodyBytes),
		sanitize.New(),
	))

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           mux,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
	}
	for _, c := range closers {
		srv.RegisterOnShutdown(c)
	}
	return srv, nil
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

// valkeyConnector returns the dial function for the shared rate-limit
// store. The dial timeout is kept short (5x the per-call timeout) because a
// reconnect attempt during an outage runs inside a request.
func valkeyConnector(v config.ValkeyConfig) func() (valkey.Client, error) {
	return func() (valkey.Client, error) {
		opt := valkey.ClientOption{
			InitAddress:  []string{v.Addr},
			SelectDB:     v.DB,
			DisableCache: true,
			Dialer:       net.Dialer{Timeout: 5 * v.Timeout},
		}
		if v.UsernameEnv != "" {
			opt.Username = os.Getenv(v.UsernameEnv)
		}
		if v.PasswordEnv != "" {
			opt.Password = os.Getenv(v.PasswordEnv)
		}
		if v.TLS {
			opt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		return valkey.NewClient(opt)
	}
}
