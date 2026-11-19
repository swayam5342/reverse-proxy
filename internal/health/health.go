// Package health provides the /healthz (liveness) and /readyz (readiness)
// endpoints, kept deliberately separate: liveness answers "is the process
// alive," readiness answers "can this gateway instance serve traffic right
// now."
//
// Upstream health deliberately does NOT affect readiness. Every gateway
// replica shares the same upstreams, so if one dead upstream made /readyz
// fail, the orchestrator would pull every replica out of rotation at once
// and take down routes to healthy upstreams too. Per-upstream failure is the
// circuit breaker's job; /readyz only reports upstream status in its body,
// for debugging.
package health

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"sync"
	"time"
)

// Checker reports whether a dependency (typically an upstream) is currently
// reachable. It must return promptly once ctx is done.
type Checker func(ctx context.Context) error

// Handler serves /healthz and /readyz.
type Handler struct {
	timeout time.Duration

	mu       sync.RWMutex
	checkers map[string]Checker
}

// NewHandler returns a Handler with no upstream checks registered. timeout
// bounds how long /readyz waits for all upstream checks together; checks
// run concurrently, so one hung upstream costs at most one timeout.
func NewHandler(timeout time.Duration) *Handler {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Handler{timeout: timeout, checkers: make(map[string]Checker)}
}

// Register adds a named dependency check (an upstream, Valkey). Its result
// is reported in the /readyz body but never makes the gateway unready.
func (h *Handler) Register(name string, c Checker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checkers[name] = c
}

// Liveness answers /healthz: 200 as long as the process can handle HTTP
// requests at all.
func (h *Handler) Liveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Readiness answers /readyz. It returns 200 whenever the process can serve
// requests, with a JSON body ("checks") reporting each registered check
// (upstreams, Valkey) as "ok" or the error it returned.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	checkers := maps.Clone(h.checkers)
	h.mu.RUnlock()

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	var (
		wg      sync.WaitGroup
		resMu   sync.Mutex
		results = make(map[string]string, len(checkers))
	)
	for name, check := range checkers {
		wg.Go(func() {
			status := "ok"
			if err := check(ctx); err != nil {
				status = err.Error()
			}
			resMu.Lock()
			results[name] = status
			resMu.Unlock()
		})
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "checks": results})
}
