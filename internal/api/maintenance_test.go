package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The maintenance-window surface. The assertions worth having are about what is
// refused: a schedule that will never fire, a window covering nothing, and a
// dependency that loops — each of which is invisible until the outage it fails
// to handle.

func maintenanceClient(t *testing.T) (*client, string) {
	t.Helper()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	resp, monitor := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "api", "type": "http", "config": map[string]any{"url": "https://example.com"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor = %d (%v)", resp.StatusCode, monitor)
	}
	return c, monitor["id"].(string)
}

func createWindow(t *testing.T, c *client, body map[string]any) map[string]any {
	t.Helper()

	resp, created := c.do(http.MethodPost, "/api/v1/maintenance-windows", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%v)", resp.StatusCode, created)
	}
	return created
}

func TestMaintenanceWindowCreateAndRead(t *testing.T) {
	t.Parallel()

	c, monitorID := maintenanceClient(t)
	starts := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)

	created := createWindow(t, c, map[string]any{
		"title":            "Sunday patching",
		"strategy":         "recurring_weekly",
		"timezone":         "Europe/London",
		"starts_at":        starts,
		"duration_minutes": 120,
		"recurrence":       map[string]any{"weekdays": []int{0}},
		"targets":          map[string]any{"monitor_ids": []string{monitorID}},
	})

	if created["state"] != "scheduled" {
		t.Errorf("state = %v, want scheduled", created["state"])
	}
	if created["timezone"] != "Europe/London" {
		t.Errorf("timezone = %v", created["timezone"])
	}
	if created["suppress_notifications"] != true {
		t.Error("suppress_notifications did not default to true")
	}
	targets := created["targets"].(map[string]any)
	if ids, _ := targets["monitor_ids"].([]any); len(ids) != 1 || ids[0] != monitorID {
		t.Errorf("targets = %v", targets)
	}

	resp, fetched := c.do(http.MethodGet, "/api/v1/maintenance-windows/"+created["id"].(string), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d", resp.StatusCode)
	}
	// The sweep materialises this on its next pass; the API wakes it on write,
	// but the two are asynchronous so only its presence is asserted here.
	if _, ok := fetched["next_occurrence_at"]; !ok {
		t.Error("next_occurrence_at is missing from the read shape")
	}
}

// State is derived on every read rather than stored, so a window whose
// occurrence has begun reads as active without anything having run.
func TestMaintenanceStateIsDerived(t *testing.T) {
	t.Parallel()

	c, monitorID := maintenanceClient(t)
	now := time.Now().UTC()

	cases := []struct {
		name  string
		start time.Duration
		want  string
	}{
		{"running now", -30 * time.Minute, "active"},
		{"later today", 6 * time.Hour, "scheduled"},
		{"finished", -6 * time.Hour, "ended"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created := createWindow(t, c, map[string]any{
				"title":     tc.name,
				"strategy":  "single",
				"starts_at": now.Add(tc.start).Format(time.RFC3339),
				"ends_at":   now.Add(tc.start + time.Hour).Format(time.RFC3339),
				"targets":   map[string]any{"monitor_ids": []string{monitorID}},
			})
			if created["state"] != tc.want {
				t.Errorf("state = %v, want %s", created["state"], tc.want)
			}
		})
	}
}

