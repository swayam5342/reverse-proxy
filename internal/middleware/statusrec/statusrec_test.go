package statusrec

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecorder_RecordsStatus(t *testing.T) {
	tests := []struct {
		name  string
		write func(w http.ResponseWriter)
		want  int
	}{
		{"implicit 200 on write", func(w http.ResponseWriter) { _, _ = w.Write([]byte("x")) }, http.StatusOK},
		{"explicit status", func(w http.ResponseWriter) { w.WriteHeader(http.StatusTeapot) }, http.StatusTeapot},
		{"nothing written", func(http.ResponseWriter) {}, http.StatusOK},
		{"1xx then final", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusCreated)
		}, http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := New(httptest.NewRecorder())
			tt.write(rec)
			if rec.Status() != tt.want {
				t.Errorf("Status() = %d, want %d", rec.Status(), tt.want)
			}
		})
	}
}

// Streaming responses (SSE, chunked downloads) need Flush to reach the real
// connection through every wrapping middleware.
func TestRecorder_FlushReachesUnderlyingWriter(t *testing.T) {
	under := httptest.NewRecorder()
	rec := New(under)

	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush through ResponseController: %v", err)
	}
	if !under.Flushed {
		t.Error("underlying writer was not flushed")
	}
}

type hijackableWriter struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	c1, _ := net.Pipe()
	return c1, bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)), nil
}

// WebSocket upgrades through httputil.ReverseProxy hijack the connection via
// http.ResponseController; that must reach the real connection.
func TestRecorder_HijackReachesUnderlyingWriter(t *testing.T) {
	under := &hijackableWriter{ResponseRecorder: httptest.NewRecorder()}
	rec := New(under)

	conn, _, err := http.NewResponseController(rec).Hijack()
	if err != nil {
		t.Fatalf("Hijack through ResponseController: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if !under.hijacked {
		t.Error("underlying writer was not hijacked")
	}
	if rec.Status() != http.StatusSwitchingProtocols {
		t.Errorf("Status() after hijack = %d, want 101", rec.Status())
	}
}

func TestRecorder_HijackUnsupportedReportsErrNotSupported(t *testing.T) {
	rec := New(httptest.NewRecorder()) // ResponseRecorder can't hijack
	_, _, err := http.NewResponseController(rec).Hijack()
	if !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("Hijack error = %v, want http.ErrNotSupported", err)
	}
}
