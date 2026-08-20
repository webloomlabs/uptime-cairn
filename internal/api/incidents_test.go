package api

import (
	"net/http"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

func openIncident(t *testing.T, c *client, title string, extra map[string]any) map[string]any {
	t.Helper()

	body := map[string]any{"title": title, "impact": "major"}
	for key, value := range extra {
		body[key] = value
	}

	resp, created := c.do(http.MethodPost, "/api/v1/incidents", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open incident = %d, want 201 (%v)", resp.StatusCode, created)
	}
	return created
}

func TestOpeningAnIncidentWithANoteStartsTheTimeline(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	monitor := createHTTPMonitor(t, c, "Checkout")

	incident := openIncident(t, c, "Checkout is failing", map[string]any{
		"monitor_ids": []string{monitor},
		"body":        "We are looking into elevated 500s on checkout.",
	})

	if incident["state"] != "investigating" {
		t.Errorf("state = %v, want investigating", incident["state"])
	}
	if incident["resolved_at"] != nil {
		t.Errorf("resolved_at = %v, want null on a fresh incident", incident["resolved_at"])
	}
	if ids := incident["monitor_ids"].([]any); len(ids) != 1 || ids[0] != monitor {
		t.Errorf("monitor_ids = %v, want the one monitor", ids)
	}

	updates := incident["updates"].([]any)
	if len(updates) != 1 {
		t.Fatalf("updates = %v, want the opening note", updates)
	}
	// The opening note goes through the same call every later one takes, so an
	// incident opened with a note and one that gained its first note a minute
	// later are the same shape.
	if first := updates[0].(map[string]any); first["state"] != "investigating" {
		t.Errorf("opening update state = %v, want investigating", first["state"])
	}
}

// State changes travel through the timeline so that every change of state
// carries the sentence explaining it.
func TestAdvancingStateGoesThroughTheTimeline(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	incident := openIncident(t, c, "Checkout is failing", nil)
	id := incident["id"].(string)

	// PATCH has no state field at all, and the decoder rejects unknown fields —
	// so trying is a 400 rather than a silent no-op.
	resp, _ := c.do(http.MethodPatch, "/api/v1/incidents/"+id, map[string]any{"state": "resolved"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch state = %d, want 400", resp.StatusCode)
	}

	resp, entry := c.do(http.MethodPost, "/api/v1/incidents/"+id+"/updates", map[string]any{
		"state": "identified",
		"body":  "A bad deploy. Rolling back.",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post update = %d, want 201 (%v)", resp.StatusCode, entry)
	}

	_, after := c.do(http.MethodGet, "/api/v1/incidents/"+id, nil)
	if after["state"] != "identified" {
		t.Fatalf("state = %v, want identified", after["state"])
	}
	if after["resolved_at"] != nil {
		t.Fatalf("resolved_at = %v, want null — the incident is not resolved", after["resolved_at"])
	}
}

func TestResolvingStampsResolvedAtAndReopeningClearsIt(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	incident := openIncident(t, c, "Checkout is failing", nil)
	id := incident["id"].(string)

	c.do(http.MethodPost, "/api/v1/incidents/"+id+"/updates", map[string]any{
		"state": "resolved", "body": "Rollback complete, error rate normal.",
	})
	_, resolved := c.do(http.MethodGet, "/api/v1/incidents/"+id, nil)
	if resolved["resolved_at"] == nil {
		t.Fatal("resolved_at is null after a resolved update")
	}

	metrics := resolved["metrics"].(map[string]any)
	if metrics["time_to_resolve_seconds"] == nil {
		t.Error("time_to_resolve_seconds is null on a resolved incident")
	}
	// Derived from the timestamps that exist, and nothing more. MTTA needs
	// acknowledgement, which is Phase 3.
	if metrics["time_to_acknowledge_seconds"] != nil {
		t.Errorf("time_to_acknowledge_seconds = %v, want null", metrics["time_to_acknowledge_seconds"])
	}

	// Reopening has to clear the stamp, or the column records the first time
	// somebody thought it was over rather than when it was.
	c.do(http.MethodPost, "/api/v1/incidents/"+id+"/updates", map[string]any{
		"state": "investigating", "body": "It is back.",
	})
	_, reopened := c.do(http.MethodGet, "/api/v1/incidents/"+id, nil)
	if reopened["resolved_at"] != nil {
		t.Fatalf("resolved_at = %v after reopening, want null", reopened["resolved_at"])
	}
}

func TestIncidentTimelineIsOldestFirst(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	incident := openIncident(t, c, "Outage", map[string]any{"body": "first"})
	id := incident["id"].(string)

	for _, note := range []string{"second", "third"} {
		c.do(http.MethodPost, "/api/v1/incidents/"+id+"/updates", map[string]any{"body": note})
	}

	_, body := c.do(http.MethodGet, "/api/v1/incidents/"+id+"/updates", nil)
	updates := body["data"].([]any)
	if len(updates) != 3 {
		t.Fatalf("updates = %d, want 3", len(updates))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got := updates[i].(map[string]any)["body"]; got != want {
			t.Errorf("update %d = %v, want %q", i, got, want)
		}
	}
}

func TestIncidentListOmitsTheTimeline(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	openIncident(t, c, "Outage", map[string]any{"body": "opening note"})

	_, body := c.do(http.MethodGet, "/api/v1/incidents", nil)
	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data = %v, want one incident", data)
	}
	// Fifty incidents each carrying a dozen updates is a response nobody reads.
	if _, present := data[0].(map[string]any)["updates"]; present {
		t.Fatal("the list carried the timeline")
	}
}

func TestIncidentFilters(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	monitor := createHTTPMonitor(t, c, "Checkout")

	openIncident(t, c, "Checkout is failing", map[string]any{"monitor_ids": []string{monitor}})
	openIncident(t, c, "Search is slow", map[string]any{"impact": "minor"})

	cases := []struct {
		query string
		want  int
	}{
		{"?impact=major", 1},
		{"?impact=major&impact=minor", 2},
		{"?monitor_id=" + monitor, 1},
		{"?search=Search", 1},
		{"?state=investigating", 2},
		{"?state=resolved", 0},
	}
	for _, tc := range cases {
		_, body := c.do(http.MethodGet, "/api/v1/incidents"+tc.query, nil)
		if got := len(body["data"].([]any)); got != tc.want {
			t.Errorf("%s returned %d, want %d", tc.query, got, tc.want)
		}
	}

	// An unrecognised value is refused rather than matching nothing.
	if resp, _ := c.do(http.MethodGet, "/api/v1/incidents?state=on_fire", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad state filter = %d, want 400", resp.StatusCode)
	}
}

func TestIncidentRefusesAMonitorThatDoesNotExist(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	resp, body := c.do(http.MethodPost, "/api/v1/incidents", map[string]any{
		"title": "Outage", "impact": "major",
		"monitor_ids": []string{"01930000-0000-7000-8000-00000000dead"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}
	// Named field, not a foreign-key error nobody can map back.
	if pointer := firstErrorPointer(body); pointer != "/monitor_ids" {
		t.Fatalf("pointer = %q, want /monitor_ids", pointer)
	}
}

// Who hears about an incident, and when the operator has said not to tell them.
//
// The default is to notify — somebody posting a public incident update has
// already decided to be public — but notify_subscribers exists precisely for the
// internal note that must not reach a customer, and a bug in that check is only
// visible from outside the company.
func TestIncidentUpdatesReachStatusPageSubscribers(t *testing.T) {
	t.Parallel()

	server, _, api := testAPI(t)
	c := newClient(t, server)
	c.setup()
	c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"general": map[string]any{"base_url": "https://cairn.example.com"},
	})

	monitor := createHTTPMonitor(t, c, "Checkout")
	page := createStatusPage(t, c, map[string]any{
		"slug": "status", "title": "Acme", "published": true, "subscriptions_enabled": true,
		"sections": []map[string]any{{"name": "Core", "monitor_ids": []string{monitor}}},
	})
	pageIdentifier := page["id"].(string)

	incident := openIncident(t, c, "Checkout is failing", map[string]any{
		"monitor_ids": []string{monitor}, "status_page_ids": []string{pageIdentifier},
		"body": "Investigating elevated errors.",
	})
	id := incident["id"].(string)

	_, announcements := relayOf(t, api).sent()
	if len(announcements) != 1 {
		t.Fatalf("opening the incident queued %d announcements, want 1", len(announcements))
	}
	opened := announcements[0]
	if opened.EventType != model.EventIncidentOpened {
		t.Errorf("event = %q", opened.EventType)
	}
	if len(opened.PageIDs) != 1 || opened.PageIDs[0].String() != pageIdentifier {
		t.Errorf("pages = %v, want only the page the incident names", opened.PageIDs)
	}
	if opened.Update != "Investigating elevated errors." {
		t.Errorf("update = %q, want the opening note verbatim", opened.Update)
	}
	if opened.Incident.Title != "Checkout is failing" || opened.BaseURL != "https://cairn.example.com" {
		t.Errorf("announcement = %+v", opened)
	}

	// An internal note. The timeline still records it — the operator's own
	// history is not edited — and nobody outside is told.
	resp, body := c.do(http.MethodPost, "/api/v1/incidents/"+id+"/updates",
		map[string]any{"body": "Suspect the payment provider. Do not publish.", "notify_subscribers": false})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post update = %d (%v)", resp.StatusCode, body)
	}
	if _, announcements := relayOf(t, api).sent(); len(announcements) != 1 {
		t.Fatalf("an update marked notify_subscribers=false was announced anyway: %d", len(announcements))
	}

	// Resolving is the message a subscriber is actually waiting for, and it is
	// announced under its own event type rather than as one more update.
	resp, body = c.do(http.MethodPost, "/api/v1/incidents/"+id+"/updates",
		map[string]any{"body": "The provider recovered.", "state": "resolved"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("resolve = %d (%v)", resp.StatusCode, body)
	}

	_, announcements = relayOf(t, api).sent()
	if len(announcements) != 2 {
		t.Fatalf("queued %d announcements, want 2", len(announcements))
	}
	resolved := announcements[1]
	if resolved.EventType != model.EventIncidentResolved {
		t.Errorf("event = %q, want %q", resolved.EventType, model.EventIncidentResolved)
	}
	if resolved.Incident.ResolvedAt == nil {
		t.Error("the resolution carried no resolved_at, which is the one fact it is for")
	}
}

// An incident attached to no status page has no subscribers to tell. Announcing
// it would mean sending somebody an incident about a monitor they cannot see.
func TestIncidentWithNoStatusPageAnnouncesNothing(t *testing.T) {
	t.Parallel()

	server, _, api := testAPI(t)
	c := newClient(t, server)
	c.setup()
	monitor := createHTTPMonitor(t, c, "Checkout")

	incident := openIncident(t, c, "Internal only", map[string]any{
		"monitor_ids": []string{monitor}, "body": "Nobody outside needs this.",
	})
	c.do(http.MethodPost, "/api/v1/incidents/"+incident["id"].(string)+"/updates",
		map[string]any{"body": "Still looking."})

	if _, announcements := relayOf(t, api).sent(); len(announcements) != 0 {
		t.Errorf("an incident with no status page queued %d announcements", len(announcements))
	}
}
