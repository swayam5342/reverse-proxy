package health

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLiveness_AlwaysOK(t *testing.T) {
	h := NewHandler()
	rec := httptest.NewRecorder()
	h.Liveness(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestReadiness_NoChecks_OK(t *testing.T) {
	h := NewHandler()
	rec := httptest.NewRecorder()
	h.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with no registered checks, got %d", rec.Code)
	}
}

func TestReadiness_FailingCheck_503(t *testing.T) {
	h := NewHandler()
	h.Register("backend", func() error { return errors.New("connection refused") })

	rec := httptest.NewRecorder()
	h.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for failing check, got %d", rec.Code)
	}
}

func TestReadiness_PassingCheck_OK(t *testing.T) {
	h := NewHandler()
	h.Register("backend", func() error { return nil })

	rec := httptest.NewRecorder()
	h.Readiness(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for passing check, got %d", rec.Code)
	}
}
