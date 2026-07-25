// Package proxy implements the core reverse proxy: a data-driven routing
// table mapping path prefixes to upstreams, and the director/transport that
// forwards requests with per-upstream dial and response timeouts. The core
// stays dumb on purpose — cross-cutting behavior belongs in middleware, not
// here.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"time"
)

// ErrNoRouteMatch is returned (via 404 to the client) when no configured
// route prefix matches the incoming request path.
var ErrNoRouteMatch = errors.New("proxy: no route matches request path")

// Upstream is a backend the gateway can forward requests to, along with the
// HTTP client configured with its dial/response timeouts.
type Upstream struct {
	Name   string
	Target *url.URL
	Client *http.Client
}

// NewUpstream builds an Upstream with a transport whose dial timeout and
// overall response timeout are bounded — per AGENTS.md, no unbounded waits
// at any layer.
func NewUpstream(name, rawURL string, dialTimeout, requestTimeout time.Duration) (*Upstream, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: parsing upstream %q url: %w", name, err)
	}
	dialer := &net.Dialer{Timeout: dialTimeout}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: requestTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   32,
	}
	return &Upstream{
		Name:   name,
		Target: target,
		Client: &http.Client{Transport: transport, Timeout: requestTimeout},
	}, nil
}

// NewReverseProxy builds an httputil.ReverseProxy targeting up. The director
// rewrites scheme/host/path only — it never forwards hop-by-hop or
// attacker-controlled trust headers (Host) verbatim into upstream-trusted
// fields.
func NewReverseProxy(up *Upstream) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Transport: up.Client.Transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = up.Target.Scheme
			req.URL.Host = up.Target.Host
			req.Host = up.Target.Host
			if _, ok := req.Header["User-Agent"]; !ok {
				req.Header.Set("User-Agent", "")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return rp
}

// Route maps a path prefix to a fully-built handler (the middleware chain
// wrapping the reverse proxy for that upstream).
type Route struct {
	PathPrefix string
	Handler    http.Handler
}

// Router dispatches requests to the route whose prefix matches, preferring
// the longest (most specific) prefix. It is the "routing table" described in
// AGENTS.md: data-driven, built once from config, never a hardcoded if-chain.
type Router struct {
	routes []Route
}

// NewRouter builds a Router from routes, sorted so longest-prefix-first
// matching is a simple linear scan.
func NewRouter(routes []Route) *Router {
	sorted := make([]Route, len(routes))
	copy(sorted, routes)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].PathPrefix) > len(sorted[j].PathPrefix)
	})
	return &Router{routes: sorted}
}

// ServeHTTP implements http.Handler, dispatching to the longest matching
// route prefix or responding 404 if none match.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, route := range rt.routes {
		if len(r.URL.Path) >= len(route.PathPrefix) && r.URL.Path[:len(route.PathPrefix)] == route.PathPrefix {
			route.Handler.ServeHTTP(w, r)
			return
		}
	}
	http.Error(w, ErrNoRouteMatch.Error(), http.StatusNotFound)
}
