// Package circuitbreaker implements a per-upstream circuit breaker so one
// dead backend can't take down the gateway. It classifies any response with
// a 5xx status as a failure and trips after a configurable threshold,
// probing recovery via a bounded number of half-open requests before fully
// closing again.
package circuitbreaker

import (
	"net/http"
	"sync"
	"time"

	"github.com/swayam5342/jgateway/internal/middleware/statusrec"
	"github.com/swayam5342/jgateway/pkg/middleware"
)

// State is the circuit breaker's current state.
type State int

const (
	// Closed: requests pass through normally; failures are counted.
	Closed State = iota
	// Open: requests are rejected immediately with 503 until the probe
	// interval elapses.
	Open
	// HalfOpen: a limited number of probe requests are allowed through to
	// test whether the upstream has recovered.
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Config configures a Breaker.
type Config struct {
	// FailureThreshold is the number of consecutive failures (in Closed
	// state) that trips the breaker to Open.
	FailureThreshold int
	// HalfOpenProbeAfter is how long the breaker stays Open before allowing
	// a probe request through (transitioning to HalfOpen).
	HalfOpenProbeAfter time.Duration
	// HalfOpenMaxRequests is how many concurrent probe requests are allowed
	// through while HalfOpen. Defaults to 1 if zero.
	HalfOpenMaxRequests int
}

// Breaker is a per-upstream circuit breaker. It is safe for concurrent use.
type Breaker struct {
	mu sync.Mutex

	cfg Config

	state            State
	consecutiveFail  int
	openedAt         time.Time
	halfOpenInFlight int
}

// New returns a Breaker in the Closed state.
func New(cfg Config) *Breaker {
	if cfg.HalfOpenMaxRequests <= 0 {
		cfg.HalfOpenMaxRequests = 1
	}
	return &Breaker{cfg: cfg, state: Closed}
}

// State returns the breaker's current state, for metrics/observability.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentStateLocked()
}

// currentStateLocked resolves an Open breaker to HalfOpen once the probe
// interval has elapsed. Callers must hold b.mu.
func (b *Breaker) currentStateLocked() State {
	if b.state == Open && time.Since(b.openedAt) >= b.cfg.HalfOpenProbeAfter {
		b.state = HalfOpen
		b.halfOpenInFlight = 0
	}
	return b.state
}

// allow reserves a slot for a request, reporting whether it may proceed.
func (b *Breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.currentStateLocked() {
	case Closed:
		return true
	case HalfOpen:
		if b.halfOpenInFlight >= b.cfg.HalfOpenMaxRequests {
			return false
		}
		b.halfOpenInFlight++
		return true
	default: // Open
		return false
	}
}

// recordResult updates breaker state based on the outcome of a permitted
// request.
func (b *Breaker) recordResult(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case HalfOpen:
		b.halfOpenInFlight--
		if success {
			b.state = Closed
			b.consecutiveFail = 0
		} else {
			b.state = Open
			b.openedAt = time.Now()
		}
	case Closed:
		if success {
			b.consecutiveFail = 0
			return
		}
		b.consecutiveFail++
		if b.consecutiveFail >= b.cfg.FailureThreshold {
			b.state = Open
			b.openedAt = time.Now()
		}
	}
}

// Middleware wraps a handler with br. Requests are rejected with 503 while
// the breaker is Open; a 5xx response counts as a failure, anything else
// counts as a success.
func Middleware(br *Breaker) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !br.allow() {
				http.Error(w, "upstream circuit open", http.StatusServiceUnavailable)
				return
			}

			rec := statusrec.New(w)
			next.ServeHTTP(rec, r)

			br.recordResult(rec.Status() < http.StatusInternalServerError)
		})
	}
}
