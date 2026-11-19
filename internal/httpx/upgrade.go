// Package httpx holds small HTTP helpers shared by several middleware.
package httpx

import (
	"net/http"
	"strings"
)

// IsUpgrade reports whether r asks to switch protocols (e.g. WebSocket):
// it has an Upgrade header and an "upgrade" token in Connection. Such
// requests hijack the connection, are long-lived, and can't be replayed.
func IsUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") == "" {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for token := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}
