// Package bodylimit caps the size of request bodies so a client can't
// exhaust gateway memory. Middleware that must read the whole body (signing
// verification, retry replay) relies on this cap being in place; it should
// run before them in the chain.
package bodylimit

import (
	"errors"
	"net/http"

	"github.com/swayam5342/jgateway/pkg/middleware"
)

// New returns a middleware that rejects requests whose declared
// Content-Length exceeds maxBytes with 413, and wraps every body in
// http.MaxBytesReader so bodies of unknown length (chunked) fail with an
// *http.MaxBytesError once they cross the limit. maxBytes <= 0 disables the
// limit.
func New(maxBytes int64) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		if maxBytes <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WriteReadError responds to an error from reading the request body: 413 if
// the body limit was exceeded, 400 otherwise. Middleware that buffers the
// body uses it so the client gets an accurate status.
func WriteReadError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "error reading request body", http.StatusBadRequest)
}
