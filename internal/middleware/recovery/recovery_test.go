package recovery

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNew_RecoversPanicAndReturns500(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	h := New(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/broken", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}

	logged := buf.String()
	if !strings.Contains(logged, "panic recovered") {
		t.Errorf("expected panic log entry, got %q", logged)
	}
	if !strings.Contains(logged, "boom") {
		t.Errorf("expected panic value in log, got %q", logged)
	}
	if !strings.Contains(logged, "/broken") {
		t.Errorf("expected request path in log, got %q", logged)
	}
}

func TestNew_PassesThroughWithoutPanic(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("expected body %q, got %q", "ok", rec.Body.String())
	}
}

func TestNew_NilLoggerDefaultsWithoutPanicking(t *testing.T) {
	h := New(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestNew_OnePanicDoesNotAffectSubsequentRequests(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("boom")
		}
		w.WriteHeader(http.StatusOK)
	}))

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if rec1.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on panicking request, got %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 on unrelated request, got %d", rec2.Code)
	}
}
