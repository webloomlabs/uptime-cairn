package api

import (
	"net/http"
	"testing"
	"time"
)

// Groups and tags from the outside, plus the two things they unblock: filtering
// a monitor list, and a maintenance window that targets a tag rather than a list
// of monitor ids.

func taxonomyClient(t *testing.T) *client {
	t.Helper()

	c := newClient(t, testServer(t))
	c.setup()
	return c
}

func createGroup(t *testing.T, c *client, body map[string]any) map[string]any {
	t.Helper()

	resp, created := c.do(http.MethodPost, "/api/v1/groups", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group = %d (%v)", resp.StatusCode, created)
	}
	return created
}

func createTag(t *testing.T, c *client, body map[string]any) map[string]any {
	t.Helper()

	resp, created := c.do(http.MethodPost, "/api/v1/tags", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tag = %d (%v)", resp.StatusCode, created)
	}
	return created
}

func createMonitorIn(t *testing.T, c *client, name string, extra map[string]any) map[string]any {
	t.Helper()

	body := map[string]any{
		"name": name, "type": "http", "config": map[string]any{"url": "https://example.com"},
	}
	for k, v := range extra {
		body[k] = v
	}
	resp, created := c.do(http.MethodPost, "/api/v1/monitors", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor = %d (%v)", resp.StatusCode, created)
	}
	return created
}

func TestGroupCreateAndRead(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	created := createGroup(t, c, map[string]any{"name": "Production", "description": "Customer facing"})

	if created["name"] != "Production" {
		t.Errorf("name = %v", created["name"])
	}
	if count, _ := created["monitor_count"].(float64); count != 0 {
		t.Errorf("monitor_count = %v", count)
	}
	// Null, not "up": an empty group has no status, and green would be the
	// dashboard inventing health.
	if created["status"] != nil {
		t.Errorf("status = %v, want null for an empty group", created["status"])
	}

	fetched := fetchOne(t, c, "/api/v1/groups/"+created["id"].(string))
	if fetched["id"] != created["id"] {
		t.Error("get returned a different group")
	}
}

// The headline: a parent group must report the worst status underneath it,
// children included.
func TestGroupReportsTheWorstStatusIncludingChildren(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	parent := createGroup(t, c, map[string]any{"name": "Production"})
	child := createGroup(t, c, map[string]any{"name": "Production / EU", "parent_group_id": parent["id"]})

	createMonitorIn(t, c, "in the parent", map[string]any{"group_id": parent["id"]})
	createMonitorIn(t, c, "in the child", map[string]any{"group_id": child["id"]})

	fetched := fetchOne(t, c, "/api/v1/groups/"+parent["id"].(string))
	if count, _ := fetched["monitor_count"].(float64); count != 2 {
		t.Errorf("monitor_count = %v, want both", count)
	}
	// Both are pending until they run, which is still a real status rather than
	// nothing.
	if fetched["status"] != "pending" {
		t.Errorf("status = %v", fetched["status"])
	}
}

// Groups nest one level in Phase 1, and the two rules that enforce it also make
// a cycle impossible.
func TestGroupNestingIsOneLevel(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	parent := createGroup(t, c, map[string]any{"name": "Production"})
	child := createGroup(t, c, map[string]any{"name": "Production / EU", "parent_group_id": parent["id"]})

	// A parent must itself have no parent.
	resp, body := c.do(http.MethodPost, "/api/v1/groups",
		map[string]any{"name": "Frankfurt", "parent_group_id": child["id"]})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("three levels = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if first := firstProblem(t, body); first["code"] != "too_deep" {
		t.Errorf("code = %v", first["code"])
	}

	// A group with children cannot itself be nested.
	other := createGroup(t, c, map[string]any{"name": "Other"})
	resp, body = c.do(http.MethodPatch, "/api/v1/groups/"+parent["id"].(string),
		map[string]any{"parent_group_id": other["id"]})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("nesting a parent = %d, want 422 (%v)", resp.StatusCode, body)
	}

	// And nothing may be its own parent.
	resp, body = c.do(http.MethodPatch, "/api/v1/groups/"+other["id"].(string),
		map[string]any{"parent_group_id": other["id"]})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("self-parent = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if first := firstProblem(t, body); first["code"] != "cycle" {
		t.Errorf("code = %v", first["code"])
	}
}

