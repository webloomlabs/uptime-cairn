package api

import (
	"net"
	"net/http"
	"strings"
)

// This file holds the one question the metrics exemption turns on: who is
// actually asking?
//
// The exemption is that a scrape from loopback needs no credential, because the
// overwhelmingly common deployment is a Prometheus on the same host and an
// endpoint that needs a credential is an endpoint somebody turns off. The
// defect that shape has is not subtle once seen: a reverse proxy on the same
// host connects from 127.0.0.1, so every request it forwards — from anywhere on
// the internet — qualifies for an exemption meant for a local scraper. What
// leaks is the full monitor inventory, because cairn_monitor_status carries
// every monitor's id, name, and type.
//
// X-Forwarded-For is not the fix on its own, and is deliberately not trusted by
// default anywhere in this server: it is a caller-supplied header, and a
// server that believes it hands anyone who can reach the port the ability to
// claim any address they like. Trusting it *from a configured proxy* is a
// different statement, and that is what trustedProxies is.

// trustedProxies is the operator's declaration of which peers are allowed to
// speak for somebody else. Empty means nobody is, which is the default.
type trustedProxies struct{ nets []*net.IPNet }

// parseTrustedProxies reads the --trusted-proxy values. Each is an IP or a
// CIDR; a bare IP becomes a single-address block so the two spellings behave
// identically, which is the sort of asymmetry an operator finds at the worst
// time.
func parseTrustedProxies(values []string) (*trustedProxies, error) {
	t := &trustedProxies{}
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, block, err := net.ParseCIDR(raw); err == nil {
			t.nets = append(t.nets, block)
			continue
		}
		ip := net.ParseIP(raw)
		if ip == nil {
			return nil, &proxyParseError{value: raw}
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		t.nets = append(t.nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return t, nil
}

type proxyParseError struct{ value string }

func (e *proxyParseError) Error() string {
	return "--trusted-proxy " + e.value + " is neither an IP address nor a CIDR block"
}

// has reports whether ip is one of the declared proxies.
func (t *trustedProxies) has(ip string) bool {
	if t == nil || len(t.nets) == 0 {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, block := range t.nets {
		if block.Contains(parsed) {
			return true
		}
	}
	return false
}

// empty reports whether any proxy was declared at all.
func (t *trustedProxies) empty() bool { return t == nil || len(t.nets) == 0 }

// resolveClient answers "who is asking", and reports whether the answer is
// trustworthy enough to base an authentication decision on.
//
// The three cases, in the order they are decided:
//
//  1. The peer is a declared proxy. X-Forwarded-For is read right to left,
//     skipping entries that are themselves declared proxies, and the first
//     address that is not one is the client. Right to left because the header
//     is appended to by each hop: everything to the left of our own trusted
//     chain was written by somebody we have no reason to believe.
//
//  2. The peer is not a declared proxy and sent no X-Forwarded-For. The peer is
//     the client, and that is a fact about the connection rather than a claim.
//
//  3. The peer is not a declared proxy and did send X-Forwarded-For. Somebody
//     is speaking for somebody else without having been authorised to, and the
//     honest answer is that we do not know who the client is. This is the case
//     that closes the same-host proxy leak by default: all three published
//     reverse-proxy recipes set X-Forwarded-For, so a proxied request no longer
//     passes as a local scrape, while a Prometheus connecting directly sends no
//     such header and still does.
func resolveClient(r *http.Request, trusted *trustedProxies) (ip string, known bool) {
	peer := clientIP(r)
	forwarded := r.Header.Get("X-Forwarded-For")

	if !trusted.has(peer) {
		if forwarded == "" {
			return peer, true
		}
		return peer, false
	}

	hops := strings.Split(forwarded, ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		// A forwarded entry may carry a port, and an IPv6 literal may be
		// bracketed. Both are legal in the wild and neither is the address.
		if host, _, err := net.SplitHostPort(hop); err == nil {
			hop = host
		}
		hop = strings.Trim(hop, "[]")
		if trusted.has(hop) {
			continue
		}
		if net.ParseIP(hop) == nil {
			// An unparseable entry means the chain is not something we can
			// reason about. Refusing to guess is the whole point of this file.
			return peer, false
		}
		return hop, true
	}

	// Every hop was a trusted proxy, or there were none: the request originated
	// at the proxy itself, which is exactly what a health check from it looks
	// like.
	return peer, true
}
