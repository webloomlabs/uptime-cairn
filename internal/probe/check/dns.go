package check

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// DNS implements the dns monitor type.
//
// It speaks the wire protocol rather than going through net.Resolver, for three
// reasons the standard resolver cannot give us. The response code is the single
// most useful field a DNS monitor can record — NXDOMAIN and SERVFAIL are
// different outages with different owners — and net.Resolver folds both into an
// error string. Half the record types the spec lists (CAA, SOA) have no resolver
// method at all. And "check this specific resolver" is the point of the monitor:
// asking the system resolver whether the system resolver works is not a check.
type DNS struct{}

// NewDNS builds the checker.
func NewDNS() *DNS { return &DNS{} }

// Type implements Checker.
func (d *DNS) Type() string { return model.TypeDNS }

// Version implements Checker.
func (d *DNS) Version() uint32 { return 1 }

// dnsConfig mirrors DnsConfig in docs/api/openapi.yaml.
type dnsConfig struct {
	Hostname       string   `json:"hostname"`
	RecordType     string   `json:"record_type"`
	Resolver       *string  `json:"resolver"`
	ResolverPort   *int     `json:"resolver_port"`
	ExpectedValues []string `json:"expected_values"`
	MatchMode      string   `json:"match_mode"`
}

const (
	defaultResolverPort = 53

	// typeCAA is queried by number: dnsmessage has no CAA resource type, so the
	// answer comes back as raw rdata and is decoded below. Certificate authority
	// authorisation is worth monitoring precisely because a wrong record is
	// invisible until an issuance fails.
	typeCAA dnsmessage.Type = 257
)

// rcodeNames spells the response codes the way DNS does. dnsmessage's own
// String() returns its Go constant name ("RCodeSuccess"), and heartbeats.code is
// stored history — an operator grepping for NXDOMAIN a year from now should find
// it.
var rcodeNames = map[dnsmessage.RCode]string{
	dnsmessage.RCodeSuccess:        "NOERROR",
	dnsmessage.RCodeFormatError:    "FORMERR",
	dnsmessage.RCodeServerFailure:  "SERVFAIL",
	dnsmessage.RCodeNameError:      "NXDOMAIN",
	dnsmessage.RCodeNotImplemented: "NOTIMP",
	dnsmessage.RCodeRefused:        "REFUSED",
}

func rcodeName(code dnsmessage.RCode) string {
	if name, ok := rcodeNames[code]; ok {
		return name
	}
	return "RCODE" + strconv.Itoa(int(code))
}

var recordTypes = map[string]dnsmessage.Type{
	"A":     dnsmessage.TypeA,
	"AAAA":  dnsmessage.TypeAAAA,
	"CNAME": dnsmessage.TypeCNAME,
	"MX":    dnsmessage.TypeMX,
	"NS":    dnsmessage.TypeNS,
	"TXT":   dnsmessage.TypeTXT,
	"SRV":   dnsmessage.TypeSRV,
	"CAA":   typeCAA,
	"SOA":   dnsmessage.TypeSOA,
	"PTR":   dnsmessage.TypePTR,
}

// Validate implements Checker.
func (d *DNS) Validate(config []byte) error {
	cfg, err := decodeDNSConfig(config)
	if err != nil {
		return err
	}
	if cfg.Hostname == "" {
		return errors.New("hostname is required")
	}
	if _, ok := recordTypes[cfg.RecordType]; !ok {
		return fmt.Errorf("record_type %q: want one of %s", cfg.RecordType, strings.Join(sortedRecordTypes(), ", "))
	}
	if _, err := queryName(cfg.Hostname, cfg.RecordType); err != nil {
		return err
	}
	if cfg.Resolver != nil && *cfg.Resolver != "" {
		if net.ParseIP(*cfg.Resolver) == nil {
			// A hostname here would mean resolving a resolver with a resolver,
			// and the failure mode when that goes wrong is genuinely confusing.
			return fmt.Errorf("resolver %q must be an IP address", *cfg.Resolver)
		}
	}
	if cfg.ResolverPort != nil {
		if err := validatePort(*cfg.ResolverPort); err != nil {
			return fmt.Errorf("resolver_port: %w", err)
		}
	}
	switch cfg.MatchMode {
	case "", "any", "all", "exact":
	default:
		return fmt.Errorf("match_mode %q: want any, all, or exact", cfg.MatchMode)
	}
	if cfg.MatchMode == "exact" && len(cfg.ExpectedValues) == 0 {
		return errors.New("match_mode exact needs expected_values; with none it asserts the answer is empty, which no record satisfies")
	}
	return nil
}

