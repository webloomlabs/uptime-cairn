package check

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The DNS checker is tested against a resolver running in this process rather
// than against a public one. A test that needs the internet is a test that fails
// on an aeroplane and gets deleted.

// fakeResolver answers whatever the test tells it to, over both UDP and TCP.
type fakeResolver struct {
	t *testing.T

	rcode dnsmessage.RCode
	// answers is called with the query so a handler can vary by record type.
	answers func(dnsmessage.Question) []dnsmessage.Resource

	// truncate makes the UDP answer set the TC bit with no records, which is
	// what a real resolver does for an answer that will not fit.
	truncate bool

	addr string
}

func startFakeResolver(t *testing.T, r *fakeResolver) *fakeResolver {
	t.Helper()
	r.t = t

	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	host, port, _ := net.SplitHostPort(packet.LocalAddr().String())

	listener, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = packet.Close(); _ = listener.Close() })
	r.addr = host + ":" + port

	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := packet.ReadFrom(buf)
			if err != nil {
				return
			}
			reply, err := r.respond(buf[:n], r.truncate)
			if err != nil {
				continue
			}
			_, _ = packet.WriteTo(reply, from)
		}
	}()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				var length [2]byte
				if _, err := readFull(conn, length[:]); err != nil {
					return
				}
				query := make([]byte, int(length[0])<<8|int(length[1]))
				if _, err := readFull(conn, query); err != nil {
					return
				}
				// Never truncated over TCP: that is the whole point of the retry.
				reply, err := r.respond(query, false)
				if err != nil {
					return
				}
				framed := append([]byte{byte(len(reply) >> 8), byte(len(reply))}, reply...)
				_, _ = conn.Write(framed)
			}()
		}
	}()

	return r
}

func (r *fakeResolver) respond(query []byte, truncate bool) ([]byte, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return nil, err
	}
	question, err := parser.Question()
	if err != nil {
		return nil, err
	}

	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:        header.ID,
		Response:  true,
		RCode:     r.rcode,
		Truncated: truncate,
	})
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return nil, err
	}
	if err := builder.Question(question); err != nil {
		return nil, err
	}
	if err := builder.StartAnswers(); err != nil {
		return nil, err
	}
	if !truncate && r.answers != nil {
		for _, resource := range r.answers(question) {
			if err := addResource(&builder, resource); err != nil {
				return nil, err
			}
		}
	}
	return builder.Finish()
}

func addResource(builder *dnsmessage.Builder, resource dnsmessage.Resource) error {
	switch body := resource.Body.(type) {
	case *dnsmessage.AResource:
		return builder.AResource(resource.Header, *body)
	case *dnsmessage.MXResource:
		return builder.MXResource(resource.Header, *body)
	case *dnsmessage.TXTResource:
		return builder.TXTResource(resource.Header, *body)
	case *dnsmessage.CNAMEResource:
		return builder.CNAMEResource(resource.Header, *body)
	default:
		return builder.UnknownResource(resource.Header, *resource.Body.(*dnsmessage.UnknownResource))
	}
}

func aRecord(name, ip string) dnsmessage.Resource {
	var addr [4]byte
	copy(addr[:], net.ParseIP(ip).To4())
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name:  dnsmessage.MustNewName(name),
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
			TTL:   60,
		},
		Body: &dnsmessage.AResource{A: addr},
	}
}

func mxRecord(name string, pref uint16, target string) dnsmessage.Resource {
	return dnsmessage.Resource{
		Header: dnsmessage.ResourceHeader{
			Name:  dnsmessage.MustNewName(name),
			Type:  dnsmessage.TypeMX,
			Class: dnsmessage.ClassINET,
			TTL:   60,
		},
		Body: &dnsmessage.MXResource{Pref: pref, MX: dnsmessage.MustNewName(target)},
	}
}

func dnsConfigFor(resolver *fakeResolver, extra string) string {
	host, port, _ := net.SplitHostPort(resolver.addr)
	config := `{"hostname":"example.test","record_type":"A","resolver":"` + host + `","resolver_port":` + port
	if extra != "" {
		config += "," + extra
	}
	return config + "}"
}

