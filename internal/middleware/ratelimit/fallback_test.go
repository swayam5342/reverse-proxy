package ratelimit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// funcStore is a Store whose answer comes from fn, counting calls.
type funcStore struct {
	calls atomic.Int32
	fn    func(ctx context.Context) (bool, error)
}

func (s *funcStore) Allow(ctx context.Context, _ string, _ float64, _ int, _ time.Time) (bool, error) {
	s.calls.Add(1)
	return s.fn(ctx)
}

func failing() *funcStore {
	return &funcStore{fn: func(context.Context) (bool, error) { return false, ErrStoreUnavailable }}
}

func call(t *testing.T, s Store) bool {
	t.Helper()
	ok, err := s.Allow(context.Background(), "k", 1, 1, time.Now())
	if err != nil {
		t.Fatalf("FallbackStore.Allow returned error %v; it must never fail", err)
	}
	return ok
}

func TestFallback_PrimaryHealthyUsesPrimary(t *testing.T) {
	primary := &funcStore{fn: func(context.Context) (bool, error) { return false, nil }}
	fallback := &recordingStore{allowed: true}
	s := NewFallbackStore(primary, fallback, FallbackConfig{})

	if call(t, s) {
		t.Error("got fallback's answer (allowed); want primary's (denied)")
	}
	if len(fallback.keys) != 0 {
		t.Errorf("fallback called %d times, want 0", len(fallback.keys))
	}
}

func TestFallback_PrimaryErrorAnswersFromFallback(t *testing.T) {
	var fallbacks int
	fallback := &recordingStore{allowed: true}
	s := NewFallbackStore(failing(), fallback, FallbackConfig{OnFallback: func() { fallbacks++ }})

	if !call(t, s) {
		t.Error("want the fallback's answer (allowed)")
	}
	if len(fallback.keys) != 1 || fallbacks != 1 {
		t.Errorf("fallback calls = %d, OnFallback calls = %d; want 1 and 1", len(fallback.keys), fallbacks)
	}
}

// During an outage, every request must not pay the primary's timeout:
// after a failure the primary is skipped until the cool-down passes.
func TestFallback_CooldownSkipsPrimary(t *testing.T) {
	primary := failing()
	s := NewFallbackStore(primary, &recordingStore{allowed: true}, FallbackConfig{Cooldown: 50 * time.Millisecond})

	call(t, s)
	call(t, s)
	if n := primary.calls.Load(); n != 1 {
		t.Fatalf("primary called %d times within cool-down, want 1", n)
	}
	time.Sleep(60 * time.Millisecond)
	call(t, s)
	if n := primary.calls.Load(); n != 2 {
		t.Errorf("primary called %d times after cool-down, want 2 (retried)", n)
	}
}

func TestFallback_SlowPrimaryTimesOut(t *testing.T) {
	slow := &funcStore{fn: func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}}
	s := NewFallbackStore(slow, &recordingStore{allowed: true}, FallbackConfig{Timeout: 20 * time.Millisecond})

	start := time.Now()
	if !call(t, s) {
		t.Error("want the fallback's answer after the primary timed out")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("call took %v, want ~20ms", elapsed)
	}
}

func TestFallback_LogsAtMostOncePer10s(t *testing.T) {
	var buf bytes.Buffer
	s := NewFallbackStore(failing(), &recordingStore{allowed: true}, FallbackConfig{
		Cooldown: time.Nanosecond, // hit the primary every time
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	})
	for range 5 {
		call(t, s)
		time.Sleep(time.Millisecond)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Errorf("logged %d lines for 5 failures, want 1:\n%s", n, buf.String())
	}
}
