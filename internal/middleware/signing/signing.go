// Package signing verifies HMAC-signed requests. The signature covers the
// method, path, timestamp, and body, so it can't be replayed against a
// different route or with a tampered payload. Verification always uses a
// constant-time comparison (hmac.Equal), never == on the signature.
package signing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/swayam5342/jgateway/pkg/middleware"
)

// Config configures signature verification for a route.
type Config struct {
	// Secret is the shared HMAC key, sourced from env/secrets — never
	// hardcoded, never logged.
	Secret []byte
	// SignatureHeader defaults to "X-Signature".
	SignatureHeader string
	// TimestampHeader defaults to "X-Signature-Timestamp".
	TimestampHeader string
	// Tolerance bounds how far the request timestamp may drift from now,
	// rejecting both stale requests and clock-skew/replay attempts. Defaults
	// to 5 minutes if zero.
	Tolerance time.Duration
}

// New returns a middleware that verifies the HMAC-SHA256 signature of
// method + "\n" + path + "\n" + timestamp + "\n" + body against the
// configured secret. It fails closed: any missing header, expired
// timestamp, or signature mismatch results in 401, and the request body is
// restored so downstream handlers can still read it.
func New(cfg Config) middleware.Middleware {
	sigHeader := cfg.SignatureHeader
	if sigHeader == "" {
		sigHeader = "X-Signature"
	}
	tsHeader := cfg.TimestampHeader
	if tsHeader == "" {
		tsHeader = "X-Signature-Timestamp"
	}
	tolerance := cfg.Tolerance
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			suppliedSig := r.Header.Get(sigHeader)
			suppliedTS := r.Header.Get(tsHeader)
			if suppliedSig == "" || suppliedTS == "" {
				http.Error(w, "missing signature headers", http.StatusUnauthorized)
				return
			}

			tsUnix, err := strconv.ParseInt(suppliedTS, 10, 64)
			if err != nil {
				http.Error(w, "invalid signature timestamp", http.StatusUnauthorized)
				return
			}
			ts := time.Unix(tsUnix, 0)
			if now := time.Now(); now.Sub(ts) > tolerance || ts.Sub(now) > tolerance {
				http.Error(w, "signature timestamp outside tolerance", http.StatusUnauthorized)
				return
			}

			var body []byte
			if r.Body != nil {
				body, err = io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "error reading body", http.StatusBadRequest)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}

			mac := hmac.New(sha256.New, cfg.Secret)
			mac.Write([]byte(r.Method))
			mac.Write([]byte("\n"))
			mac.Write([]byte(r.URL.Path))
			mac.Write([]byte("\n"))
			mac.Write([]byte(suppliedTS))
			mac.Write([]byte("\n"))
			mac.Write(body)
			expected := mac.Sum(nil)

			suppliedRaw, err := hex.DecodeString(suppliedSig)
			if err != nil || !hmac.Equal(suppliedRaw, expected) {
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Sign computes the hex-encoded HMAC-SHA256 signature for the given
// request components, matching what New verifies. Useful for clients and
// tests constructing signed requests.
func Sign(secret []byte, method, path string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(method))
	mac.Write([]byte("\n"))
	mac.Write([]byte(path))
	mac.Write([]byte("\n"))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
