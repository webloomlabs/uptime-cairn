package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func createStatusPage(t *testing.T, c *client, body map[string]any) map[string]any {
	t.Helper()

	resp, created := c.do(http.MethodPost, "/api/v1/status-pages", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status page = %d, want 201 (%v)", resp.StatusCode, created)
	}
	return created
}

func TestStatusPageSlugMustBeURLSafeAndUnique(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	createStatusPage(t, c, map[string]any{"slug": "status", "title": "Acme Status"})

	// The slug is a public path segment somebody will bookmark, so it is
	// supplied rather than derived — and therefore has to be refused when it
	// would not survive a URL.
	for _, slug := range []string{"Status", "my status", "-leading"} {
		resp, body := c.do(http.MethodPost, "/api/v1/status-pages",
			map[string]any{"slug": slug, "title": "Other"})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("slug %q = %d, want 422 (%v)", slug, resp.StatusCode, body)
		}
	}

	// A 409 rather than a 422: the request is well-formed and the current state
	// is the problem.
	resp, body := c.do(http.MethodPost, "/api/v1/status-pages",
		map[string]any{"slug": "status", "title": "Other"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate slug = %d, want 409 (%v)", resp.StatusCode, body)
	}
}

func TestUnpublishedPageIsInvisibleRatherThanForbidden(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	createStatusPage(t, c, map[string]any{"slug": "status", "title": "Acme Status"})

	// 404, not 403. An operator building a page before launch should not have
	// its existence confirmed by the error code.
	resp, _ := c.do(http.MethodGet, "/api/v1/public/status-pages/status", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unpublished page = %d, want 404", resp.StatusCode)
	}

	c.do(http.MethodPatch, "/api/v1/status-pages/"+pageID(t, c, "status"), map[string]any{"published": true})
	resp, _ = c.do(http.MethodGet, "/api/v1/public/status-pages/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("published page = %d, want 200", resp.StatusCode)
	}
}

// The property the public projection exists for: a visitor learns a monitor's
// name and status and nothing about how it is checked.
func TestPublicPageCarriesNoMonitorConfiguration(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	monitor := createHTTPMonitor(t, c, "Checkout")

	createStatusPage(t, c, map[string]any{
		"slug": "status", "title": "Acme Status", "published": true,
		"sections": []map[string]any{
			{"name": "Core", "monitor_ids": []string{monitor}},
		},
	})

	resp, body := c.do(http.MethodGet, "/api/v1/public/status-pages/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public page = %d (%v)", resp.StatusCode, body)
	}
	if body["overall_status"] != "degraded" {
		// One pending monitor and nothing down.
		t.Errorf("overall_status = %v, want degraded", body["overall_status"])
	}

	sections := body["sections"].([]any)
	monitors := sections[0].(map[string]any)["monitors"].([]any)
	entry := monitors[0].(map[string]any)
	if entry["name"] != "Checkout" {
		t.Errorf("name = %v", entry["name"])
	}
	for _, leaked := range []string{"config", "target", "interval_seconds", "type", "enabled"} {
		if _, present := entry[leaked]; present {
			t.Errorf("public monitor carries %q", leaked)
		}
	}

	// And the whole document, checked as text: the password lives in the
	// monitor's config and must appear nowhere on a page served to strangers.
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "s3cret-live") || strings.Contains(string(encoded), "example.com/health") {
		t.Fatalf("public page leaked monitor configuration: %s", encoded)
	}
}

func TestPasswordProtectedPageGatesTheRead(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	createStatusPage(t, c, map[string]any{
		"slug": "status", "title": "Acme Status", "published": true,
		"visibility": "password", "password": "open-sesame-please",
	})

	resp, _ := c.do(http.MethodGet, "/api/v1/public/status-pages/status", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("locked page = %d, want 401", resp.StatusCode)
	}

	resp, body := c.do(http.MethodPost, "/api/v1/public/status-pages/status/authenticate",
		map[string]any{"password": "wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401 (%v)", resp.StatusCode, body)
	}

	resp, body = c.do(http.MethodPost, "/api/v1/public/status-pages/status/authenticate",
		map[string]any{"password": "open-sesame-please"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct password = %d, want 200 (%v)", resp.StatusCode, body)
	}

	// The cookie the previous call set now unlocks the page.
	resp, _ = c.do(http.MethodGet, "/api/v1/public/status-pages/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlocked page = %d, want 200", resp.StatusCode)
	}
}

