package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// sent is what a provider actually put on the wire. Separate from the recorder
// so it can be copied out from under the lock.
type sent struct {
	method  string
	path    string
	query   url.Values
	headers http.Header
	body    string
}

type capture struct {
	mu   sync.Mutex
	last sent
}

func (c *capture) get() sent {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := c.last
	out.headers = c.last.headers.Clone()
	return out
}

// testSender routes every provider — including the ones with a hard-coded host
// like Telegram and PagerDuty — at one test server, so all thirteen are exercised
// through the same harness rather than only the configurable ones.
func testSender(t *testing.T, status int, response string) (*Sender, *capture) {
	t.Helper()
	return testSenderFunc(t, func() int { return status }, response)
}

// testSenderFunc is the same harness with a scripted status, so a test can make
// the first attempt fail and the second succeed.
func testSenderFunc(t *testing.T, status func() int, response string) (*Sender, *capture) {
	t.Helper()

	recorded := &capture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		recorded.mu.Lock()
		recorded.last = sent{
			method: r.Method, path: r.URL.Path,
			query: r.URL.Query(), headers: r.Header.Clone(), body: string(raw),
		}
		recorded.mu.Unlock()

		w.WriteHeader(status())
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)

	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	redirect := &redirectTransport{host: base.Host, inner: http.DefaultTransport}

	sender := NewSender()
	sender.client = &http.Client{Transport: redirect}
	sender.insecure = &http.Client{Transport: redirect}
	return sender, recorded
}

type redirectTransport struct {
	host  string
	inner http.RoundTripper
}

func (t *redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = t.host
	clone.Host = ""
	return t.inner.RoundTrip(clone)
}

func decodeBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	return out
}

