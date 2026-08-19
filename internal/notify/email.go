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
	"time"
)

// SMTP, from the standard library.
//
// No mail dependency, which AGENTS.md §5 asks for and which is easy to justify
// here: the message this program sends is one text/plain part with no
// attachments, and the hard part of SMTP is not composing that — it is the
// connection modes, which net/smtp already handles.
//
// One deliberate omission: instance-wide SMTP settings. The spec's
// use_instance_smtp defaults to true and this build has no settings endpoint to
// hold them, so a channel that asks for them is refused at save time with the
// alternative spelled out, rather than accepted and silently undeliverable.

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
