package api

import (
	"net/http"
	"slices"
	"testing"
	"time"
)

// Which status pages a maintenance window is announced on, and what a visitor
// to one of them is told.
//
// auto_status_pages puts the window on every page that shows a monitor it
// covers, by whichever route it covers it. status_page_ids is taken as given,
// and omitting both keeps the window off every page, as it always has.

type pageFixture struct {
	c                    *client
	direct, grouped      string // monitor ids
	tagged, unlisted     string
	group, tag           string
	pageDirect, pageBoth string // pageBoth shows grouped and tagged
	pageTagged           string
}

func newPageFixture(t *testing.T) pageFixture {
	t.Helper()

	c := newClient(t, testServer(t))
	c.setup()

	resp, group := c.do(http.MethodPost, "/api/v1/groups", map[string]any{"name": "Payments"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group = %d (%v)", resp.StatusCode, group)
	}
	tag := createTag(t, c, map[string]any{"name": "edge"})

	f := pageFixture{c: c, group: group["id"].(string), tag: tag["id"].(string)}
	f.direct = createMonitorIn(t, c, "website", nil)["id"].(string)
	f.grouped = createMonitorIn(t, c, "billing", map[string]any{"group_id": f.group})["id"].(string)
	f.tagged = createMonitorIn(t, c, "cdn", map[string]any{"tag_ids": []string{f.tag}})["id"].(string)
	f.unlisted = createMonitorIn(t, c, "internal", nil)["id"].(string)

	f.pageDirect = createStatusPage(t, c, map[string]any{
		"slug": "public", "title": "Public", "published": true,
		"sections": []map[string]any{{"name": "Web", "monitor_ids": []string{f.direct}}},
	})["id"].(string)
	f.pageBoth = createStatusPage(t, c, map[string]any{
		"slug": "partners", "title": "Partners", "published": true,
		"sections": []map[string]any{
			{"name": "Billing", "monitor_ids": []string{f.grouped}},
			{"name": "Delivery", "monitor_ids": []string{f.tagged}},
		},
	})["id"].(string)
	f.pageTagged = createStatusPage(t, c, map[string]any{
		"slug": "cdn", "title": "CDN", "published": true,
		"sections": []map[string]any{{"name": "Edge", "monitor_ids": []string{f.tagged}}},
	})["id"].(string)
	return f
}

// singleWindow is a window an hour from now that asks for its pages to be
// worked out.
func singleWindow(targets map[string]any) map[string]any {
	now := time.Now().UTC()
	return map[string]any{
		"title":             "Database upgrade",
		"strategy":          "single",
		"starts_at":         now.Add(time.Hour).Format(time.RFC3339),
		"ends_at":           now.Add(2 * time.Hour).Format(time.RFC3339),
		"targets":           targets,
		"auto_status_pages": true,
	}
}

func pageIDsOf(t *testing.T, window map[string]any) []string {
	t.Helper()

	raw, ok := window["status_page_ids"].([]any)
	if !ok && window["status_page_ids"] != nil {
		t.Fatalf("status_page_ids = %#v, want an array", window["status_page_ids"])
	}
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		out = append(out, id.(string))
	}
	slices.Sort(out)
	return out
}

