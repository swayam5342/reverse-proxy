// Package retry re-executes a request against the next handler when it
// fails with a retryable (502/503/504) status, using jittered exponential
// backoff. Per AGENTS.md, retries are idempotency-aware: only safe methods
// (GET, HEAD, OPTIONS, PUT, DELETE) or a POST explicitly marked idempotent
// via the Idempotency-Key header are retried. Backoff is always jittered —
// never a tight loop — and stops early if the request context is done.
//
// Responses are streamed, not buffered: an attempt is committed to the
// client as soon as it produces a non-retryable status (or is the last
// attempt), so successful responses of any size and streaming responses
// (SSE, chunked) pass straight through. Only retryable error responses are
// held back, and their bodies are capped at maxHeldErrorBody.
//
// Ordering: the request body is buffered for replay, so the body-limit
// middleware must run before this one.
package retry

import (
	"bytes"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/swayam5342/jgateway/internal/httpx"
	"github.com/swayam5342/jgateway/internal/middleware/bodylimit"
	"github.com/swayam5342/jgateway/pkg/middleware"
)

// maxHeldErrorBody caps how much of a retryable error response body is kept
// in case it ends up being the final response.
const maxHeldErrorBody = 64 << 10

// Config configures retry behavior.
type Config struct {
	MaxAttempts int // total attempts including the first, e.g. 3 = up to 2 retries
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// New returns a retrying middleware. Only requests whose method is
// idempotency-safe (see isIdempotent) are retried; everything else, and any
// protocol-upgrade request, is forwarded exactly once with the real
// ResponseWriter.
func New(cfg Config) middleware.Middleware {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.MaxAttempts == 1 || !isIdempotent(r) || httpx.IsUpgrade(r) {
				next.ServeHTTP(w, r)
				return
			}

			var bodyBytes []byte
			if r.Body != nil && r.Body != http.NoBody {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					bodylimit.WriteReadError(w, err)
					return
				}
				bodyBytes = b
			}

			for attempt := 1; ; attempt++ {
				reqAttempt := r.Clone(r.Context())
				if bodyBytes != nil {
					reqAttempt.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				}

				aw := &attemptWriter{w: w, header: make(http.Header), final: attempt == cfg.MaxAttempts}
				next.ServeHTTP(aw, reqAttempt)
				aw.finish()
				if aw.committed {
					return
				}

				if !sleepWithJitter(r.Context(), cfg.BaseBackoff, cfg.MaxBackoff, attempt) {
					aw.flushHeld()
					return
				}
			}
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
	jitter := time.Duration(rand.Int64N(int64(backoff)/2 + 1)) //nolint:gosec // G404: jitter needs spread, not unpredictability.
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

// attemptWriter is the ResponseWriter for one attempt. On the first final
// (>= 200) status it decides: if the status isn't retryable, or this is the
// last attempt, it commits — copies headers to the real writer and streams
// everything from then on. Otherwise it holds the (capped) error response
// so it can be discarded on retry, or written if retrying stops early.
type attemptWriter struct {
	w      http.ResponseWriter
	header http.Header
	final  bool

	decided   bool
	committed bool
	status    int
	held      bytes.Buffer
}

func (a *attemptWriter) Header() http.Header {
	if a.committed {
		return a.w.Header()
	}
	return a.header
}

func (a *attemptWriter) WriteHeader(code int) {
	if a.decided {
		return
	}
	if code < http.StatusOK {
		// Informational responses (e.g. 103 Early Hints) are dropped: we
		// don't yet know whether this attempt will be the one we keep.
		return
	}
	a.decided = true
	a.status = code
	if a.final || !isRetryableStatus(code) {
		a.committed = true
		maps.Copy(a.w.Header(), a.header)
		a.w.WriteHeader(code)
	}
}

func (a *attemptWriter) Write(p []byte) (int, error) {
	if !a.decided {
		a.WriteHeader(http.StatusOK)
	}
	if a.committed {
		return a.w.Write(p)
	}
	if room := maxHeldErrorBody - a.held.Len(); room > 0 {
		a.held.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// FlushError flushes the real writer once the attempt is committed; while
// an error response is held there's nothing to flush yet.
func (a *attemptWriter) FlushError() error {
	if !a.decided {
		a.WriteHeader(http.StatusOK)
	}
	if !a.committed {
		return nil
	}
	return http.NewResponseController(a.w).Flush()
}

// Unwrap exposes the real writer to http.ResponseController (deadlines).
func (a *attemptWriter) Unwrap() http.ResponseWriter { return a.w }

// finish commits an attempt whose handler returned without writing
// anything, matching net/http's implicit 200.
func (a *attemptWriter) finish() {
	if !a.decided {
		a.WriteHeader(http.StatusOK)
	}
}

// flushHeld writes the held error response to the real writer.
func (a *attemptWriter) flushHeld() {
	maps.Copy(a.w.Header(), a.header)
	a.w.WriteHeader(a.status)
	_, _ = a.w.Write(a.held.Bytes())
}
