package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/swayam5342/jgateway/pkg/middleware"
)

func mustJWT(t *testing.T, cfg JWTConfig) middleware.Middleware {
	t.Helper()
	mw, err := NewJWT(cfg)
	if err != nil {
		t.Fatalf("NewJWT: %v", err)
	}
	return mw
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub": "user-1",
		"iss": "https://issuer.example",
		"aud": "my-api",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

func without(c jwt.MapClaims, key string) jwt.MapClaims {
	out := jwt.MapClaims{}
	for k, v := range c {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func with(c jwt.MapClaims, key string, val any) jwt.MapClaims {
	out := without(c, key)
	out[key] = val
	return out
}

func sign(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return s
}

func status(mw middleware.Middleware, token string) int {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mw(okHandler()).ServeHTTP(rec, req)
	return rec.Code
}

func TestJWT_RequiredClaims(t *testing.T) {
	secret := []byte("test-secret")
	mw := mustJWT(t, JWTConfig{Secret: secret, Issuer: "https://issuer.example", Audience: "my-api"})

	tests := []struct {
		name   string
		claims jwt.MapClaims
		want   int
	}{
		{"all claims valid", validClaims(), http.StatusOK},
		{"missing exp never expires: rejected", without(validClaims(), "exp"), http.StatusUnauthorized},
		{"missing sub: rejected", without(validClaims(), "sub"), http.StatusUnauthorized},
		{"wrong issuer", with(validClaims(), "iss", "https://evil.example"), http.StatusUnauthorized},
		{"missing issuer", without(validClaims(), "iss"), http.StatusUnauthorized},
		{"wrong audience", with(validClaims(), "aud", "other-api"), http.StatusUnauthorized},
		{"audience in list", with(validClaims(), "aud", []string{"other-api", "my-api"}), http.StatusOK},
		{"not yet valid", with(validClaims(), "nbf", time.Now().Add(time.Hour).Unix()), http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := status(mw, sign(t, jwt.SigningMethodHS256, secret, tt.claims)); got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestJWT_IssuerAudienceOptional(t *testing.T) {
	secret := []byte("test-secret")
	mw := mustJWT(t, JWTConfig{Secret: secret})
	claims := without(without(validClaims(), "iss"), "aud")
	if got := status(mw, sign(t, jwt.SigningMethodHS256, secret, claims)); got != http.StatusOK {
		t.Errorf("status = %d, want 200 when issuer/audience not configured", got)
	}
}

func TestJWT_LeewayToleratesClockSkew(t *testing.T) {
	secret := []byte("test-secret")
	claims := with(validClaims(), "exp", time.Now().Add(-10*time.Second).Unix())
	token := sign(t, jwt.SigningMethodHS256, secret, claims)

	if got := status(mustJWT(t, JWTConfig{Secret: secret}), token); got != http.StatusUnauthorized {
		t.Errorf("no leeway: status = %d, want 401", got)
	}
	if got := status(mustJWT(t, JWTConfig{Secret: secret, Leeway: time.Minute}), token); got != http.StatusOK {
		t.Errorf("1m leeway: status = %d, want 200", got)
	}
}

func pemBlock(t *testing.T, typ string, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func pkixPEM(t *testing.T, pub crypto.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return pemBlock(t, "PUBLIC KEY", der)
}

// Identity providers (Auth0, Cognito, Keycloak, Google) sign with
// asymmetric keys; the gateway verifies with their public key or cert.
func TestJWT_AsymmetricKeys(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)

	certTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "issuer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, certTmpl, certTmpl, &rsaKey.PublicKey, rsaKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	tests := []struct {
		name   string
		pem    []byte
		method jwt.SigningMethod
		priv   any
	}{
		{"RS256 public key", pkixPEM(t, &rsaKey.PublicKey), jwt.SigningMethodRS256, rsaKey},
		{"PS256 public key", pkixPEM(t, &rsaKey.PublicKey), jwt.SigningMethodPS256, rsaKey},
		{"RS256 certificate", pemBlock(t, "CERTIFICATE", certDER), jwt.SigningMethodRS256, rsaKey},
		{"RS256 PKCS1 key", pemBlock(t, "RSA PUBLIC KEY", x509.MarshalPKCS1PublicKey(&rsaKey.PublicKey)), jwt.SigningMethodRS256, rsaKey},
		{"ES256 public key", pkixPEM(t, &ecKey.PublicKey), jwt.SigningMethodES256, ecKey},
		{"EdDSA public key", pkixPEM(t, edPub), jwt.SigningMethodEdDSA, edPriv},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := mustJWT(t, JWTConfig{PublicKeyPEM: tt.pem})
			if got := status(mw, sign(t, tt.method, tt.priv, validClaims())); got != http.StatusOK {
				t.Errorf("status = %d, want 200", got)
			}
		})
	}

	// Algorithm confusion: an attacker HMAC-signs a token using the
	// (public) PEM bytes as the secret. Must be rejected.
	t.Run("HS256 signed with public key bytes rejected", func(t *testing.T) {
		pubPEM := pkixPEM(t, &rsaKey.PublicKey)
		mw := mustJWT(t, JWTConfig{PublicKeyPEM: pubPEM})
		if got := status(mw, sign(t, jwt.SigningMethodHS256, pubPEM, validClaims())); got != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", got)
		}
	})

	t.Run("token signed by a different key rejected", func(t *testing.T) {
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		mw := mustJWT(t, JWTConfig{PublicKeyPEM: pkixPEM(t, &rsaKey.PublicKey)})
		if got := status(mw, sign(t, jwt.SigningMethodRS256, other, validClaims())); got != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", got)
		}
	})
}

func TestNewJWT_InvalidConfig(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	tests := []struct {
		name string
		cfg  JWTConfig
	}{
		{"no key", JWTConfig{}},
		{"both secret and public key", JWTConfig{Secret: []byte("s"), PublicKeyPEM: pkixPEM(t, &rsaKey.PublicKey)}},
		{"garbage PEM", JWTConfig{PublicKeyPEM: []byte("not a pem")}},
		{"private key instead of public", JWTConfig{PublicKeyPEM: pemBlock(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewJWT(tt.cfg); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}
