package ratelimit

import (
	"strconv"
	"testing"
	"time"
)

// Every distinct key (e.g. every client IP ever seen) used to stay in memory
// forever. A bucket that has refilled to full is indistinguishable from a
// brand-new one, so the store can drop it without changing behavior.
func TestMemoryStore_EvictsRefilledBuckets(t *testing.T) {
	s := NewMemoryStore()
	t0 := time.Now()
	for i := range 1000 {
		s.Allow("ip-"+strconv.Itoa(i), 1, 2, t0)
	}

	// Keep one key hot: drained just before the sweep, so it is not full.
	hotAt := t0.Add(sweepInterval)
	s.Allow("hot", 1, 2, hotAt)
	s.Allow("hot", 1, 2, hotAt)

	// Trigger a sweep well after the cold buckets have refilled.
	s.Allow("trigger", 1, 2, hotAt.Add(time.Millisecond))

	if n := s.Len(); n != 2 {
		t.Fatalf("Len() after sweep = %d, want 2 (hot + trigger)", n)
	}
	// The hot key kept its state: still empty, so the next request is denied.
	if s.Allow("hot", 1, 2, hotAt.Add(2*time.Millisecond)) {
		t.Error("hot bucket lost its state during sweep")
	}
}

func TestMemoryStore_NoSweepBeforeInterval(t *testing.T) {
	s := NewMemoryStore()
	t0 := time.Now()
	for i := range 10 {
		s.Allow("ip-"+strconv.Itoa(i), 1000, 1, t0)
	}
	s.Allow("x", 1000, 1, t0.Add(sweepInterval/2))
	if n := s.Len(); n != 11 {
		t.Errorf("Len() = %d, want 11 (no sweep before interval)", n)
	}
}
