// Package recovery provides the top-level panic-recovery middleware that
// protects the gateway from a bug in any single middleware or handler taking
// down unrelated requests.
package recovery

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/swayam5342/jgateway/pkg/middleware"
)

// New returns a middleware that recovers from panics in the wrapped handler,
// logs the panic and stack trace, and responds with 500 instead of crashing
// the process or leaving the connection hanging.
func New(logger *slog.Logger) middleware.Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						"panic", rec,
						"path", r.URL.Path,
						"stack", string(debug.Stack()),
					)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
