package api

import (
	"net/http"
	"testing"
)

// createKey mints a key through the API and returns its plaintext and id.
func createKey(t *testing.T, c *client, name string, scopes ...string) (string, string) {
	t.Helper()

	resp, body := c.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": name, "scopes": scopes,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create key = %d, want 201 (%v)", resp.StatusCode, body)
	}
	key, _ := body["key"].(string)
	id, _ := body["id"].(string)
	if key == "" {
		t.Fatal("creation response carried no key")
	}
	return key, id
}

// A key authenticates without CSRF — it is not ambient, so there is nothing for
// a hostile page to ride.
func TestAPIKeyAuthenticates(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	owner := newClient(t, server)
	owner.setup()

	key, id := createKey(t, owner, "automation", "monitors:read", "monitors:write")

	bot := newClient(t, server)
	bot.bearer = key

	resp, session := bot.do(http.MethodGet, "/api/v1/auth/session", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session with key = %d, want 200", resp.StatusCode)
	}
	if session["principal_type"] != "api_key" {
		t.Errorf("principal_type = %v, want api_key", session["principal_type"])
	}
	if session["api_key_id"] != id {
		t.Errorf("api_key_id = %v, want %s", session["api_key_id"], id)
	}

	resp, _ = bot.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "From automation", "type": "http", "interval_seconds": 60, "timeout_seconds": 30,
		"config": map[string]string{"url": "https://example.com"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("key write without CSRF = %d, want 201", resp.StatusCode)
	}
}

// The whole point of scoping: a read key is a read key.
func TestAPIKeyScopesAreEnforced(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	owner := newClient(t, server)
	owner.setup()

	key, _ := createKey(t, owner, "read-only", "monitors:read")

	bot := newClient(t, server)
	bot.bearer = key

	if resp, _ := bot.do(http.MethodGet, "/api/v1/monitors", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("read with monitors:read = %d, want 200", resp.StatusCode)
	}

	resp, body := bot.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "Nope", "type": "http", "config": map[string]string{"url": "https://example.com"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write with monitors:read = %d, want 403", resp.StatusCode)
	}
	if body["type"] != errorBase+"insufficient-scope" {
		t.Errorf("type = %v, want the insufficient-scope problem", body["type"])
	}

	// heartbeats:read is its own scope, not something monitors:read implies.
	if resp, _ := bot.do(http.MethodGet, "/api/v1/api-keys", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("listing keys with monitors:read = %d, want 403", resp.StatusCode)
	}
}

// Without this, the weakest key in an install can mint the strongest one and
// every scope boundary below it is decorative.
func TestAPIKeyCannotEscalate(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	owner := newClient(t, server)
	owner.setup()

	key, _ := createKey(t, owner, "key-manager", "api_keys:write", "monitors:read")

	bot := newClient(t, server)
	bot.bearer = key

	resp, body := bot.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "escalated", "scopes": []string{"monitors:write"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("escalating key creation = %d, want 403 (%v)", resp.StatusCode, body)
	}

	// It can still mint what it holds, including read implied by its own write.
	resp, _ = bot.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "sibling", "scopes": []string{"monitors:read", "api_keys:read"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("creating a key within its own scopes = %d, want 201", resp.StatusCode)
	}
}

func TestAPIKeyRevocationIsImmediate(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	owner := newClient(t, server)
	owner.setup()

	key, id := createKey(t, owner, "temporary", "monitors:read")

	bot := newClient(t, server)
	bot.bearer = key
	if resp, _ := bot.do(http.MethodGet, "/api/v1/monitors", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("key does not work before revocation")
	}

	if resp, _ := owner.do(http.MethodDelete, "/api/v1/api-keys/"+id, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", resp.StatusCode)
	}

	if resp, _ := bot.do(http.MethodGet, "/api/v1/monitors", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Error("a revoked key still authenticated")
	}

	// The row survives revocation so audit entries stay resolvable.
	resp, body := owner.do(http.MethodGet, "/api/v1/api-keys/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get revoked key = %d, want 200", resp.StatusCode)
	}
	if body["revoked_at"] == nil {
		t.Error("revoked_at is not set on the revoked key")
	}
}

// The plaintext exists in exactly one response and nowhere else.
func TestAPIKeyMaterialIsNeverReturnedAgain(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	owner := newClient(t, server)
	owner.setup()

	key, id := createKey(t, owner, "once", "monitors:read")

	_, body := owner.do(http.MethodGet, "/api/v1/api-keys/"+id, nil)
	if _, present := body["key"]; present {
		t.Error("retrieving a key returned its plaintext")
	}
	if prefix, _ := body["prefix"].(string); prefix == "" || len(prefix) >= len(key) {
		t.Errorf("prefix = %q, want a short non-secret identifier", prefix)
	}

	_, list := owner.do(http.MethodGet, "/api/v1/api-keys", nil)
	data, _ := list["data"].([]any)
	for _, item := range data {
		if entry, ok := item.(map[string]any); ok {
			if _, present := entry["key"]; present {
				t.Error("listing keys returned plaintext key material")
			}
		}
	}
}

func TestAPIKeyValidation(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	owner := newClient(t, server)
	owner.setup()

	resp, body := owner.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "bad", "scopes": []string{"monitors:destroy"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown scope = %d, want 422 (%v)", resp.StatusCode, body)
	}

	resp, _ = owner.do(http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "no scopes", "scopes": []string{}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("empty scopes = %d, want 422", resp.StatusCode)
	}
}

// Account credentials are the user's own. A key that could enrol or remove a
// second factor would be a key that could take the account.
func TestAPIKeysCannotChangeAccountCredentials(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	owner := newClient(t, server)
	owner.setup()

	key, _ := createKey(t, owner, "full", "api_keys:write", "monitors:write")

	bot := newClient(t, server)
	bot.bearer = key

	resp, _ := bot.do(http.MethodPost, "/api/v1/auth/totp", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("TOTP enrolment with an API key = %d, want 403", resp.StatusCode)
	}
}
