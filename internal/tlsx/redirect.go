package tlsx

import (
	"net"
	"net/http"
	"slices"
	"strings"
)

// RedirectHandler answers plaintext requests with a 301 to the same host
// and path over HTTPS (on the default port). If allowedHosts is non-empty,
// a request for any other host, or with no Host at all, gets 400 instead,
// so the listener can't be used to bounce clients to arbitrary hosts.
func RedirectHandler(allowedHosts []string) http.Handler {
	allowed := make([]string, len(allowedHosts))
	for i, h := range allowedHosts {
		allowed[i] = strings.ToLower(h)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		if host == "" || (len(allowed) > 0 && !slices.Contains(allowed, host)) {
			http.Error(w, "unknown host", http.StatusBadRequest)
			return
		}
		if strings.Contains(host, ":") { // IPv6 literal
			host = "[" + host + "]"
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}
