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

func sendEmail(ctx context.Context, _ *Sender, c conf, ev Event) (Receipt, error) {
	to := c.list("to")
	cc := c.list("cc")
	if len(to) == 0 {
		return Receipt{}, fmt.Errorf("no recipients configured")
	}

	body := composeMail(c, ev, to, cc)
	receipt := Receipt{Payload: truncate(body, maxRecordedPayload)}

	return receipt, deliverSMTP(ctx, relayFrom(c), append(append([]string{}, to...), cc...), body)
}

// relay is a resolved SMTP endpoint: what to dial, how to encrypt it, and who to
// authenticate as.
//
// It exists because two callers now need the same conversation with different
// message-building around it — a notification channel, whose settings come from
// its own config, and a status page bulletin, whose settings come from the
// instance relay and which builds one message per recipient. Duplicating the
// connection modes for the second one would mean two implementations of implicit
// TLS versus STARTTLS, and the one used less often would be the one that breaks.
type relay struct {
	host       string
	port       int
	encryption string
	username   string
	password   string

	// from is the envelope sender, and the address a bounce goes to.
	from string
	// fromName is the display name, used only when a caller composes headers.
	fromName string
}

func relayFrom(c conf) relay {
	encryption := c.str("smtp_encryption", "starttls")
	return relay{
		host:       c.str("smtp_host", ""),
		port:       c.num("smtp_port", defaultSMTPPort(encryption)),
		encryption: encryption,
		username:   c.str("smtp_username", ""),
		password:   c.str("smtp_password", ""),
		from:       c.str("from_address", ""),
		fromName:   c.str("from_name", ""),
	}
}

// deliverSMTP holds one conversation and hands over one message.
//
// A fresh connection per message rather than a pooled one, including on the
// bulletin path where several go out together. Pooling would mean holding a
// session open across a fan-out that can stall on any single recipient, and mail
// servers close idle sessions on their own schedule — a reconnect that only
// happens under load is a bug that only appears under load.
func deliverSMTP(ctx context.Context, r relay, recipients []string, message string) error {
	ctx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()

	address := net.JoinHostPort(r.host, strconv.Itoa(r.port))
	dialer := &net.Dialer{}

	var conn net.Conn
	var err error
	if r.encryption == "tls" {
		// Implicit TLS, the port-465 form: the connection is encrypted before
		// the server says anything.
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: r.host}}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", address, err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, r.host)
	if err != nil {
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer func() { _ = client.Close() }()

	if r.encryption == "starttls" {
		if err := client.StartTLS(&tls.Config{ServerName: r.host}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}

	if r.username != "" {
		// PlainAuth refuses to send a password over an unencrypted connection to
		// anything but localhost. That refusal is the standard library
		// protecting the operator, and it is not this program's place to work
		// around it — the resulting error names the problem.
		if err := client.Auth(smtp.PlainAuth("", r.username, r.password, r.host)); err != nil {
			return fmt.Errorf("smtp authentication: %w", err)
		}
	}

	if err := client.Mail(r.from); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", recipient, err)
		}
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := writer.Write([]byte(message)); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish message: %w", err)
	}

	// Quit rather than just closing: a server that has accepted the message
	// still gets to say so, and a QUIT failure after a clean DATA close is not
	// a delivery failure.
	_ = client.Quit()
	return nil
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

	headers.WriteString(base64Lines(Title(ev) + "\n\n" + Body(ev)))
	return headers.String()
}

// base64Lines encodes a body and wraps it to the 76 columns RFC 2045 fixes.
//
// base64 rather than quoted-printable because SMTP's 998-octet line limit is a
// real constraint and a user template — or an incident update somebody pasted a
// URL into — can contain a line of any length.
func base64Lines(body string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(body + "\n"))

	var out strings.Builder
	for len(encoded) > 76 {
		out.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	out.WriteString(encoded + "\r\n")
	return out.String()
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
// decides whether an email channel asking for one is accepted — and whether a
// status page can promise a subscriber anything at all.
func InstanceSMTPConfigured() bool {
	instanceSMTP.mu.RLock()
	defer instanceSMTP.mu.RUnlock()
	return instanceSMTP.settings.Configured()
}

// instanceRelay resolves the instance-wide relay for a caller that has no
// channel config of its own. Status page bulletins are the case: a subscriber
// belongs to a page, not to a notification channel, so there is nothing else for
// them to inherit from.
func instanceRelay() (relay, bool) {
	instanceSMTP.mu.RLock()
	settings := instanceSMTP.settings
	instanceSMTP.mu.RUnlock()

	if !settings.Configured() {
		return relay{}, false
	}
	encryption := settings.Encryption
	if encryption == "" {
		encryption = "starttls"
	}
	port := settings.Port
	if port == 0 {
		port = defaultSMTPPort(encryption)
	}
	return relay{
		host:       settings.Host,
		port:       port,
		encryption: encryption,
		username:   settings.Username,
		password:   settings.Password,
		from:       settings.FromAddress,
		fromName:   settings.FromName,
	}, true
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
