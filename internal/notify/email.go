package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SMTP, from the standard library.
//
// No mail dependency, which AGENTS.md §5 asks for and which is easy to justify
// here: the message this program sends is one text/plain part with no
// attachments, and the hard part of SMTP is not composing that — it is the
// connection modes, which net/smtp already handles.
//
// Instance-wide SMTP is supported: a channel with use_instance_smtp true
// inherits the relay configured under /api/v1/settings, and one that names its
// own host keeps it. A channel asking for the instance relay when none is
// configured is refused at save time with the alternative spelled out, rather
// than accepted and silently undeliverable.

// smtpTimeout bounds the whole conversation. A mail server that accepts the
// connection and then stalls is a common failure, and without this the delivery
// worker would sit in it.
const smtpTimeout = 30 * time.Second

func sendEmail(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	encryption := c.str("smtp_encryption", "starttls")
	host := c.str("smtp_host", "")
	port := c.num("smtp_port", defaultSMTPPort(encryption))

	to := c.list("to")
	cc := c.list("cc")
	if len(to) == 0 {
		return Receipt{}, fmt.Errorf("no recipients configured")
	}

	body := composeMail(c, ev, to, cc)
	receipt := Receipt{Payload: truncate(body, maxRecordedPayload)}

	ctx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()

	address := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := &net.Dialer{}

	var conn net.Conn
	var err error
	if encryption == "tls" {
		// Implicit TLS, the port-465 form: the connection is encrypted before
		// the server says anything.
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: host}}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return receipt, fmt.Errorf("connect to %s: %w", address, err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return receipt, fmt.Errorf("smtp handshake: %w", err)
	}
	defer func() { _ = client.Close() }()

	if encryption == "starttls" {
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return receipt, fmt.Errorf("starttls: %w", err)
		}
	}

	if user := c.str("smtp_username", ""); user != "" {
		// PlainAuth refuses to send a password over an unencrypted connection to
		// anything but localhost. That refusal is the standard library
		// protecting the operator, and it is not this program's place to work
		// around it — the resulting error names the problem.
		auth := smtp.PlainAuth("", user, c.str("smtp_password", ""), host)
		if err := client.Auth(auth); err != nil {
			return receipt, fmt.Errorf("smtp authentication: %w", err)
		}
	}

	if err := client.Mail(envelopeSender(c)); err != nil {
		return receipt, fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, recipient := range append(append([]string{}, to...), cc...) {
		if err := client.Rcpt(recipient); err != nil {
			return receipt, fmt.Errorf("RCPT TO %s: %w", recipient, err)
		}
	}

	writer, err := client.Data()
	if err != nil {
		return receipt, fmt.Errorf("DATA: %w", err)
	}
	if _, err := writer.Write([]byte(body)); err != nil {
		return receipt, fmt.Errorf("write message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return receipt, fmt.Errorf("finish message: %w", err)
	}

	// Quit rather than just closing: a server that has accepted the message
	// still gets to say so, and a QUIT failure after a clean DATA close is not
	// a delivery failure.
	_ = client.Quit()
	return receipt, nil
}

func defaultSMTPPort(encryption string) int {
	switch encryption {
	case "tls":
		return 465
	case "none":
		return 25
	default:
		return 587
	}
}

func envelopeSender(c conf) string { return c.str("from_address", "") }

// composeMail builds one text/plain message.
//
// base64 for the body rather than quoted-printable, because SMTP's 998-octet
// line limit is a real constraint and a user template can contain a line of any
// length. The subject is RFC 2047 encoded for the same reason: a monitor named
// in anything but ASCII must not arrive as mojibake.
func composeMail(c conf, ev Event, to, cc []string) string {
	// net/mail does the quoting and the RFC 2047 encoding, and gets both right
	// for a display name containing a comma, a quote, or anything outside ASCII
	// — three cases a hand-built header gets wrong in three different ways.
	from := c.str("from_address", "")
	if name := c.str("from_name", ""); name != "" {
		from = (&mail.Address{Name: name, Address: from}).String()
	}

	var headers strings.Builder
	fmt.Fprintf(&headers, "From: %s\r\n", from)
	fmt.Fprintf(&headers, "To: %s\r\n", strings.Join(to, ", "))
	if len(cc) > 0 {
		fmt.Fprintf(&headers, "Cc: %s\r\n", strings.Join(cc, ", "))
	}
	fmt.Fprintf(&headers, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", Title(ev)))
	fmt.Fprintf(&headers, "Date: %s\r\n", ev.OccurredAt.Format(time.RFC1123Z))
	fmt.Fprintf(&headers, "Message-ID: <%s@uptime-cairn>\r\n", ev.ID.String())
	// Threads an outage and its recovery together in any client that honours it,
	// so a mailbox shows one conversation per incident rather than two messages
	// that have to be matched by eye.
	fmt.Fprintf(&headers, "References: <%s@uptime-cairn>\r\n", ev.DedupKey())
	headers.WriteString("MIME-Version: 1.0\r\n")
	headers.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	headers.WriteString("Content-Transfer-Encoding: base64\r\n")
	headers.WriteString("Auto-Submitted: auto-generated\r\n")
	headers.WriteString("\r\n")

	encoded := base64.StdEncoding.EncodeToString([]byte(Title(ev) + "\n\n" + Body(ev) + "\n"))
	for len(encoded) > 76 {
		headers.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	headers.WriteString(encoded + "\r\n")

	return headers.String()
}

// Instance-wide SMTP.
//
// Held in the package rather than threaded through every call because the two
// places that need it are far apart: config validation, which is a package
// function called from the API's write path, and delivery, which happens on a
// worker several layers down. Passing it through both would mean a parameter on
// Validate and a field on Sender for one channel type out of thirteen.
//
// The password is the plaintext, held only in memory. It arrives from the
// settings endpoint, which opened the sealed envelope, and is never written back
// anywhere from here.
var instanceSMTP struct {
	mu       sync.RWMutex
	settings InstanceSMTP
}

// InstanceSMTP is the relay an email channel uses when use_instance_smtp is true.
type InstanceSMTP struct {
	Host        string
	Port        int
	Username    string
	Password    string
	Encryption  string
	FromAddress string
	FromName    string
}

// Configured reports whether the relay is usable. Host and sender both, because
// a relay with no sender address fails on its first message.
func (s InstanceSMTP) Configured() bool { return s.Host != "" && s.FromAddress != "" }

// SetInstanceSMTP installs the instance relay. Called at startup from the stored
// settings and again whenever they change, so an operator who configures mail
// does not have to restart before the next alert uses it.
func SetInstanceSMTP(settings InstanceSMTP) {
	instanceSMTP.mu.Lock()
	defer instanceSMTP.mu.Unlock()
	instanceSMTP.settings = settings
}

// InstanceSMTPConfigured reports whether a relay is available, which is what
// decides whether an email channel asking for one is accepted.
func InstanceSMTPConfigured() bool {
	instanceSMTP.mu.RLock()
	defer instanceSMTP.mu.RUnlock()
	return instanceSMTP.settings.Configured()
}

// withInstanceSMTP overlays the instance relay onto a channel's config.
//
// The channel's own values win where it set them, so a channel that names a
// from_address keeps it while inheriting the host and credentials. That is the
// useful case: one relay, several senders.
func withInstanceSMTP(config map[string]any) map[string]any {
	useInstance := true
	if v, ok := config["use_instance_smtp"].(bool); ok {
		useInstance = v
	}
	if !useInstance {
		return config
	}

	instanceSMTP.mu.RLock()
	settings := instanceSMTP.settings
	instanceSMTP.mu.RUnlock()

	if !settings.Configured() {
		return config
	}

	merged := make(map[string]any, len(config)+7)
	for key, value := range config {
		merged[key] = value
	}
	for key, value := range map[string]any{
		"smtp_host":       settings.Host,
		"smtp_port":       settings.Port,
		"smtp_username":   settings.Username,
		"smtp_password":   settings.Password,
		"smtp_encryption": settings.Encryption,
		"from_address":    settings.FromAddress,
		"from_name":       settings.FromName,
	} {
		if value == "" || value == 0 {
			continue
		}
		if existing, set := merged[key]; set && existing != "" && existing != nil {
			continue
		}
		merged[key] = value
	}
	return merged
}
