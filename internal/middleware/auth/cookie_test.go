package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// Browsers attach cookies to cross-site requests automatically, so a token
// that arrived in a cookie must also pass a cross-origin (CSRF) check for
// state-changing methods. A bearer header can't be attached by a foreign
// site, so it skips that check.
func TestJWT_Cookie(t *testing.T) {
	secret := []byte("test-secret")
	token := sign(t, jwt.SigningMethodHS256, secret, validClaims())
	mw := mustJWT(t, JWTConfig{Secret: secret, Cookie: "session", TrustedOrigins: []string{"https://app.example.com"}})
	noCookieMW := mustJWT(t, JWTConfig{Secret: secret})

	tests := []struct {
		name    string
		mw      func(http.Handler) http.Handler
		method  string
		bearer  string // full Authorization header value
		cookie  string
		headers map[string]string
		want    int
	}{
		{name: "cookie GET", mw: mw, method: http.MethodGet, cookie: token, want: http.StatusOK},
		{name: "cookie POST cross-site", mw: mw, method: http.MethodPost, cookie: token,
			headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, want: http.StatusForbidden},
		{name: "cookie POST from trusted origin", mw: mw, method: http.MethodPost, cookie: token,
			headers: map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://app.example.com"}, want: http.StatusOK},
		{name: "cookie POST same-origin", mw: mw, method: http.MethodPost, cookie: token,
			headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, want: http.StatusOK},
		{name: "bearer POST cross-site skips CSRF", mw: mw, method: http.MethodPost, bearer: "Bearer " + token,
			headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, want: http.StatusOK},
		{name: "malformed header does not fall back to cookie", mw: mw, method: http.MethodGet, bearer: "Basic abc", cookie: token, want: http.StatusUnauthorized},
		{name: "invalid cookie token", mw: mw, method: http.MethodGet, cookie: "garbage", want: http.StatusUnauthorized},
		{name: "neither header nor cookie", mw: mw, method: http.MethodGet, want: http.StatusUnauthorized},
		{name: "cookie ignored when not configured", mw: noCookieMW, method: http.MethodGet, cookie: token, want: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "https://api.example.com/x", nil)
			if tt.bearer != "" {
				req.Header.Set("Authorization", tt.bearer)
			}
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "session", Value: tt.cookie})
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			tt.mw(okHandler()).ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestNewJWT_InvalidTrustedOrigin(t *testing.T) {
	_, err := NewJWT(JWTConfig{Secret: []byte("s"), Cookie: "session", TrustedOrigins: []string{"not an origin"}})
	if !errors.Is(err, ErrInvalidJWTConfig) {
		t.Errorf("err = %v, want ErrInvalidJWTConfig", err)
	}
}