func TestPasswordVisibilityWithoutAPasswordIsRefused(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	resp, body := c.do(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "status", "title": "Acme Status", "visibility": "password",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if pointer := firstErrorPointer(body); pointer != "/password" {
		t.Fatalf("pointer = %q, want /password", pointer)
	}
}

func TestCustomCSSIsRefusedRatherThanSanitised(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	// Stripping tags from CSS is whack-a-mole against an attacker who only has
	// to win once, on a page served to the operator's customers.
	resp, body := c.do(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "status", "title": "Acme", "custom_css": "body { background: url(javascript:alert(1)) }",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}
}

func TestAMonitorAppearsInAtMostOneSection(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	monitor := createHTTPMonitor(t, c, "Checkout")

	resp, body := c.do(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "status", "title": "Acme",
		"sections": []map[string]any{
			{"name": "Core", "monitor_ids": []string{monitor}},
			{"name": "Also core", "monitor_ids": []string{monitor}},
		},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if code := firstErrorCode(body); code != "duplicate" {
		t.Fatalf("code = %q, want duplicate", code)
	}
}

func TestSubscriptionIsDoubleOptInAndTheAddressIsNeverReturned(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	created := createStatusPage(t, c, map[string]any{
		"slug": "status", "title": "Acme", "published": true, "subscriptions_enabled": true,
	})
	id := created["id"].(string)

	resp, body := c.do(http.MethodPost, "/api/v1/public/status-pages/status/subscribers",
		map[string]any{"channel": "email", "target": "alice@example.com"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("subscribe = %d, want 202 (%v)", resp.StatusCode, body)
	}
	if body["status"] != "pending_confirmation" {
		t.Fatalf("status = %v, want pending_confirmation", body["status"])
	}

	// A repeat request gets the same answer. Telling a stranger that an address
	// is already subscribed turns this endpoint into a membership oracle.
	resp, repeat := c.do(http.MethodPost, "/api/v1/public/status-pages/status/subscribers",
		map[string]any{"channel": "email", "target": "alice@example.com"})
	if resp.StatusCode != http.StatusAccepted || repeat["status"] != "pending_confirmation" {
		t.Fatalf("repeat subscribe = %d %v, want the identical answer", resp.StatusCode, repeat)
	}

	_, listed := c.do(http.MethodGet, "/api/v1/status-pages/"+id+"/subscribers", nil)
	subscribers := listed["data"].([]any)
	if len(subscribers) != 1 {
		t.Fatalf("subscribers = %v, want one", subscribers)
	}

	entry := subscribers[0].(map[string]any)
	if entry["confirmed"] != false {
		t.Errorf("confirmed = %v, want false before opt-in", entry["confirmed"])
	}
	// Masked even for an authenticated operator: this list is an export of
	// somebody else's customers.
	if entry["target"] == "alice@example.com" {
		t.Fatalf("target = %v, want it masked", entry["target"])
	}
	if entry["target"] != "al…@example.com" {
		t.Errorf("target = %v, want the masked form", entry["target"])
	}
}

func TestSubscribingToAPageThatDoesNotAcceptItIsRefused(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	createStatusPage(t, c, map[string]any{"slug": "status", "title": "Acme", "published": true})

	resp, body := c.do(http.MethodPost, "/api/v1/public/status-pages/status/subscribers",
		map[string]any{"channel": "email", "target": "alice@example.com"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("subscribe = %d, want 422 (%v)", resp.StatusCode, body)
	}
}

func TestPublicPageShowsPublishedIncidents(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	monitor := createHTTPMonitor(t, c, "Checkout")
	created := createStatusPage(t, c, map[string]any{
		"slug": "status", "title": "Acme", "published": true,
		"sections": []map[string]any{{"name": "Core", "monitor_ids": []string{monitor}}},
	})
	pageIdentifier := created["id"].(string)

	openIncident(t, c, "Checkout is failing", map[string]any{
		"monitor_ids": []string{monitor}, "status_page_ids": []string{pageIdentifier},
		"body": "Investigating elevated errors.",
	})
	openIncident(t, c, "Internal only", nil)

	_, body := c.do(http.MethodGet, "/api/v1/public/status-pages/status", nil)
	active := body["active_incidents"].([]any)
	if len(active) != 1 {
		t.Fatalf("active_incidents = %v, want only the published one", active)
	}
	incident := active[0].(map[string]any)
	if incident["title"] != "Checkout is failing" {
		t.Errorf("title = %v", incident["title"])
	}
	if updates := incident["updates"].([]any); len(updates) != 1 {
		t.Errorf("updates = %v, want the opening note", updates)
	}
}

func pageID(t *testing.T, c *client, slug string) string {
	t.Helper()

	_, body := c.do(http.MethodGet, "/api/v1/status-pages", nil)
	for _, entry := range body["data"].([]any) {
		page := entry.(map[string]any)
		if page["slug"] == slug {
			return page["id"].(string)
		}
	}
	t.Fatalf("no status page with slug %q", slug)
	return ""
}
