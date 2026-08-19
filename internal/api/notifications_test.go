package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The notification-channel surface, tested from the outside.
//
// The assertions that matter most are the ones about what does not come back:
// this is the endpoint where a bot token would leak, and it would leak silently.

func authedClient(t *testing.T) (*client, *httptest.Server) {
	t.Helper()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()
	return c, server
}

func createChannel(t *testing.T, c *client, body map[string]any) map[string]any {
	t.Helper()

	resp, created := c.do(http.MethodPost, "/api/v1/notification-channels", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%v)", resp.StatusCode, created)
	}
	return created
}

func TestChannelCreateAndRead(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	created := createChannel(t, c, map[string]any{
		"name": "Ops Slack",
		"type": "slack",
		"config": map[string]any{
			"webhook_url": "https://hooks.slack.com/services/T/B/X",
			"channel":     "#ops",
		},
	})

	if created["name"] != "Ops Slack" || created["type"] != "slack" {
		t.Fatalf("created = %v", created)
	}
	if created["enabled"] != true {
		t.Error("enabled did not default to true")
	}
	if count, _ := created["monitor_count"].(float64); count != 0 {
		t.Errorf("monitor_count = %v", count)
	}

	resp, fetched := c.do(http.MethodGet, "/api/v1/notification-channels/"+created["id"].(string), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d", resp.StatusCode)
	}
	if fetched["id"] != created["id"] {
		t.Errorf("get returned a different channel")
	}
}

// The whole point of splitting config from secrets: the token cannot appear in a
// read response, because it is not in the column the read response serialises.
func TestSecretsNeverComeBack(t *testing.T) {
	t.Parallel()

	c, server := authedClient(t)
	const token = "xoxb-super-secret-value"
	created := createChannel(t, c, map[string]any{
		"name":   "Ops Slack",
		"type":   "slack",
		"config": map[string]any{"webhook_url": "https://hooks.slack.com/" + token},
	})

	config := created["config"].(map[string]any)
	if config["webhook_url"] != "__redacted__" {
		t.Errorf("webhook_url = %v, want the redaction marker", config["webhook_url"])
	}

	// And nowhere else in any representation of it.
	for _, path := range []string{
		"/api/v1/notification-channels",
		"/api/v1/notification-channels/" + created["id"].(string),
	} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
		for _, cookie := range c.http.Jar.Cookies(nil) {
			req.AddCookie(cookie)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(raw), token) {
			t.Errorf("%s leaked the secret:\n%s", path, raw)
		}
	}
}

// The round trip a form performs: GET, change one field, PATCH the whole object
// back. Without the marker being stripped this overwrites the webhook URL with
// asterisks and the channel silently stops working.
func TestPatchingBackARedactedReadKeepsTheSecret(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	created := createChannel(t, c, map[string]any{
		"name":   "Ops Slack",
		"type":   "slack",
		"config": map[string]any{"webhook_url": "https://hooks.slack.com/original"},
	})
	id := created["id"].(string)

	config := created["config"].(map[string]any)
	config["channel"] = "#incidents"

	resp, updated := c.do(http.MethodPatch, "/api/v1/notification-channels/"+id,
		map[string]any{"config": config})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d (%v)", resp.StatusCode, updated)
	}

	updatedConfig := updated["config"].(map[string]any)
	if updatedConfig["channel"] != "#incidents" {
		t.Errorf("the edit was lost: %v", updatedConfig)
	}
	if updatedConfig["webhook_url"] != "__redacted__" {
		t.Errorf("webhook_url = %v — the stored secret should still be set", updatedConfig["webhook_url"])
	}
}

func TestPatchCannotChangeType(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	created := createChannel(t, c, map[string]any{
		"name":   "Ops Slack",
		"type":   "slack",
		"config": map[string]any{"webhook_url": "https://hooks.slack.com/x"},
	})

	resp, body := c.do(http.MethodPatch, "/api/v1/notification-channels/"+created["id"].(string),
		map[string]any{"type": "discord"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("patch = %d, want 422 (%v)", resp.StatusCode, body)
	}
}

func TestChannelValidationReportsEveryProblem(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/notification-channels", map[string]any{
		"type":   "ntfy",
		"config": map[string]any{"priority": 99, "nonsense": true},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422", resp.StatusCode)
	}

	errors, _ := body["errors"].([]any)
	pointers := map[string]bool{}
	for _, item := range errors {
		entry := item.(map[string]any)
		pointers[entry["pointer"].(string)] = true
	}
	for _, want := range []string{"/name", "/config/topic", "/config/priority", "/config/nonsense"} {
		if !pointers[want] {
			t.Errorf("no problem reported at %s (got %v)", want, pointers)
		}
	}
}

