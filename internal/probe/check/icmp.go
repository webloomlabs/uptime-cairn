package check

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// ICMP implements the icmp monitor type.
//
// The whole design turns on one fact: most container runtimes do not grant raw
// sockets, so an ICMP monitor that assumes them fails on the majority of
// installs. Three things follow.
//
// First, two socket modes are tried — the unprivileged datagram-ICMP socket
// first, the raw socket second — because the unprivileged one works unadorned on
// macOS and on Linux hosts whose net.ipv4.ping_group_range includes the running
// group, and needs no capability at all.
//
// Second, when neither opens, the failure is unknown and not down. The target
// may be perfectly healthy; this probe simply cannot ask. Reporting down here
// would page an on-call rotation about a container permission.
//
// Third, fallback_to_tcp exists precisely for that case, and every heartbeat it
// produces says it fell back — a monitor that quietly changed what it measures
// is worse than one that failed.
type ICMP struct {
	// Socket capability is a property of the host and cannot change while the
	// process runs, so it is probed once and remembered. Probing per check would
	// mean an open/close syscall pair on every ping.
	v4, v6 socketMode
}

// socketMode is one address family's answer to "can I ping at all, and how".
type socketMode struct {
	once sync.Once

	// network is the Go network name that opened successfully: "udp4" for the
	// unprivileged datagram socket, "ip4:icmp" for the raw one.
	network string
	ok      bool
	reason  string
}

// NewICMP builds the checker. Capability is not probed here: opening sockets
// during construction would make the composition root fail in a way that has
// nothing to do with whether any ICMP monitor exists.
func NewICMP() *ICMP { return &ICMP{} }

// Type implements Checker.
func (i *ICMP) Type() string { return model.TypeICMP }

// Version implements Checker.
func (i *ICMP) Version() uint32 { return 1 }

// icmpConfig mirrors IcmpConfig in docs/api/openapi.yaml.
type icmpConfig struct {
	Hostname        string `json:"hostname"`
	PacketSize      *int   `json:"packet_size"`
	PacketCount     *int   `json:"packet_count"`
	FallbackToTCP   bool   `json:"fallback_to_tcp"`
	FallbackTCPPort *int   `json:"fallback_tcp_port"`
	IPFamily        string `json:"ip_family"`
}

const (
	defaultPacketSize  = 56
	defaultPacketCount = 1

	// echoTokenSize is the payload prefix that identifies our own replies. The
	// unprivileged socket has the kernel rewrite the echo identifier, so the
	// identifier cannot be used for matching; a random token in the payload can,
	// and it works identically on both socket modes.
	echoTokenSize = 8
)

// Validate implements Checker.
func (i *ICMP) Validate(config []byte) error {
	cfg, err := decodeICMPConfig(config)
	if err != nil {
		return err
	}
	if err := validateHostname(cfg.Hostname); err != nil {
		return err
	}
	if cfg.PacketSize != nil {
		if s := *cfg.PacketSize; s < echoTokenSize || s > 65507 {
			return fmt.Errorf("packet_size %d is outside %d-65507", s, echoTokenSize)
		}
	}
	if cfg.PacketCount != nil {
		if c := *cfg.PacketCount; c < 1 || c > 10 {
			return fmt.Errorf("packet_count %d is outside 1-10", c)
		}
	}
	if cfg.FallbackToTCP {
		if cfg.FallbackTCPPort == nil {
			return errors.New("fallback_tcp_port is required when fallback_to_tcp is set")
		}
		if err := validatePort(*cfg.FallbackTCPPort); err != nil {
			return fmt.Errorf("fallback_tcp_port: %w", err)
		}
	}
	return validateIPFamily(cfg.IPFamily)
}

// Availability implements Availability. ICMP stays available even where raw
// sockets are not: a monitor with fallback_to_tcp set still runs, and refusing
// the assignment outright would take that away. The reason travels with the
// registration so an operator can see why their pings are coming back unknown
// without reading heartbeat messages one at a time.
func (i *ICMP) Availability() (bool, string) {
	v4 := i.mode(&i.v4, "udp4", "ip4:icmp")
	v6 := i.mode(&i.v6, "udp6", "ip6:ipv6-icmp")

	switch {
	case v4.ok && v6.ok:
		return true, ""
	case v4.ok:
		return true, "IPv6 ping unavailable: " + v6.reason
	case v6.ok:
		return true, "IPv4 ping unavailable: " + v4.reason
	default:
		return true, "no ICMP socket available (" + v4.reason + "); only monitors with fallback_to_tcp will run here"
	}
}

