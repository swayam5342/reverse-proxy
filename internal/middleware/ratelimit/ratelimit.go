// Package ratelimit implements a token-bucket rate limiter middleware with a
// pluggable Store. Per AGENTS.md, the rate limit key must be chosen
// carefully: client IP alone is spoofable behind a proxy, so IP is only
// trusted from X-Forwarded-For when the caller explicitly says the upstream
// LB is trusted; otherwise prefer API key (+ IP as a secondary dimension).
package ratelimit

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// KeyIP buckets by client IP only. Only safe when TrustForwardedFor is
	// true and the gateway sits behind a trusted LB that sets/overwrites
	// X-Forwarded-For; otherwise this key is trivially spoofable.
	KeyIP KeyStrategy = "ip"
)

// Config configures the rate limiter middleware.
type Config struct {
	RequestsPerSecond float64
	Burst             int
	Strategy          KeyStrategy
	// TrustForwardedFor must only be true if the gateway sits behind a
	// trusted load balancer that sets X-Forwarded-For itself (never taking
	// an attacker-supplied value as-is).
	TrustForwardedFor bool
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

			if !store.Allow(key, cfg.RequestsPerSecond, cfg.Burst, time.Now()) {
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
		return identitySubject + "|" + clientIP(r, cfg.TrustForwardedFor)
	case KeyIP:
		return clientIP(r, cfg.TrustForwardedFor)
	default:
		return ""
	}
}

func clientIP(r *http.Request, trustForwardedFor bool) string {
	if trustForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Leftmost entry is the original client per the (trusted) LB's
			// convention.
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
