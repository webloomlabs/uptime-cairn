package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/store/sqlite"
)

// bar pulls the uptime stones for the first monitor on a published page.
func bar(t *testing.T, c *client, slug string) (entries []any, monitor map[string]any) {
	t.Helper()

	resp, body := c.do(http.MethodGet, "/api/v1/public/status-pages/"+slug, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public page = %d (%v)", resp.StatusCode, body)
	}
	sections := body["sections"].([]any)
	monitor = sections[0].(map[string]any)["monitors"].([]any)[0].(map[string]any)

	raw, ok := monitor["uptime_bar"]
	if !ok || raw == nil {
		t.Fatal("no uptime_bar on the public page: the bar renders as nothing at all")
	}
	return raw.([]any), monitor
}

func publishPageWith(t *testing.T, c *client, st *sqlite.Store, extra map[string]any) {
	t.Helper()

	m := seedHistory(t, st, 3*time.Hour, time.Minute, alwaysUp)
	body := map[string]any{
		"slug": "status", "title": "Acme Status", "published": true,
		"sections": []map[string]any{
			{"name": "Core", "monitor_ids": []string{m.ID.String()}},
		},
	}
	for k, v := range extra {
		body[k] = v
	}
	createStatusPage(t, c, body)
}

// The bar spans the page's own window, one stone per day, whether or not those
// days have data — which is what the schema promises and what makes the bar
// appear at all on an instance that has not been running for ninety days.
func TestUptimeBarSpansTheConfiguredWindow(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	publishPageWith(t, c, st, map[string]any{"uptime_bar_days": 30})

	entries, _ := bar(t, c, "status")
	if len(entries) != 30 {
		t.Fatalf("bar = %d stones, want 30 (uptime_bar_days)", len(entries))
	}

	// Oldest first, so the last stone is today — the day the pipeline has not
	// closed yet, and the one a visitor checking on an incident is looking at.
	last := entries[len(entries)-1].(map[string]any)
	today := time.Now().UTC().Format(time.DateOnly)
	if last["date"] != today {
		t.Errorf("newest stone = %v, want today (%s)", last["date"], today)
	}
	if last["uptime_ratio"] != float64(1) {
		t.Errorf("today's ratio = %v, want 1", last["uptime_ratio"])
	}

	// And a day before the monitor existed is null, not zero. A page that draws
	// "we were not running yet" as an outage invents one.
	first := entries[0].(map[string]any)
	if first["uptime_ratio"] != nil {
		t.Errorf("oldest stone ratio = %v, want null", first["uptime_ratio"])
	}
}

// show_uptime_percentage governs the figure beside the name. The bar is its own
// setting, and turning the percentage off must not take the bar with it.
func TestUptimeBarIsIndependentOfTheUptimePercentage(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	publishPageWith(t, c, st, map[string]any{"show_uptime_percentage": false})

	entries, monitor := bar(t, c, "status")
	if len(entries) != 90 {
		t.Fatalf("bar = %d stones, want 90 (the default window)", len(entries))
	}
	if monitor["uptime_percentage"] != nil {
		t.Errorf("uptime_percentage = %v, want null with the setting off", monitor["uptime_percentage"])
	}
}
