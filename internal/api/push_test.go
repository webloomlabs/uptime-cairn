package api

import (
	"net/http"
	"strings"
	"testing"
)

// createPushMonitor creates a push monitor and returns the one-time token and
// the monitor id.
func createPushMonitor(t *testing.T, c *client, body map[string]any) (token, id string) {
	t.Helper()

	resp, out := c.do(http.MethodPost, "/api/v1/monitors", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create push monitor = %d, want 201 (%v)", resp.StatusCode, out)
	}
	config, _ := out["config"].(map[string]any)
	token, _ = config["push_token"].(string)
	id, _ = out["id"].(string)
	return token, id
}

func TestPushMonitorLifecycle(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	token, id := createPushMonitor(t, c, map[string]any{
		"name": "Nightly backup",
		"type": "push",
		"config": map[string]any{
			"expected_interval_seconds": 3600,
			"grace_period_seconds":      60,
		},
	})
	if token == "" {
		t.Fatal("creation returned no push token; it is shown once and never again")
	}

	// The URL is handed back alongside it, because a token with no URL is a
	// support question.
	resp, out := c.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get monitor = %d", resp.StatusCode)
	}
	config, _ := out["config"].(map[string]any)
	if _, present := config["push_token"]; present {
		t.Error("the push token is readable after creation; it must be stored as a hash only")
	}

	// The ingest itself: unauthenticated, GET, no flags — the shape a cron job
	// can actually call.
	push := newClient(t, server)
	resp, out = push.do(http.MethodGet, "/api/v1/push/"+token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push = %d, want 200 (%v)", resp.StatusCode, out)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Errorf("push response = %v", out)
	}

	resp, out = c.do(http.MethodGet, "/api/v1/monitors/"+id+"/heartbeats", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeats = %d", resp.StatusCode)
	}
	beats, _ := out["data"].([]any)
	if len(beats) != 1 {
		t.Fatalf("recorded %d heartbeats, want 1 (%v)", len(beats), out)
	}
	beat, _ := beats[0].(map[string]any)
	if beat["status"] != "up" {
		t.Errorf("heartbeat status = %v, want up", beat["status"])
	}

	resp, out = c.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get monitor = %d", resp.StatusCode)
	}
	if out["status"] != "up" {
		t.Errorf("monitor status = %v, want up", out["status"])
	}
}

func TestPushParameters(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	token, id := createPushMonitor(t, c, map[string]any{
		"name": "Reporting job", "type": "push", "config": map[string]any{},
	})
	push := newClient(t, server)

	// A pusher reporting its own failure is taken at its word, and the message
	// and client-measured duration are recorded with it.
	resp, out := push.do(http.MethodGet, "/api/v1/push/"+token+"?status=down&msg=disk+full&ping=12.5", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push = %d (%v)", resp.StatusCode, out)
	}

	_, out = c.do(http.MethodGet, "/api/v1/monitors/"+id+"/heartbeats", nil)
	beats, _ := out["data"].([]any)
	if len(beats) != 1 {
		t.Fatalf("recorded %d heartbeats, want 1", len(beats))
	}
	beat, _ := beats[0].(map[string]any)
	if beat["status"] != "down" {
		t.Errorf("status = %v, want down", beat["status"])
	}
	if msg, _ := beat["message"].(string); !strings.Contains(msg, "disk full") {
		t.Errorf("message = %v, want the pusher's", beat["message"])
	}
	if rt, _ := beat["response_time_ms"].(float64); rt != 12.5 {
		t.Errorf("response_time_ms = %v, want 12.5", beat["response_time_ms"])
	}

	// The POST form with a body, which is what a script with structured output
	// would send.
	resp, out = push.do(http.MethodPost, "/api/v1/push/"+token, map[string]any{
		"status": "up", "message": "recovered", "response_time_ms": 3,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push post = %d (%v)", resp.StatusCode, out)
	}
	_, out = c.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	if out["status"] != "up" {
		t.Errorf("monitor status = %v, want up", out["status"])
	}

	// A bad parameter is a 400 rather than a silently ignored one: a cron job
	// sending status=UP should be told, not quietly recorded as up.
	resp, _ = push.do(http.MethodGet, "/api/v1/push/"+token+"?status=UP", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad status = %d, want 400", resp.StatusCode)
	}
}

// Every unusable token gets the same 404, so the endpoint cannot be used to
// confirm which tokens exist.
func TestPushUnknownToken(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	push := newClient(t, server)

	// A wrong token and a well-formed-but-unissued one answer identically.
	for _, token := range []string{"nope", strings.Repeat("a", 43)} {
		resp, _ := push.do(http.MethodGet, "/api/v1/push/"+token, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("token %q = %d, want 404", token, resp.StatusCode)
		}
	}

	// An empty token is not a short token: /api/v1/push/ does not match the
	// route at all, so it falls through to the authenticated catch-all and
	// answers 401 like every other unknown path under /api/v1.
	resp, _ := push.do(http.MethodGet, "/api/v1/push/", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty token = %d, want 401", resp.StatusCode)
	}
}

// The ingest is unauthenticated on purpose — the token is the credential, and a
// cron job cannot hold a session.
func TestPushNeedsNoCredential(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()
	token, _ := createPushMonitor(t, c, map[string]any{
		"name": "Cron", "type": "push", "config": map[string]any{},
	})

	// A brand-new client: no session cookie, no CSRF token, no bearer.
	anonymous := newClient(t, server)
	resp, out := anonymous.do(http.MethodGet, "/api/v1/push/"+token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous push = %d, want 200 (%v)", resp.StatusCode, out)
	}
}

func TestPushConfigValidation(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	// push has no checker — no probe runs it — so without its own validation it
	// would be the one type whose config reaches storage unchecked.
	rejected := []map[string]any{
		{"expected_interval_seconds": 5},
		{"expected_interval_seconds": 99999999},
		{"grace_period_seconds": -1},
		{"nonsense": true},
	}
	for _, config := range rejected {
		resp, _ := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
			"name": "Bad", "type": "push", "config": config,
		})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("config %v = %d, want 422", config, resp.StatusCode)
		}
	}
}

// Every type this build registers a checker for must be creatable. Before this
// change the API named http directly, so a registered checker was still refused.
func TestAllRegisteredTypesAreCreatable(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	// The test server registers only http; anything else the spec names but this
	// build cannot run must be refused with not_implemented rather than accepted
	// into a monitor that would sit pending forever.
	resp, body := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "Ping", "type": "icmp", "config": map[string]any{"hostname": "example.com"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unregistered type = %d, want 422 (%v)", resp.StatusCode, body)
	}
	errors, _ := body["errors"].([]any)
	if len(errors) == 0 {
		t.Fatal("no validation detail returned")
	}
	first, _ := errors[0].(map[string]any)
	if first["code"] != "not_implemented" {
		t.Errorf("code = %v, want not_implemented", first["code"])
	}

	// A type the spec does not define is a different answer again.
	resp, body = c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "Nope", "type": "carrier_pigeon", "config": map[string]any{},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("undefined type = %d, want 422", resp.StatusCode)
	}
	errors, _ = body["errors"].([]any)
	first, _ = errors[0].(map[string]any)
	if first["code"] != "invalid" {
		t.Errorf("code = %v, want invalid", first["code"])
	}
}
