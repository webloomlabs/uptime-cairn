package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
)

// Resend (https://resend.com/docs/api-reference/emails/send-email): the same
// mail an SMTP channel sends, but over one HTTPS call instead of a relay the
// operator has to host, configure, and keep delivering to.
//
// It is a separate channel type rather than a mode of `email` because the
// configuration has almost nothing in common: no host, no port, no encryption
// negotiation, no instance relay to inherit — one API key and a sender. Folding
// it into the email schema would give every SMTP channel five fields it cannot
// use, and would make `use_instance_smtp` mean three things.
//
// The API key travels in the Authorization header, never in the body or the
// query string, so nothing about the credential reaches the delivery log: the
// recorded payload is the rendered message, which is what an operator needs to
// see when a send failed.
func sendResend(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	to := c.list("to")
	if len(to) == 0 {
		return Receipt{}, fmt.Errorf("no recipients configured")
	}

	text, err := message(c, "message_template", ev)
	if err != nil {
		return Receipt{}, err
	}

	// net/mail quotes the display name and RFC 2047-encodes it, so a from_name
	// containing a comma, a quote, or a non-ASCII character arrives intact —
	// the same reason composeMail builds the SMTP header through it.
	from := c.str("from_address", "")
	if name := c.str("from_name", ""); name != "" {
		from = (&mail.Address{Name: name, Address: from}).String()
	}

	payload := map[string]any{
		"from":    from,
		"to":      to,
		"subject": Title(ev),
		"text":    text,
	}
	if cc := c.list("cc"); len(cc) > 0 {
		payload["cc"] = cc
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}

	return s.do(ctx, request{
		url:         "https://api.resend.com/emails",
		contentType: "application/json",
		body:        body,
		headers:     map[string]string{"Authorization": "Bearer " + c.str("api_key", "")},
		verifyTLS:   true,
	})
}
