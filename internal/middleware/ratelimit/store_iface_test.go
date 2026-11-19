package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swayam5342/jgateway/internal/middleware/auth"
)

// allow calls s.Allow with a background context and fails the test on error.
func allow(t *testing.T, s Store, key string, rps float64, burst int, now time.Time) bool {
	t.Helper()
	ok, err := s.Allow(context.Background(), key, rps, burst, now)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	return ok
}

// recordingStore records the keys it's asked about and answers from a
// fixed result.
type recordingStore struct {
	keys    []string
	allowed bool
	err     error
}

func (s *recordingStore) Allow(_ context.Context, key string, _ float64, _ int, _ time.Time) (bool, error) {
	s.keys = append(s.keys, key)
	return s.allowed, s.err
}

func identityRequest(subject string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	return r.WithContext(auth.NewContext(r.Context(), auth.Identity{Subject: subject, Method: "api_key"}))
}

// A shared store (Valkey) holds every route's buckets, so keys are
// namespaced per route, and hashed so secrets and IPs never appear in key
// names.
func TestNew_StoreKeyIsHashedAndNamespaced(t *testing.T) {
	store := &recordingStore{allowed: true}
	h := New(Config{Route: "/api", RequestsPerSecond: 1, Burst: 1, Strategy: KeyAPIKey}, store)(okHandler())
	h.ServeHTTP(httptest.NewRecorder(), identityRequest("secret-key"))

	if len(store.keys) != 1 {
		t.Fatalf("store called %d times, want 1", len(store.keys))
	}
	got := store.keys[0]
	if got != StoreKey("/api", "secret-key") {
		t.Errorf("key = %q, want StoreKey(/api, secret-key) = %q", got, StoreKey("/api", "secret-key"))
	}
	if !strings.HasPrefix(got, "jgw:rl:/api:") || len(got) != len("jgw:rl:/api:")+32 {
		t.Errorf("key %q is not jgw:rl:/api:<32 hex>", got)
	}
	if strings.Contains(got, "secret-key") {
		t.Errorf("key %q contains the raw identity", got)
	}
}

func TestNew_OnRejectCalledOn429(t *testing.T) {
	var rejects int
	h := New(Config{Route: "/api", RequestsPerSecond: 0.001, Burst: 1, Strategy: KeyAPIKey, OnReject: func() { rejects++ }}, NewMemoryStore())(okHandler())
	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), identityRequest("u"))
	}
	if rejects != 1 {
		t.Errorf("OnReject called %d times, want 1", rejects)
	}
}

// Without a fallback wrapper, a store error must not let traffic through
// unlimited.
func TestNew_StoreErrorFailsClosed503(t *testing.T) {
	called := false
	store := &recordingStore{err: errors.New("valkey down")}
	h := New(Config{Route: "/api", RequestsPerSecond: 1, Burst: 1, Strategy: KeyAPIKey}, store)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, identityRequest("u"))
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Errorf("status %d, next called %v; want 503 and not called", rec.Code, called)
	}
}
