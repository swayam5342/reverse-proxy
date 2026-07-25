package ratelimit

import (
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
	// stored bucket state as a side effect of the check.
	Allow(key string, rps float64, burst int, now time.Time) bool
}

// bucket is a token bucket: tokens replenish continuously at rps and cap at
// burst capacity.
type bucket struct {
	tokens     float64
	lastRefill time.Time
}

// MemoryStore is an in-process Store keyed by string, guarded by a single
// mutex. Fine for a single gateway instance; use a distributed Store when
// running multiple replicas that must share a limit.
type MemoryStore struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

// NewMemoryStore returns an empty in-memory token bucket store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{buckets: make(map[string]*bucket)}
}

// Allow implements Store using a token bucket: on each call it refills
// tokens for elapsed time (capped at burst) and consumes one token if
// available.
func (s *MemoryStore) Allow(key string, rps float64, burst int, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(burst), lastRefill: now}
		s.buckets[key] = b
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rps
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
		b.lastRefill = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
