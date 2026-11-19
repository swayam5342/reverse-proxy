package signing

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func okHandlerEchoingBody() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	})
}

func signedRequest(t *testing.T, secret []byte, method, path string, body []byte, ts time.Time) *http.Request {
	t.Helper()
	tsUnix := ts.Unix()
	sig := Sign(secret, method, path, tsUnix, body)
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("X-Signature", sig)
	req.Header.Set("X-Signature-Timestamp", strconv.FormatInt(tsUnix, 10))
	return req
}

func TestSigning_ValidSignature(t *testing.T) {
	secret := []byte("shared-secret")
	body := []byte(`{"hello":"world"}`)
	req := signedRequest(t, secret, http.MethodPost, "/api/thing", body, time.Now())

	h := New(Config{Secret: secret})(okHandlerEchoingBody())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(body) {
		t.Errorf("expected body to be forwarded intact, got %q", rec.Body.String())
	}
}

func TestSigning_TamperedBodyRejected(t *testing.T) {
	secret := []byte("shared-secret")
	req := signedRequest(t, secret, http.MethodPost, "/api/thing", []byte("original"), time.Now())
	req.Body = io.NopCloser(bytes.NewReader([]byte("tampered")))

	h := New(Config{Secret: secret})(okHandlerEchoingBody())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for tampered body, got %d", rec.Code)
	}
}

func TestSigning_ExpiredTimestampRejected(t *testing.T) {
	secret := []byte("shared-secret")
	req := signedRequest(t, secret, http.MethodGet, "/api/thing", nil, time.Now().Add(-time.Hour))

	h := New(Config{Secret: secret, Tolerance: 5 * time.Minute})(okHandlerEchoingBody())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for stale timestamp, got %d", rec.Code)
	}
}

func TestSigning_MissingHeadersRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/thing", nil)
	h := New(Config{Secret: []byte("s")})(okHandlerEchoingBody())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing headers, got %d", rec.Code)
	}
}

func TestSigning_WrongSecretRejected(t *testing.T) {
	req := signedRequest(t, []byte("secret-a"), http.MethodGet, "/api/thing", nil, time.Now())
	h := New(Config{Secret: []byte("secret-b")})(okHandlerEchoingBody())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong secret, got %d", rec.Code)
	}
}

// When the body-limit middleware's cap trips while signing buffers the body,
// the client must get 413, not a generic 400.
func TestSigning_OversizedBodyReturns413(t *testing.T) {
	secret := []byte("shared-secret")
	body := bytes.Repeat([]byte("x"), 100)
	req := signedRequest(t, secret, http.MethodPost, "/api/thing", body, time.Now())
	rec := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(rec, req.Body, 10)

	New(Config{Secret: secret})(okHandlerEchoingBody()).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", rec.Code)
	}
}