func sorted(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

func TestMaintenanceWindowJoinsThePagesShowingWhatItCovers(t *testing.T) {
	t.Parallel()

	f := newPageFixture(t)

	cases := []struct {
		name    string
		targets map[string]any
		want    []string
	}{
		{"monitor", map[string]any{"monitor_ids": []string{f.direct}}, sorted(f.pageDirect)},
		{"group", map[string]any{"group_ids": []string{f.group}}, sorted(f.pageBoth)},
		{"tag", map[string]any{"tag_ids": []string{f.tag}}, sorted(f.pageBoth, f.pageTagged)},
		{"mixed", map[string]any{
			"monitor_ids": []string{f.direct}, "tag_ids": []string{f.tag},
		}, sorted(f.pageDirect, f.pageBoth, f.pageTagged)},
		{"on no page", map[string]any{"monitor_ids": []string{f.unlisted}}, []string{}},
	}
	for _, tc := range cases {
		created := createWindow(t, f.c, singleWindow(tc.targets))
		if got := pageIDsOf(t, created); !slices.Equal(got, tc.want) {
			t.Errorf("%s: status_page_ids = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestExplicitStatusPagesAreTakenAsGiven(t *testing.T) {
	t.Parallel()

	f := newPageFixture(t)

	// A page that does not show the monitor is still the operator's call.
	body := singleWindow(map[string]any{"monitor_ids": []string{f.direct}})
	delete(body, "auto_status_pages")
	body["status_page_ids"] = []string{f.pageTagged}
	if got := pageIDsOf(t, createWindow(t, f.c, body)); !slices.Equal(got, []string{f.pageTagged}) {
		t.Errorf("explicit list: status_page_ids = %v, want only %s", got, f.pageTagged)
	}

	// auto_status_pages false is the same as leaving it out.
	body = singleWindow(map[string]any{"monitor_ids": []string{f.direct}})
	body["auto_status_pages"] = false
	if got := pageIDsOf(t, createWindow(t, f.c, body)); len(got) != 0 {
		t.Errorf("auto_status_pages false: status_page_ids = %v, want none", got)
	}
}

// Leaving both fields out has meant "no pages" since 1.0, and a script relying
// on that must not start announcing its windows in public.
func TestOmittingStatusPagesStillMeansNone(t *testing.T) {
	t.Parallel()

	f := newPageFixture(t)
	body := singleWindow(map[string]any{"monitor_ids": []string{f.direct}})
	delete(body, "auto_status_pages")

	if got := pageIDsOf(t, createWindow(t, f.c, body)); len(got) != 0 {
		t.Errorf("status_page_ids = %v, want none when neither field is sent", got)
	}
	if shown := publicMaintenance(t, f.c, "public"); len(shown) != 0 {
		t.Errorf("public page lists %v for a window attached to no page", shown)
	}
}

func TestAutoAndExplicitStatusPagesConflict(t *testing.T) {
	t.Parallel()

	f := newPageFixture(t)
	body := singleWindow(map[string]any{"monitor_ids": []string{f.direct}})
	body["status_page_ids"] = []string{f.pageDirect}

	resp, problem := f.c.do(http.MethodPost, "/api/v1/maintenance-windows", body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, problem)
	}
	if first := firstProblem(t, problem); first["pointer"] != "/auto_status_pages" || first["code"] != "conflict" {
		t.Errorf("problem = %v, want a conflict on /auto_status_pages", first)
	}
}

// An update is a replacement, so auto_status_pages on it works the pages out
// again from the targets the replacement carries.
func TestReplacingAWindowRederivesItsPages(t *testing.T) {
	t.Parallel()

	f := newPageFixture(t)
	created := createWindow(t, f.c, singleWindow(map[string]any{"monitor_ids": []string{f.direct}}))
	id := created["id"].(string)

	resp, updated := f.c.do(http.MethodPatch, "/api/v1/maintenance-windows/"+id, map[string]any{
		"targets":           map[string]any{"group_ids": []string{f.group}},
		"auto_status_pages": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update = %d (%v)", resp.StatusCode, updated)
	}
	if got := pageIDsOf(t, updated); !slices.Equal(got, []string{f.pageBoth}) {
		t.Errorf("status_page_ids = %v after retargeting, want %s", got, f.pageBoth)
	}
}

func publicMaintenance(t *testing.T, c *client, slug string) []map[string]any {
	t.Helper()

	resp, body := c.do(http.MethodGet, "/api/v1/public/status-pages/"+slug, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public page = %d (%v)", resp.StatusCode, body)
	}
	var out []map[string]any
	for _, entry := range body["scheduled_maintenance"].([]any) {
		out = append(out, entry.(map[string]any))
	}
	return out
}

func TestPublicPageAnnouncesAutoAttachedWindows(t *testing.T) {
	t.Parallel()

	f := newPageFixture(t)
	now := time.Now().UTC()

	createWindow(t, f.c, map[string]any{
		"title": "Running now", "strategy": "single",
		"starts_at":         now.Add(-time.Minute).Format(time.RFC3339),
		"duration_minutes":  60,
		"targets":           map[string]any{"monitor_ids": []string{f.direct}},
		"auto_status_pages": true,
	})
	// Over and done with. Still a valid record — it annotates history — but
	// not something a visitor needs telling about.
	createWindow(t, f.c, map[string]any{
		"title": "Last night", "strategy": "single",
		"starts_at":         now.Add(-3 * time.Hour).Format(time.RFC3339),
		"ends_at":           now.Add(-2 * time.Hour).Format(time.RFC3339),
		"targets":           map[string]any{"monitor_ids": []string{f.direct}},
		"auto_status_pages": true,
	})
	// Kept internal by the operator, so it must not appear.
	createWindow(t, f.c, map[string]any{
		"title": "Internal", "strategy": "single",
		"starts_at":            now.Add(time.Hour).Format(time.RFC3339),
		"duration_minutes":     30,
		"show_on_status_pages": false,
		"targets":              map[string]any{"monitor_ids": []string{f.direct}},
		"auto_status_pages":    true,
	})

	shown := publicMaintenance(t, f.c, "public")
	if len(shown) != 1 || shown[0]["title"] != "Running now" {
		t.Fatalf("scheduled_maintenance = %v, want only the running window", shown)
	}
	if shown[0]["ends_at"] == nil {
		t.Error("ends_at is missing; a duration-based window still has an end")
	}

	if other := publicMaintenance(t, f.c, "cdn"); len(other) != 0 {
		t.Errorf("a page not showing the monitor lists %v", other)
	}
}

// A recurring window's starts_at is the anchor of its rule. A visitor should see
// the next occurrence, including straight after the write, before the sweep has
// materialised next_occurrence_at.
func TestPublicPageShowsTheNextOccurrenceNotTheAnchor(t *testing.T) {
	t.Parallel()

	f := newPageFixture(t)
	anchor := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Hour).Add(-time.Hour)

	createWindow(t, f.c, map[string]any{
		"title": "Nightly", "strategy": "recurring_daily", "timezone": "UTC",
		"starts_at":         anchor.Format(time.RFC3339),
		"duration_minutes":  30,
		"targets":           map[string]any{"monitor_ids": []string{f.direct}},
		"auto_status_pages": true,
	})

	shown := publicMaintenance(t, f.c, "public")
	if len(shown) != 1 {
		t.Fatalf("scheduled_maintenance = %v, want the recurring window", shown)
	}
	starts, err := time.Parse(time.RFC3339, shown[0]["starts_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(starts) > 24*time.Hour {
		t.Errorf("starts_at = %v, want an occurrence within the last day or ahead, not the anchor %v", starts, anchor)
	}
}
