// Package sanitize removes request headers that only the gateway may set.
//
// Backends trust X-Auth-* (set by the auth middleware after successful
// authentication), so every client-supplied header under that prefix is
// dropped on entry, on every route, before any other middleware runs.
package sanitize

import (
	"net/http"
	"strings"

	"github.com/swayam5342/jgateway/pkg/middleware"
)

// reservedPrefix is the canonical header prefix reserved for the gateway.
const reservedPrefix = "X-Auth-"

// New returns a middleware that strips every request header whose
// canonical name starts with "X-Auth-", however the client spelled it.
func New() middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for name := range r.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(name), reservedPrefix) {
					delete(r.Header, name)
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