// Check implements Checker.
func (d *DNS) Check(ctx context.Context, config []byte) Observation {
	cfg, err := decodeDNSConfig(config)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}

	name, err := queryName(cfg.Hostname, cfg.RecordType)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}

	servers, err := resolverAddresses(cfg)
	if err != nil {
		// No resolver configured and none discoverable: this probe cannot ask
		// the question. That is unknown, not down.
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}

	server, header, answers, elapsed, failure := query(ctx, servers, name, recordTypes[cfg.RecordType])
	if failure != nil {
		return *failure
	}

	obs := Observation{
		Status:       model.StatusUp,
		ResponseTime: &elapsed,
		Code:         rcodeName(header.RCode),
	}

	if header.RCode != dnsmessage.RCodeSuccess {
		// A response code from the resolver is an answer about the name, so it
		// is a verdict: NXDOMAIN means the record the user asked about is gone.
		obs.Status = model.StatusDown
		obs.Class = ClassDNS
		obs.Message = fmt.Sprintf("%s answered %s for %s %s", server, rcodeName(header.RCode), cfg.RecordType, cfg.Hostname)
		return obs
	}
	if len(answers) == 0 {
		obs.Status = model.StatusDown
		obs.Class = ClassAssertion
		obs.Message = fmt.Sprintf("no %s record for %s", cfg.RecordType, cfg.Hostname)
		return obs
	}

	if msg := matchAnswers(cfg, answers); msg != "" {
		obs.Status = model.StatusDown
		obs.Class = ClassAssertion
		obs.Message = msg
		return obs
	}
	obs.Message = strings.Join(canonical(answers), ", ")
	return obs
}

// answer is one record, in two forms: the canonical rendering the message shows,
// and an alias for the compound types. An operator writing an expected value for
// an MX record will write "mail.example.com" as often as "10 mail.example.com",
// and refusing the first is pedantry that costs a support conversation.
type answer struct {
	value string
	alias string
}

func matchAnswers(cfg dnsConfig, answers []answer) string {
	if len(cfg.ExpectedValues) == 0 {
		return ""
	}

	mode := cfg.MatchMode
	if mode == "" {
		mode = "any"
	}
	got := canonical(answers)

	matched := func(expected string) bool {
		for _, a := range answers {
			if strings.EqualFold(a.value, expected) || (a.alias != "" && strings.EqualFold(a.alias, expected)) {
				return true
			}
		}
		return false
	}

	switch mode {
	case "any":
		for _, expected := range cfg.ExpectedValues {
			if matched(expected) {
				return ""
			}
		}
		return fmt.Sprintf("none of the expected values are present; answer was %s", strings.Join(got, ", "))
	case "all":
		var missing []string
		for _, expected := range cfg.ExpectedValues {
			if !matched(expected) {
				missing = append(missing, expected)
			}
		}
		if len(missing) == 0 {
			return ""
		}
		return fmt.Sprintf("missing expected %s: %s", plural(len(missing), "value"), strings.Join(missing, ", "))
	default: // exact
		want := make([]string, len(cfg.ExpectedValues))
		copy(want, cfg.ExpectedValues)
		have := append([]string(nil), got...)
		sortFold(want)
		sortFold(have)
		if len(want) == len(have) {
			same := true
			for i := range want {
				if !strings.EqualFold(want[i], have[i]) {
					same = false
					break
				}
			}
			if same {
				return ""
			}
		}
		return fmt.Sprintf("answer %s does not exactly match %s", strings.Join(got, ", "), strings.Join(cfg.ExpectedValues, ", "))
	}
}

func canonical(answers []answer) []string {
	out := make([]string, len(answers))
	for i, a := range answers {
		out[i] = a.value
	}
	return out
}

func sortFold(s []string) {
	sort.Slice(s, func(i, j int) bool { return strings.ToLower(s[i]) < strings.ToLower(s[j]) })
}