func TestMaintenanceValidation(t *testing.T) {
	t.Parallel()

	c, monitorID := maintenanceClient(t)
	starts := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	targets := map[string]any{"monitor_ids": []string{monitorID}}

	cases := []struct {
		name        string
		body        map[string]any
		wantPointer string
	}{
		{
			name: "a window covering nothing suppresses nothing",
			body: map[string]any{"title": "Empty", "strategy": "single",
				"starts_at": starts, "duration_minutes": 60},
			wantPointer: "/targets",
		},
		{
			name: "a recurring window needs a duration",
			body: map[string]any{"title": "No duration", "strategy": "recurring_daily",
				"starts_at": starts, "targets": targets},
			wantPointer: "/duration_minutes",
		},
		{
			name: "a single window needs an end",
			body: map[string]any{"title": "No end", "strategy": "single",
				"starts_at": starts, "targets": targets},
			wantPointer: "/ends_at",
		},
		{
			name: "an offset is not a timezone",
			body: map[string]any{"title": "Bad zone", "strategy": "recurring_daily",
				"timezone": "+11:00", "starts_at": starts, "duration_minutes": 60, "targets": targets},
			wantPointer: "/timezone",
		},
		{
			name: "an unknown strategy",
			body: map[string]any{"title": "Nope", "strategy": "every_other_tuesday",
				"starts_at": starts, "duration_minutes": 60, "targets": targets},
			wantPointer: "/strategy",
		},
		{
			// The one that would otherwise be discovered by its silence.
			name: "a weekly window with no weekday never fires",
			body: map[string]any{"title": "No weekday", "strategy": "recurring_weekly",
				"starts_at": starts, "duration_minutes": 60, "targets": targets},
			wantPointer: "/recurrence",
		},
		{
			name: "an unparseable cron",
			body: map[string]any{"title": "Bad cron", "strategy": "cron",
				"starts_at": starts, "duration_minutes": 60, "targets": targets,
				"recurrence": map[string]any{"cron": "0 2 * *"}},
			wantPointer: "/recurrence",
		},
		{
			name: "a recurrence that has already stopped",
			body: map[string]any{"title": "Expired", "strategy": "recurring_daily",
				"starts_at": starts, "duration_minutes": 60, "targets": targets,
				"recurrence": map[string]any{"until": "2020-01-01T00:00:00Z"}},
			wantPointer: "/recurrence",
		},
		{
			name: "a target that does not exist",
			body: map[string]any{"title": "Ghost", "strategy": "single",
				"starts_at": starts, "duration_minutes": 60,
				"targets": map[string]any{"monitor_ids": []string{"018f3a1c-4e5b-7c2d-9f10-2b3c4d5e6f70"}}},
			wantPointer: "/targets/monitor_ids",
		},
		{
			name: "state is derived, not set",
			body: map[string]any{"title": "Cancel me", "strategy": "single", "state": "cancelled",
				"starts_at": starts, "duration_minutes": 60, "targets": targets},
			wantPointer: "/state",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := c.do(http.MethodPost, "/api/v1/maintenance-windows", tc.body)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
			}
			errors, _ := body["errors"].([]any)
			pointers := make([]string, 0, len(errors))
			for _, item := range errors {
				pointers = append(pointers, item.(map[string]any)["pointer"].(string))
			}
			if !strings.Contains(strings.Join(pointers, " "), tc.wantPointer) {
				t.Errorf("problems at %v, want one at %s", pointers, tc.wantPointer)
			}
		})
	}
}

// Groups, tags, and status pages have tables and no API yet. Referencing one
// must be a validation error naming the field, not a foreign-key failure.
func TestReferencingAnEntityWithNoAPIYetIsAValidationError(t *testing.T) {
	t.Parallel()

	c, _ := maintenanceClient(t)
	starts := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	ghost := "018f3a1c-4e5b-7c2d-9f10-2b3c4d5e6f70"

	for field, targets := range map[string]map[string]any{
		"/targets/group_ids": {"group_ids": []string{ghost}},
		"/targets/tag_ids":   {"tag_ids": []string{ghost}},
	} {
		resp, body := c.do(http.MethodPost, "/api/v1/maintenance-windows", map[string]any{
			"title": "Ghost", "strategy": "single", "starts_at": starts,
			"duration_minutes": 60, "targets": targets,
		})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("%s: create = %d (%v)", field, resp.StatusCode, body)
		}
	}
}

func TestMaintenanceUpdateReplacesTargets(t *testing.T) {
	t.Parallel()

	c, monitorID := maintenanceClient(t)
	resp, second := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "second", "type": "http", "config": map[string]any{"url": "https://example.org"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor = %d", resp.StatusCode)
	}

	starts := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	created := createWindow(t, c, map[string]any{
		"title": "Patching", "strategy": "single", "starts_at": starts, "duration_minutes": 60,
		"targets": map[string]any{"monitor_ids": []string{monitorID, second["id"].(string)}},
	})

	resp, updated := c.do(http.MethodPatch, "/api/v1/maintenance-windows/"+created["id"].(string),
		map[string]any{
			"title": "Patching (reduced)", "strategy": "single", "starts_at": starts,
			"duration_minutes": 60,
			"targets":          map[string]any{"monitor_ids": []string{monitorID}},
		})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d (%v)", resp.StatusCode, updated)
	}
	if updated["title"] != "Patching (reduced)" {
		t.Errorf("title = %v", updated["title"])
	}
	if ids, _ := updated["targets"].(map[string]any)["monitor_ids"].([]any); len(ids) != 1 {
		t.Errorf("targets = %v, want exactly one", ids)
	}
}

