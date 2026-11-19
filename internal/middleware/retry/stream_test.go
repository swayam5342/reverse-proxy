package retry

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func fastConfig(attempts int) Config {
	return Config{MaxAttempts: attempts, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
}

// Successful responses must stream straight through: buffering them would
// hold arbitrarily large responses in memory and break SSE/streaming.
func TestRetry_SuccessfulResponseStreamsWithoutBuffering(t *testing.T) {
	under := httptest.NewRecorder()
	var sawStreamed bool
	h := New(fastConfig(3))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("chunk1"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		// Before the handler returns, the client side must already have it.
		sawStreamed = under.Flushed && under.Body.String() == "chunk1"
		_, _ = w.Write([]byte("chunk2"))
	}))

	h.ServeHTTP(under, httptest.NewRequest(http.MethodGet, "/", nil))

	if !sawStreamed {
		t.Error("first chunk was not delivered before the handler finished")
	}
	if got := under.Body.String(); got != "chunk1chunk2" {
		t.Errorf("body = %q, want chunk1chunk2", got)
	}
}

// Bodies of discarded (retried) attempts must never leak into the response.
func TestRetry_DiscardsBodiesOfRetriedAttempts(t *testing.T) {
	var calls int32
	h := New(fastConfig(3))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("X-Attempt", "first")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("first-error"))
			return
		}
		w.Header().Set("X-Attempt", "second")
		_, _ = w.Write([]byte("second-ok"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "second-ok" {
		t.Errorf("got %d %q, want 200 \"second-ok\"", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Values("X-Attempt"); len(got) != 1 || got[0] != "second" {
		t.Errorf("X-Attempt = %v, want only the final attempt's header", got)
	}
}

// If the client goes away (or the request deadline passes) during backoff,
// the last error response is what gets written, not an empty 200.
func TestRetry_ContextDoneDuringBackoffReturnsLastError(t *testing.T) {
	var calls int32
	h := New(Config{MaxAttempts: 3, BaseBackoff: time.Hour, MaxBackoff: time.Hour})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("down"))
		}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))

	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "down" {
		t.Errorf("got %d %q, want 503 \"down\"", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// The request body is replayed identically on each attempt.
func TestRetry_ReplaysRequestBody(t *testing.T) {
	var bodies []string
	h := New(fastConfig(2))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/", bytes.NewReader([]byte("payload"))))

	if len(bodies) != 2 || bodies[0] != "payload" || bodies[1] != "payload" {
		t.Errorf("bodies = %q, want payload twice", bodies)
	}
}

func TestRetry_OversizedBodyReturns413(t *testing.T) {
	var calls int32
	h := New(fastConfig(2))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(make([]byte, 100)))
	req.Body = http.MaxBytesReader(rec, req.Body, 10)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if calls != 0 {
		t.Errorf("upstream called %d times for an oversized body, want 0", calls)
	}
}

// Protocol upgrades (WebSockets) can't be replayed; they pass straight
// through so the handler can hijack the real connection.
func TestRetry_UpgradeRequestsPassThroughUnretried(t *testing.T) {
	var calls int32
	under := httptest.NewRecorder()
	var gotWriter http.ResponseWriter
	h := New(fastConfig(3))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotWriter = w
		w.WriteHeader(http.StatusBadGateway)
	}))

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	h.ServeHTTP(under, req)

	if calls != 1 {
		t.Errorf("calls = %d, want 1 (upgrades are never retried)", calls)
	}
	if gotWriter != http.ResponseWriter(under) {
		t.Error("upgrade request should get the real ResponseWriter so it can hijack")
	}
}
