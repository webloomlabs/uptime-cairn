package check

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The ping path itself needs a socket the host may refuse to hand out, so the
// tests here concentrate on the parts that must be right precisely when it does
// refuse — which is most container runtimes, and therefore most installs.

func TestICMPValidate(t *testing.T) {
	t.Parallel()

	checker := NewICMP()
	rejected := map[string]string{
		"no hostname":           `{}`,
		"packet size too small": `{"hostname":"example.com","packet_size":4}`,
		"packet size too large": `{"hostname":"example.com","packet_size":70000}`,
		"packet count too high": `{"hostname":"example.com","packet_count":11}`,
		"packet count zero":     `{"hostname":"example.com","packet_count":0}`,
		"fallback without port": `{"hostname":"example.com","fallback_to_tcp":true}`,
		"fallback port invalid": `{"hostname":"example.com","fallback_to_tcp":true,"fallback_tcp_port":0}`,
		"url in hostname":       `{"hostname":"https://example.com"}`,
		"unknown field":         `{"hostname":"example.com","nope":1}`,
	}
	for name, config := range rejected {
		if err := checker.Validate([]byte(config)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	accepted := []string{
		`{"hostname":"example.com"}`,
		`{"hostname":"192.0.2.1","packet_count":3,"packet_size":128}`,
		`{"hostname":"example.com","fallback_to_tcp":true,"fallback_tcp_port":443}`,
	}
	for _, config := range accepted {
		if err := checker.Validate([]byte(config)); err != nil {
			t.Errorf("%s rejected: %v", config, err)
		}
	}
}

// Without fallback, a probe that cannot open an ICMP socket reports unknown —
// never down. The target may be perfectly healthy; this probe simply cannot ask,
// and paging an on-call rotation about a container permission is the specific
// failure this taxonomy exists to prevent.
func TestICMPUnavailableWithoutFallbackIsUnknown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := (&ICMP{}).unavailable(ctx, icmpConfig{Hostname: "example.com"}, "no raw sockets here")
	if obs.Status != model.StatusUnknown {
		t.Errorf("status = %s, want unknown", obs.Status)
	}
	if obs.Class != ClassCapability {
		t.Errorf("class = %q, want %q", obs.Class, ClassCapability)
	}
	// The message has to tell an operator what to do about it. "operation not
	// permitted" on its own has sent a great many people to a search engine.
	for _, want := range []string{"CAP_NET_RAW", "ping_group_range", "fallback_to_tcp"} {
		if !strings.Contains(obs.Message, want) {
			t.Errorf("message does not mention %q: %s", want, obs.Message)
		}
	}
}

// With fallback configured, the check runs against the named TCP port — and says
// so on every heartbeat. A monitor that quietly changed what it measures is
// worse than one that failed.
func TestICMPFallbackSaysSoEveryTime(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := icmpConfig{Hostname: "127.0.0.1", FallbackToTCP: true, FallbackTCPPort: &port}
	obs := (&ICMP{}).unavailable(ctx, cfg, "no raw sockets here")

	if obs.Status != model.StatusUp {
		t.Errorf("status = %s, want up (%s)", obs.Status, obs.Message)
	}
	if obs.Code != "tcp_fallback" {
		t.Errorf("code = %q, want tcp_fallback — the heartbeat must record that it fell back", obs.Code)
	}
	if !strings.Contains(obs.Message, strconv.Itoa(port)) {
		t.Errorf("message does not name the port it checked instead: %s", obs.Message)
	}

	// And a failing fallback is still down, with the fallback noted.
	closed := freePort(t)
	cfg.FallbackTCPPort = &closed
	obs = (&ICMP{}).unavailable(ctx, cfg, "no raw sockets here")
	if obs.Status != model.StatusDown {
		t.Errorf("status = %s, want down", obs.Status)
	}
	if obs.Code != "tcp_fallback" {
		t.Errorf("code = %q, want tcp_fallback", obs.Code)
	}
}

// ICMP stays available even where the socket is refused, because a monitor with
// fallback_to_tcp still runs. Advertising it unavailable would have the control
// plane withhold the assignment and take the fallback away.
func TestICMPAvailabilityNeverWithholdsAssignments(t *testing.T) {
	t.Parallel()

	available, reason := NewICMP().Availability()
	if !available {
		t.Errorf("ICMP advertised unavailable (%q); monitors with fallback_to_tcp would never be assigned", reason)
	}
}

func TestResolveForPingHonoursFamily(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addr, err := resolveForPing(ctx, "127.0.0.1", "ipv4")
	if err != nil {
		t.Fatalf("ipv4 literal: %v", err)
	}
	if addr.IP.To4() == nil {
		t.Errorf("resolved %s, want an IPv4 address", addr.IP)
	}

	// An IPv4-only name asked for over IPv6 has no answer, and saying so beats
	// silently pinging the wrong family.
	if _, err := resolveForPing(ctx, "127.0.0.1", "ipv6"); err == nil {
		t.Error("an IPv4 literal resolved for ipv6")
	}
}
