package timeout

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func remainingDeadline(t *testing.T, r *http.Request) time.Duration {
	t.Helper()
	var remaining time.Duration
	New(Config{RequestTimeout: 3 * time.Second, UpgradeTimeout: time.Hour})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dl, ok := r.Context().Deadline()
			if !ok {
				t.Fatal("request context has no deadline")
			}
			remaining = time.Until(dl)
		})).ServeHTTP(httptest.NewRecorder(), r)
	return remaining
}

func TestNew_SetsRequestDeadline(t *testing.T) {
	got := remainingDeadline(t, httptest.NewRequest(http.MethodGet, "/", nil))
	if got <= 2*time.Second || got > 3*time.Second {
		t.Errorf("remaining deadline = %v, want ~3s", got)
	}
}

// Upgraded connections (WebSockets) are long-lived, and ReverseProxy closes
// them when the request context ends, so they get the upgrade budget.
func TestNew_UpgradeGetsUpgradeDeadline(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")

	if got := remainingDeadline(t, r); got < 59*time.Minute || got > time.Hour {
		t.Errorf("remaining deadline = %v, want ~1h", got)
	}
}
