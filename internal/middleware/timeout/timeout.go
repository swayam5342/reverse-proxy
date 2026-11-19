// Package timeout puts an overall deadline on every request's context, so
// the whole chain (retries and backoff included) and the upstream call stop
// at a known time. The reverse proxy turns an expired deadline into a clean
// 504, which only reaches the client if the deadline is shorter than the
// server's write timeout (config validation enforces this).
//
// Ordering: run it outermost (right after panic recovery) so every other
// middleware sees the deadline.
package timeout

import (
	"context"
	"net/http"
	"time"

	"github.com/swayam5342/jgateway/internal/httpx"
	"github.com/swayam5342/jgateway/pkg/middleware"
)

// Config configures request deadlines.
type Config struct {
	// RequestTimeout bounds an ordinary request end to end.
	RequestTimeout time.Duration
	// UpgradeTimeout bounds the total lifetime of an upgraded (WebSocket)
	// connection.
	UpgradeTimeout time.Duration
}

// New returns a middleware that attaches a deadline to the request
// context: RequestTimeout normally, or UpgradeTimeout for protocol-upgrade
// requests. httputil.ReverseProxy closes an upgraded connection when the
// request context ends, so UpgradeTimeout is the connection's maximum
// lifetime. (net/http clears the server's read/write deadlines on hijack,
// so those don't apply to upgraded connections.)
func New(cfg Config) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			budget := cfg.RequestTimeout
			if httpx.IsUpgrade(r) {
				budget = cfg.UpgradeTimeout
			}
			if budget <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), budget)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
