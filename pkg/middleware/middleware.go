// Package middleware defines the reusable Middleware contract shared by every
// gateway middleware and the helper used to chain them into a single handler.
package middleware

import "net/http"

// Middleware wraps an http.Handler to add cross-cutting behavior (auth, rate
// limiting, logging, etc). Implementations must never panic: recover
// internally, or rely on a top-level recovery middleware, so that a bug in
// one middleware cannot take down unrelated requests.
type Middleware func(http.Handler) http.Handler

// Chain applies mws to h in the order given, so that mws[0] is the outermost
// (first to run) middleware and mws[len(mws)-1] runs immediately before h.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
