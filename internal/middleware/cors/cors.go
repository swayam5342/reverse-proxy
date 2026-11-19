// Package cors implements per-route CORS for browser frontends.
//
// Ordering: run it after logging/metrics (so preflights are observed) and
// before auth. Browsers send preflight requests without credentials, so a
// preflight that reached auth would always be rejected with 401.
//
// The gateway is the single source of CORS headers: any Access-Control-*
// header set by the upstream is removed from the response.
package cors

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/swayam5342/jgateway/pkg/middleware"
)

// Defaults used when the corresponding Config list is empty.
var (
	defaultMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}
	defaultHeaders = []string{"Authorization", "Content-Type"}
)

// Config configures CORS for one route. Origins are exact
// "scheme://host[:port]" strings, or "*" for any origin (not allowed with
// AllowCredentials; config validation enforces that).
type Config struct {
	AllowedOrigins   []string
	AllowedMethods   []string // default: GET, HEAD, POST, PUT, PATCH, DELETE
	AllowedHeaders   []string // default: Authorization, Content-Type
	ExposeHeaders    []string
	AllowCredentials bool
	MaxAge           time.Duration
}

// New returns the CORS middleware.
//
// Preflights (OPTIONS with Origin and Access-Control-Request-Method) are
// answered here and never forwarded: 204 with CORS headers for an allowed
// origin and method, 403 otherwise. Other requests from an allowed origin
// get CORS headers added to the response; requests from other origins pass
// through without them, which makes the browser block the response while
// leaving non-browser callers unaffected.
func New(cfg Config) middleware.Middleware {
	methods := cfg.AllowedMethods
	if len(methods) == 0 {
		methods = defaultMethods
	}
	methods = upper(methods)
	headers := cfg.AllowedHeaders
	if len(headers) == 0 {
		headers = defaultHeaders
	}
	anyOrigin := slices.Contains(cfg.AllowedOrigins, "*")

	allowOriginValue := func(origin string) (string, bool) {
		switch {
		case anyOrigin && !cfg.AllowCredentials:
			return "*", true
		case slices.Contains(cfg.AllowedOrigins, origin):
			return origin, true
		default:
			return "", false
		}
	}
	setCommon := func(h http.Header, value string) {
		h.Set("Access-Control-Allow-Origin", value)
		if cfg.AllowCredentials {
			h.Set("Access-Control-Allow-Credentials", "true")
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			value, allowed := allowOriginValue(origin)

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h := w.Header()
				h.Add("Vary", "Origin")
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				reqMethod := strings.ToUpper(r.Header.Get("Access-Control-Request-Method"))
				if !allowed || !slices.Contains(methods, reqMethod) {
					http.Error(w, "CORS request not allowed", http.StatusForbidden)
					return
				}
				setCommon(h, value)
				h.Set("Access-Control-Allow-Methods", strings.Join(methods, ", "))
				h.Set("Access-Control-Allow-Headers", strings.Join(headers, ", "))
				if cfg.MaxAge > 0 {
					h.Set("Access-Control-Max-Age", strconv.Itoa(int(cfg.MaxAge.Seconds())))
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(&writer{ResponseWriter: w, apply: func(h http.Header) {
				for name := range h {
					if strings.HasPrefix(name, "Access-Control-") {
						delete(h, name)
					}
				}
				h.Add("Vary", "Origin")
				if !allowed {
					return
				}
				setCommon(h, value)
				if len(cfg.ExposeHeaders) > 0 {
					h.Set("Access-Control-Expose-Headers", strings.Join(cfg.ExposeHeaders, ", "))
				}
			}}, r)
		})
	}
}

func upper(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}

// writer applies the gateway's CORS headers just before the response
// headers are sent, replacing any the upstream set. It forwards Flush and
// exposes the real writer via Unwrap, so streaming keeps working.
type writer struct {
	http.ResponseWriter
	apply   func(http.Header)
	applied bool
}

func (w *writer) WriteHeader(code int) {
	if !w.applied && code >= http.StatusOK {
		w.apply(w.Header())
		w.applied = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *writer) Write(p []byte) (int, error) {
	if !w.applied {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// FlushError commits headers (applying CORS) and flushes the real writer.
func (w *writer) FlushError() error {
	if !w.applied {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap returns the real writer, for http.ResponseController.
func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
