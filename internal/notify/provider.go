package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The delivery half: thirteen providers behind one function.
//
// Every one of them either speaks HTTP or shells out, so the shared machinery
// here is small and the per-provider files are almost entirely the shape of
// somebody else's JSON. That is the honest description of this feature — the
// difficulty is not the transport, it is that a failure has to be visible.

// Receipt is what one send actually did, kept so the test endpoint and the
// delivery log can show it rather than assert it.
type Receipt struct {
	StatusCode *int

	// Payload is what went on the wire, truncated. Secrets are never in it: a
	// bot token lives in the URL or a header, and neither is recorded.
	Payload string
}

// maxRecordedPayload bounds what a delivery row stores. A user template can be
// any size, and notification_deliveries is subject to retention precisely
// because of what a rendered payload can carry.
const maxRecordedPayload = 4096

// maxProviderError bounds how much of a provider's response body is quoted back.
// Enough for a real error message, not enough for an HTML error page.
const maxProviderError = 1024

// ProviderError is a non-2xx answer, carrying the status so the retry policy can
// tell "the receiver was restarting" from "this token is wrong". Retrying the
// second three times produces three identical failures and delays the moment the
// operator is told which it was.
type ProviderError struct {
	StatusCode int
	Status     string
	Detail     string
}

func (e *ProviderError) Error() string {
	if e.Detail == "" {
		return e.Status
	}
	return e.Status + ": " + e.Detail
}

// Permanent reports whether another attempt is pointless. 408, 425, and 429 are
// the three 4xx codes that mean "not now" rather than "not ever".
func (e *ProviderError) Permanent() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return e.StatusCode >= 400 && e.StatusCode < 500
}

// Sender delivers one event to one channel.
type Sender struct {
	client   *http.Client
	insecure *http.Client

	// apprisePath is the resolved apprise binary, empty when the instance does
	// not have one. Apprise is the meta-provider and is the only channel whose
	// availability depends on the host rather than on the build.
	apprisePath string

	// now is injectable so tests are not timing-dependent.
	now func() time.Time
}

// NewSender builds the delivery client.
func NewSender() *Sender {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	insecureTransport := transport.Clone()
	insecureTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per channel, see verify_tls

	return &Sender{
		client:      &http.Client{Transport: transport},
		insecure:    &http.Client{Transport: insecureTransport},
		apprisePath: lookupApprise(),
		now:         func() time.Time { return time.Now().UTC() },
	}
}

// AppriseAvailable reports whether the meta-provider can run here. Surfaced as a
// capability so the UI hides a channel type the instance cannot deliver, rather
// than offering a control that fails on first use.
func (s *Sender) AppriseAvailable() bool { return s.apprisePath != "" }

type sendFunc func(context.Context, *Sender, conf, Event) (Receipt, error)

var senders = map[string]sendFunc{
	"email":     sendEmail,
	"webhook":   sendWebhook,
	"slack":     sendSlack,
	"discord":   sendDiscord,
	"telegram":  sendTelegram,
	"matrix":    sendMatrix,
	"gotify":    sendGotify,
	"ntfy":      sendNtfy,
	"msteams":   sendMSTeams,
	"pagerduty": sendPagerDuty,
	"opsgenie":  sendOpsgenie,
	"twilio":    sendTwilio,
	"apprise":   sendApprise,
}

// Send delivers one event through one channel's merged configuration.
func (s *Sender) Send(ctx context.Context, channelType string, config map[string]any, ev Event) (Receipt, error) {
	send, ok := senders[channelType]
	if !ok {
		return Receipt{}, fmt.Errorf("no provider for channel type %q", channelType)
	}
	if channelType == model.ChannelEmail {
		// The instance relay is overlaid here rather than at save time, so a
		// channel picks up a settings change on its next delivery instead of
		// carrying a stale copy of a host that has since moved.
		config = withInstanceSMTP(config)
	}
	return send(ctx, s, conf(config), ev)
}

// conf is typed access to a channel's merged configuration. Every getter takes
// the default from the spec, so a config that omits a field behaves the way the
// documentation says rather than the way Go's zero value says.
type conf map[string]any

func (c conf) str(name, fallback string) string {
	if v, ok := c[name].(string); ok && v != "" {
		return v
	}
	return fallback
}

func (c conf) num(name string, fallback int) int {
	if v, ok := c[name].(float64); ok {
		return int(v)
	}
	if v, ok := c[name].(int); ok {
		return v
	}
	return fallback
}

func (c conf) flag(name string, fallback bool) bool {
	if v, ok := c[name].(bool); ok {
		return v
	}
	return fallback
}

func (c conf) list(name string) []string {
	raw, ok := c[name].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func (c conf) mapping(name string) map[string]string {
	raw, ok := c[name].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for key, item := range raw {
		if text, ok := item.(string); ok {
			out[key] = text
		}
	}
	return out
}

// objects reads an array of objects, used only by Opsgenie responders.
func (c conf) objects(name string) []map[string]any {
	raw, ok := c[name].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if entry, ok := item.(map[string]any); ok {
			out = append(out, entry)
		}
	}
	return out
}

// request is one outbound call, described rather than performed, so every
// provider builds the same struct and the retry, timeout, and error-quoting
// rules live in one place.
type request struct {
	method      string
	url         string
	contentType string
	body        []byte
	headers     map[string]string
	verifyTLS   bool
	timeout     time.Duration

	// record is what the delivery log stores. Usually the body, but not always:
	// a form-encoded Twilio request carries an SMS body worth recording and an
	// account SID that is not.
	record string
}

