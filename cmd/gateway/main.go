// Command gateway wires the reverse proxy, its middleware chains, and the
// HTTP server together from config. It contains no proxying/middleware
// logic itself — that lives in internal/proxy and internal/middleware/*, per
// AGENTS.md's "core proxy is dumb" design principle.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/swayam5342/jgateway/internal/config"
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