func TestDNSCheck(t *testing.T) {
	t.Parallel()

	resolver := startFakeResolver(t, &fakeResolver{
		answers: func(dnsmessage.Question) []dnsmessage.Resource {
			return []dnsmessage.Resource{
				aRecord("example.test.", "192.0.2.10"),
				aRecord("example.test.", "192.0.2.11"),
			}
		},
	})

	checker := NewDNS()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tests := []struct {
		name  string
		extra string
		want  model.Status
		class ErrorClass
	}{
		{name: "record resolves", want: model.StatusUp},
		{name: "any matches one", extra: `"expected_values":["192.0.2.11"],"match_mode":"any"`, want: model.StatusUp},
		{name: "any matches none", extra: `"expected_values":["198.51.100.1"],"match_mode":"any"`, want: model.StatusDown, class: ClassAssertion},
		{name: "all present", extra: `"expected_values":["192.0.2.10","192.0.2.11"],"match_mode":"all"`, want: model.StatusUp},
		{name: "all with one missing", extra: `"expected_values":["192.0.2.10","198.51.100.1"],"match_mode":"all"`, want: model.StatusDown, class: ClassAssertion},
		{name: "exact in any order", extra: `"expected_values":["192.0.2.11","192.0.2.10"],"match_mode":"exact"`, want: model.StatusUp},
		{name: "exact with an extra record present", extra: `"expected_values":["192.0.2.10"],"match_mode":"exact"`, want: model.StatusDown, class: ClassAssertion},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := dnsConfigFor(resolver, tc.extra)
			if err := checker.Validate([]byte(config)); err != nil {
				t.Fatalf("validate: %v", err)
			}
			obs := checker.Check(ctx, []byte(config))
			if obs.Status != tc.want {
				t.Errorf("status = %s, want %s (%s)", obs.Status, tc.want, obs.Message)
			}
			if tc.class != "" && obs.Class != tc.class {
				t.Errorf("class = %q, want %q", obs.Class, tc.class)
			}
			if obs.Code != "NOERROR" {
				t.Errorf("code = %q, want NOERROR", obs.Code)
			}
		})
	}
}

// NXDOMAIN is a verdict, not a failure to ask: the resolver answered, and the
// answer is that the name is gone. Reporting it as unknown would let a deleted
// record pass silently, which is the thing a DNS monitor exists to catch.
func TestDNSNXDomainIsDown(t *testing.T) {
	t.Parallel()

	resolver := startFakeResolver(t, &fakeResolver{rcode: dnsmessage.RCodeNameError})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := NewDNS().Check(ctx, []byte(dnsConfigFor(resolver, "")))
	if obs.Status != model.StatusDown {
		t.Errorf("status = %s, want down", obs.Status)
	}
	if obs.Code != "NXDOMAIN" {
		t.Errorf("code = %q, want NXDOMAIN — heartbeats.code is stored history and a Go constant name is not greppable", obs.Code)
	}
}

// A resolver that does not answer says nothing about the record. Unknown, not
// down: one probe's broken egress must not open outages on every DNS monitor
// assigned to it.
func TestDNSUnreachableResolverIsUnknown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	config := `{"hostname":"example.test","record_type":"A","resolver":"127.0.0.1","resolver_port":` +
		strconv.Itoa(freePort(t)) + `}`
	obs := NewDNS().Check(ctx, []byte(config))
	if obs.Status != model.StatusUnknown {
		t.Errorf("status = %s, want unknown (%s)", obs.Status, obs.Message)
	}
	if obs.Class != ClassDNS {
		t.Errorf("class = %q, want %q", obs.Class, ClassDNS)
	}
}