// Subscribing to an event nothing raises produces a channel that looks
// configured and never fires. Saying so at save time is the whole difference.
func TestSubscribingToAnUnraisedEventIsRefused(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/notification-channels", map[string]any{
		"name":   "Incidents",
		"type":   "slack",
		"config": map[string]any{"webhook_url": "https://hooks.slack.com/x"},
		"events": []string{"incident.opened"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if !strings.Contains(problemDetail(body), "nothing in this build raises it") {
		t.Errorf("detail = %v", body)
	}
}

func TestChannelDelete(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	created := createChannel(t, c, map[string]any{
		"name":   "Ops Slack",
		"type":   "slack",
		"config": map[string]any{"webhook_url": "https://hooks.slack.com/x"},
	})
	id := created["id"].(string)

	resp, _ := c.do(http.MethodDelete, "/api/v1/notification-channels/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp, _ = c.do(http.MethodGet, "/api/v1/notification-channels/"+id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d", resp.StatusCode)
	}
	resp, _ = c.do(http.MethodDelete, "/api/v1/notification-channels/"+id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", resp.StatusCode)
	}
}

// Default channels attach to monitors created afterwards. That is what makes
// "set alerting up once" work, and it is the only assignment path a user who
// never touches the field ever exercises.
func TestDefaultChannelsAttachToNewMonitors(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	fallback := createChannel(t, c, map[string]any{
		"name":       "Everything",
		"type":       "slack",
		"config":     map[string]any{"webhook_url": "https://hooks.slack.com/x"},
		"is_default": true,
	})

	resp, monitor := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "http", "config": map[string]any{"url": "https://example.com"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor = %d (%v)", resp.StatusCode, monitor)
	}

	assigned, _ := monitor["notification_channel_ids"].([]any)
	if len(assigned) != 1 || assigned[0] != fallback["id"] {
		t.Errorf("assignments = %v, want the default channel", assigned)
	}
}

// An empty array means "no alerts for this monitor", which a deliberately quiet
// monitor needs and which must not be confused with "unset".
func TestExplicitEmptyChannelListMeansSilence(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	createChannel(t, c, map[string]any{
		"name":       "Everything",
		"type":       "slack",
		"config":     map[string]any{"webhook_url": "https://hooks.slack.com/x"},
		"is_default": true,
	})

	resp, monitor := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "noisy-dev-box", "type": "http",
		"config":                   map[string]any{"url": "https://example.com"},
		"notification_channel_ids": []string{},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor = %d (%v)", resp.StatusCode, monitor)
	}
	if assigned, _ := monitor["notification_channel_ids"].([]any); len(assigned) != 0 {
		t.Errorf("assignments = %v, want none", assigned)
	}
}

func TestUnknownChannelIDIsAValidationError(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "http",
		"config":                   map[string]any{"url": "https://example.com"},
		"notification_channel_ids": []string{"018f3a1c-4e5b-7c2d-9f10-2b3c4d5e6f70"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}
}

// The target promoted out of config is the line an alert leads with, so it has
// to survive the write. It is not on the wire — Monitor has no target field —
// so it is read back through the store.
func TestMonitorTargetIsPromoted(t *testing.T) {
	t.Parallel()

	server, store := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()

	resp, monitor := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "http",
		"config": map[string]any{"url": "https://api.example.com/health"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v)", resp.StatusCode, monitor)
	}

	id, ok := model.ParseID(monitor["id"].(string))
	if !ok {
		t.Fatal("unparseable id")
	}
	stored, err := store.GetMonitor(t.Context(), id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Monitor.Target != "https://api.example.com/health" {
		t.Errorf("target = %q, so every alert about this monitor would omit what it points at", stored.Monitor.Target)
	}
}

func TestTemplateVariablesArePublished(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	resp, body := c.do(http.MethodGet, "/api/v1/notification-channels/template-variables", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d", resp.StatusCode)
	}

	data, _ := body["data"].([]any)
	if len(data) < 10 {
		t.Fatalf("%d variables published", len(data))
	}
	for _, item := range data {
		entry := item.(map[string]any)
		for _, field := range []string{"name", "type", "description", "example"} {
			if _, ok := entry[field]; !ok {
				t.Errorf("%v has no %s", entry["name"], field)
			}
		}
	}
}

