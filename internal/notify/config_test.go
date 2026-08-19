package notify

import (
	"encoding/json"
	"strings"
	"testing"
)

func cfg(raw string) map[string]any {
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		panic(err)
	}
	return out
}

func pointers(problems []Problem) []string {
	out := make([]string, 0, len(problems))
	for _, p := range problems {
		out = append(out, p.Pointer)
	}
	return out
}

// Every type in the spec's enum must have a schema and a provider. A type that
// validates but cannot be delivered is a channel that silently does nothing.
func TestEveryChannelTypeHasSchemaAndProvider(t *testing.T) {
	t.Parallel()

	for _, channelType := range Types() {
		if _, ok := schemas[channelType]; !ok {
			t.Errorf("%s has no config schema", channelType)
		}
		if _, ok := senders[channelType]; !ok {
			t.Errorf("%s has no provider: a channel of this type would validate and deliver nothing", channelType)
		}
	}
	if len(Types()) != 13 {
		t.Errorf("%d channel types, spec defines 13", len(Types()))
	}
}

func TestValidateAcceptsAWellFormedChannel(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"slack":     `{"webhook_url":"https://hooks.slack.com/services/T/B/X"}`,
		"discord":   `{"webhook_url":"https://discord.com/api/webhooks/1/x","username":"cairn"}`,
		"telegram":  `{"bot_token":"123:abc","chat_id":"-100123"}`,
		"matrix":    `{"homeserver_url":"https://matrix.example.com","room_id":"!abc:example.com","access_token":"tok"}`,
		"gotify":    `{"server_url":"https://gotify.example.com","application_token":"tok","priority":8}`,
		"ntfy":      `{"topic":"alerts","priority":4}`,
		"msteams":   `{"webhook_url":"https://outlook.office.com/webhook/x"}`,
		"pagerduty": `{"integration_key":"key","severity":"critical"}`,
		"opsgenie":  `{"api_key":"key","responders":[{"type":"team","value":"platform"}]}`,
		"twilio":    `{"account_sid":"AC1","auth_token":"tok","from_number":"+1","to_numbers":["+2"]}`,
		"apprise":   `{"urls":["mailto://user:pass@example.com"]}`,
		"webhook":   `{"url":"https://example.com/hook","method":"POST","body_template":"{\"m\":\"{{monitor.name}}\"}"}`,
		"email":     `{"to":["ops@example.com"],"use_instance_smtp":false,"smtp_host":"smtp.example.com","from_address":"cairn@example.com"}`,
	}

	for channelType, raw := range cases {
		if problems := Validate(channelType, cfg(raw)); len(problems) > 0 {
			t.Errorf("%s rejected a valid config: %v", channelType, problems)
		}
	}
	if len(cases) != len(Types()) {
		t.Errorf("this test covers %d types, there are %d", len(cases), len(Types()))
	}
}

func TestValidateReportsEveryBadFieldAtOnce(t *testing.T) {
	t.Parallel()

	problems := Validate("ntfy", cfg(`{"topic":"alerts","priority":9,"auth_type":"nonsense","nope":true}`))
	got := strings.Join(pointers(problems), " ")

	for _, want := range []string{"/config/priority", "/config/auth_type", "/config/nope"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %v", want, pointers(problems))
		}
	}
}

func TestMissingRequiredFieldIsReported(t *testing.T) {
	t.Parallel()

	problems := Validate("telegram", cfg(`{"chat_id":"-100"}`))
	if len(problems) != 1 || problems[0].Pointer != "/config/bot_token" {
		t.Fatalf("problems = %v", problems)
	}
	if problems[0].Code != "required" {
		t.Errorf("code = %s", problems[0].Code)
	}
}

func TestUnknownChannelTypeIsRejected(t *testing.T) {
	t.Parallel()

	problems := Validate("carrier_pigeon", cfg(`{}`))
	if len(problems) != 1 || problems[0].Pointer != "/type" {
		t.Fatalf("problems = %v", problems)
	}
}

// The three cross-field rules exist because each one is individually valid and
// jointly useless — the configuration that looks set up and delivers nothing.
func TestCrossFieldRules(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		channelType string
		config      string
		wantPointer string
	}{
		{"instance smtp is not implemented", "email",
			`{"to":["ops@example.com"]}`, "/config/use_instance_smtp"},
		{"basic auth needs a password", "ntfy",
			`{"topic":"a","auth_type":"basic","username":"u"}`, "/config/password"},
		{"a GET webhook sends no body", "webhook",
			`{"url":"https://example.com","method":"GET","body_template":"hi"}`, "/config/body_template"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := Validate(tc.channelType, cfg(tc.config))
			if !strings.Contains(strings.Join(pointers(problems), " "), tc.wantPointer) {
				t.Errorf("want a problem at %s, got %v", tc.wantPointer, pointers(problems))
			}
		})
	}
}