func TestDeletingAGroupUngroupsItsMonitors(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	group := createGroup(t, c, map[string]any{"name": "Production"})
	monitor := createMonitorIn(t, c, "api", map[string]any{"group_id": group["id"]})

	resp, _ := c.do(http.MethodDelete, "/api/v1/groups/"+group["id"].(string), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}

	survived := fetchOne(t, c, "/api/v1/monitors/"+monitor["id"].(string))
	if survived["group_id"] != nil {
		t.Errorf("group_id = %v, want the monitor ungrouped rather than deleted", survived["group_id"])
	}
}

func TestTagSlugIsDerived(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	created := createTag(t, c, map[string]any{"name": "Customer Facing", "color": "#ff0000"})

	if created["slug"] != "customer-facing" {
		t.Errorf("slug = %v", created["slug"])
	}
	if created["color"] != "#ff0000" {
		t.Errorf("color = %v", created["color"])
	}
}

func TestTagDefaultsToNeutralGrey(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	created := createTag(t, c, map[string]any{"name": "Untinted"})
	if created["color"] != "#6b7280" {
		t.Errorf("color = %v, want the spec's neutral default", created["color"])
	}
}

// Two tags that look the same in a list are two tags nobody can tell apart, so
// the second is a 409 rather than a 422: the request is fine, the current state
// is the problem.
func TestDuplicateTagSlugIsAConflict(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	createTag(t, c, map[string]any{"name": "Production"})

	resp, body := c.do(http.MethodPost, "/api/v1/tags", map[string]any{"name": "PRODUCTION"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate = %d, want 409 (%v)", resp.StatusCode, body)
	}
	if detail, _ := body["detail"].(string); detail == "" {
		t.Error("the conflict does not say which slug is taken")
	}
}

func TestTagValidation(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	cases := map[string]map[string]any{
		"/name":  {"name": "日本語"},
		"/color": {"name": "Coloured", "color": "red"},
	}

	for pointer, body := range cases {
		resp, problem := c.do(http.MethodPost, "/api/v1/tags", body)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("%s: create = %d (%v)", pointer, resp.StatusCode, problem)
		}
		if first := firstProblem(t, problem); first["pointer"] != pointer {
			t.Errorf("pointer = %v, want %s", first["pointer"], pointer)
		}
	}
}

func TestDeletingATagUntagsItsMonitors(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	tag := createTag(t, c, map[string]any{"name": "Edge"})
	monitor := createMonitorIn(t, c, "api", map[string]any{"tag_ids": []string{tag["id"].(string)}})

	if ids, _ := monitor["tag_ids"].([]any); len(ids) != 1 {
		t.Fatalf("tag_ids = %v on create", monitor["tag_ids"])
	}

	resp, _ := c.do(http.MethodDelete, "/api/v1/tags/"+tag["id"].(string), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}

	survived := fetchOne(t, c, "/api/v1/monitors/"+monitor["id"].(string))
	if ids, _ := survived["tag_ids"].([]any); len(ids) != 0 {
		t.Errorf("tag_ids = %v, want the monitor untagged rather than deleted", ids)
	}
}

// A tag you cannot filter by is not a tag.
func TestMonitorListFilters(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	parent := createGroup(t, c, map[string]any{"name": "Production"})
	child := createGroup(t, c, map[string]any{"name": "Production / EU", "parent_group_id": parent["id"]})
	edge := createTag(t, c, map[string]any{"name": "Edge"})
	database := createTag(t, c, map[string]any{"name": "Database"})

	createMonitorIn(t, c, "edge in parent", map[string]any{
		"group_id": parent["id"], "tag_ids": []string{edge["id"].(string)}})
	createMonitorIn(t, c, "db in child", map[string]any{
		"group_id": child["id"], "tag_ids": []string{database["id"].(string)}})
	createMonitorIn(t, c, "loose", nil)

	cases := map[string]int{
		"":                                   3,
		"?group_id=" + parent["id"].(string): 2, // reaches the child
		"?group_id=" + child["id"].(string):  1,
		"?tag_id=" + edge["id"].(string):     1,
		"?tag_id=" + edge["id"].(string) + "&tag_id=" + database["id"].(string): 2, // OR within the parameter
		"?group_id=" + child["id"].(string) + "&tag_id=" + edge["id"].(string):  0, // AND across them
	}

	for query, want := range cases {
		body := fetchOne(t, c, "/api/v1/monitors"+query)
		data, _ := body["data"].([]any)
		if len(data) != want {
			t.Errorf("%q returned %d monitors, want %d", query, len(data), want)
		}
	}
}

