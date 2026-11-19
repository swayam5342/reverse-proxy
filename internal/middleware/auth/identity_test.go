package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestParseAPIKeys(t *testing.T) {
	sum := sha256.Sum256([]byte("bare"))
	bareName := "key-" + hex.EncodeToString(sum[:])[:8]

	tests := []struct {
		in   string
		want []APIKey
	}{
		{"billing:s1,reports:s2", []APIKey{{"billing", "s1"}, {"reports", "s2"}}},
		{"a:k1, b:k2,", []APIKey{{"a", "k1"}, {"b", "k2"}}},
		{"bare", []APIKey{{bareName, "bare"}}},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := ParseAPIKeys(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseAPIKeys(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// The raw key is a secret: the identity, the forwarded headers and the
// rate-limit key must use the key's name instead.
func TestAPIKey_SubjectIsNameNotKey(t *testing.T) {
	var (
		gotID  Identity
		gotHdr http.Header
	)
	h := NewAPIKey(APIKeyConfig{Keys: ParseAPIKeys("billing:s1")})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, _ = IdentityFromContext(r.Context())
		gotHdr = r.Header.Clone()
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "s1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotID.Subject != "billing" {
		t.Errorf("identity subject = %q, want billing", gotID.Subject)
	}
	if gotHdr.Get(HeaderSubject) != "billing" || gotHdr.Get(HeaderMethod) != "api_key" {
		t.Errorf("identity headers = %q/%q, want billing/api_key", gotHdr.Get(HeaderSubject), gotHdr.Get(HeaderMethod))
	}
	for name, vals := range gotHdr {
		if name != "X-Api-Key" && strings.Contains(strings.Join(vals, ","), "s1") {
			t.Errorf("header %s carries the raw key", name)
		}
	}
}

func TestJWT_SetsIdentityHeaders(t *testing.T) {
	secret := []byte("test-secret")
	var got http.Header
	mw := mustJWT(t, JWTConfig{Secret: secret})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+sign(t, jwt.SigningMethodHS256, secret, validClaims()))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got.Get(HeaderSubject) != "user-1" || got.Get(HeaderMethod) != "jwt" {
		t.Errorf("identity headers = %q/%q, want user-1/jwt", got.Get(HeaderSubject), got.Get(HeaderMethod))
	}
}
