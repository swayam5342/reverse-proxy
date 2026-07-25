package circuitbreaker

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func handlerWithStatus(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	})
}

func TestBreaker_TripsAfterThreshold(t *testing.T) {
	br := New(Config{FailureThreshold: 2, HalfOpenProbeAfter: time.Hour})
	h := Middleware(br)(handlerWithStatus(http.StatusInternalServerError))

	for i := range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("request %d: expected upstream 500 to pass through, got %d", i, rec.Code)
		}
	}

	if br.State() != Open {
		t.Fatalf("expected breaker Open after %d failures, got %s", 2, br.State())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 while breaker open, got %d", rec.Code)
	}
}

func TestBreaker_HalfOpenProbeRecovers(t *testing.T) {
	br := New(Config{FailureThreshold: 1, HalfOpenProbeAfter: 10 * time.Millisecond, HalfOpenMaxRequests: 1})
	failing := Middleware(br)(handlerWithStatus(http.StatusInternalServerError))

	rec := httptest.NewRecorder()
	failing.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if br.State() != Open {
		t.Fatalf("expected Open after single failure with threshold 1, got %s", br.State())
	}

	time.Sleep(20 * time.Millisecond)
	if br.State() != HalfOpen {
		t.Fatalf("expected HalfOpen after probe interval elapsed, got %s", br.State())
	}

	healthy := Middleware(br)(handlerWithStatus(http.StatusOK))
	rec2 := httptest.NewRecorder()
	healthy.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected probe request to pass through, got %d", rec2.Code)
	}
	if br.State() != Closed {
		t.Fatalf("expected Closed after successful probe, got %s", br.State())
	}
}

func TestBreaker_HalfOpenFailureReopens(t *testing.T) {
	br := New(Config{FailureThreshold: 1, HalfOpenProbeAfter: 10 * time.Millisecond, HalfOpenMaxRequests: 1})
	failing := Middleware(br)(handlerWithStatus(http.StatusInternalServerError))

	failing.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	time.Sleep(20 * time.Millisecond)

	failing.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if br.State() != Open {
		t.Fatalf("expected Open again after half-open probe failed, got %s", br.State())
	}
}

func TestBreaker_SuccessDoesNotTrip(t *testing.T) {
	br := New(Config{FailureThreshold: 1, HalfOpenProbeAfter: time.Hour})
	h := Middleware(br)(handlerWithStatus(http.StatusOK))

	for i := range 5 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
	}
	if br.State() != Closed {
		t.Errorf("expected breaker to remain Closed on success, got %s", br.State())
	}
}
