// Package ratelimit implements a token-bucket rate limiter middleware with a
// pluggable Store. Per AGENTS.md, the rate limit key must be chosen
// carefully: client IP alone is spoofable behind a proxy, so
// X-Forwarded-For is only consulted when the direct peer is a configured
// trusted proxy, and even then it is read right-to-left so a client can't
// inject its own entry; otherwise prefer API key (+ IP as a secondary
// dimension).
package ratelimit

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/swayam5342/jgateway/internal/httpx"
	"github.com/swayam5342/jgateway/internal/middleware/auth"
	"github.com/swayam5342/jgateway/pkg/middleware"
)

// KeyStrategy selects how the rate-limit bucket key is derived from a
// request.
type KeyStrategy string

const (
	// KeyAPIKey buckets solely by the authenticated identity (from the auth
	// middleware's context value). Requires auth to run first in the chain.
	KeyAPIKey KeyStrategy = "api_key"
	// KeyAPIKeyIP buckets by identity+IP, narrowing blast radius of a single
	// leaked key.
	KeyAPIKeyIP KeyStrategy = "api_key_ip"
	// KeyIP buckets by client IP only (see httpx.ClientIP for how it's derived).
	KeyIP KeyStrategy = "ip"
)

// Config configures the rate limiter middleware.
type Config struct {
	RequestsPerSecond float64
	Burst             int
	Strategy          KeyStrategy
	// TrustedProxies lists the networks of load balancers / proxies in front
	// of the gateway. X-Forwarded-For is only honored when the direct peer
	// is in one of them. Leave empty if clients connect directly.
	TrustedProxies []netip.Prefix
	// Route namespaces this limiter's keys in a shared store (normally the
	// route's path prefix), so routes keep independent limits.
	Route string
	// OnReject, if set, is called once for every request rejected with 429.
	OnReject func()
}

// StoreKey returns the key a limiter for route uses in its Store for the
// given client key: "jgw:rl:<route>:<first 32 hex chars of sha256(key)>".
// Hashing keeps API-key names and client IPs out of the shared store's key
// space.
func StoreKey(route, key string) string {
	sum := sha256.Sum256([]byte(key))
	return "jgw:rl:" + route + ":" + hex.EncodeToString(sum[:])[:32]
}

// New returns a rate-limiting middleware backed by store. Requests beyond
// the configured rate get 429 with a Retry-After header.
func New(cfg Config, store Store) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := rateLimitKey(r, cfg)
			if key == "" {
				// No usable identity/IP to key on — fail closed rather than
				// let an unbounded bucket-less request through.
				http.Error(w, "rate limit key unavailable", http.StatusTooManyRequests)
				return
			}

			allowed, err := store.Allow(r.Context(), StoreKey(cfg.Route, key), cfg.RequestsPerSecond, cfg.Burst, time.Now())
			if err != nil {
				// Only reachable without a FallbackStore: fail closed.
				http.Error(w, "rate limit unavailable", http.StatusServiceUnavailable)
				return
			}
			if !allowed {
				if cfg.OnReject != nil {
					cfg.OnReject()
				}
				retryAfter := 1
				if cfg.RequestsPerSecond > 0 {
					retryAfter = int(1/cfg.RequestsPerSecond) + 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func rateLimitKey(r *http.Request, cfg Config) string {
	identitySubject := ""
	if id, ok := auth.IdentityFromContext(r.Context()); ok {
		identitySubject = id.Subject
	}

	switch cfg.Strategy {
	case KeyAPIKey:
		return identitySubject
	case KeyAPIKeyIP:
		if identitySubject == "" {
			return ""
		}
		return identitySubject + "|" + httpx.ClientIP(r, cfg.TrustedProxies)
	case KeyIP:
		return httpx.ClientIP(r, cfg.TrustedProxies)
	default:
		return ""
	}
}