// exchange sends one query over UDP and retries over TCP when the answer is
// truncated. Skipping the TCP retry is how a monitor comes to report "no TXT
// record" for a domain whose TXT records are simply larger than 512 bytes.
// query asks each resolver in turn and returns the first that answers.
//
// "Answers" means the resolver responded, not that it liked the question:
// NXDOMAIN is a verdict about the record and stops the walk, because a second
// resolver disagreeing with the first is not something to paper over. Only a
// resolver that could not be reached moves on to the next one.
//
// Each attempt gets an equal share of whatever time is left, so a host with
// three nameservers and a dead first one cannot spend the monitor's entire
// timeout discovering that. With one candidate the share is the whole budget,
// which is what a monitor naming its own resolver should get.
//
// The failure returned when nothing answers describes the *first* resolver
// tried, because that is the one the operator expected to be used, and appends
// how many others were tried after it.
func query(ctx context.Context, servers []string, name dnsmessage.Name, recordType dnsmessage.Type) (
	server string, header dnsmessage.Header, answers []answer, elapsed time.Duration, failure *Observation,
) {
	var first *Observation

	for i, candidate := range servers {
		attemptCtx, cancel := shareOfDeadline(ctx, len(servers)-i)

		start := time.Now()
		h, a, err := exchange(attemptCtx, candidate, name, recordType)
		took := time.Since(start)
		cancel()

		if err == nil {
			return candidate, h, a, took, nil
		}

		obs := classify(err, took)
		// A failure here is a failure to reach the resolver, not a verdict about
		// the record: a resolver we cannot reach tells us nothing about whether
		// the name resolves elsewhere.
		if obs.Status == model.StatusDown {
			obs.Status = model.StatusUnknown
		}
		obs.Class = ClassDNS
		obs.Message = "querying " + candidate + ": " + obs.Message
		if first == nil {
			first = &obs
		}

		// The parent deadline is spent; trying the rest would only report the
		// same cancellation.
		if ctx.Err() != nil {
			break
		}
	}

	if len(servers) > 1 && first != nil {
		first.Message += fmt.Sprintf(" (and %d other resolver(s) from /etc/resolv.conf)", len(servers)-1)
	}
	return "", dnsmessage.Header{}, nil, 0, first
}

// shareOfDeadline gives one attempt its portion of the time left, and never more
// than the caller has. With no deadline on the parent there is nothing to divide,
// so the attempt inherits the parent unchanged.
func shareOfDeadline(ctx context.Context, remaining int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || remaining <= 1 {
		return context.WithCancel(ctx)
	}
	share := time.Until(deadline) / time.Duration(remaining)
	if share <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, share)
}

func exchange(ctx context.Context, server string, name dnsmessage.Name, recordType dnsmessage.Type) (dnsmessage.Header, []answer, error) {
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return dnsmessage.Header{}, nil, fmt.Errorf("seed query id: %w", err)
	}
	id := binary.BigEndian.Uint16(idBytes[:])

	query, err := buildQuery(id, name, recordType)
	if err != nil {
		return dnsmessage.Header{}, nil, err
	}

	response, err := roundTrip(ctx, "udp", server, query)
	if err != nil {
		return dnsmessage.Header{}, nil, err
	}

	header, answers, truncated, err := parseResponse(response, id, recordType)
	if err != nil {
		return dnsmessage.Header{}, nil, err
	}
	if !truncated {
		return header, answers, nil
	}

	response, err = roundTrip(ctx, "tcp", server, query)
	if err != nil {
		return dnsmessage.Header{}, nil, fmt.Errorf("answer was truncated and the TCP retry failed: %w", err)
	}
	header, answers, _, err = parseResponse(response, id, recordType)
	return header, answers, err
}

func buildQuery(id uint16, name dnsmessage.Name, recordType dnsmessage.Type) ([]byte, error) {
	builder := dnsmessage.NewBuilder(make([]byte, 0, 512), dnsmessage.Header{
		ID:               id,
		RecursionDesired: true,
	})
	if err := builder.StartQuestions(); err != nil {
		return nil, fmt.Errorf("build query: %w", err)
	}
	if err := builder.Question(dnsmessage.Question{
		Name:  name,
		Type:  recordType,
		Class: dnsmessage.ClassINET,
	}); err != nil {
		return nil, fmt.Errorf("build query: %w", err)
	}
	msg, err := builder.Finish()
	if err != nil {
		return nil, fmt.Errorf("build query: %w", err)
	}
	return msg, nil
}

