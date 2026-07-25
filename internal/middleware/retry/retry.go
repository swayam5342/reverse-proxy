// Package retry re-executes a request against the next handler when it
// fails with a retryable (5xx) status, using jittered exponential backoff.
// Per AGENTS.md, retries are idempotency-aware: only safe methods (GET,
// HEAD, OPTIONS, PUT, DELETE) or a POST explicitly marked idempotent via the
// Idempotency-Key header are retried. Backoff is always jittered — never a
// tight loop — and stops early if the request context is done.
package retry

import (
	"bytes"
	"io"
	"maps"
	"math/rand"
	"net/http"
	"time"

	"github.com/swayam5342/jgateway/pkg/middleware"
)

// Config configures retry behavior.
type Config struct {
	MaxAttempts int // total attempts including the first, e.g. 3 = up to 2 retries
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// New returns a retrying middleware. Only requests whose method is
// idempotency-safe (see isIdempotent) are retried; everything else is
// forwarded exactly once, matching normal handler behavior.
func New(cfg Config) middleware.Middleware {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isIdempotent(r) {
				next.ServeHTTP(w, r)
				return
			}

			var bodyBytes []byte
			if r.Body != nil {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "error reading body", http.StatusBadRequest)
					return
				}
				bodyBytes = b
			}

			var buf *responseBuffer
			for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
				reqAttempt := r.Clone(r.Context())
				if bodyBytes != nil {
					reqAttempt.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				}

				buf = newResponseBuffer()
				next.ServeHTTP(buf, reqAttempt)

				if !isRetryableStatus(buf.status) || attempt == cfg.MaxAttempts {
					break
				}

				if !sleepWithJitter(r.Context(), cfg.BaseBackoff, cfg.MaxBackoff, attempt) {
					break
				}
			}

			buf.flushTo(w)
		})
	}
}

// isIdempotent reports whether r's method is safe to retry automatically.
// GET/HEAD/OPTIONS/PUT/DELETE are idempotent by HTTP semantics; POST is only
// retried if the caller explicitly marks it safe via Idempotency-Key.
func isIdempotent(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	case http.MethodPost:
		return r.Header.Get("Idempotency-Key") != ""
	default:
		return false
	}
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// sleepWithJitter waits base*2^(attempt-1) capped at max, plus up to 50%
// jitter, returning false without waiting the full duration if ctx is done
// first.
func sleepWithJitter(ctx interface{ Done() <-chan struct{} }, base, maxBackoff time.Duration, attempt int) bool {
	if base <= 0 {
		base = 50 * time.Millisecond
	}
	if maxBackoff <= 0 {
		maxBackoff = 5 * time.Second
	}

	backoff := base << (attempt - 1)
	if backoff <= 0 || backoff > maxBackoff {
		backoff = maxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(backoff)/2 + 1))
	wait := backoff/2 + jitter

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// responseBuffer captures a handler's response so it can be discarded (on
// retry) or flushed to the real ResponseWriter (on the final attempt).
type responseBuffer struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func newResponseBuffer() *responseBuffer {
	return &responseBuffer{header: make(http.Header), status: http.StatusOK}
}

func (b *responseBuffer) Header() http.Header { return b.header }

func (b *responseBuffer) WriteHeader(code int) {
	if b.wroteHeader {
		return
	}
	b.status = code
	b.wroteHeader = true
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	return b.body.Write(p)
}

func (b *responseBuffer) flushTo(w http.ResponseWriter) {
	maps.Copy(w.Header(), b.header)
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