func TestBadTemplateIsRejectedAtSaveTime(t *testing.T) {
	t.Parallel()

	problems := Validate("slack", cfg(`{"webhook_url":"https://hooks.slack.com/x","message_template":"{{monitor.nmae}}"}`))
	if len(problems) != 1 || problems[0].Code != "invalid_template" {
		t.Fatalf("problems = %v", problems)
	}
}

// The rule the whole feature rests on: a secret goes into the envelope, never
// into config, and comes back as a marker or not at all.
func TestSplitKeepsSecretsOutOfConfig(t *testing.T) {
	t.Parallel()

	public, secret := Split("telegram", cfg(`{"bot_token":"123:abc","chat_id":"-100","parse_mode":"HTML"}`))

	if _, leaked := public["bot_token"]; leaked {
		t.Error("bot_token is in the public config")
	}
	if secret["bot_token"] != "123:abc" {
		t.Errorf("bot_token = %v", secret["bot_token"])
	}
	if public["chat_id"] != "-100" || public["parse_mode"] != "HTML" {
		t.Errorf("public config lost a field: %v", public)
	}

	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "123:abc") {
		t.Errorf("the serialised config contains the bot token: %s", encoded)
	}
}

func TestRedactMarksSetSecretsWithoutRevealingThem(t *testing.T) {
	t.Parallel()

	public, secret := Split("ntfy", cfg(`{"topic":"a","auth_type":"token","token":"secret-value"}`))
	out := Redact("ntfy", public, secret)

	if out["token"] != Redacted {
		t.Errorf("token = %v, want the redaction marker", out["token"])
	}
	if _, present := out["password"]; present {
		t.Error("an unset secret was reported as set")
	}
	if out["topic"] != "a" {
		t.Errorf("a public field was lost: %v", out)
	}
}

func TestRedactMasksArraySecretsElementByElement(t *testing.T) {
	t.Parallel()

	public, secret := Split("apprise", cfg(`{"urls":["mailto://u:p@example.com","tgram://tok/chat"]}`))
	out := Redact("apprise", public, secret)

	urls, ok := out["urls"].([]any)
	if !ok || len(urls) != 2 {
		t.Fatalf("urls = %v", out["urls"])
	}
	for _, u := range urls {
		if u != Redacted {
			t.Errorf("an apprise URL came back in full: %v", u)
		}
	}
}

// A generic webhook's headers routinely carry an Authorization value, so they
// are encrypted at rest — but the UI still has to edit them, so they are not
// redacted. That split is deliberate and easy to get wrong in either direction.
func TestWebhookHeadersAreEncryptedButReadable(t *testing.T) {
	t.Parallel()

	public, secret := Split("webhook", cfg(`{"url":"https://example.com","headers":{"Authorization":"Bearer x"}}`))
	if _, leaked := public["headers"]; leaked {
		t.Error("headers are stored in plaintext config")
	}
	out := Redact("webhook", public, secret)
	headers, ok := out["headers"].(map[string]any)
	if !ok || headers["Authorization"] != "Bearer x" {
		t.Errorf("headers did not come back for editing: %v", out["headers"])
	}
}

// The round trip a UI form performs: GET, edit one field, PATCH the whole
// object back. Without StripRedacted that overwrites the bot token with
// asterisks.
func TestRedactedMarkerRoundTripsWithoutOverwriting(t *testing.T) {
	t.Parallel()

	public, secret := Split("telegram", cfg(`{"bot_token":"123:abc","chat_id":"-100"}`))
	read := Redact("telegram", public, secret)

	read["chat_id"] = "-200"
	StripRedacted(read)

	if _, present := read["bot_token"]; present {
		t.Error("the marker survived and would overwrite the stored token")
	}

	merged := Merge(secret, read)
	if problems := Validate("telegram", merged); len(problems) > 0 {
		t.Fatalf("the merged config is invalid: %v", problems)
	}
	if merged["bot_token"] != "123:abc" {
		t.Errorf("bot_token = %v, want the original", merged["bot_token"])
	}
	if merged["chat_id"] != "-200" {
		t.Errorf("chat_id = %v, want the edit", merged["chat_id"])
	}
}

func TestValidateAcceptsTheRedactionMarkerInPlace(t *testing.T) {
	t.Parallel()

	// Belt and braces: a caller that sends the marker through unstripped must
	// not see a confusing "not a URL" error about a value it never chose.
	if problems := Validate("slack", cfg(`{"webhook_url":"`+Redacted+`"}`)); len(problems) > 0 {
		t.Errorf("the marker was rejected as a value: %v", problems)
	}
}
