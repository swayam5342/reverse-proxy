package ratelimit

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	lb := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}

	tests := []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		xff     []string
		want    string
	}{
		{"no trusted proxies ignores spoofed XFF", nil, "1.2.3.4:5000", []string{"6.6.6.6"}, "1.2.3.4"},
		{"direct (untrusted) peer ignores XFF", lb, "1.2.3.4:5000", []string{"6.6.6.6"}, "1.2.3.4"},
		{"trusted LB, no XFF", lb, "10.0.0.1:5000", nil, "10.0.0.1"},
		{"trusted LB appends real client", lb, "10.0.0.1:5000", []string{"1.1.1.1"}, "1.1.1.1"},
		// The bug: the client prepends a fake entry and the LB appends the
		// real one. The leftmost entry is attacker-controlled.
		{"client-prepended spoof is skipped", lb, "10.0.0.1:5000", []string{"6.6.6.6, 1.1.1.1"}, "1.1.1.1"},
		{"chain of trusted proxies", lb, "10.0.0.1:5000", []string{"1.1.1.1, 10.0.0.2, 10.0.0.3"}, "1.1.1.1"},
		{"multiple XFF header lines", lb, "10.0.0.1:5000", []string{"6.6.6.6", "1.1.1.1"}, "1.1.1.1"},
		{"garbage entry stops at last trusted hop", lb, "10.0.0.1:5000", []string{"1.1.1.1, not-an-ip"}, "10.0.0.1"},
		{"all hops trusted returns furthest", lb, "10.0.0.1:5000", []string{"10.0.0.9, 10.0.0.2"}, "10.0.0.9"},
		{"ipv6 client", lb, "10.0.0.1:5000", []string{"2001:db8::1"}, "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r, tt.trusted); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
