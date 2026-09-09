package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrustedProxies turns the config's trusted_proxies (bare IPs) into host
// prefixes for clientAddr's trust check. Addresses are unmapped so an IPv4
// entry matches the same address arriving IPv4-mapped over a dual-stack socket.
func ParseTrustedProxies(ips []string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ips {
		if ip, err := netip.ParseAddr(strings.TrimSpace(s)); err == nil {
			ip = ip.Unmap()
			out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
		}
	}
	return out
}

// clientAddr is the address a client is identified by (prompt-002 decision 8).
// When the direct peer is a trusted proxy, it reads X-Forwarded-For RIGHT TO
// LEFT, skipping further trusted hops, and returns the first untrusted address —
// the real client the trusted proxy appended. When the peer is NOT a trusted
// proxy, X-Forwarded-For is ignored entirely: the leftmost entries are
// client-spoofable, so believing them would let anyone forge an identity and
// defeat eviction and the per-address rate limits.
//
// The guarantee holds only when trusted_proxies contains proxy addresses and no
// client connects from a trusted address: an entity connecting from a trusted
// address is, by configuration, believed to speak for whatever it forwards.
// All addresses are unmapped so IPv4 and its IPv4-mapped IPv6 form compare
// equal.
func (srv *Server) clientAddr(r *http.Request) string {
	peer := peerAddr(r.RemoteAddr)
	if !peer.IsValid() {
		return r.RemoteAddr
	}
	if !srv.trusted(peer) {
		return peer.String()
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			continue
		}
		ip = ip.Unmap()
		if srv.trusted(ip) {
			continue // another trusted hop; keep walking left
		}
		return ip.String() // first untrusted from the right = the client
	}
	// Every forwarded hop was trusted (or the header was absent/malformed):
	// fall back to the leftmost claimed client, else the peer.
	if ip, err := netip.ParseAddr(strings.TrimSpace(parts[0])); err == nil {
		return ip.Unmap().String()
	}
	return peer.String()
}

func (srv *Server) trusted(ip netip.Addr) bool {
	for _, p := range srv.opts.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// peerAddr extracts the netip.Addr from a RemoteAddr "host:port", unmapped.
func peerAddr(remote string) netip.Addr {
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}
