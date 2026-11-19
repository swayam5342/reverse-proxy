package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Store is the pluggable backing store for rate limit counters. A memory
// store is provided for single-instance deployments; a Redis-backed Store
// can be swapped in for multi-instance deployments without changing the
// middleware.
type Store interface {
	// Allow reports whether a request for key is allowed right now, given
	// rps (sustained rate) and burst (bucket capacity). It mutates the
	// stored bucket state as a side effect of the check. A store backed by
	// the network honours ctx and returns an error if it can't answer;
	// now is the caller's clock, which such a store may ignore in favour of
	// its own.
	Allow(ctx context.Context, key string, rps float64, burst int, now time.Time) (bool, error)
}

// sweepInterval is how often MemoryStore drops buckets that have refilled.
const sweepInterval = time.Minute

// bucket is a token bucket: tokens replenish continuously at rps and cap at
// burst capacity. rps and burst are remembered so a sweep can tell when the
// bucket has refilled.
type bucket struct {
	tokens     float64
	lastRefill time.Time
	rps        float64
	burst      int
}

// full reports whether b would be at capacity if refilled at now, i.e. it
// is indistinguishable from a freshly created bucket.
func (b *bucket) full(now time.Time) bool {
	return b.tokens+now.Sub(b.lastRefill).Seconds()*b.rps >= float64(b.burst)
}

// MemoryStore is an in-process Store keyed by string, guarded by a single
// mutex. Fine for a single gateway instance; use a distributed Store when
// running multiple replicas that must share a limit.
//
// Memory is bounded by the number of keys active within roughly one refill
// period (burst/rps): at most once per sweepInterval, Allow drops every
// bucket that has refilled to capacity. Dropping a full bucket is lossless,
// since a missing key starts as a full bucket anyway. Sweeping inline
// avoids a background goroutine that would need its own lifecycle.
type MemoryStore struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

// NewMemoryStore returns an empty in-memory token bucket store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{buckets: make(map[string]*bucket)}
}

// Allow implements Store using a token bucket: on each call it refills
// tokens for elapsed time (capped at burst) and consumes one token if
// available. It never returns an error.
func (s *MemoryStore) Allow(_ context.Context, key string, rps float64, burst int, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.maybeSweepLocked(now)

	b, ok := s.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(burst), lastRefill: now}
		s.buckets[key] = b
	}
	b.rps, b.burst = rps, burst

	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rps
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
		b.lastRefill = now
	}

	if b.tokens < 1 {
		return false, nil
	}
	b.tokens--
	return true, nil
}

// maybeSweepLocked drops refilled buckets if sweepInterval has elapsed
// since the last sweep. Callers must hold s.mu.
func (s *MemoryStore) maybeSweepLocked(now time.Time) {
	if s.lastSweep.IsZero() {
		s.lastSweep = now
		return
	}
	if now.Sub(s.lastSweep) < sweepInterval {
		return
	}
	s.lastSweep = now
	for key, b := range s.buckets {
		if b.full(now) {
			delete(s.buckets, key)
		}
	}
}

// Len returns the number of buckets currently held.
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buckets)
}
