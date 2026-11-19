package sanitize

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Backends trust X-Auth-* because only the gateway sets them, so whatever
// a client sends under that prefix must never get through.
func TestSanitize_StripsClientAuthHeaders(t *testing.T) {
	var got http.Header
	h := New()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Auth-Subject", "admin")
	req.Header["x-auth-method"] = []string{"jwt"} // non-canonical spelling
	req.Header.Set("X-Auth-Anything", "x")
	req.Header.Set("X-Other", "keep")
	h.ServeHTTP(httptest.NewRecorder(), req)

	for name := range got {
		if len(name) >= 7 && http.CanonicalHeaderKey(name)[:7] == "X-Auth-" {
			t.Errorf("client header %q reached next handler", name)
		}
	}
	if got.Get("X-Other") != "keep" {
		t.Error("unrelated header was removed")
	}
}
