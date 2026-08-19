package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// The three that wake somebody up. They differ from the chat channels in one
// structural way: an outage and its recovery are two edges of one incident, not
// two messages. Getting that wrong leaves an operator with a resolved service
// and an open page, which is the failure that makes people stop trusting the
// integration.

// ErrNotApplicable means the provider deliberately sent nothing — a recovery
// reaching a channel configured not to auto-resolve, for instance. Recorded as
// a suppressed delivery rather than a successful one, because "we chose not to"
// and "it worked" are different answers to the only question anybody asks after
// an incident.
var ErrNotApplicable = errors.New("nothing to deliver for this event")

func sendPagerDuty(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	action := "trigger"
	if ev.Resolves() {
		if !c.flag("auto_resolve", true) {
			return Receipt{}, ErrNotApplicable
		}
		action = "resolve"
	}

	payload := map[string]any{
		"routing_key":  c.str("integration_key", ""),
		"event_action": action,
		// Keyed by monitor, which is what makes the resolve close the incident
		// the trigger opened rather than opening a second one.
		"dedup_key": ev.DedupKey(),
	}
	if action == "trigger" {
		payload["payload"] = map[string]any{
			"summary":   truncate(Title(ev)+" — "+firstLine(Body(ev)), 1024),
			"severity":  c.str("severity", "error"),
			"source":    sourceOf(ev),
			"timestamp": ev.OccurredAt.Format("2006-01-02T15:04:05.000Z07:00"),
			"component": ev.Monitor.Type,
			"custom_details": map[string]any{
				"monitor":  ev.Monitor.Name,
				"target":   ev.Monitor.Target,
				"message":  heartbeatMessage(ev),
				"instance": ev.Instance.Name,
			},
		}
		if link := monitorURL(ev); link != "" {
			payload["links"] = []any{map[string]any{"href": link, "text": "Open in " + ev.Instance.Name}}
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}

	endpoint := "https://events.pagerduty.com/v2/enqueue"
	if c.str("region", "us") == "eu" {
		endpoint = "https://events.eu.pagerduty.com/v2/enqueue"
	}
	return s.postJSON(ctx, endpoint, body, nil)
}

func sendOpsgenie(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	host := "https://api.opsgenie.com"
	if c.str("region", "us") == "eu" {
		host = "https://api.eu.opsgenie.com"
	}
	headers := map[string]string{"Authorization": "GenieKey " + c.str("api_key", "")}

	// alias is Opsgenie's deduplication key, and closing by alias is what makes
	// a recovery close the alert this monitor opened.
	alias := ev.DedupKey()

	if ev.Resolves() {
		if !c.flag("auto_close", true) {
			return Receipt{}, ErrNotApplicable
		}
		body, err := json.Marshal(map[string]any{
			"note":   Title(ev) + " — " + firstLine(Body(ev)),
			"source": sourceOf(ev),
		})
		if err != nil {
			return Receipt{}, err
		}
		endpoint := fmt.Sprintf("%s/v2/alerts/%s/close?identifierType=alias", host, url.PathEscape(alias))
		return s.postJSON(ctx, endpoint, body, headers)
	}

	payload := map[string]any{
		// Opsgenie truncates message at 130 characters server-side; doing it
		// here means the text that survives is the text this program chose.
		"message":     truncate(Title(ev), 120),
		"alias":       alias,
		"description": Body(ev),
		"priority":    c.str("priority", "P3"),
		"source":      sourceOf(ev),
		"details": map[string]any{
			"monitor":  ev.Monitor.Name,
			"target":   ev.Monitor.Target,
			"type":     ev.Monitor.Type,
			"instance": ev.Instance.Name,
		},
	}
	if responders := opsgenieResponders(c); len(responders) > 0 {
		payload["responders"] = responders
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Receipt{}, err
	}
	return s.postJSON(ctx, host+"/v2/alerts", body, headers)
}

// opsgenieResponders maps the spec's {type, value} pairs onto the field Opsgenie
// names each entity by, which is not the same field for each type.
func opsgenieResponders(c conf) []any {
	var out []any
	for _, entry := range c.objects("responders") {
		kind, _ := entry["type"].(string)
		value, _ := entry["value"].(string)
		if kind == "" || value == "" {
			continue
		}
		key := "name"
		if kind == "user" {
			key = "username"
		}
		out = append(out, map[string]any{"type": kind, key: value})
	}
	return out
}

// smsLimit is the spec's stated cut. SMS is billed per 160-character segment,
// so an unbounded template is a bill as well as a truncated message.
const smsLimit = 1600

func sendTwilio(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	text, err := message(c, "message_template", ev)
	if err != nil {
		return Receipt{}, err
	}
	text = truncate(Title(ev)+" "+collapse(text), smsLimit)

	sid := c.str("account_sid", "")
	endpoint := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", url.PathEscape(sid))
	headers := map[string]string{
		"Authorization": "Basic " + base64.StdEncoding.EncodeToString(
			[]byte(sid+":"+c.str("auth_token", ""))),
	}

	numbers := c.list("to_numbers")
	if len(numbers) == 0 {
		return Receipt{}, errors.New("no destination numbers configured")
	}

	// One request per recipient, because Twilio's API is one message per call.
	// The first failure stops the loop and is reported with the number it
	// happened on: partial delivery reported as success is how an operator
	// learns three months later that one phone never rang.
	var last Receipt
	for i, number := range numbers {
		form := url.Values{}
		form.Set("From", c.str("from_number", ""))
		form.Set("To", number)
		form.Set("Body", text)

		receipt, err := s.do(ctx, request{
			url:         endpoint,
			contentType: "application/x-www-form-urlencoded",
			body:        []byte(form.Encode()),
			headers:     headers,
			verifyTLS:   true,
			// The form carries the account SID; the recorded payload is the
			// message, which is the part worth keeping.
			record: text,
		})
		if err != nil {
			if i > 0 {
				return receipt, fmt.Errorf("sent to %d of %d recipients, then %s: %w", i, len(numbers), number, err)
			}
			return receipt, err
		}
		last = receipt
	}
	return last, nil
}

// sourceOf is what an on-call tool shows as the origin of the alert. The target
// where there is one, because "which host" is the first question.
func sourceOf(ev Event) string {
	if ev.Monitor.Target != "" {
		return ev.Monitor.Target
	}
	if ev.Instance.Name != "" {
		return ev.Instance.Name
	}
	return "uptime-cairn"
}

func monitorURL(ev Event) string {
	if ev.Instance.BaseURL == "" {
		return ""
	}
	return strings.TrimRight(ev.Instance.BaseURL, "/") + "/monitors/" + ev.Monitor.ID.String()
}

func heartbeatMessage(ev Event) string {
	if ev.Heartbeat == nil {
		return ""
	}
	return ev.Heartbeat.Message
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