func TestMonitorRejectsUnknownGroupAndTag(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	ghost := "018f3a1c-4e5b-7c2d-9f10-2b3c4d5e6f70"

	for pointer, extra := range map[string]map[string]any{
		"/group_id": {"group_id": ghost},
		"/tag_ids":  {"tag_ids": []string{ghost}},
	} {
		body := map[string]any{
			"name": "api", "type": "http", "config": map[string]any{"url": "https://example.com"},
		}
		for k, v := range extra {
			body[k] = v
		}
		resp, problem := c.do(http.MethodPost, "/api/v1/monitors", body)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("%s: create = %d (%v)", pointer, resp.StatusCode, problem)
		}
		if first := firstProblem(t, problem); first["pointer"] != pointer {
			t.Errorf("pointer = %v, want %s", first["pointer"], pointer)
		}
	}
}

// The other thing tags unblock: a maintenance window that keeps covering
// monitors added after it was created.
func TestMaintenanceWindowCanTargetATag(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	tag := createTag(t, c, map[string]any{"name": "Production"})
	group := createGroup(t, c, map[string]any{"name": "Edge"})

	resp, created := c.do(http.MethodPost, "/api/v1/maintenance-windows", map[string]any{
		"title": "Everything production", "strategy": "single",
		"starts_at":        time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		"duration_minutes": 60,
		"targets": map[string]any{
			"tag_ids":   []string{tag["id"].(string)},
			"group_ids": []string{group["id"].(string)},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%v)", resp.StatusCode, created)
	}

	targets := created["targets"].(map[string]any)
	if ids, _ := targets["tag_ids"].([]any); len(ids) != 1 {
		t.Errorf("tag_ids = %v", targets["tag_ids"])
	}
	if ids, _ := targets["group_ids"].([]any); len(ids) != 1 {
		t.Errorf("group_ids = %v", targets["group_ids"])
	}
}

func TestTaxonomyEndpointsRequireAuthentication(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	for _, path := range []string{"/api/v1/groups", "/api/v1/tags"} {
		resp, _ := c.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, resp.StatusCode)
		}
	}
}

func firstProblem(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	errors, _ := body["errors"].([]any)
	if len(errors) == 0 {
		t.Fatalf("no validation detail in %v", body)
	}
	return errors[0].(map[string]any)
}

// An explicit null promotes a group to the top level. encoding/json collapses
// null into "absent" for a plain pointer, so this is the case a naive
// **string would silently drop.
func TestNullParentPromotesAGroupToTopLevel(t *testing.T) {
	t.Parallel()

	c := taxonomyClient(t)
	parent := createGroup(t, c, map[string]any{"name": "Production"})
	child := createGroup(t, c, map[string]any{"name": "Production / EU", "parent_group_id": parent["id"]})

	// Absent leaves it alone.
	resp, updated := c.do(http.MethodPatch, "/api/v1/groups/"+child["id"].(string),
		map[string]any{"description": "Frankfurt"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d (%v)", resp.StatusCode, updated)
	}
	if updated["parent_group_id"] != parent["id"] {
		t.Errorf("an absent parent_group_id cleared the parent: %v", updated["parent_group_id"])
	}

	// Null clears it.
	resp, updated = c.do(http.MethodPatch, "/api/v1/groups/"+child["id"].(string),
		map[string]any{"parent_group_id": nil})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d (%v)", resp.StatusCode, updated)
	}
	if updated["parent_group_id"] != nil {
		t.Errorf("parent_group_id = %v, want null", updated["parent_group_id"])
	}
}
