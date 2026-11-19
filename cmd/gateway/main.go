// Command gateway wires the reverse proxy, its middleware chains, and the
// HTTP server together from config. It contains no proxying/middleware
// logic itself — that lives in internal/proxy and internal/middleware/*, per
// AGENTS.md's "core proxy is dumb" design principle.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/swayam5342/jgateway/internal/config"
)

func main() {
	check := flag.Bool("healthcheck", false, "GET -healthcheck-url and exit 0 if it returns 200, 1 otherwise (for container healthchecks)")
	checkURL := flag.String("healthcheck-url", defaultHealthcheckURL, "URL probed by -healthcheck")
	flag.Parse()
	if *check {
		os.Exit(healthcheck(*checkURL, defaultHealthcheckTimeout))
	}

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
	s, err := newServers(cfg, logger)
	if err != nil {
		return err
	}
	defer s.Close()

	// Every listener runs until the first one fails or a signal arrives;
	// then all of them drain together.
	listeners := []struct {
		srv *http.Server
		tls bool
	}{{s.Public, s.TLS}, {s.Admin, false}}
	if s.HTTP != nil {
		listeners = append(listeners, struct {
			srv *http.Server
			tls bool
		}{s.HTTP, false})
	}

	serveErr := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() {
			logger.Info("gateway listening", "addr", l.srv.Addr, "tls", l.tls)
			var err error
			if l.tls {
				err = l.srv.ListenAndServeTLS("", "")
			} else {
				err = l.srv.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("listener %s: %w", l.srv.Addr, err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var runErr error
	select {
	case runErr = <-serveErr:
		logger.Error("listener failed, shutting down", "error", runErr)
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining in-flight requests", "timeout", cfg.Server.ShutdownTimeout)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	var wg sync.WaitGroup
	shutdownErrs := make([]error, len(listeners))
	for i, l := range listeners {
		wg.Go(func() { shutdownErrs[i] = l.srv.Shutdown(shutdownCtx) })
	}
	wg.Wait()

	if err := errors.Join(append([]error{runErr}, shutdownErrs...)...); err != nil {
		return err
	}
	logger.Info("gateway shut down cleanly")
	return nil
}
