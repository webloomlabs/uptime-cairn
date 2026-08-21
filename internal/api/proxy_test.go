package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func request(t *testing.T, peer, forwarded string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.RemoteAddr = peer
	if forwarded != "" {
		r.Header.Set("X-Forwarded-For", forwarded)
	}
	return r
}

func trust(t *testing.T, values ...string) *trustedProxies {
	t.Helper()
	p, err := parseTrustedProxies(values)
	if err != nil {
		t.Fatalf("parseTrustedProxies(%v): %v", values, err)
	}
	return p
}

// The leak this exists to close: a reverse proxy on the same host connects from
// 127.0.0.1, so every request it forwards inherits an exemption meant for a
// local scraper. All three published recipes set X-Forwarded-For, so its
// presence from an undeclared peer is the signal that the peer is speaking for
// somebody else without having been authorised to.
func TestResolveClientDistrustsAnUndeclaredForwarder(t *testing.T) {
	t.Parallel()

	ip, known := resolveClient(request(t, "127.0.0.1:52000", "203.0.113.7"), trust(t))
	if known {
		t.Fatalf("resolveClient trusted an undeclared forwarder, resolved %q", ip)
	}
}

// The case the exemption exists for, which must keep working: a Prometheus on
// the same host connects directly and sends no such header.
func TestResolveClientAcceptsADirectLocalScrape(t *testing.T) {
	t.Parallel()

	ip, known := resolveClient(request(t, "127.0.0.1:52000", ""), trust(t))
	if !known || !isLoopback(ip) {
		t.Fatalf("resolveClient = (%q, %v), want a known loopback client", ip, known)
	}
}

// With the proxy declared, the header becomes evidence rather than a claim, and
// the real client is what the decision is made against.
func TestResolveClientReadsTheChainFromADeclaredProxy(t *testing.T) {
	t.Parallel()

	trusted := trust(t, "127.0.0.1", "10.0.0.0/8")

	ip, known := resolveClient(request(t, "127.0.0.1:52000", "203.0.113.7"), trusted)
	if !known || ip != "203.0.113.7" {
		t.Fatalf("resolveClient = (%q, %v), want 203.0.113.7 known", ip, known)
	}
	if isLoopback(ip) {
		t.Error("a forwarded internet client resolved as loopback")
	}

	// Right to left, skipping our own hops: everything to the left of the
	// trusted chain was written by somebody we have no reason to believe.
	ip, known = resolveClient(request(t, "127.0.0.1:52000", "9.9.9.9, 203.0.113.7, 10.1.2.3"), trusted)
	if !known || ip != "203.0.113.7" {
		t.Fatalf("resolveClient = (%q, %v), want 203.0.113.7 known", ip, known)
	}
}

// A local scraper reaching the install through the declared proxy is still a
// local scraper: every hop was trusted, so the request originated at the proxy.
func TestResolveClientAllowsAProxysOwnRequest(t *testing.T) {
	t.Parallel()

	ip, known := resolveClient(request(t, "127.0.0.1:52000", "127.0.0.1"), trust(t, "127.0.0.1"))
	if !known || !isLoopback(ip) {
		t.Fatalf("resolveClient = (%q, %v), want a known loopback client", ip, known)
	}
}

// An entry that is not an address means the chain is not something we can reason
// about. Refusing to guess is the point.
func TestResolveClientRefusesAnUnparseableChain(t *testing.T) {
	t.Parallel()

	if _, known := resolveClient(request(t, "127.0.0.1:52000", "unknown"), trust(t, "127.0.0.1")); known {
		t.Error("resolveClient claimed to know the client behind an unparseable chain")
	}
}

// Forwarded entries carry ports and brackets in the wild, and neither is the
// address.
func TestResolveClientStripsPortsAndBrackets(t *testing.T) {
	t.Parallel()

	trusted := trust(t, "127.0.0.1")
	for _, hop := range []string{"203.0.113.7:41234", "203.0.113.7"} {
		ip, known := resolveClient(request(t, "127.0.0.1:1", hop), trusted)
		if !known || ip != "203.0.113.7" {
			t.Errorf("hop %q resolved to (%q, %v)", hop, ip, known)
		}
	}
	ip, known := resolveClient(request(t, "127.0.0.1:1", "[2001:db8::1]"), trusted)
	if !known || ip != "2001:db8::1" {
		t.Errorf("bracketed IPv6 resolved to (%q, %v)", ip, known)
	}
}

// A bare IP and its /32 must behave identically; an asymmetry between the two
// spellings is the sort of thing found at the worst time.
func TestParseTrustedProxiesAcceptsBothSpellings(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"10.0.0.5", "10.0.0.5/32"} {
		p := trust(t, value)
		if !p.has("10.0.0.5") {
			t.Errorf("%q does not match 10.0.0.5", value)
		}
		if p.has("10.0.0.6") {
			t.Errorf("%q matches 10.0.0.6", value)
		}
	}
	if _, err := parseTrustedProxies([]string{"not-an-address"}); err == nil {
		t.Error("parseTrustedProxies accepted a value that is neither an IP nor a CIDR")
	}
}
