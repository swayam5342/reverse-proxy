package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/swayam5342/jgateway/internal/middleware/auth"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestMemoryStore_AllowsBurstThenBlocks(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now()

	for i := range 3 {
		if !allow(t, s, "k", 1, 3, now) {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}
	if allow(t, s, "k", 1, 3, now) {
		t.Fatal("4th immediate request should be blocked, burst exhausted")
	}
	if !allow(t, s, "k", 1, 3, now.Add(time.Second)) {
		t.Fatal("request after 1s refill at 1rps should be allowed")
	}
}

func TestNew_IPStrategy_BlocksAfterLimit(t *testing.T) {
	h := New(Config{RequestsPerSecond: 1, Burst: 1, Strategy: KeyIP}, NewMemoryStore())(okHandler())

	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "1.2.3.4:5555"
		return r
	}

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req())
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request expected 200, got %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req())
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second immediate request expected 429, got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
}

func TestNew_APIKeyStrategy_RequiresIdentity(t *testing.T) {
	h := New(Config{RequestsPerSecond: 1, Burst: 1, Strategy: KeyAPIKey}, NewMemoryStore())(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected fail-closed 429 with no identity, got %d", rec.Code)
	}
}

func TestNew_APIKeyStrategy_SeparateBucketsPerIdentity(t *testing.T) {
	h := New(Config{RequestsPerSecond: 1, Burst: 1, Strategy: KeyAPIKey}, NewMemoryStore())(okHandler())

	withIdentity := func(subject string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		ctx := auth.NewContext(r.Context(), auth.Identity{Subject: subject, Method: "api_key"})
		return r.WithContext(ctx)
	}

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, withIdentity("user-a"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("user-a first request expected 200, got %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, withIdentity("user-b"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("user-b (different bucket) expected 200, got %d", rec2.Code)
	}
}