// roundTrip writes one query and reads one response. UDP carries the message
// bare; TCP prefixes it with a two-byte length, per RFC 1035 §4.2.2.
func roundTrip(ctx context.Context, network, server string, query []byte) ([]byte, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if network == "tcp" {
		framed := make([]byte, 2+len(query))
		binary.BigEndian.PutUint16(framed, uint16(len(query))) //nolint:gosec // a DNS query this package builds is far below 65535 bytes
		copy(framed[2:], query)
		if _, err := conn.Write(framed); err != nil {
			return nil, err
		}
		var length [2]byte
		if _, err := readFull(conn, length[:]); err != nil {
			return nil, err
		}
		body := make([]byte, binary.BigEndian.Uint16(length[:]))
		if _, err := readFull(conn, body); err != nil {
			return nil, err
		}
		return body, nil
	}

	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	// 4096 is the EDNS0 advertised size everything settled on; without EDNS0 the
	// resolver will not exceed 512 and will set the truncation bit instead.
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := conn.Read(buf[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

// parseResponse decodes the answer section into rendered values.
func parseResponse(response []byte, id uint16, recordType dnsmessage.Type) (dnsmessage.Header, []answer, bool, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(response)
	if err != nil {
		return dnsmessage.Header{}, nil, false, fmt.Errorf("malformed response: %w", err)
	}
	if header.ID != id {
		// Off-path spoofing, or a stale datagram from a previous query on a
		// reused port. Either way it is not the answer to the question we asked.
		return dnsmessage.Header{}, nil, false, errors.New("response id does not match the query")
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return dnsmessage.Header{}, nil, false, fmt.Errorf("malformed response: %w", err)
	}
	if header.Truncated {
		return header, nil, true, nil
	}

	var answers []answer
	for {
		resourceHeader, err := parser.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return dnsmessage.Header{}, nil, false, fmt.Errorf("malformed answer: %w", err)
		}

		// A CNAME in the answer to a non-CNAME query is the chain, not the
		// answer. Recording it as one would make an A monitor with expected
		// values fail against a perfectly correct aliased record.
		if resourceHeader.Type != recordType {
			if err := parser.SkipAnswer(); err != nil {
				return dnsmessage.Header{}, nil, false, fmt.Errorf("malformed answer: %w", err)
			}
			continue
		}

		got, err := parseResource(&parser, recordType)
		if err != nil {
			return dnsmessage.Header{}, nil, false, err
		}
		answers = append(answers, got)
	}
	return header, answers, false, nil
}

//nolint:cyclop // one arm per record type; splitting it would hide the mapping this function exists to state
func parseResource(parser *dnsmessage.Parser, recordType dnsmessage.Type) (answer, error) {
	switch recordType {
	case dnsmessage.TypeA:
		r, err := parser.AResource()
		if err != nil {
			return answer{}, err
		}
		return answer{value: net.IP(r.A[:]).String()}, nil
	case dnsmessage.TypeAAAA:
		r, err := parser.AAAAResource()
		if err != nil {
			return answer{}, err
		}
		return answer{value: net.IP(r.AAAA[:]).String()}, nil
	case dnsmessage.TypeCNAME:
		r, err := parser.CNAMEResource()
		if err != nil {
			return answer{}, err
		}
		return answer{value: trimRoot(r.CNAME.String())}, nil
	case dnsmessage.TypeNS:
		r, err := parser.NSResource()
		if err != nil {
			return answer{}, err
		}
		return answer{value: trimRoot(r.NS.String())}, nil
	case dnsmessage.TypePTR:
		r, err := parser.PTRResource()
		if err != nil {
			return answer{}, err
		}
		return answer{value: trimRoot(r.PTR.String())}, nil
	case dnsmessage.TypeMX:
		r, err := parser.MXResource()
		if err != nil {
			return answer{}, err
		}
		host := trimRoot(r.MX.String())
		return answer{value: strconv.Itoa(int(r.Pref)) + " " + host, alias: host}, nil
	case dnsmessage.TypeTXT:
		r, err := parser.TXTResource()
		if err != nil {
			return answer{}, err
		}
		// The strings of one TXT record are concatenated, which is what every
		// consumer of SPF and DKIM does with them.
		return answer{value: strings.Join(r.TXT, "")}, nil
	case dnsmessage.TypeSRV:
		r, err := parser.SRVResource()
		if err != nil {
			return answer{}, err
		}
		target := trimRoot(r.Target.String())
		return answer{
			value: fmt.Sprintf("%d %d %d %s", r.Priority, r.Weight, r.Port, target),
			alias: target,
		}, nil
	case dnsmessage.TypeSOA:
		r, err := parser.SOAResource()
		if err != nil {
			return answer{}, err
		}
		ns := trimRoot(r.NS.String())
		return answer{
			value: fmt.Sprintf("%s %s %d %d %d %d %d", ns, trimRoot(r.MBox.String()),
				r.Serial, r.Refresh, r.Retry, r.Expire, r.MinTTL),
			alias: ns,
		}, nil
	default: // CAA, decoded from raw rdata
		r, err := parser.UnknownResource()
		if err != nil {
			return answer{}, err
		}
		return parseCAA(r.Data)
	}
}

// parseCAA decodes RFC 8659 rdata: a one-byte flags field, a length-prefixed
// tag, and the value filling the remainder.
func parseCAA(data []byte) (answer, error) {
	if len(data) < 2 {
		return answer{}, errors.New("malformed CAA record")
	}
	flags := data[0]
	tagLen := int(data[1])
	if len(data) < 2+tagLen {
		return answer{}, errors.New("malformed CAA record")
	}
	tag := string(data[2 : 2+tagLen])
	value := string(data[2+tagLen:])
	return answer{
		value: fmt.Sprintf("%d %s %q", flags, tag, value),
		alias: value,
	}, nil
}

// queryName turns the configured hostname into a wire-format name, taking the
// reverse-lookup detour when the record type is PTR and the hostname is an
// address — which is the only way anyone actually writes a PTR monitor.
func queryName(hostname, recordType string) (dnsmessage.Name, error) {
	name := hostname
	if recordType == "PTR" {
		if ip := net.ParseIP(hostname); ip != nil {
			reversed, err := reverseName(ip)
			if err != nil {
				return dnsmessage.Name{}, err
			}
			name = reversed
		}
	}
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	parsed, err := dnsmessage.NewName(name)
	if err != nil {
		return dnsmessage.Name{}, fmt.Errorf("hostname %q: %w", hostname, err)
	}
	return parsed, nil
}

func reverseName(ip net.IP) (string, error) {
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa.", v4[3], v4[2], v4[1], v4[0]), nil
	}
	v6 := ip.To16()
	if v6 == nil {
		return "", fmt.Errorf("%s is not an IP address", ip)
	}
	var sb strings.Builder
	for i := len(v6) - 1; i >= 0; i-- {
		sb.WriteString(strconv.FormatUint(uint64(v6[i]&0x0f), 16))
		sb.WriteByte('.')
		sb.WriteString(strconv.FormatUint(uint64(v6[i]>>4), 16))
		sb.WriteByte('.')
	}
	sb.WriteString("ip6.arpa.")
	return sb.String(), nil
}

// resolverAddresses picks the servers to query, in the order they will be tried.
//
// A configured resolver is the whole list, and deliberately so: a DNS monitor
// naming a resolver exists to interrogate *that* resolver, and quietly asking a
// different one would answer a question nobody asked. Falling back to
// net.Resolver would defeat the same point.
//
// With none configured the list is every nameserver in resolv.conf, in file
// order. That file is a fallback list — every resolver implementation walks it
// until one answers — and taking only the first entry means a host whose primary
// nameserver is unreachable can never run a DNS monitor at all. Because a
// resolver that cannot be reached is reported as `unknown` rather than down (see
// Check), the symptom is not an alert: it is a monitor that sits on pending
// forever, showing no failures, monitoring nothing.
func resolverAddresses(cfg dnsConfig) ([]string, error) {
	port := defaultResolverPort
	if cfg.ResolverPort != nil {
		port = *cfg.ResolverPort
	}
	if cfg.Resolver != nil && *cfg.Resolver != "" {
		return []string{net.JoinHostPort(*cfg.Resolver, strconv.Itoa(port))}, nil
	}

	servers, err := systemNameservers()
	if err != nil || len(servers) == 0 {
		return nil, errors.New("no resolver configured and none found in /etc/resolv.conf; set resolver on this monitor")
	}

	out := make([]string, 0, len(servers))
	for _, server := range servers {
		out = append(out, net.JoinHostPort(server, strconv.Itoa(port)))
	}
	return out, nil
}

func systemNameservers() ([]string, error) {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var servers []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.IndexAny(line, "#;"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" && net.ParseIP(fields[1]) != nil {
			servers = append(servers, fields[1])
		}
	}
	return servers, scanner.Err()
}

func trimRoot(name string) string { return strings.TrimSuffix(name, ".") }

func sortedRecordTypes() []string {
	out := make([]string, 0, len(recordTypes))
	for t := range recordTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func decodeDNSConfig(config []byte) (dnsConfig, error) {
	var cfg dnsConfig
	dec := json.NewDecoder(strings.NewReader(string(config)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
