package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestIsUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		connection []string
		upgrade    string
		want       bool
	}{
		{"websocket", []string{"Upgrade"}, "websocket", true},
		{"token list, mixed case", []string{"keep-alive, UPGRADE"}, "websocket", true},
		{"multiple header lines", []string{"keep-alive", "upgrade"}, "h2c", true},
		{"upgrade header without connection token", []string{"keep-alive"}, "websocket", false},
		{"connection token without upgrade header", []string{"upgrade"}, "", false},
		{"plain request", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			for _, c := range tt.connection {
				r.Header.Add("Connection", c)
			}
			if tt.upgrade != "" {
				r.Header.Set("Upgrade", tt.upgrade)
			}
			if got := IsUpgrade(r); got != tt.want {
				t.Errorf("IsUpgrade() = %v, want %v", got, tt.want)
			}
		})
	}
}
