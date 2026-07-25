// Package health provides the /healthz (liveness) and /readyz (readiness)
// endpoints, kept deliberately separate: liveness answers "is the process
// alive," readiness answers "can it actually serve traffic right now."
package health

import (
	"encoding/json"
	"net/http"
	"sync"
)

// Checker reports whether a dependency (typically an upstream) is currently
// healthy. It should be cheap and non-blocking-for-long — readiness is
// polled frequently by orchestrators.
type Checker func() error

// Handler serves /healthz and /readyz.
type Handler struct {
	mu       sync.RWMutex
	checkers map[string]Checker
}

// NewHandler returns a Handler with no readiness checks registered; use
// Register to add upstream/dependency checks.
func NewHandler() *Handler {
	return &Handler{checkers: make(map[string]Checker)}
}

// Register adds a named readiness check. All registered checks must pass
// for /readyz to report ready.
func (h *Handler) Register(name string, c Checker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checkers[name] = c
}

// Liveness answers /healthz: 200 as long as the process can handle HTTP
// requests at all. It never depends on upstream state, so a dead backend
// never causes the orchestrator to kill and restart a perfectly healthy
// gateway process.
func (h *Handler) Liveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Readiness answers /readyz: 200 only if every registered checker passes,
// otherwise 503 with a JSON body naming which checks failed.
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	failures := make(map[string]string)
	for name, check := range h.checkers {
		if err := check(); err != nil {
			failures[name] = err.Error()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if len(failures) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": false, "failures": failures})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ready": true})
}
