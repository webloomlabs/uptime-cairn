package api

import (
	"net/http"
	"testing"
)

func TestWebhookSecretIsShownOnceAndNeverAgain(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	resp, created := c.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"name":   "Ops bus",
		"url":    "https://hooks.example.com/cairn",
		"events": []string{"monitor.down", "monitor.up"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%v)", resp.StatusCode, created)
	}

	secret, ok := created["secret"].(string)
	if !ok || secret == "" {
		t.Fatal("the creation response carried no signing secret")
	}
	prefix, _ := created["secret_prefix"].(string)
	if prefix == "" || len(prefix) >= len(secret) {
		t.Fatalf("secret_prefix = %q, want enough to recognise the secret and not enough to sign with", prefix)
	}

	// Every later read carries the prefix and nothing else. The secret is
	// encrypted rather than hashed — a delivery has to recompute an HMAC with it
	// — so "never returned" is a property of the read shape, not of the storage.
	id := created["id"].(string)
	_, read := c.do(http.MethodGet, "/api/v1/webhooks/"+id, nil)
	if _, present := read["secret"]; present {
		t.Fatal("a subsequent read returned the signing secret")
	}
	if read["secret_prefix"] != prefix {
		t.Errorf("secret_prefix = %v, want %q", read["secret_prefix"], prefix)
	}
}

func TestWebhookHeadersAreNeverReturned(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	// Putting an Authorization header here is the expected case rather than a
	// misuse, which is why the values are encrypted and why the read shape has
	// nowhere to put them.
	resp, created := c.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://hooks.example.com/cairn", "events": []string{"monitor.down"},
		"headers": map[string]string{"Authorization": "Bearer receiver-token"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v)", resp.StatusCode, created)
	}
	if _, present := created["headers"]; present {
		t.Fatal("the response carried the header map")
	}
}

func TestWebhookRefusesAnEventTypeTheSpecDoesNotDefine(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	resp, body := c.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://hooks.example.com/cairn", "events": []string{"monitor.exploded"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if pointer := firstErrorPointer(body); pointer != "/events/0" {
		t.Fatalf("pointer = %q, want the index of the bad event", pointer)
	}
}

func TestWebhookRefusesAnEmptyEventList(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	// An empty list would mean "the monitor state changes", which is the default
	// a webhook gets when the field is absent — but the spec requires at least
	// one, so an explicit empty array is a mistake worth naming.
	resp, body := c.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://hooks.example.com/cairn", "events": []string{},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}
}

func TestWebhookDeliveryLogStartsEmptyAndPaginates(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	_, created := c.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://hooks.example.com/cairn", "events": []string{"monitor.down"},
	})
	id := created["id"].(string)

	resp, body := c.do(http.MethodGet, "/api/v1/webhooks/"+id+"/deliveries", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deliveries = %d (%v)", resp.StatusCode, body)
	}
	if data := body["data"].([]any); len(data) != 0 {
		t.Fatalf("data = %v, want an empty page", data)
	}
	if body["pagination"].(map[string]any)["has_more"] != false {
		t.Error("has_more is true on an empty log")
	}

	// A webhook that has never delivered says so rather than reporting a
	// success it never had.
	_, read := c.do(http.MethodGet, "/api/v1/webhooks/"+id, nil)
	if read["last_delivery_at"] != nil {
		t.Errorf("last_delivery_at = %v, want null", read["last_delivery_at"])
	}
	if read["last_delivery_outcome"] != nil {
		t.Errorf("last_delivery_outcome = %v, want null", read["last_delivery_outcome"])
	}
}

func TestRedeliveringAnotherWebhooksDeliveryIsNotFound(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	_, first := c.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://hooks.example.com/one", "events": []string{"monitor.down"},
	})

	// 404 rather than 403: the caller has no business learning that the id
	// exists somewhere else.
	resp, _ := c.do(http.MethodPost,
		"/api/v1/webhooks/"+first["id"].(string)+"/deliveries/01930000-0000-7000-8000-00000000dead/redeliver", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("redeliver = %d, want 404", resp.StatusCode)
	}
}

func TestWebhookScopesAreEnforced(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	// A key that can read webhooks cannot create one — the scope table is the
	// contract, and this is the endpoint where a missing check would let a
	// read-only integration rewrite where events go.
	_, key := c.do(http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "Reader", "scopes": []string{"webhooks:read"},
	})

	reader := newClient(t, testServer(t))
	reader.base = c.base
	reader.bearer = key["key"].(string)

	if resp, _ := reader.do(http.MethodGet, "/api/v1/webhooks", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("read with webhooks:read = %d, want 200", resp.StatusCode)
	}
	resp, _ := reader.do(http.MethodPost, "/api/v1/webhooks", map[string]any{
		"url": "https://hooks.example.com/cairn", "events": []string{"monitor.down"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("write with webhooks:read = %d, want 403", resp.StatusCode)
	}
}
