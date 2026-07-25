package retry

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetry_RetriesOnGatewayError(t *testing.T) {
	var calls int32
	h := New(Config{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&calls, 1)
			if n < 3 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected eventual 200, got %d", rec.Code)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
}

func TestRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	h := New(Config{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected final 503, got %d", rec.Code)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("expected exactly MaxAttempts=2 calls, got %d", calls)
	}
}

func TestRetry_DoesNotRetryNonIdempotentPOST(t *testing.T) {
	var calls int32
	h := New(Config{MaxAttempts: 3, BaseBackoff: time.Millisecond})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusBadGateway)
		}),
	)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected exactly 1 call for non-idempotent POST, got %d", calls)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 passed through, got %d", rec.Code)
	}
}

func TestRetry_RetriesPOSTWithIdempotencyKey(t *testing.T) {
	var calls int32
	h := New(Config{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&calls, 1)
			if n < 2 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Idempotency-Key", "abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected eventual 200, got %d", rec.Code)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("expected 2 attempts, got %d", calls)
	}
}

func TestRetry_DoesNotRetryClientErrors(t *testing.T) {
	var calls int32
	h := New(Config{MaxAttempts: 3, BaseBackoff: time.Millisecond})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusNotFound)
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected exactly 1 call for 404 (not retryable), got %d", calls)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 passed through, got %d", rec.Code)
	}
}