// A truncated UDP answer must be retried over TCP. Without the retry a domain
// whose records simply exceed 512 bytes reports "no record", which is a
// fabricated outage.
func TestDNSTruncationRetriesOverTCP(t *testing.T) {
	t.Parallel()

	resolver := startFakeResolver(t, &fakeResolver{
		truncate: true,
		answers: func(dnsmessage.Question) []dnsmessage.Resource {
			return []dnsmessage.Resource{aRecord("example.test.", "192.0.2.10")}
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := NewDNS().Check(ctx, []byte(dnsConfigFor(resolver, `"expected_values":["192.0.2.10"]`)))
	if obs.Status != model.StatusUp {
		t.Errorf("status = %s, want up (%s)", obs.Status, obs.Message)
	}
}

// An operator writing an MX expectation writes the hostname, not the preference
// number in front of it. Both spellings have to match or the feature is a
// support conversation.
func TestDNSMXMatchesHostAlone(t *testing.T) {
	t.Parallel()

	resolver := startFakeResolver(t, &fakeResolver{
		answers: func(dnsmessage.Question) []dnsmessage.Resource {
			return []dnsmessage.Resource{mxRecord("example.test.", 10, "mail.example.test.")}
		},
	})
	host, port, _ := net.SplitHostPort(resolver.addr)
	base := `{"hostname":"example.test","record_type":"MX","resolver":"` + host + `","resolver_port":` + port

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, expected := range []string{"mail.example.test", "10 mail.example.test"} {
		config := base + `,"expected_values":["` + expected + `"]}`
		obs := NewDNS().Check(ctx, []byte(config))
		if obs.Status != model.StatusUp {
			t.Errorf("expected value %q did not match: %s", expected, obs.Message)
		}
	}
}

// A CNAME in the answer to an A query is the chain, not the answer. Counting it
// would fail every expected-values assertion against an aliased record.
func TestDNSIgnoresOffTypeRecords(t *testing.T) {
	t.Parallel()

	resolver := startFakeResolver(t, &fakeResolver{
		answers: func(dnsmessage.Question) []dnsmessage.Resource {
			return []dnsmessage.Resource{
				{
					Header: dnsmessage.ResourceHeader{
						Name:  dnsmessage.MustNewName("example.test."),
						Type:  dnsmessage.TypeCNAME,
						Class: dnsmessage.ClassINET,
					},
					Body: &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("target.test.")},
				},
				aRecord("target.test.", "192.0.2.10"),
			}
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := NewDNS().Check(ctx, []byte(dnsConfigFor(resolver, `"expected_values":["192.0.2.10"],"match_mode":"exact"`)))
	if obs.Status != model.StatusUp {
		t.Errorf("status = %s, want up (%s)", obs.Status, obs.Message)
	}
}

func TestDNSValidate(t *testing.T) {
	t.Parallel()

	checker := NewDNS()
	rejected := map[string]string{
		"no hostname":       `{"record_type":"A"}`,
		"no record type":    `{"hostname":"example.com"}`,
		"unknown type":      `{"hostname":"example.com","record_type":"ANY"}`,
		"resolver hostname": `{"hostname":"example.com","record_type":"A","resolver":"dns.example.com"}`,
		"bad match mode":    `{"hostname":"example.com","record_type":"A","match_mode":"some"}`,
		"exact with none":   `{"hostname":"example.com","record_type":"A","match_mode":"exact"}`,
		"unknown field":     `{"hostname":"example.com","record_type":"A","nope":1}`,
	}
	for name, config := range rejected {
		if err := checker.Validate([]byte(config)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	for _, recordType := range sortedRecordTypes() {
		config := `{"hostname":"example.com","record_type":"` + recordType + `"}`
		if err := checker.Validate([]byte(config)); err != nil {
			t.Errorf("record type %s rejected: %v", recordType, err)
		}
	}
}

func TestReverseName(t *testing.T) {
	t.Parallel()

	name, err := queryName("8.8.4.4", "PTR")
	if err != nil {
		t.Fatalf("queryName: %v", err)
	}
	if got := name.String(); got != "4.4.8.8.in-addr.arpa." {
		t.Errorf("PTR name = %q, want 4.4.8.8.in-addr.arpa.", got)
	}

	name, err = queryName("2001:db8::1", "PTR")
	if err != nil {
		t.Fatalf("queryName v6: %v", err)
	}
	want := "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa."
	if got := name.String(); got != want {
		t.Errorf("PTR name = %q, want %q", got, want)
	}
}