// do performs the call and turns a non-2xx into an error carrying the
// provider's own words. Summarising them would be a disservice: "delivery
// failed" is not something an operator can act on, and "invalid_token" is.
func (s *Sender) do(ctx context.Context, r request) (Receipt, error) {
	timeout := r.timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := r.method
	if method == "" {
		method = http.MethodPost
	}

	var body io.Reader
	if len(r.body) > 0 {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.url, body)
	if err != nil {
		return Receipt{}, fmt.Errorf("build request: %w", err)
	}
	if r.contentType != "" && len(r.body) > 0 {
		req.Header.Set("Content-Type", r.contentType)
	}
	req.Header.Set("User-Agent", "uptime-cairn")
	for key, value := range r.headers {
		req.Header.Set(key, value)
	}

	client := s.client
	if !r.verifyTLS {
		client = s.insecure
	}

	recorded := r.record
	if recorded == "" {
		recorded = string(r.body)
	}
	receipt := Receipt{Payload: truncate(recorded, maxRecordedPayload)}

	resp, err := client.Do(req)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = resp.Body.Close() }()

	status := resp.StatusCode
	receipt.StatusCode = &status

	// Read regardless of outcome: the body is the error message on failure and
	// has to be drained on success for the connection to be reused.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxProviderError))
	if status < 200 || status >= 300 {
		return receipt, &ProviderError{
			StatusCode: status,
			Status:     resp.Status,
			Detail:     collapse(strings.TrimSpace(string(raw))),
		}
	}
	return receipt, nil
}

// postJSON is the shape eight of the thirteen providers use.
func (s *Sender) postJSON(ctx context.Context, url string, payload []byte, headers map[string]string) (Receipt, error) {
	return s.do(ctx, request{
		url:         url,
		contentType: "application/json",
		body:        payload,
		headers:     headers,
		verifyTLS:   true,
	})
}

// Title is the one-line summary a provider uses as a subject, a heading, or the
// whole message where the transport has no room for more.
func Title(ev Event) string {
	label := strings.ToUpper(statusWord(ev))
	name := ev.Monitor.Name
	if name == "" {
		name = ev.Monitor.Target
	}
	return fmt.Sprintf("[%s] %s", label, name)
}

// Body is the default rendering, used whenever the channel has no template.
//
// Every line is something the reader has to be told to act: what broke, what it
// was before, where it is, and what the check actually said. The provider's own
// message is quoted rather than paraphrased, because it is the only part of this
// that could not have been written in advance.
func Body(ev Event) string {
	var b strings.Builder

	name := ev.Monitor.Name
	if name == "" {
		name = ev.Monitor.Target
	}
	fmt.Fprintf(&b, "%s is %s", name, strings.ToUpper(statusWord(ev)))
	if ev.PreviousStatus != "" && ev.PreviousStatus != ev.Monitor.Status {
		fmt.Fprintf(&b, " (was %s)", ev.PreviousStatus)
	}
	b.WriteString("\n")

	if ev.Monitor.Target != "" {
		fmt.Fprintf(&b, "\nTarget: %s", ev.Monitor.Target)
	}
	if hb := ev.Heartbeat; hb != nil {
		if hb.Message != "" {
			fmt.Fprintf(&b, "\nMessage: %s", hb.Message)
		}
		if hb.Code != "" {
			fmt.Fprintf(&b, "\nCode: %s", hb.Code)
		}
		if hb.ResponseTimeMs != nil {
			fmt.Fprintf(&b, "\nResponse time: %s ms", strconv.FormatFloat(*hb.ResponseTimeMs, 'f', -1, 64))
		}
		if hb.Attempt > 1 {
			fmt.Fprintf(&b, "\nAttempt: %d", hb.Attempt)
		}
	}
	fmt.Fprintf(&b, "\nTime: %s", ev.OccurredAt.Format(time.RFC3339))
	if ev.Instance.Name != "" {
		fmt.Fprintf(&b, "\nInstance: %s", ev.Instance.Name)
	}
	return b.String()
}

// message renders the channel's template if it has one, and the default
// otherwise. mode decides escaping, which matters only where the rendered text
// lands inside JSON the provider builds rather than JSON the user wrote.
func message(c conf, field string, ev Event) (string, error) {
	tmpl := c.str(field, "")
	if tmpl == "" {
		return Body(ev), nil
	}
	out, err := Render(tmpl, Context(ev), EscapeNone)
	if err != nil {
		return "", err
	}
	return out, nil
}

// statusWord is what the alert calls the state. Taken from the event type rather
// than the status field, so an event whose subject has already moved on still
// describes the transition it was raised for.
func statusWord(ev Event) string {
	switch ev.Type {
	case "monitor.up":
		return "up"
	case "monitor.down":
		return "down"
	case "monitor.pending":
		return "pending"
	}
	if ev.Monitor.Status != "" {
		return ev.Monitor.Status
	}
	return strings.TrimPrefix(ev.Type, "monitor.")
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "… (truncated)"
}

// collapse folds a multi-line provider error onto one line. Delivery errors are
// shown in a table cell and stored in last_error; a stack trace formatted across
// forty lines makes both unreadable.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
