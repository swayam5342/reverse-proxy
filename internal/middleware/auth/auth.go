// Package auth verifies JWT bearer tokens or API keys before allowing a
// request through the chain. Per AGENTS.md, this middleware fails closed:
// any error validating the credential results in 401, never a silent
// pass-through.
package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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

// ErrInvalidJWTConfig is returned by NewJWT when the verification key is
// missing, ambiguous, or unparseable.
var ErrInvalidJWTConfig = errors.New("auth: invalid jwt config")

// JWTConfig configures bearer-token verification. Exactly one of Secret or
// PublicKeyPEM must be set. Both must come from an env var or secrets
// manager — never hardcoded, never logged.
type JWTConfig struct {
	// Secret is a shared HMAC key; tokens must be HS256/HS384/HS512.
	Secret []byte
	// PublicKeyPEM is the issuer's public key ("PUBLIC KEY" or
	// "RSA PUBLIC KEY") or X.509 certificate ("CERTIFICATE") in PEM form.
	// The accepted algorithms follow from the key type: RSA → RS*/PS*,
	// ECDSA → ES*, Ed25519 → EdDSA. This is what identity providers such as
	// Auth0, Cognito, Keycloak and Google use.
	PublicKeyPEM []byte
	// Issuer, if set, must equal the token's "iss" claim.
	Issuer string
	// Audience, if set, must appear in the token's "aud" claim.
	Audience string
	// Leeway tolerates clock skew between the issuer and the gateway when
	// checking exp/nbf/iat.
	Leeway time.Duration
}

// NewJWT returns a middleware that verifies the Authorization: Bearer <jwt>
// header. A token is accepted only if its signature verifies with the
// configured key and an algorithm matching that key's type, it carries an
// unexpired "exp" and a non-empty "sub", and (when configured) its issuer
// and audience match. Anything else results in 401.
//
// NewJWT returns an error wrapping ErrInvalidJWTConfig if the key config is
// unusable, so a misconfigured gateway fails at startup.
func NewJWT(cfg JWTConfig) (middleware.Middleware, error) {
	key, algs, err := verificationKey(cfg)
	if err != nil {
		return nil, err
	}

	opts := []jwt.ParserOption{
		jwt.WithValidMethods(algs),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(cfg.Leeway),
	}
	if cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(cfg.Audience))
	}
	parser := jwt.NewParser(opts...)
	keyFunc := func(*jwt.Token) (any, error) { return key, nil }

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(authz, prefix) {
				http.Error(w, "missing or malformed authorization header", http.StatusUnauthorized)
				return
			}
			raw := strings.TrimPrefix(authz, prefix)

			// WithValidMethods rejects any alg outside the key's family
			// before keyFunc's key is used, which blocks alg:none and
			// RS256→HS256 confusion.
			token, err := parser.Parse(raw, keyFunc)
			if err != nil || !token.Valid {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}

			subject, err := token.Claims.GetSubject()
			if err != nil || subject == "" {
				http.Error(w, "invalid token claims", http.StatusUnauthorized)
				return
			}

			authenticated(w, r, next, Identity{Subject: subject, Method: "jwt"})
		})
	}, nil
}

// verificationKey resolves cfg to the key handed to the JWT parser and the
// algorithms allowed for it.
func verificationKey(cfg JWTConfig) (any, []string, error) {
	switch {
	case len(cfg.Secret) > 0 && len(cfg.PublicKeyPEM) > 0:
		return nil, nil, fmt.Errorf("%w: set either a secret or a public key, not both", ErrInvalidJWTConfig)
	case len(cfg.Secret) > 0:
		return cfg.Secret, []string{"HS256", "HS384", "HS512"}, nil
	case len(cfg.PublicKeyPEM) > 0:
		pub, err := parsePublicKeyPEM(cfg.PublicKeyPEM)
		if err != nil {
			return nil, nil, err
		}
		switch pub.(type) {
		case *rsa.PublicKey:
			return pub, []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"}, nil
		case *ecdsa.PublicKey:
			return pub, []string{"ES256", "ES384", "ES512"}, nil
		case ed25519.PublicKey:
			return pub, []string{"EdDSA"}, nil
		default:
			return nil, nil, fmt.Errorf("%w: unsupported public key type %T", ErrInvalidJWTConfig, pub)
		}
	default:
		return nil, nil, fmt.Errorf("%w: no secret or public key configured", ErrInvalidJWTConfig)
	}
}

