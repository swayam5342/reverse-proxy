// Package ratelimit implements a token-bucket rate limiter middleware with a
// pluggable Store. Per AGENTS.md, the rate limit key must be chosen
// carefully: client IP alone is spoofable behind a proxy, so
// X-Forwarded-For is only consulted when the direct peer is a configured
// trusted proxy, and even then it is read right-to-left so a client can't
// inject its own entry; otherwise prefer API key (+ IP as a secondary
// dimension).
package ratelimit

import (
	"net"
	"net/http"
	"net/netip"
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
	// KeyIP buckets by client IP only (see ClientIP for how it's derived).
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
		return identitySubject + "|" + ClientIP(r, cfg.TrustedProxies)
	case KeyIP:
		return ClientIP(r, cfg.TrustedProxies)
	default:
		return ""
	}
}

// ClientIP returns the IP of the client that originated r.
//
// If the direct peer (r.RemoteAddr) is not a trusted proxy, it is the
// client and X-Forwarded-For is ignored. Otherwise X-Forwarded-For is walked
// from the right, the end that trusted proxies append to, skipping trusted
// hops; the first untrusted address is the client. Entries to the left of
// it were supplied by the client and are never trusted. If an entry can't
// be parsed, the last trusted hop is returned rather than guessing.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer, ok := parseAddr(r.RemoteAddr)
	if !ok {
		return r.RemoteAddr
	}
	if !isTrusted(peer, trusted) {
		return peer.String()
	}

	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}

	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		addr, ok := parseAddr(strings.TrimSpace(hops[i]))
		if !ok {
			break
		}
		client = addr
		if !isTrusted(addr, trusted) {
			break
		}
	}
	return client.String()
}

// parseAddr parses "ip" or "ip:port" / "[ipv6]:port".
func parseAddr(s string) (netip.Addr, bool) {
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
