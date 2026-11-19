package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestJWT_ValidToken(t *testing.T) {
	secret := []byte("test-secret")
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "user-1",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	signed, err := token.SignedString(secret)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}

	h := mustJWT(t, JWTConfig{Secret: secret})(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestJWT_FailsClosed(t *testing.T) {
	secret := []byte("test-secret")
	expired := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "user-1",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
	})
	expiredSigned, _ := expired.SignedString(secret)

	wrongKeySigned, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject: "user-1",
	}).SignedString([]byte("wrong-secret"))

	tests := []struct {
		name string
		auth string
	}{
		{"no header", ""},
		{"malformed", "not-a-bearer-token"},
		{"expired", "Bearer " + expiredSigned},
		{"wrong signature", "Bearer " + wrongKeySigned},
		{"garbage token", "Bearer garbage.garbage.garbage"},
	}

	h := mustJWT(t, JWTConfig{Secret: secret})(okHandler())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", rec.Code)
			}
		})
	}
}

func TestAPIKey_ValidAndInvalid(t *testing.T) {
	h := NewAPIKey(APIKeyConfig{ValidKeys: []string{"key-a", "key-b"}})(okHandler())

	t.Run("valid key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-API-Key", "key-b")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("invalid key fails closed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-API-Key", "wrong")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})

	t.Run("missing key fails closed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rec.Code)
		}
	})
}

func TestAPIKey_CustomHeader(t *testing.T) {
	h := NewAPIKey(APIKeyConfig{ValidKeys: []string{"key-a"}, HeaderName: "X-Custom-Key"})(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Custom-Key", "key-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}
