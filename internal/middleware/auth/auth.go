// Package auth verifies JWT bearer tokens or API keys before allowing a
// request through the chain. Per AGENTS.md, this middleware fails closed:
// any error validating the credential results in 401, never a silent
// pass-through.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/swayam5342/jgateway/pkg/middleware"
)

type contextKey string

const identityContextKey contextKey = "auth.identity"

// Identity is the authenticated caller, attached to the request context on
// success so downstream middleware (e.g. rate limiting by API key) can read
// it via IdentityFromContext.
type Identity struct {
	Subject string
	Method  string // "jwt" or "api_key"
}

// IdentityFromContext returns the Identity attached by this middleware, if
// any request upstream of it authenticated successfully.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey).(Identity)
	return id, ok
}

// NewContext attaches id to ctx the same way the auth middleware does on
// success. Exported for tests and for handlers composing chains outside
// this package.
func NewContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityContextKey, id)
}

// JWTConfig configures bearer-token verification.
type JWTConfig struct {
	// Secret is the HMAC signing key. Must come from an env var or secrets
	// manager — never hardcoded, never logged.
	Secret []byte
}

// NewJWT returns a middleware that verifies the Authorization: Bearer <jwt>
// header using HMAC signature verification. Any missing header, malformed
// token, bad signature, or expired token results in 401.
func NewJWT(cfg JWTConfig) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(authz, prefix) {
				http.Error(w, "missing or malformed authorization header", http.StatusUnauthorized)
				return
			}
			raw := strings.TrimPrefix(authz, prefix)

			token, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, jwt.ErrTokenSignatureInvalid
				}
				return cfg.Secret, nil
			}, jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}))
			if err != nil || !token.Valid {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}

			subject, err := token.Claims.GetSubject()
			if err != nil {
				http.Error(w, "invalid token claims", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), identityContextKey, Identity{Subject: subject, Method: "jwt"})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// APIKeyConfig configures API-key verification.
type APIKeyConfig struct {
	// ValidKeys is the set of accepted keys, sourced from env/secrets.
	ValidKeys []string
	// HeaderName defaults to "X-API-Key" if empty.
	HeaderName string
}

// NewAPIKey returns a middleware that verifies a request-supplied API key
// against ValidKeys using a constant-time comparison, so response timing
// can't be used to discover valid keys. Any missing or unrecognized key
// results in 401.
func NewAPIKey(cfg APIKeyConfig) middleware.Middleware {
	header := cfg.HeaderName
	if header == "" {
		header = "X-API-Key"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			supplied := r.Header.Get(header)
			if supplied == "" {
				http.Error(w, "missing api key", http.StatusUnauthorized)
				return
			}

			var matched bool
			for _, valid := range cfg.ValidKeys {
				if subtle.ConstantTimeCompare([]byte(supplied), []byte(valid)) == 1 {
					matched = true
				}
			}
			if !matched {
				http.Error(w, "invalid api key", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), identityContextKey, Identity{Subject: supplied, Method: "api_key"})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
