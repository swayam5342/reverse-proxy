// Package statusrec provides the response-status recorder shared by the
// observability and circuit-breaker middleware.
//
// A naive wrapper that embeds http.ResponseWriter hides the optional
// interfaces of the real writer (http.Flusher, http.Hijacker), which breaks
// streaming responses and WebSocket upgrades proxied through
// httputil.ReverseProxy. Recorder forwards Flush and Hijack explicitly and
// implements Unwrap so http.ResponseController can reach the real
// connection for everything else (deadlines, full-duplex).
package statusrec

import (
	"bufio"
	"net"
	"net/http"
)

// Recorder wraps an http.ResponseWriter and remembers the last status code
// written. It is not safe for concurrent writes, like http.ResponseWriter
// itself.
type Recorder struct {
	http.ResponseWriter
	status int
}

// New wraps w. Status defaults to 200, which is what net/http sends if the
// handler writes a body (or nothing) without calling WriteHeader.
func New(w http.ResponseWriter) *Recorder {
	return &Recorder{ResponseWriter: w, status: http.StatusOK}
}

// Status returns the last status code written: the final status for a
// normal response, or 101 for a hijacked (upgraded) connection.
func (r *Recorder) Status() int { return r.status }

// WriteHeader records code and forwards it. 1xx informational codes are
// recorded too but are overwritten by the final status that follows them.
func (r *Recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// FlushError flushes the underlying writer, returning http.ErrNotSupported
// if it can't be flushed. http.ResponseController prefers this over Flush.
func (r *Recorder) FlushError() error {
	return http.NewResponseController(r.ResponseWriter).Flush()
}

// Flush implements http.Flusher for callers that type-assert directly.
func (r *Recorder) Flush() { _ = r.FlushError() }

// Hijack takes over the underlying connection (used for protocol upgrades
// such as WebSockets) and records the response as 101 Switching Protocols.
func (r *Recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.status = http.StatusSwitchingProtocols
	}
	return conn, brw, err
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (r *Recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
