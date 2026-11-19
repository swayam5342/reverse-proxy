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
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/swayam5342/jgateway/internal/httpx"
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

// Options configures how NewReverseProxy rewrites requests.
type Options struct {
	// StripPrefix, if set, is removed from the start of the request path
	// before forwarding ("/api/users/42" → "/42"; empty becomes "/").
	StripPrefix string
	// TrustedProxies is used to resolve the client IP sent upstream in
	// X-Forwarded-For (see httpx.ClientIP).
	TrustedProxies []netip.Prefix
}

// NewReverseProxy builds an httputil.ReverseProxy targeting up.
//
// It uses ReverseProxy.Rewrite, which strips hop-by-hop headers and every
// client-supplied Forwarded / X-Forwarded-* header before Rewrite runs. The
// gateway then sets X-Forwarded-For (the resolved client IP),
// X-Forwarded-Host (the original Host) and X-Forwarded-Proto itself, so a
// backend can trust them. The outbound Host is the upstream's, and an
// upstream URL path ("http://svc/v1") is prepended to the request path.
func NewReverseProxy(up *Upstream, opts Options) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: up.Client.Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			stripPrefix(pr.Out.URL, opts.StripPrefix)
			pr.SetURL(up.Target)

			proto := "http"
			if pr.In.TLS != nil {
				proto = "https"
			}
			pr.Out.Header.Set("X-Forwarded-For", httpx.ClientIP(pr.In, opts.TrustedProxies))
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", proto)

			if _, ok := pr.Out.Header["User-Agent"]; !ok {
				pr.Out.Header.Set("User-Agent", "")
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
}

// stripPrefix removes prefix from u's path, keeping any percent-encoding in
// RawPath intact.
func stripPrefix(u *url.URL, prefix string) {
	if prefix == "" || prefix == "/" {
		return
	}
	trim := func(p string) string {
		p = strings.TrimPrefix(p, prefix)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return p
	}
	u.Path = trim(u.Path)
	if u.RawPath != "" {
		u.RawPath = trim(u.RawPath)
	}
}

// matchPrefix reports whether path falls under prefix on a path-segment
// boundary: "/api/users" matches "/api/users" and "/api/users/42" but not
// "/api/users-admin". A prefix ending in "/" matches anything below it.
func matchPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(path, prefix)
	}
	return strings.HasPrefix(path, prefix+"/")
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
		if matchPrefix(r.URL.Path, route.PathPrefix) {
			route.Handler.ServeHTTP(w, r)
			return
		}
	}
	http.Error(w, ErrNoRouteMatch.Error(), http.StatusNotFound)
}