func parsePublicKeyPEM(data []byte) (any, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%w: public key is not valid PEM", ErrInvalidJWTConfig)
	}
	var (
		pub any
		err error
	)
	switch block.Type {
	case "PUBLIC KEY":
		pub, err = x509.ParsePKIXPublicKey(block.Bytes)
	case "RSA PUBLIC KEY":
		pub, err = x509.ParsePKCS1PublicKey(block.Bytes)
	case "CERTIFICATE":
		var cert *x509.Certificate
		if cert, err = x509.ParseCertificate(block.Bytes); err == nil {
			pub = cert.PublicKey
		}
	default:
		return nil, fmt.Errorf("%w: unsupported PEM block %q (want PUBLIC KEY, RSA PUBLIC KEY or CERTIFICATE)", ErrInvalidJWTConfig, block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: parsing public key: %w", ErrInvalidJWTConfig, err)
	}
	return pub, nil
}

// APIKey is one accepted key and the name it authenticates as. The name is
// what identifies the caller everywhere (context identity, X-Auth-Subject,
// rate-limit key, logs); the key itself never leaves this middleware.
type APIKey struct {
	Name string
	Key  string
}

// ParseAPIKeys parses a comma-separated list of "name:key" entries, as held
// in the env var named by auth.api_keys_env. Whitespace is trimmed and
// empty entries are skipped. An entry without a name ("key") is accepted
// and named "key-" plus the first 8 hex digits of its SHA-256, so a bare
// key still never appears as an identity. Keys of unnamed entries must not
// contain ':'.
func ParseAPIKeys(s string) []APIKey {
	var keys []APIKey
	for entry := range strings.SplitSeq(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, key, named := strings.Cut(entry, ":")
		if !named {
			sum := sha256.Sum256([]byte(entry))
			name, key = "key-"+hex.EncodeToString(sum[:])[:8], entry
		}
		keys = append(keys, APIKey{Name: strings.TrimSpace(name), Key: strings.TrimSpace(key)})
	}
	return keys
}

// APIKeyConfig configures API-key verification.
type APIKeyConfig struct {
	// Keys is the set of accepted keys, sourced from env/secrets.
	Keys []APIKey
	// HeaderName defaults to "X-API-Key" if empty.
	HeaderName string
}

// NewAPIKey returns a middleware that verifies a request-supplied API key
// against Keys using a constant-time comparison over every key, so response
// timing can't be used to discover valid keys. Any missing or unrecognized
// key results in 401. On success the caller is identified by the key's
// name.
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

			matched := -1
			for i, k := range cfg.Keys {
				if subtle.ConstantTimeCompare([]byte(supplied), []byte(k.Key)) == 1 {
					matched = i
				}
			}
			if matched < 0 {
				http.Error(w, "invalid api key", http.StatusUnauthorized)
				return
			}

			authenticated(w, r, next, Identity{Subject: cfg.Keys[matched].Name, Method: "api_key"})
		})
	}
}

// Identity headers set on the request forwarded upstream after successful
// authentication. Client-supplied copies are stripped on entry by the
// sanitize middleware, so backends can trust them.
const (
	HeaderSubject = "X-Auth-Subject"
	HeaderMethod  = "X-Auth-Method"
)

// authenticated attaches id to the request context and identity headers,
// then calls next.
func authenticated(w http.ResponseWriter, r *http.Request, next http.Handler, id Identity) {
	r.Header.Set(HeaderSubject, id.Subject)
	r.Header.Set(HeaderMethod, id.Method)
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityContextKey, id)))
}