// Check implements Checker.
func (i *ICMP) Check(ctx context.Context, config []byte) Observation {
	cfg, err := decodeICMPConfig(config)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}

	start := time.Now()
	addr, err := resolveForPing(ctx, cfg.Hostname, cfg.IPFamily)
	if err != nil {
		return classify(err, time.Since(start))
	}

	network := "udp4"
	raw := "ip4:icmp"
	mode := &i.v4
	if addr.IP.To4() == nil {
		network, raw, mode = "udp6", "ip6:ipv6-icmp", &i.v6
	}

	m := i.mode(mode, network, raw)
	if !m.ok {
		return i.unavailable(ctx, cfg, m.reason)
	}
	return ping(ctx, m.network, addr, cfg)
}

// unavailable is the restricted-container path: either fall back to TCP and say
// so on every heartbeat, or report unknown with an explanation the operator can
// act on.
func (i *ICMP) unavailable(ctx context.Context, cfg icmpConfig, reason string) Observation {
	if !cfg.FallbackToTCP || cfg.FallbackTCPPort == nil {
		return Observation{
			Status: model.StatusUnknown,
			Class:  ClassCapability,
			Message: "ICMP unavailable on this probe: " + reason +
				". Grant CAP_NET_RAW, widen net.ipv4.ping_group_range, or set fallback_to_tcp on this monitor",
		}
	}

	obs := tcpConnect(ctx, cfg.Hostname, *cfg.FallbackTCPPort, cfg.IPFamily)
	// Recorded on every heartbeat, per the spec: a monitor that silently
	// switched from "is this host reachable" to "is this port open" is
	// measuring something the operator did not ask for.
	obs.Code = "tcp_fallback"
	prefix := fmt.Sprintf("ICMP unavailable (%s); checked TCP %d instead", reason, *cfg.FallbackTCPPort)
	if obs.Message == "" {
		obs.Message = prefix
	} else {
		obs.Message = prefix + ": " + obs.Message
	}
	return obs
}

// mode probes one address family's socket capability, once.
func (m *socketMode) get() socketMode {
	return socketMode{network: m.network, ok: m.ok, reason: m.reason}
}

func (i *ICMP) mode(m *socketMode, unprivileged, privileged string) socketMode {
	m.once.Do(func() {
		// Unprivileged first. It needs no capability, and preferring the raw
		// socket where both work would mean asking for a privilege we do not
		// need in order to do the same job.
		if conn, err := icmp.ListenPacket(unprivileged, ""); err == nil {
			_ = conn.Close()
			m.network, m.ok = unprivileged, true
			return
		}
		conn, err := icmp.ListenPacket(privileged, "")
		if err == nil {
			_ = conn.Close()
			m.network, m.ok = privileged, true
			return
		}
		m.reason = describeSocketFailure(err)
	})
	return m.get()
}

// describeSocketFailure turns a socket error into something an operator can act
// on. "operation not permitted" on its own has sent a great many people to a
// search engine.
func describeSocketFailure(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return "raw and unprivileged ICMP sockets both refused (no CAP_NET_RAW, and this process's group is outside net.ipv4.ping_group_range)"
	}
	return "cannot open an ICMP socket: " + err.Error()
}

