package ratelimit

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Fallback defaults, used when the corresponding FallbackConfig field is
// zero.
const (
	defaultFallbackTimeout  = 50 * time.Millisecond
	defaultFallbackCooldown = 5 * time.Second
	fallbackLogInterval     = 10 * time.Second
)

// FallbackConfig configures a FallbackStore.
type FallbackConfig struct {
	// Timeout bounds each call to the primary store. Default 50ms.
	Timeout time.Duration
	// Cooldown is how long the primary is skipped after it fails, so an
	// outage doesn't add Timeout to every request. Default 5s.
	Cooldown time.Duration
	// OnFallback, if set, is called for every request answered by the
	// fallback store (e.g. to count it in a metric).
	OnFallback func()
	// Logger receives at most one warning per 10s while the primary is
	// failing. Defaults to slog.Default().
	Logger *slog.Logger
}

// FallbackStore answers from a primary Store (Valkey) and, whenever the
// primary errors, times out, or is cooling down after a failure, from a
// fallback Store (per-process memory) instead. It never returns an error,
// so a shared-store outage degrades limits to per-copy precision instead of
// taking rate-limited routes down. The primary is retried automatically
// once the cool-down passes.
type FallbackStore struct {
	primary, fallback Store
	cfg               FallbackConfig

	mu        sync.Mutex
	downUntil time.Time
	lastLog   time.Time
}

// NewFallbackStore returns a FallbackStore over primary and fallback.
func NewFallbackStore(primary, fallback Store, cfg FallbackConfig) *FallbackStore {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultFallbackTimeout
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = defaultFallbackCooldown
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &FallbackStore{primary: primary, fallback: fallback, cfg: cfg}
}

// Allow implements Store and never returns an error.
func (s *FallbackStore) Allow(ctx context.Context, key string, rps float64, burst int, now time.Time) (bool, error) {
	if s.primaryUsable() {
		pctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
		allowed, err := s.primary.Allow(pctx, key, rps, burst, now)
		cancel()
		if err == nil {
			return allowed, nil
		}
		s.primaryFailed(err)
	}

	if s.cfg.OnFallback != nil {
		s.cfg.OnFallback()
	}
	allowed, err := s.fallback.Allow(ctx, key, rps, burst, now)
	if err != nil {
		// The fallback is in-memory and doesn't fail; if it ever does, allow
		// rather than turn a rate-limit problem into an outage.
		return true, nil
	}
	return allowed, nil
}

func (s *FallbackStore) primaryUsable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !time.Now().Before(s.downUntil)
}

func (s *FallbackStore) primaryFailed(err error) {
	s.mu.Lock()
	now := time.Now()
	s.downUntil = now.Add(s.cfg.Cooldown)
	shouldLog := now.Sub(s.lastLog) >= fallbackLogInterval
	if shouldLog {
		s.lastLog = now
	}
	s.mu.Unlock()

	if shouldLog {
		s.cfg.Logger.Warn("rate-limit store unavailable, using in-memory limits",
			"error", err, "retry_after", s.cfg.Cooldown)
	}
}