// Every provider is driven through the same table. The assertions are
// deliberately about the parts a receiver would reject if they were wrong —
// where the credential goes, and whether the payload is well-formed.
func TestProvidersDeliver(t *testing.T) {
	t.Parallel()

	cases := []struct {
		channelType string
		config      map[string]any
		check       func(t *testing.T, got sent)
	}{
		{
			channelType: "slack",
			config:      cfg(`{"webhook_url":"https://hooks.slack.com/services/T/B/X","channel":"#ops"}`),
			check: func(t *testing.T, got sent) {
				body := decodeBody(t, got.body)
				if body["channel"] != "#ops" {
					t.Errorf("channel = %v", body["channel"])
				}
				attachments, ok := body["attachments"].([]any)
				if !ok || len(attachments) != 1 {
					t.Fatalf("attachments = %v", body["attachments"])
				}
			},
		},
		{
			channelType: "discord",
			config:      cfg(`{"webhook_url":"https://discord.com/api/webhooks/1/x"}`),
			check: func(t *testing.T, got sent) {
				body := decodeBody(t, got.body)
				if _, ok := body["embeds"].([]any); !ok {
					t.Errorf("no embeds: %v", body)
				}
			},
		},
		{
			channelType: "msteams",
			config:      cfg(`{"webhook_url":"https://outlook.office.com/webhook/x"}`),
			check: func(t *testing.T, got sent) {
				body := decodeBody(t, got.body)
				if body["@type"] != "MessageCard" {
					t.Errorf("@type = %v", body["@type"])
				}
			},
		},
		{
			channelType: "telegram",
			config:      cfg(`{"bot_token":"123:abc","chat_id":"-100","parse_mode":"HTML"}`),
			check: func(t *testing.T, got sent) {
				if !strings.HasPrefix(got.path, "/bot123:abc/sendMessage") {
					t.Errorf("path = %s", got.path)
				}
				body := decodeBody(t, got.body)
				if body["chat_id"] != "-100" || body["parse_mode"] != "HTML" {
					t.Errorf("body = %v", body)
				}
			},
		},
		{
			channelType: "matrix",
			config:      cfg(`{"homeserver_url":"https://matrix.example.com","room_id":"!r:example.com","access_token":"tok"}`),
			check: func(t *testing.T, got sent) {
				if got.method != http.MethodPut {
					t.Errorf("method = %s, want PUT so the send is idempotent", got.method)
				}
				if got.headers.Get("Authorization") != "Bearer tok" {
					t.Errorf("authorization = %q", got.headers.Get("Authorization"))
				}
			},
		},
		{
			channelType: "gotify",
			config:      cfg(`{"server_url":"https://gotify.example.com","application_token":"tok"}`),
			check: func(t *testing.T, got sent) {
				if got.headers.Get("X-Gotify-Key") != "tok" {
					t.Errorf("token not in header: %v", got.headers)
				}
				// The credential must not reach the access log.
				if got.query.Get("token") != "" {
					t.Error("the application token is in the query string")
				}
			},
		},
		{
			channelType: "ntfy",
			config:      cfg(`{"topic":"alerts","auth_type":"basic","username":"u","password":"p","tags":["rotating_light"]}`),
			check: func(t *testing.T, got sent) {
				user, pass, ok := parseBasic(got.headers.Get("Authorization"))
				if !ok || user != "u" || pass != "p" {
					t.Errorf("authorization = %q", got.headers.Get("Authorization"))
				}
				body := decodeBody(t, got.body)
				if body["topic"] != "alerts" {
					t.Errorf("topic = %v", body["topic"])
				}
			},
		},
		{
			channelType: "pagerduty",
			config:      cfg(`{"integration_key":"key","severity":"critical"}`),
			check: func(t *testing.T, got sent) {
				body := decodeBody(t, got.body)
				if body["event_action"] != "trigger" {
					t.Errorf("event_action = %v", body["event_action"])
				}
				if body["routing_key"] != "key" {
					t.Errorf("routing_key = %v", body["routing_key"])
				}
				payload, _ := body["payload"].(map[string]any)
				if payload["severity"] != "critical" {
					t.Errorf("severity = %v", payload["severity"])
				}
			},
		},
		{
			channelType: "opsgenie",
			config:      cfg(`{"api_key":"key","priority":"P1","responders":[{"type":"user","value":"ada"}]}`),
			check: func(t *testing.T, got sent) {
				if got.headers.Get("Authorization") != "GenieKey key" {
					t.Errorf("authorization = %q", got.headers.Get("Authorization"))
				}
				body := decodeBody(t, got.body)
				responders, _ := body["responders"].([]any)
				if len(responders) != 1 {
					t.Fatalf("responders = %v", body["responders"])
				}
				// A user is named by username, a team by name. Getting this
				// wrong means Opsgenie silently pages nobody.
				entry := responders[0].(map[string]any)
				if entry["username"] != "ada" {
					t.Errorf("responder = %v", entry)
				}
			},
		},
		{
			channelType: "twilio",
			config:      cfg(`{"account_sid":"AC1","auth_token":"tok","from_number":"+1","to_numbers":["+2"]}`),
			check: func(t *testing.T, got sent) {
				user, pass, ok := parseBasic(got.headers.Get("Authorization"))
				if !ok || user != "AC1" || pass != "tok" {
					t.Errorf("authorization = %q", got.headers.Get("Authorization"))
				}
				form, err := url.ParseQuery(got.body)
				if err != nil {
					t.Fatal(err)
				}
				if form.Get("To") != "+2" || form.Get("From") != "+1" {
					t.Errorf("form = %v", form)
				}
			},
		},
		{
			channelType: "webhook",
			config:      cfg(`{"url":"https://example.com/hook","headers":{"X-Monitor":"{{monitor.name}}"}}`),
			check: func(t *testing.T, got sent) {
				body := decodeBody(t, got.body)
				if body["type"] != "monitor.down" {
					t.Errorf("envelope type = %v", body["type"])
				}
				// Header interpolation is the difference between reaching an
				// endpoint that wants a per-alert value and not.
				if got.headers.Get("X-Monitor") != `API "edge"` {
					t.Errorf("X-Monitor = %q", got.headers.Get("X-Monitor"))
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.channelType, func(t *testing.T) {
			t.Parallel()

			sender, recorded := testSender(t, http.StatusOK, `{"ok":true}`)
			if _, err := sender.Send(context.Background(), tc.channelType, tc.config, sampleEvent()); err != nil {
				t.Fatalf("send: %v", err)
			}
			tc.check(t, recorded.get())
		})
	}
}

func parseBasic(header string) (user, pass string, ok bool) {
	r := &http.Request{Header: http.Header{"Authorization": []string{header}}}
	return r.BasicAuth()
}

// A monitor named with a quote in it is the payload that breaks a receiver, and
// the default envelope has to survive it.
func TestDefaultEnvelopeIsValidJSONForAwkwardNames(t *testing.T) {
	t.Parallel()

	ev := sampleEvent()
	ev.Monitor.Name = "line\nbreak \"quoted\" \\ backslash"

	var decoded map[string]any
	if err := json.Unmarshal(Envelope(ev), &decoded); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	data := decoded["data"].(map[string]any)
	monitor := data["monitor"].(map[string]any)
	if monitor["name"] != ev.Monitor.Name {
		t.Errorf("name round-tripped as %q", monitor["name"])
	}
}

// The recovery half of an on-call integration: a resolve must close the incident
// the trigger opened, keyed by monitor, or the operator is left with a healthy
// service and an open page.
func TestPagerDutyResolvesTheIncidentItOpened(t *testing.T) {
	t.Parallel()

	config := cfg(`{"integration_key":"key"}`)

	sender, recorded := testSender(t, http.StatusAccepted, `{}`)
	down := sampleEvent()
	if _, err := sender.Send(context.Background(), "pagerduty", config, down); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	triggerKey := decodeBody(t, recorded.get().body)["dedup_key"]

	up := sampleEvent()
	up.Type = "monitor.up"
	up.Monitor.Status = "up"
	if _, err := sender.Send(context.Background(), "pagerduty", config, up); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	resolved := decodeBody(t, recorded.get().body)

	if resolved["event_action"] != "resolve" {
		t.Errorf("event_action = %v", resolved["event_action"])
	}
	if resolved["dedup_key"] != triggerKey {
		t.Errorf("dedup_key = %v, want the trigger's %v", resolved["dedup_key"], triggerKey)
	}
}

func TestAutoResolveOffSuppressesRatherThanSends(t *testing.T) {
	t.Parallel()

	sender, _ := testSender(t, http.StatusAccepted, `{}`)
	up := sampleEvent()
	up.Type = "monitor.up"

	_, err := sender.Send(context.Background(), "pagerduty", cfg(`{"integration_key":"k","auto_resolve":false}`), up)
	if err == nil || !strings.Contains(err.Error(), "nothing to deliver") {
		t.Fatalf("err = %v, want ErrNotApplicable", err)
	}
}

// The operator needs the provider's real words. "Delivery failed" is not
// something anybody can act on.
func TestProviderErrorCarriesTheResponse(t *testing.T) {
	t.Parallel()

	sender, _ := testSender(t, http.StatusForbidden, `{"error":"invalid_token"}`)
	_, err := sender.Send(context.Background(), "slack", cfg(`{"webhook_url":"https://hooks.slack.com/x"}`), sampleEvent())
	if err == nil {
		t.Fatal("a 403 was reported as success")
	}
	if !strings.Contains(err.Error(), "invalid_token") {
		t.Errorf("error does not quote the provider: %v", err)
	}

	var provider *ProviderError
	if !asProviderError(err, &provider) {
		t.Fatalf("error is not a ProviderError: %T", err)
	}
	if !provider.Permanent() {
		t.Error("a 403 should not be retried: three identical failures only delay telling the operator")
	}
}

func TestRateLimitIsRetryable(t *testing.T) {
	t.Parallel()

	sender, _ := testSender(t, http.StatusTooManyRequests, `slow down`)
	_, err := sender.Send(context.Background(), "discord", cfg(`{"webhook_url":"https://discord.com/api/webhooks/1/x"}`), sampleEvent())

	var provider *ProviderError
	if !asProviderError(err, &provider) {
		t.Fatalf("error is not a ProviderError: %v", err)
	}
	if provider.Permanent() {
		t.Error("429 means not now, not never")
	}
	if !retryable(err) {
		t.Error("a rate limit should be retried")
	}
}

func asProviderError(err error, target **ProviderError) bool {
	return errors.As(err, target)
}
