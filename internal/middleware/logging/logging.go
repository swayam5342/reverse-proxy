// Package logging provides structured per-request logging and request ID
// propagation. It generates an X-Request-Id if the client didn't supply one,
// echoes it back on the response, and logs one structured line per request
// with the request ID, upstream/route, status, and latency — satisfying the
// AGENTS.md observability requirement of at minimum a propagated/generated
// request ID plus one log line per request.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/swayam5342/jgateway/internal/middleware/statusrec"
	"github.com/swayam5342/jgateway/pkg/middleware"
)

type contextKey string

const requestIDContextKey contextKey = "logging.request_id"

const requestIDHeader = "X-Request-Id"

// RequestIDFromContext returns the request ID associated with ctx, if any.
func RequestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDContextKey).(string)
	return id, ok
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(b)
}

// New returns a middleware that assigns/propagates a request ID and logs one
// structured line per request via logger (or slog.Default() if nil).
// route names the route/upstream this handler serves, for log correlation.
func New(logger *slog.Logger, route string) middleware.Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get(requestIDHeader)
			if requestID == "" {
				requestID = newRequestID()
			}
			w.Header().Set(requestIDHeader, requestID)

			ctx := context.WithValue(r.Context(), requestIDContextKey, requestID)
			r = r.WithContext(ctx)

			rec := statusrec.New(w)
			start := time.Now()
			next.ServeHTTP(rec, r)
			latency := time.Since(start)

			logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("request_id", requestID),
				slog.String("route", route),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.Status()),
				slog.Duration("latency", latency),
			)
		})
	}
}
