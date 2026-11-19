package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/valkey-io/valkey-go"
)

// ErrStoreUnavailable is returned by a Store that can't answer right now
// (e.g. Valkey is unreachable). FallbackStore treats it as a signal to
// answer from its in-memory fallback.
var ErrStoreUnavailable = errors.New("ratelimit: store unavailable")

// tokenBucketScript is the same token bucket as MemoryStore, run atomically
// inside Valkey. It uses Valkey's own clock (TIME) so gateway copies with
// skewed clocks agree, and expires the key once it would have refilled to
// full (plus 1s), which is the shared-store equivalent of MemoryStore's
// sweep. Returns 1 if allowed, 0 if not.
var tokenBucketScript = valkey.NewLuaScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local rps, burst = tonumber(ARGV[1]), tonumber(ARGV[2])
local v = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens, ts = tonumber(v[1]), tonumber(v[2])
if tokens == nil then tokens, ts = burst, now end
tokens = math.min(burst, tokens + math.max(0, now - ts) / 1000 * rps)
local allowed = 0
if tokens >= 1 then tokens = tokens - 1; allowed = 1 end
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', now)
redis.call('PEXPIRE', KEYS[1], math.ceil(burst / rps * 1000) + 1000)
return allowed
`)

// ValkeyStore is a Store shared by every gateway copy pointed at the same
// Valkey. It connects lazily: if Valkey is down at startup (or the first
// connect fails), each later call retries the connect, so the gateway
// recovers without a restart. While one call is connecting, concurrent
// calls return ErrStoreUnavailable immediately instead of queueing.
type ValkeyStore struct {
	connect func() (valkey.Client, error)

	connectMu sync.Mutex // held only while dialing

	mu     sync.RWMutex
	client valkey.Client
	closed bool
}

// NewValkeyStore returns a store that obtains its client from connect on
// first use (and again after a failed attempt). connect should bound its
// own dial time (valkey.ClientOption.Dialer.Timeout).
func NewValkeyStore(connect func() (valkey.Client, error)) *ValkeyStore {
	return &ValkeyStore{connect: connect}
}

// Allow implements Store. It ignores now: the script uses Valkey's clock.
func (s *ValkeyStore) Allow(ctx context.Context, key string, rps float64, burst int, _ time.Time) (bool, error) {
	c, err := s.getClient()
	if err != nil {
		return false, err
	}
	n, err := tokenBucketScript.Exec(ctx, c, []string{key}, []string{
		strconv.FormatFloat(rps, 'f', -1, 64),
		strconv.Itoa(burst),
	}).AsInt64()
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	return n == 1, nil
}

// Ping reports whether Valkey is reachable, connecting first if needed.
// Used for the informational valkey entry in /readyz.
func (s *ValkeyStore) Ping(ctx context.Context) error {
	c, err := s.getClient()
	if err != nil {
		return err
	}
	if err := c.Do(ctx, c.B().Ping().Build()).Error(); err != nil {
		return fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	return nil
}

// Close closes the Valkey client, if one was connected. A closed store
// never reconnects; every later call returns ErrStoreUnavailable.
func (s *ValkeyStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
}

func (s *ValkeyStore) getClient() (valkey.Client, error) {
	s.mu.RLock()
	c, closed := s.client, s.closed
	s.mu.RUnlock()
	if closed {
		return nil, fmt.Errorf("%w: closed", ErrStoreUnavailable)
	}
	if c != nil {
		return c, nil
	}

	if !s.connectMu.TryLock() {
		return nil, fmt.Errorf("%w: connect in progress", ErrStoreUnavailable)
	}
	defer s.connectMu.Unlock()

	s.mu.RLock()
	c = s.client
	s.mu.RUnlock()
	if c != nil {
		return c, nil
	}

	c, err := s.connect()
	if err != nil {
		return nil, fmt.Errorf("%w: connect: %w", ErrStoreUnavailable, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return nil, fmt.Errorf("%w: closed", ErrStoreUnavailable)
	}
	s.client = c
	return c, nil
}
