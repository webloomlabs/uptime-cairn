package notify

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestComposeMailHeaders(t *testing.T) {
	t.Parallel()

	ev := sampleEvent()
	message := composeMail(conf(cfg(`{"from_address":"cairn@example.com","from_name":"Uptime Cairn"}`)),
		ev, []string{"ops@example.com"}, []string{"cc@example.com"})

	headers, body, found := strings.Cut(message, "\r\n\r\n")
	if !found {
		t.Fatal("no header/body separator")
	}

	for _, want := range []string{
		`From: "Uptime Cairn" <cairn@example.com>`,
		"To: ops@example.com",
		"Cc: cc@example.com",
		"Content-Transfer-Encoding: base64",
		"Auto-Submitted: auto-generated",
	} {
		if !strings.Contains(headers, want) {
			t.Errorf("missing header %q in:\n%s", want, headers)
		}
	}

	// An outage and its recovery thread together, so a mailbox shows one
	// conversation per incident rather than two messages to match up by eye.
	if !strings.Contains(headers, "References: <"+ev.DedupKey()+"@uptime-cairn>") {
		t.Errorf("no threading header:\n%s", headers)
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(body), "\r\n", ""))
	if err != nil {
		t.Fatalf("body is not base64: %v", err)
	}
	if !strings.Contains(string(decoded), "unexpected status 503") {
		t.Errorf("the check's own message is missing from the body:\n%s", decoded)
	}
}

// SMTP's 998-octet line limit is real, and a user template can produce a line of
// any length. base64 is what keeps a long one deliverable.
// A display name outside ASCII must arrive readable rather than as mojibake,
// and one containing a comma must not split the header into two addresses.
func TestComposeMailEncodesAwkwardDisplayNames(t *testing.T) {
	t.Parallel()

	message := composeMail(conf(cfg(`{"from_address":"c@example.com","from_name":"Überwachung, Ops"}`)),
		sampleEvent(), []string{"ops@example.com"}, nil)

	header, _, _ := strings.Cut(message, "\r\n\r\n")
	line := ""
	for _, candidate := range strings.Split(header, "\r\n") {
		if strings.HasPrefix(candidate, "From: ") {
			line = strings.TrimPrefix(candidate, "From: ")
		}
	}
	if line == "" {
		t.Fatalf("no From header:\n%s", header)
	}

	address, err := mail.ParseAddress(line)
	if err != nil {
		t.Fatalf("From is not a parseable address: %v (%q)", err, line)
	}
	if address.Name != "Überwachung, Ops" {
		t.Errorf("display name round-tripped as %q", address.Name)
	}
	if address.Address != "c@example.com" {
		t.Errorf("address = %q", address.Address)
	}
}

func TestComposeMailWrapsLongLines(t *testing.T) {
	t.Parallel()

	ev := sampleEvent()
	ev.Heartbeat.Message = strings.Repeat("x", 5000)
	message := composeMail(conf(cfg(`{"from_address":"c@example.com"}`)), ev, []string{"ops@example.com"}, nil)

	for _, line := range strings.Split(message, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a %d-octet line would be refused by a conforming server", len(line))
		}
	}
}

// The whole conversation, against a server that answers like a real one. The
// per-line assertions above cannot catch an out-of-order command.
func TestSendEmailSpeaksSMTP(t *testing.T) {
	t.Parallel()

	server := newFakeSMTP(t)
	sender := NewSender()

	config := cfg(`{
		"to": ["ops@example.com"],
		"cc": ["cc@example.com"],
		"use_instance_smtp": false,
		"smtp_encryption": "none",
		"from_address": "cairn@example.com"
	}`)
	config["smtp_host"] = server.host
	config["smtp_port"] = float64(server.port)

	receipt, err := sendEmail(context.Background(), sender, conf(config), sampleEvent())
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(receipt.Payload, "Subject:") {
		t.Errorf("the delivery log recorded no message: %q", receipt.Payload)
	}

	conversation := server.transcript()
	for _, want := range []string{
		"MAIL FROM:<cairn@example.com>",
		"RCPT TO:<ops@example.com>",
		"RCPT TO:<cc@example.com>",
		"DATA",
		"QUIT",
	} {
		if !strings.Contains(conversation, want) {
			t.Errorf("missing %q in:\n%s", want, conversation)
		}
	}
}

// fakeSMTP is a server that says yes to everything, which is enough to prove the
// client speaks the protocol in the right order.
type fakeSMTP struct {
	host string
	port int

	mu       sync.Mutex
	lines    []string
	messages []sentMail
}

// sentMail is one complete message the server accepted. Captured per connection
// rather than read out of the transcript, because a status page bulletin opens
// several connections at once and an interleaved transcript cannot say which
// body went to which address — which is exactly what those tests are about.
type sentMail struct {
	recipients []string
	data       string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	server := &fakeSMTP{host: host, port: port}

	// A loop rather than a single Accept: a status page bulletin opens one
	// connection per subscriber, and a server that answers the first and hangs
	// up on the rest would make a fan-out test pass for the wrong reason.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				server.serve(conn)
			}()
		}
	}()
	return server
}

func (s *fakeSMTP) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	write("220 fake ESMTP")
	inData := false
	current := sentMail{}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		s.mu.Lock()
		s.lines = append(s.lines, line)
		s.mu.Unlock()

		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.messages = append(s.messages, current)
				s.mu.Unlock()
				current = sentMail{}
				write("250 2.0.0 Ok: queued")
				continue
			}
			current.data += line + "\r\n"
			continue
		}

		switch verb := strings.ToUpper(strings.Fields(line + " ")[0]); verb {
		case "EHLO":
			write("250-fake")
			write("250 8BITMIME")
		case "HELO":
			write("250 fake")
		case "RCPT":
			// "RCPT TO:<someone@example.com>". The verb is matched
			// case-insensitively and the address is taken verbatim, because it
			// is the thing being asserted on.
			if _, address, found := strings.Cut(line, ":"); found {
				current.recipients = append(current.recipients,
					strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(address), "<"), ">"))
			}
			write("250 2.0.0 Ok")
		case "DATA":
			inData = true
			write("354 End data with <CR><LF>.<CR><LF>")
		case "QUIT":
			write("221 Bye")
			return
		default:
			write("250 2.0.0 Ok")
		}
	}
}

// sent returns the messages the server has accepted so far.
func (s *fakeSMTP) sent() []sentMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sentMail(nil), s.messages...)
}

func (s *fakeSMTP) transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}