// A preview that renders through a different path than delivery is a preview
// that lies, so this is the same renderer — including the escaping.
func TestTemplatePreview(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/notification-channels/preview", map[string]any{
		"template": "{{monitor.name}} is {{status}}",
		"headers":  map[string]string{"X-Monitor": "{{monitor.id}}"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d (%v)", resp.StatusCode, body)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v (%v)", body["ok"], body["error"])
	}
	if rendered, _ := body["rendered_body"].(string); !strings.Contains(rendered, "Sample monitor") {
		t.Errorf("rendered_body = %q", rendered)
	}
	if headers, _ := body["rendered_headers"].(map[string]any); len(headers) != 1 {
		t.Errorf("rendered_headers = %v", body["rendered_headers"])
	}
	if _, ok := body["context_used"].(map[string]any); !ok {
		t.Error("the preview does not show what was available to the template")
	}
}

// A broken template is the user's typo, shown inline with its position — not a
// 5xx, which would say the server was at fault.
func TestBrokenTemplatePreviewIsA200WithTheMistakeLocated(t *testing.T) {
	t.Parallel()

	c, _ := authedClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/notification-channels/preview", map[string]any{
		"template": "ok\n{{monitor.nmae}}",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d, want 200", resp.StatusCode)
	}
	if body["ok"] != false {
		t.Fatalf("ok = %v", body["ok"])
	}
	failure, _ := body["error"].(map[string]any)
	if line, _ := failure["line"].(float64); line != 2 {
		t.Errorf("line = %v, want 2", failure["line"])
	}
	if !strings.Contains(failure["message"].(string), "monitor.nmae") {
		t.Errorf("message = %v", failure["message"])
	}
}

// Test-fire reports the provider's answer rather than asserting success, which
// is the entire reason the button exists.
func TestTestFireReportsTheProvidersAnswer(t *testing.T) {
	t.Parallel()

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"invalid_token"}`)
	}))
	t.Cleanup(provider.Close)

	c, _ := authedClient(t)
	created := createChannel(t, c, map[string]any{
		"name":   "Broken webhook",
		"type":   "webhook",
		"config": map[string]any{"url": provider.URL},
	})

	resp, body := c.do(http.MethodPost,
		"/api/v1/notification-channels/"+created["id"].(string)+"/test", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test = %d, want 200 — the request succeeded, the delivery did not", resp.StatusCode)
	}
	if body["delivered"] != false {
		t.Errorf("delivered = %v", body["delivered"])
	}
	if code, _ := body["status_code"].(float64); code != http.StatusForbidden {
		t.Errorf("status_code = %v", body["status_code"])
	}
	if failure, _ := body["error"].(string); !strings.Contains(failure, "invalid_token") {
		t.Errorf("error = %q — the provider's own words are what the operator needs", failure)
	}

	// And the channel now says it is broken, without anybody reading a log.
	_, fetched := c.do(http.MethodGet, "/api/v1/notification-channels/"+created["id"].(string), nil)
	if fetched["last_error"] == nil {
		t.Error("last_error is unset after a failed test")
	}
}

func TestTestFireSucceeds(t *testing.T) {
	t.Parallel()

	var received string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		received = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(provider.Close)

	c, _ := authedClient(t)
	created := createChannel(t, c, map[string]any{
		"name": "Working webhook",
		"type": "webhook",
		"config": map[string]any{
			"url":           provider.URL,
			"body_template": `{"text":"{{monitor.name}} is {{status}}"}`,
		},
	})

	resp, body := c.do(http.MethodPost,
		"/api/v1/notification-channels/"+created["id"].(string)+"/test", map[string]any{
			"sample_event": "monitor.up",
		})
	if resp.StatusCode != http.StatusOK || body["delivered"] != true {
		t.Fatalf("test = %d (%v)", resp.StatusCode, body)
	}

	var decoded map[string]string
	if err := json.Unmarshal([]byte(received), &decoded); err != nil {
		t.Fatalf("the rendered payload is not valid JSON: %v (%s)", err, received)
	}
	if decoded["text"] != "Sample monitor is up" {
		t.Errorf("text = %q", decoded["text"])
	}
	if payload, _ := body["rendered_payload"].(string); payload != received {
		t.Errorf("rendered_payload does not match what was sent:\n%q\n%q", payload, received)
	}
}

func TestChannelEndpointsRequireAuthentication(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)

	for _, path := range []string{
		"/api/v1/notification-channels",
		"/api/v1/notification-channels/template-variables",
	} {
		resp, _ := c.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, resp.StatusCode)
		}
	}
}

func problemDetail(body map[string]any) string {
	detail, _ := body["detail"].(string)
	errors, _ := body["errors"].([]any)
	for _, item := range errors {
		entry, _ := item.(map[string]any)
		message, _ := entry["message"].(string)
		detail += " " + message
	}
	return detail
}
