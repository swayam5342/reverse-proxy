package bodylimit

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readingHandler reads the whole body, reporting 413 if the limit tripped
// mid-read (the case for chunked bodies with no Content-Length).
func readingHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		_, err := io.ReadAll(r.Body)
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestNew(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		contentLength int64 // -1 = unknown (chunked)
		wantStatus    int
		wantCalled    bool
	}{
		{"under limit", "12345", 5, http.StatusOK, true},
		{"exactly at limit", "1234567890", 10, http.StatusOK, true},
		{"declared length over limit rejected before handler", "12345678901", 11, http.StatusRequestEntityTooLarge, false},
		{"chunked body over limit trips while reading", "12345678901", -1, http.StatusRequestEntityTooLarge, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called bool
			h := New(10)(readingHandler(&called))

			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			req.ContentLength = tt.contentLength
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if called != tt.wantCalled {
				t.Errorf("handler called = %v, want %v", called, tt.wantCalled)
			}
		})
	}
}
