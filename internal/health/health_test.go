package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLiveness_AlwaysOK(t *testing.T) {
	h := NewHandler(time.Second)
	rec := httptest.NewRecorder()
	h.Liveness(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestReadiness_NoChecks_OK(t *testing.T) {
	h := NewHandler(time.Second)
	rec := httptest.NewRecorder()
	h.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with no registered checks, got %d", rec.Code)
	}
}

// A single dead upstream must not mark the gateway unready: if it did, the
// orchestrator would pull every gateway replica out of rotation and take
// down routes to healthy upstreams too. The failure is reported in the body
// for debugging only.
func TestReadiness_FailingUpstreamCheck_StillReadyAndReported(t *testing.T) {
	h := NewHandler(time.Second)
	h.Register("backend", func(context.Context) error { return errors.New("connection refused") })
	h.Register("other", func(context.Context) error { return nil })

	rec := httptest.NewRecorder()
	h.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite failing upstream check, got %d", rec.Code)
	}

	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if !body.Ready {
		t.Error("expected ready=true")
	}
	if body.Checks["backend"] != "connection refused" {
		t.Errorf("checks[backend] = %q, want the check error", body.Checks["backend"])
	}
	if body.Checks["other"] != "ok" {
		t.Errorf("checks[other] = %q, want ok", body.Checks["other"])
	}
}

// Readiness is polled frequently; a hung upstream must not hang the probe.
func TestReadiness_HungCheckBoundedByTimeout(t *testing.T) {
	h := NewHandler(50 * time.Millisecond)
	for _, name := range []string{"a", "b", "c"} {
		h.Register(name, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
	}

	start := time.Now()
	rec := httptest.NewRecorder()
	h.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	elapsed := time.Since(start)

	// Checks run concurrently, so three hung checks cost one timeout, not three.
	if elapsed > 140*time.Millisecond {
		t.Errorf("readiness took %v; checks should run concurrently under a 50ms timeout", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}
