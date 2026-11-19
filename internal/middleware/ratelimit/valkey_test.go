package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
)

// valkeyAddr returns VALKEY_ADDR or skips the test. Run locally with
//
//	docker run -d --rm -p 6379:6379 valkey/valkey:8.1.1
//	VALKEY_ADDR=localhost:6379 go test ./...
func valkeyAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		t.Skip("VALKEY_ADDR not set; skipping test that needs a real Valkey")
	}
	return addr
}

func connectTo(addr string) func() (valkey.Client, error) {
	return func() (valkey.Client, error) {
		return valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}, DisableCache: true})
	}
}

func newValkeyStore(t *testing.T) *ValkeyStore {
	t.Helper()
	s := NewValkeyStore(connectTo(valkeyAddr(t)))
	t.Cleanup(s.Close)
	return s
}

func uniqueKey(t *testing.T) string {
	return fmt.Sprintf("jgw:test:%s:%d", t.Name(), rand.Int64())
}

func mustAllow(t *testing.T, s Store, key string, rps float64, burst int) bool {
	t.Helper()
	ok, err := s.Allow(context.Background(), key, rps, burst, time.Now())
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	return ok
}

func TestValkeyStore_BurstThenBlocks(t *testing.T) {
	s, key := newValkeyStore(t), uniqueKey(t)
	for i := range 3 {
		if !mustAllow(t, s, key, 1, 3) {
			t.Fatalf("request %d within burst denied", i)
		}
	}
	if mustAllow(t, s, key, 1, 3) {
		t.Fatal("4th request allowed, burst should be exhausted")
	}
}

func TestValkeyStore_Refills(t *testing.T) {
	s, key := newValkeyStore(t), uniqueKey(t)
	if !mustAllow(t, s, key, 10, 1) {
		t.Fatal("first request denied")
	}
	if mustAllow(t, s, key, 10, 1) {
		t.Fatal("second immediate request allowed")
	}
	time.Sleep(150 * time.Millisecond) // 10 rps refills one token in 100ms
	if !mustAllow(t, s, key, 10, 1) {
		t.Fatal("request after refill denied")
	}
}

// Idle keys expire on their own once they'd be full again, so the store
// doesn't grow without bound.
func TestValkeyStore_KeyExpires(t *testing.T) {
	addr := valkeyAddr(t)
	s, key := newValkeyStore(t), uniqueKey(t)
	mustAllow(t, s, key, 10, 1) // full refill = 100ms, so PEXPIRE = 1100ms

	c, err := connectTo(addr)()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	pttl, err := c.Do(context.Background(), c.B().Pttl().Key(key).Build()).AsInt64()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if pttl <= 0 || pttl > 1100 {
		t.Errorf("PTTL = %dms, want in (0, 1100]", pttl)
	}
}

// The point of Valkey: separate gateway processes share one limit.
func TestValkeyStore_SharedAcrossClients(t *testing.T) {
	a, b, key := newValkeyStore(t), newValkeyStore(t), uniqueKey(t)
	if !mustAllow(t, a, key, 0.001, 1) {
		t.Fatal("first gateway denied")
	}
	if mustAllow(t, b, key, 0.001, 1) {
		t.Fatal("second gateway allowed: limit not shared")
	}
}

// Valkey may be down when the gateway starts: the store keeps trying to
// connect on later calls instead of staying broken until a restart.
func TestValkeyStore_ConnectsLazily(t *testing.T) {
	var calls atomic.Int32
	addr := os.Getenv("VALKEY_ADDR")
	s := NewValkeyStore(func() (valkey.Client, error) {
		if calls.Add(1) == 1 || addr == "" {
			return nil, errors.New("connection refused")
		}
		return connectTo(addr)()
	})
	defer s.Close()

	if _, err := s.Allow(context.Background(), uniqueKey(t), 1, 1, time.Now()); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Allow while down: err = %v, want ErrStoreUnavailable", err)
	}
	if addr == "" {
		t.Skip("VALKEY_ADDR not set; skipping the reconnect half")
	}
	if !mustAllow(t, s, uniqueKey(t), 1, 1) {
		t.Fatal("Allow after Valkey came back denied")
	}
	mustAllow(t, s, uniqueKey(t), 1, 1)
	if n := calls.Load(); n != 2 {
		t.Errorf("connect called %d times, want 2 (client cached once connected)", n)
	}
}

// While one request is (slowly) connecting, others must not queue behind
// it: they get ErrStoreUnavailable at once so the fallback can answer.
func TestValkeyStore_ConcurrentCallsDontWaitForConnect(t *testing.T) {
	release := make(chan struct{})
	s := NewValkeyStore(func() (valkey.Client, error) {
		<-release
		return nil, errors.New("still down")
	})
	defer s.Close()

	go func() { _, _ = s.Allow(context.Background(), "k", 1, 1, time.Now()) }()
	time.Sleep(20 * time.Millisecond) // let the first call take the connect lock

	start := time.Now()
	_, err := s.Allow(context.Background(), "k", 1, 1, time.Now())
	close(release)
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("err = %v, want ErrStoreUnavailable", err)
	}
	if waited := time.Since(start); waited > 10*time.Millisecond {
		t.Errorf("second call waited %v for the in-progress connect", waited)
	}
}

// After shutdown closes the store, in-flight requests must not dial a new
// connection.
func TestValkeyStore_ClosedStoreDoesNotReconnect(t *testing.T) {
	var calls atomic.Int32
	s := NewValkeyStore(func() (valkey.Client, error) {
		calls.Add(1)
		return nil, errors.New("down")
	})
	s.Close()
	if _, err := s.Allow(context.Background(), "k", 1, 1, time.Now()); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("err = %v, want ErrStoreUnavailable", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("connect called %d times after Close, want 0", n)
	}
}
