// Package metrics provides the Prometheus middleware and the shared
// collectors other middleware (circuit breaker, rate limiter, retry) report
// into. Collectors are registered once at package init against the default
// registry, so importing this package from multiple routes/middlewares
// never double-registers.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/swayam5342/jgateway/internal/middleware/statusrec"
	"github.com/swayam5342/jgateway/pkg/middleware"
)

var (
	// RequestsTotal counts requests labeled by route, method, and status.
	RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total requests handled, labeled by route, method, and status.",
	}, []string{"route", "method", "status"})

	// RequestDuration is the request latency histogram labeled by route and
	// status.
	RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_request_duration_seconds",
		Help:    "Request latency in seconds, labeled by route and status.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "status"})

	// CircuitBreakerState reports 0=closed, 1=half_open, 2=open per upstream.
	CircuitBreakerState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_circuit_breaker_state",
		Help: "Circuit breaker state per upstream (0=closed, 1=half_open, 2=open).",
	}, []string{"upstream"})

	// RateLimitRejections counts requests rejected by the rate limiter,
	// labeled by route.
	RateLimitRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_rate_limit_rejections_total",
		Help: "Total requests rejected by the rate limiter, labeled by route.",
	}, []string{"route"})

	// StoreFallbacks counts rate-limit decisions answered by the in-memory
	// fallback because the shared store (Valkey) was unavailable.
	StoreFallbacks = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gateway_rate_limit_store_fallback_total",
		Help: "Rate-limit decisions answered by the in-memory fallback because the shared store was unavailable.",
	})

	// RetryAttempts counts retry attempts (beyond the first try) issued per
	// route.
	RetryAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_retry_attempts_total",
		Help: "Total retry attempts issued, labeled by route.",
	}, []string{"route"})
)

func init() {
	prometheus.MustRegister(RequestsTotal, RequestDuration, CircuitBreakerState, RateLimitRejections, RetryAttempts, StoreFallbacks)
}

// Handler returns the /metrics HTTP handler for the default Prometheus
// registry.
func Handler() http.Handler {
	return promhttp.Handler()
}

// New returns a middleware that records request count and latency for
// route against RequestsTotal/RequestDuration.
func New(route string) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := statusrec.New(w)

			next.ServeHTTP(rec, r)

			status := strconv.Itoa(rec.Status())
			RequestsTotal.WithLabelValues(route, r.Method, status).Inc()
			RequestDuration.WithLabelValues(route, status).Observe(time.Since(start).Seconds())
		})
	}
}