func TestMaintenanceListFilters(t *testing.T) {
	t.Parallel()

	c, monitorID := maintenanceClient(t)
	now := time.Now().UTC()

	createWindow(t, c, map[string]any{
		"title": "Running", "strategy": "single",
		"starts_at": now.Add(-time.Hour).Format(time.RFC3339),
		"ends_at":   now.Add(time.Hour).Format(time.RFC3339),
		"targets":   map[string]any{"monitor_ids": []string{monitorID}},
	})
	createWindow(t, c, map[string]any{
		"title": "Upcoming", "strategy": "single",
		"starts_at": now.Add(24 * time.Hour).Format(time.RFC3339),
		"ends_at":   now.Add(25 * time.Hour).Format(time.RFC3339),
		"targets":   map[string]any{"monitor_ids": []string{monitorID}},
	})

	for query, want := range map[string]int{
		"":                         2,
		"?state=active":            1,
		"?state=scheduled":         1,
		"?search=Run":              1,
		"?monitor_id=" + monitorID: 2,
	} {
		resp, body := c.do(http.MethodGet, "/api/v1/maintenance-windows"+query, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", query, resp.StatusCode)
		}
		data, _ := body["data"].([]any)
		if len(data) != want {
			t.Errorf("%q returned %d windows, want %d", query, len(data), want)
		}
	}
}

func TestMaintenanceDelete(t *testing.T) {
	t.Parallel()

	c, monitorID := maintenanceClient(t)
	created := createWindow(t, c, map[string]any{
		"title": "Patching", "strategy": "single",
		"starts_at":        time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		"duration_minutes": 60,
		"targets":          map[string]any{"monitor_ids": []string{monitorID}},
	})
	id := created["id"].(string)

	resp, _ := c.do(http.MethodDelete, "/api/v1/maintenance-windows/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp, _ = c.do(http.MethodGet, "/api/v1/maintenance-windows/"+id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d", resp.StatusCode)
	}
}

// Dependency parents, validated at the point a monitor is written.
func TestDependencyParent(t *testing.T) {
	t.Parallel()

	c, parentID := maintenanceClient(t)

	resp, child := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "child", "type": "http", "config": map[string]any{"url": "https://example.org"},
		"parent_monitor_id": parentID,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v)", resp.StatusCode, child)
	}
	if child["parent_monitor_id"] != parentID {
		t.Errorf("parent_monitor_id = %v", child["parent_monitor_id"])
	}

	// And it survives a read.
	fetched := fetchOne(t, c, "/api/v1/monitors/"+child["id"].(string))
	if fetched["parent_monitor_id"] != parentID {
		t.Errorf("parent_monitor_id = %v after a read", fetched["parent_monitor_id"])
	}
}

func TestDependencyParentMustExist(t *testing.T) {
	t.Parallel()

	c, _ := maintenanceClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "orphan", "type": "http", "config": map[string]any{"url": "https://example.org"},
		"parent_monitor_id": "018f3a1c-4e5b-7c2d-9f10-2b3c4d5e6f70",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}
	errors, _ := body["errors"].([]any)
	first := errors[0].(map[string]any)
	if first["pointer"] != "/parent_monitor_id" || first["code"] != "not_found" {
		t.Errorf("problem = %v", first)
	}
}

// Ten levels is far past any real topology, and the bound is what stops a chain
// that somehow became a loop from being an infinite walk in ingest.
func TestDependencyChainDepthIsBounded(t *testing.T) {
	t.Parallel()

	c, root := maintenanceClient(t)
	parent := root

	for depth := 1; depth <= maxDependencyDepth+1; depth++ {
		resp, body := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
			"name": fmt.Sprintf("level-%d", depth), "type": "http",
			"config":            map[string]any{"url": "https://example.org"},
			"parent_monitor_id": parent,
		})
		if depth < maxDependencyDepth {
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("level %d = %d (%v)", depth, resp.StatusCode, body)
			}
			parent = body["id"].(string)
			continue
		}
		if resp.StatusCode == http.StatusCreated {
			parent = body["id"].(string)
			continue
		}
		errors, _ := body["errors"].([]any)
		first := errors[0].(map[string]any)
		if first["code"] != "too_deep" {
			t.Errorf("code = %v, want too_deep", first["code"])
		}
		return
	}
	t.Errorf("a chain deeper than %d levels was accepted", maxDependencyDepth)
}