// ping sends packet_count echo requests and waits for their replies.
//
// One reply is enough to call the host up: a monitor is not a packet-loss
// measurement tool, and treating a single dropped packet as an outage on a
// medium that is explicitly best-effort produces alerts nobody trusts. Partial
// loss is still reported in the message.
func ping(ctx context.Context, network string, addr *net.IPAddr, cfg icmpConfig) Observation {
	size := defaultPacketSize
	if cfg.PacketSize != nil {
		size = *cfg.PacketSize
	}
	count := defaultPacketCount
	if cfg.PacketCount != nil {
		count = *cfg.PacketCount
	}

	conn, err := icmp.ListenPacket(network, "")
	if err != nil {
		return Observation{
			Status:  model.StatusUnknown,
			Class:   ClassCapability,
			Message: describeSocketFailure(err),
		}
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	// The datagram socket is addressed as UDP even though no UDP is involved;
	// the raw socket is addressed as a plain IP address.
	var destination net.Addr = addr
	if strings.HasPrefix(network, "udp") {
		destination = &net.UDPAddr{IP: addr.IP, Zone: addr.Zone}
	}

	echoType := icmp.Type(ipv4.ICMPTypeEcho)
	if strings.HasPrefix(network, "udp6") || strings.HasPrefix(network, "ip6") {
		echoType = ipv6.ICMPTypeEchoRequest
	}

	token := make([]byte, echoTokenSize)
	if _, err := rand.Read(token); err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassCapability, Message: "cannot seed echo payload: " + err.Error()}
	}
	payload := make([]byte, size)
	copy(payload, token)

	id := os.Getpid() & 0xffff
	var (
		received int
		total    time.Duration
		lastErr  string
		timedOut bool
	)

	for seq := 1; seq <= count; seq++ {
		if ctx.Err() != nil {
			break
		}

		message := icmp.Message{
			Type: echoType,
			Code: 0,
			Body: &icmp.Echo{ID: id, Seq: seq, Data: payload},
		}
		wire, err := message.Marshal(nil)
		if err != nil {
			return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: "encode echo request: " + err.Error()}
		}

		sent := time.Now()
		if _, err := conn.WriteTo(wire, destination); err != nil {
			lastErr = err.Error()
			continue
		}

		rtt, err := awaitEcho(conn, token, seq, sent)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				timedOut = true
			} else {
				lastErr = err.Error()
			}
			continue
		}
		received++
		total += rtt
	}

	switch {
	case received == 0:
		// No ResponseTime: nothing was measured, and zero is a measurement of
		// zero, which is a different claim (model.Heartbeat).
		obs := Observation{Status: model.StatusDown, Class: ClassNetwork}
		obs.Message = fmt.Sprintf("no reply from %s after %d %s", addr.IP, count, plural(count, "packet"))
		// A read timeout is the expected shape of "no reply" and its error text
		// is socket plumbing; anything else is a real failure worth naming.
		if timedOut {
			obs.Class = ClassTimeout
		} else if lastErr != "" {
			obs.Message += ": " + lastErr
		}
		return obs
	default:
		mean := total / time.Duration(received)
		obs := Observation{Status: model.StatusUp, ResponseTime: &mean}
		if received < count {
			obs.Message = fmt.Sprintf("%d of %d packets lost", count-received, count)
		}
		return obs
	}
}

// awaitEcho reads until our own reply arrives or the deadline passes. Other
// traffic on the socket is skipped rather than answered: on a raw socket every
// process's ICMP arrives here, and treating a stranger's reply as ours would
// report a response time measured from someone else's packet.
func awaitEcho(conn *icmp.PacketConn, token []byte, seq int, sent time.Time) (time.Duration, error) {
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		rtt := time.Since(sent)

		// Protocol number, not the socket network: ICMPv4 is 1, ICMPv6 is 58.
		proto := 1
		if conn.IPv6PacketConn() != nil {
			proto = 58
		}
		parsed, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		echo, ok := parsed.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		if echo.Seq != seq || len(echo.Data) < len(token) || string(echo.Data[:len(token)]) != string(token) {
			continue
		}
		return rtt, nil
	}
}

// resolveForPing resolves the hostname to a single address honouring ip_family.
// The resolution error is returned unwrapped so classify() can tell "no such
// host" (a fact about the target) from "the resolver is broken" (a fact about
// this probe).
func resolveForPing(ctx context.Context, host, family string) (*net.IPAddr, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		switch family {
		case "ipv4":
			if addr.IP.To4() == nil {
				continue
			}
		case "ipv6":
			if addr.IP.To4() != nil {
				continue
			}
		}
		return &addr, nil
	}
	return nil, fmt.Errorf("%s has no %s address", host, familyName(family))
}

func familyName(family string) string {
	switch family {
	case "ipv4":
		return "IPv4"
	case "ipv6":
		return "IPv6"
	default:
		return "IP"
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func decodeICMPConfig(config []byte) (icmpConfig, error) {
	var cfg icmpConfig
	dec := json.NewDecoder(strings.NewReader(string(config)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
