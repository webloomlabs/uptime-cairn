package notify

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"
)

// A message with files on it, through the instance relay.
//
// This is the third caller of the SMTP conversation in deliverSMTP — after
// notification channels and status-page bulletins — and it is here rather than in
// the package that needed it for the reason the second one was: the hard part of
// SMTP is the connection modes, and a second implementation of implicit TLS
// versus STARTTLS is a second thing to get wrong in the half that is used less
// often.
//
// What is genuinely new is the multipart body. The package doc says "one
// text/plain part with no attachments", which was true of every message this
// program sent until a report needed to travel as a file. That sentence is now
// wrong for this one path and right for the rest, and the split is deliberate:
// alerts stay single-part, because an alert with an attachment is a worse alert.
//
// # Why a report is attached rather than linked
//
// A link would be better — smaller messages, one copy, revocable. It needs a
// share link, and a share link is an unauthenticated credential whose generation
// is human-led work under AGENTS.md rule 8. Until that exists, a link in this
// message could only point at an authenticated endpoint, which is a link the
// client it was sent to cannot open. An attachment they can open beats a link
// they cannot.

// MaxMailBytes bounds one message.
//
// Fifteen megabytes of encoded body, which is under the twenty-five most relays
// accept and well under the ten megabytes some enforce on the receiving side
// after base64 inflates the payload by a third. A report over the limit is a
// **skipped** delivery naming the size rather than a message the relay rejects
// after this program has spent thirty seconds sending it — a refusal here is a
// row an operator can act on, and a relay's refusal is a stack trace in a log.
const MaxMailBytes = 15 << 20

// ErrNoRelay means no instance SMTP relay is configured. It is a skip rather
// than a failure: an install that has not configured mail has not failed to send
// mail, and recording it as a failure would put a red mark against an operator
// who never asked for email in the first place.
var ErrNoRelay = errors.New("no SMTP relay is configured")

// ErrMailTooLarge means the composed message exceeds MaxMailBytes.
var ErrMailTooLarge = errors.New("message exceeds the size limit")

// Attachment is one file on a message.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Mail is one message to send through the instance relay.
type Mail struct {
	To      []string
	Subject string

	// Body is text/plain. There is no HTML alternative, and that is a decision:
	// the report itself is the document, the covering message is a sentence, and
	// a two-part message would double the work for a paragraph nobody reads
	// twice.
	Body string

	Attachments []Attachment

	// MessageID makes the message identifiable and repeatable. Derived from the
	// thing being sent rather than from the clock, so a redelivery of the same
	// run threads with the original in any client that honours References
	// instead of arriving as an unrelated second message.
	MessageID string

	// Sent is the Date header. A parameter rather than a clock read, following
	// the rest of this subsystem.
	Sent time.Time
}

// SendMail delivers one message through the instance relay.
//
// Returns ErrNoRelay when none is configured, which the caller records as a
// skip. Every other error is a real delivery failure and is worth retrying.
func SendMail(ctx context.Context, m Mail) error {
	if len(m.To) == 0 {
		return errors.New("no recipients")
	}
	relay, ok := instanceRelay()
	if !ok {
		return ErrNoRelay
	}

	message, err := composeMultipart(relay, m)
	if err != nil {
		return err
	}
	return deliverSMTP(ctx, relay, m.To, message)
}

// composeMultipart builds the message.
//
// Single-part when there is nothing to attach, so a delivery with no artifact
// produces the same shape of message the rest of this package sends rather than
// a multipart wrapper around one text part.
func composeMultipart(r relay, m Mail) (string, error) {
	from := r.from
	if r.fromName != "" {
		// net/mail does the quoting and the RFC 2047 encoding, and gets both
		// right for a display name containing a comma, a quote, or anything
		// outside ASCII.
		from = (&mail.Address{Name: r.fromName, Address: r.from}).String()
	}

	sent := m.Sent
	if sent.IsZero() {
		sent = time.Now()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(m.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", sent.Format(time.RFC1123Z))
	if m.MessageID != "" {
		fmt.Fprintf(&b, "Message-ID: <%s@uptime-cairn>\r\n", m.MessageID)
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	// Auto-generated rather than auto-replied: a scheduled report is generated
	// by this program, and the header is what stops an out-of-office answering
	// it — which is how a mailing loop starts.
	b.WriteString("Auto-Submitted: auto-generated\r\n")

	if len(m.Attachments) == 0 {
		b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString(base64Lines(m.Body))
		return checkSize(b.String())
	}

	// A fixed boundary derived from the message id rather than a random one.
	// Deterministic output is the rule everywhere else in this subsystem, and a
	// boundary is one of the two places a random source would otherwise creep
	// back in. It cannot collide with the content: base64 has no hyphens.
	boundary := "cairn-" + boundaryToken(m.MessageID)

	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", boundary)
	b.WriteString("This is a message in MIME format.\r\n")

	fmt.Fprintf(&b, "\r\n--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b.WriteString(base64Lines(m.Body))

	for _, a := range m.Attachments {
		contentType := a.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		fmt.Fprintf(&b, "\r\n--%s\r\n", boundary)
		// The filename is RFC 2047 encoded in `name` and left plain in
		// `filename`, which is what mail clients in practice read. It comes from
		// the report's own naming rather than from a client-supplied string, so
		// there is no user input on it — the same property the artifact path
		// has, and for the same reason.
		fmt.Fprintf(&b, "Content-Type: %s; name=\"%s\"\r\n", contentType,
			mime.QEncoding.Encode("utf-8", a.Filename))
		b.WriteString("Content-Transfer-Encoding: base64\r\n")
		fmt.Fprintf(&b, "Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", a.Filename)
		b.WriteString(base64Bytes(a.Data))
	}

	fmt.Fprintf(&b, "\r\n--%s--\r\n", boundary)
	return checkSize(b.String())
}

func checkSize(message string) (string, error) {
	if len(message) > MaxMailBytes {
		return "", fmt.Errorf("%w: %d bytes, limit is %d", ErrMailTooLarge, len(message), MaxMailBytes)
	}
	return message, nil
}

// boundaryToken reduces an identifier to something safe in a boundary. Empty
// input still produces a usable boundary rather than a bare prefix, because two
// parts separated by "--cairn-" and nothing else is a message no client can
// parse.
func boundaryToken(id string) string {
	var out strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out.WriteRune(r)
		}
	}
	if out.Len() == 0 {
		return "boundary"
	}
	return out.String()
}

// base64Bytes encodes an attachment, wrapped to the 76 columns RFC 2045 fixes.
func base64Bytes(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)

	var out strings.Builder
	out.Grow(len(encoded) + len(encoded)/76*2 + 2)
	for len(encoded) > 76 {
		out.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	out.WriteString(encoded + "\r\n")
	return out.String()
}
