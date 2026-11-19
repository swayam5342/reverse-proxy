package httpx

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// ClientIP returns the IP of the client that originated r.
//
// If the direct peer (r.RemoteAddr) is not a trusted proxy, it is the
// client and X-Forwarded-For is ignored. Otherwise X-Forwarded-For is walked
// from the right, the end that trusted proxies append to, skipping trusted
// hops; the first untrusted address is the client. Entries to the left of
// it were supplied by the client and are never trusted. If an entry can't
// be parsed, the last trusted hop is returned rather than guessing.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer, ok := parseAddr(r.RemoteAddr)
	if !ok {
		return r.RemoteAddr
	}
	if !isTrusted(peer, trusted) {
		return peer.String()
	}

	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}

	client := peer
	for _, hop := range slices.Backward(hops) {
		addr, ok := parseAddr(strings.TrimSpace(hop))
		if !ok {
			break
		}
		client = addr
		if !isTrusted(addr, trusted) {
			break
		}
	}
	return client.String()
}

// parseAddr parses "ip" or "ip:port" / "[ipv6]:port".
func parseAddr(s string) (netip.Addr, bool) {
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
