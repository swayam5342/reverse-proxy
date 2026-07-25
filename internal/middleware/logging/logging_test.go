package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogging_GeneratesRequestIDAndLogsLine(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	var seenID string
	h := New(logger, "test-route")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := RequestIDFromContext(r.Context())
		seenID = id
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seenID == "" {
		t.Fatal("expected a generated request id in context")
	}
	if got := rec.Header().Get(requestIDHeader); got != seenID {
		t.Errorf("response header request id %q != context id %q", got, seenID)
	}

	var logLine map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logLine); err != nil {
		t.Fatalf("expected one JSON log line, got %q: %v", buf.String(), err)
	}
	if logLine["request_id"] != seenID {
		t.Errorf("log request_id = %v, want %v", logLine["request_id"], seenID)
	}
	if logLine["status"] != float64(http.StatusTeapot) {
		t.Errorf("log status = %v, want %v", logLine["status"], http.StatusTeapot)
	}
}

func TestLogging_PropagatesExistingRequestID(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	h := New(logger, "test-route")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(requestIDHeader, "client-supplied-id")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get(requestIDHeader); got != "client-supplied-id" {
		t.Errorf("expected propagated request id, got %q", got)
	}
}
