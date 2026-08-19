package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// The generic webhook: the channel that exists so the other twelve are not the
// limit. Method, headers, and body are user-defined with interpolation, which is
// the difference between integrating with an arbitrary endpoint and telling the
// user to go and write a bridge (PHASE-1-PLAN.md §3.4).

// Envelope is the default payload — the spec's EventEnvelope — sent whenever a
// webhook channel has no body template. Documented in the contract precisely so
// a receiver can be written against it before the sender is configured.
func Envelope(ev Event) []byte {
	data := map[string]any{
		"monitor": map[string]any{
			"id":          ev.Monitor.ID.String(),
			"name":        ev.Monitor.Name,
			"description": nullable(ev.Monitor.Description),
			"type":        ev.Monitor.Type,
			"target":      nullable(ev.Monitor.Target),
			"status":      ev.Monitor.Status,
		},
	}
	if ev.PreviousStatus != "" {
		data["previous_status"] = ev.PreviousStatus
	}
	if hb := ev.Heartbeat; hb != nil {
		data["heartbeat"] = map[string]any{
			"monitor_id":       ev.Monitor.ID.String(),
			"time":             hb.Time.Format(time.RFC3339Nano),
			"status":           hb.Status,
			"response_time_ms": hb.ResponseTimeMs,
			"message":          nullable(hb.Message),
			"code":             nullable(hb.Code),
			"attempt":          hb.Attempt,
			"important":        hb.Important,
		}
	}

	payload := map[string]any{
		"id":          ev.ID.String(),
		"type":        ev.Type,
		"occurred_at": ev.OccurredAt.Format(time.RFC3339Nano),
		"instance": map[string]any{
			"name":     ev.Instance.Name,
			"base_url": ev.Instance.BaseURL,
			"version":  ev.Instance.Version,
		},
		"data": data,
	}

	// Indented: a webhook payload is read by a human at least once, when they
	// are working out why their receiver rejected it.
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		// Every value here is a string, a number, a bool, or nil. Unreachable
		// short of a change to this function that breaks that.
		return []byte(`{"error":"could not encode event"}`)
	}
	return encoded
}

func sendWebhook(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	method := c.str("method", http.MethodPost)
	contentType := c.str("content_type", "application/json")

	var body []byte
	if method != http.MethodGet {
		tmpl := c.str("body_template", "")
		switch {
		case tmpl == "":
			body = Envelope(ev)
		default:
			// Escaping follows the declared content type, so a monitor named
			// `He said "hi"` produces a payload the receiver accepts. The
			// alternative is a template that works until the day a name has a
			// quote in it, which is the day of the outage.
			mode := EscapeNone
			if strings.Contains(strings.ToLower(contentType), "json") {
				mode = EscapeJSON
			}
			rendered, err := Render(tmpl, Context(ev), mode)
			if err != nil {
				return Receipt{}, err
			}
			body = []byte(rendered)
		}
	}

	// Header values interpolate too: an endpoint wanting the monitor id in a
	// header is a real integration, and templating only the body would not
	// reach it.
	headers := map[string]string{}
	for key, value := range c.mapping("headers") {
		rendered, err := Render(value, Context(ev), EscapeNone)
		if err != nil {
			return Receipt{}, err
		}
		headers[key] = rendered
	}

	return s.do(ctx, request{
		method:      method,
		url:         c.str("url", ""),
		contentType: contentType,
		body:        body,
		headers:     headers,
		verifyTLS:   c.flag("verify_tls", true),
		timeout:     time.Duration(c.num("timeout_seconds", 10)) * time.Second,
	})
}

// nullable renders an unset string as JSON null rather than "". The spec's
// fields are nullable, and a receiver testing for null should not have to also
// test for empty.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
