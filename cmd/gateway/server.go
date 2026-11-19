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
	"github.com/swayam5342/jgateway/internal/middleware/circuitbreaker"
	"github.com/swayam5342/jgateway/internal/middleware/metrics"
	"github.com/swayam5342/jgateway/internal/middleware/ratelimit"
	"github.com/swayam5342/jgateway/internal/middleware/recovery"
	"github.com/swayam5342/jgateway/internal/middleware/sanitize"
	"github.com/swayam5342/jgateway/internal/middleware/timeout"
	"github.com/swayam5342/jgateway/internal/proxy"
	"github.com/swayam5342/jgateway/internal/tlsx"
	pkgmw "github.com/swayam5342/jgateway/pkg/middleware"
)

// servers are the gateway's listeners, built but not started.
type servers struct {
	// Public serves only the proxied routes. With TLS on it must be started with
	// ListenAndServeTLS("", "") / ServeTLS(ln, "", "").
	Public *http.Server
	// HTTP is the plaintext listener (ACME HTTP-01 + redirect to HTTPS);
	// nil when TLS is off.
	HTTP *http.Server
	// Admin serves /healthz, /readyz and /metrics on server.admin_addr.
	Admin *http.Server
	// TLS reports whether Public terminates TLS.
	TLS bool
	// Close releases resources held for the servers (the Valkey client).
	// Call it after shutting the servers down.
	Close func()
}

// newServers builds every listener (routes, middleware chains, health and
// metrics endpoints, TLS, timeouts) from a validated config, without
// starting any of them.
func newServers(cfg *config.Config, logger *slog.Logger) (*servers, error) {
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

	// One breaker per upstream, shared by every route that reaches it.
	breakers := make(map[string]*circuitbreaker.Breaker)
	for _, u := range cfg.Upstreams {
		if u.CircuitBreaker.Enabled {
			breakers[u.Name] = newBreaker(u)
		}
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
		handler, err := buildRouteHandler(logger, cfg.Server, routeCfg, upstreamCfgs[routeCfg.Upstream], up, sharedStore, breakers[routeCfg.Upstream])
		if err != nil {
			return nil, err
		}
		routes = append(routes, proxy.Route{PathPrefix: routeCfg.PathPrefix, Handler: handler})
	}

	router := proxy.NewRouter(routes)

	// The admin listener alone serves health, readiness and metrics.
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/healthz", healthHandler.Liveness)
	adminMux.HandleFunc("/readyz", healthHandler.Readiness)
	adminMux.Handle("/metrics", metrics.Handler())

	// The public listener serves only proxied routes. The ServeMux is kept
	// for its path cleaning: "/api/x/../../admin" is redirected to
	// "/admin" before routing, so a path can't match one route's prefix
	// and then resolve to another path upstream.
	mux := http.NewServeMux()
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

	s := &servers{
		Public: &http.Server{
			Addr:              cfg.Server.Addr,
			Handler:           mux,
			ReadTimeout:       cfg.Server.ReadTimeout,
			WriteTimeout:      cfg.Server.WriteTimeout,
			IdleTimeout:       cfg.Server.IdleTimeout,
			ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		},
		Admin: &http.Server{
			Addr:              cfg.Server.AdminAddr,
			Handler:           adminMux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       5 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		Close: func() {
			for _, c := range closers {
				c()
			}
		},
	}

	if cfg.TLS.Enabled() {
		tlsCfg, httpHandler, err := tlsx.ServerTLS(cfg.TLS, func(err error) {
			metrics.TLSCertReloadErrors.Inc()
			logger.Error("tls certificate reload failed, still serving the previous certificate", "error", err)
		})
		if err != nil {
			s.Close()
			return nil, err
		}
		s.TLS = true
		s.Public.TLSConfig = tlsCfg
		s.HTTP = &http.Server{
			Addr:              cfg.TLS.HTTPAddr,
			Handler:           httpHandler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       5 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
	} else {
		logger.Warn("tls.mode is off: serving plaintext HTTP; terminate TLS in front of the gateway in production")
	}
	return s, nil
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

// newBreaker builds u's circuit breaker and keeps the
// gateway_circuit_breaker_state gauge (0 closed, 1 half-open, 2 open) in
// step with it.
func newBreaker(u config.Upstream) *circuitbreaker.Breaker {
	gauge := metrics.CircuitBreakerState.WithLabelValues(u.Name)
	gauge.Set(0)
	return circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold:    u.CircuitBreaker.FailureThreshold,
		HalfOpenProbeAfter:  u.CircuitBreaker.HalfOpenProbeAfter,
		HalfOpenMaxRequests: u.CircuitBreaker.HalfOpenMaxRequests,
		OnStateChange: func(_, to circuitbreaker.State) {
			switch to {
			case circuitbreaker.Closed:
				gauge.Set(0)
			case circuitbreaker.HalfOpen:
				gauge.Set(1)
			case circuitbreaker.Open:
				gauge.Set(2)
			}
		},
	})
}
