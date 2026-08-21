package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// createHTTPMonitor makes a monitor and returns its id. The config carries a
// basic-auth password, so the tests below have a real secret to reason about.
func createHTTPMonitor(t *testing.T, c *client, name string) string {
	t.Helper()

	resp, body := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": name,
		"type": "http",
		"config": map[string]any{
			"url": "https://example.com/health",
			"auth": map[string]any{
				"type":     "basic",
				"username": "cairn",
				"password": "s3cret-live",
			},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor = %d, want 201 (%v)", resp.StatusCode, body)
	}
	return body["id"].(string)
}

func TestPatchLeavesUnmentionedFieldsAlone(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	resp, body := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{
		"interval_seconds": 120,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["name"] != "Checkout" {
		t.Errorf("name = %v, want it unchanged", body["name"])
	}
	if body["interval_seconds"] != 120.0 {
		t.Errorf("interval_seconds = %v, want 120", body["interval_seconds"])
	}
	if body["timeout_seconds"] != 30.0 {
		t.Errorf("timeout_seconds = %v, want the stored 30", body["timeout_seconds"])
	}
}

// The property this endpoint lives or dies on: a client that reads a monitor,
// edits one field, and submits the whole object back must not destroy the
// credential it was never shown.
func TestPatchEchoingARedactedSecretKeepsTheStoredOne(t *testing.T) {
	t.Parallel()

	server, store, api := testAPI(t)
	c := newClient(t, server)
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	read, body := c.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	if read.StatusCode != http.StatusOK {
		t.Fatalf("get = %d (%v)", read.StatusCode, body)
	}
	config := body["config"].(map[string]any)
	auth := config["auth"].(map[string]any)
	if auth["password"] != "__redacted__" {
		t.Fatalf("password came back as %v, want the redaction marker", auth["password"])
	}

	// Exactly what a form would send: the document it was given, with one field
	// changed.
	auth["username"] = "cairn-2"
	resp, patched := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{"config": config})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%v)", resp.StatusCode, patched)
	}

	monitor, err := store.GetMonitor(t.Context(), mustParseID(t, id))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(monitor.Monitor.ConfigSecrets) == 0 {
		t.Fatal("the monitor has no sealed credentials left after the patch")
	}
	if got := string(monitor.Monitor.Config); strings.Contains(got, "s3cret-live") || strings.Contains(got, "__redacted__") {
		t.Fatalf("stored config = %s, want neither the plaintext nor the marker", got)
	}

	// And the credential still works, which is the only proof that matters.
	openConfig := openMonitorConfig(t, api, monitor.Monitor)
	if !strings.Contains(string(openConfig), "s3cret-live") {
		t.Fatalf("reassembled config = %s, want the original password", openConfig)
	}
	if !strings.Contains(string(openConfig), "cairn-2") {
		t.Fatalf("reassembled config = %s, want the new username", openConfig)
	}
}

func TestPatchRefusesALiteralRedactionMarkerAsANewPassword(t *testing.T) {
	t.Parallel()

	server, store, api := testAPI(t)
	c := newClient(t, server)
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	// A caller inventing the marker rather than echoing one: the config it sends
	// has no other auth fields, so there is nothing to carry over and the marker
	// must not become the password.
	resp, body := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{
		"config": map[string]any{
			"url":  "https://example.com/health",
			"auth": map[string]any{"type": "basic", "username": "cairn", "password": "__redacted__"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%v)", resp.StatusCode, body)
	}

	monitor, err := store.GetMonitor(t.Context(), mustParseID(t, id))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	opened := string(openMonitorConfig(t, api, monitor.Monitor))
	if strings.Contains(opened, "__redacted__") {
		t.Fatalf("config = %s, want the marker resolved rather than stored", opened)
	}
	if !strings.Contains(opened, "s3cret-live") {
		t.Fatalf("config = %s, want the stored password preserved", opened)
	}
}

func TestPatchCannotChangeAMonitorsType(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	// `type` is not a field on MonitorUpdate, and the decoder rejects unknown
	// fields — so this is a 400 rather than a silently ignored key. A monitor
	// whose type changed would have a history of two different things.
	resp, _ := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{"type": "tcp"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch type = %d, want 400", resp.StatusCode)
	}
}

func TestPatchRefusesATimeoutThatWouldOutliveTheInterval(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	// Only the timeout is sent. The check is against the merged pair, because
	// raising a timeout past an interval the caller did not mention is the same
	// mistake as lowering the interval below it.
	resp, body := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{"timeout_seconds": 90})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("patch = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if pointer := firstErrorPointer(body); pointer != "/timeout_seconds" {
		t.Fatalf("pointer = %q, want /timeout_seconds", pointer)
	}
}

func TestNullClearsAGroupAssignment(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	_, group := c.do(http.MethodPost, "/api/v1/groups", map[string]any{"name": "Production"})
	groupID := group["id"].(string)
	id := createHTTPMonitor(t, c, "Checkout")

	if resp, body := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{"group_id": groupID}); resp.StatusCode != http.StatusOK {
		t.Fatalf("assign group = %d (%v)", resp.StatusCode, body)
	}

	// An explicit null, which a *string cannot distinguish from an absent field.
	resp, body := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{"group_id": nil})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear group = %d (%v)", resp.StatusCode, body)
	}
	if body["group_id"] != nil {
		t.Fatalf("group_id = %v, want null", body["group_id"])
	}

	// And an absent field leaves it alone, which is the other half of the claim.
	if resp, _ := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{"group_id": groupID}); resp.StatusCode != http.StatusOK {
		t.Fatalf("reassign group = %d", resp.StatusCode)
	}
	_, after := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{"name": "Checkout API"})
	if after["group_id"] != groupID {
		t.Fatalf("group_id = %v after an unrelated patch, want %q", after["group_id"], groupID)
	}
}

// Reparenting is the first endpoint that can actually close a dependency loop:
// a new monitor is nobody's ancestor, but an existing one can be reparented onto
// its own descendant.
func TestReparentingOntoADescendantIsRefused(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	parent := createHTTPMonitor(t, c, "Router")
	child := createHTTPMonitor(t, c, "Switch")

	if resp, body := c.do(http.MethodPatch, "/api/v1/monitors/"+child,
		map[string]any{"parent_monitor_id": parent}); resp.StatusCode != http.StatusOK {
		t.Fatalf("set parent = %d (%v)", resp.StatusCode, body)
	}

	resp, body := c.do(http.MethodPatch, "/api/v1/monitors/"+parent,
		map[string]any{"parent_monitor_id": child})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("cycle = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if code := firstErrorCode(body); code != "cycle" {
		t.Fatalf("code = %q, want cycle", code)
	}
}

func TestPauseStopsTheCheckAndResumeReturnsToPending(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	resp, body := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/pause", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause = %d (%v)", resp.StatusCode, body)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	if body["status"] != "paused" {
		t.Errorf("status = %v, want paused", body["status"])
	}
	if body["next_check_at"] != nil {
		t.Errorf("next_check_at = %v, want null — nothing is scheduled", body["next_check_at"])
	}

	// pending rather than whatever it was: it has not been checked since, and
	// reporting a stale verdict as current is how a monitor that broke while
	// paused stays green.
	resp, body = c.do(http.MethodPost, "/api/v1/monitors/"+id+"/resume", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume = %d (%v)", resp.StatusCode, body)
	}
	if body["status"] != "pending" {
		t.Errorf("status = %v, want pending", body["status"])
	}
}

func TestCheckNowIsRateLimitedPerMonitor(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	// The first check runs against a host that does not resolve, which is fine:
	// what is under test is the limiter, and a failed check is still a check.
	if resp, body := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/check", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first check = %d, want 200 (%v)", resp.StatusCode, body)
	}

	resp, body := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/check", nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second check = %d, want 429 (%v)", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After header on the 429")
	}
}

func TestCheckNowRefusesAPushMonitor(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	_, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "Nightly backup", "type": "push", "config": map[string]any{},
	})
	id := created["id"].(string)

	// A push monitor is a deadline, not a check. Running one would write a
	// heartbeat the target did not send.
	resp, body := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/check", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("check = %d, want 422 (%v)", resp.StatusCode, body)
	}
}

func TestBulkReportsEachIdentifierSeparately(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	first := createHTTPMonitor(t, c, "One")
	second := createHTTPMonitor(t, c, "Two")
	missing := "01930000-0000-7000-8000-00000000dead"

	resp, body := c.do(http.MethodPost, "/api/v1/monitors/bulk", map[string]any{
		"monitor_ids": []string{first, missing, second},
		"operation":   "disable",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bulk = %d, want 200 (%v)", resp.StatusCode, body)
	}

	succeeded := body["succeeded"].([]any)
	failed := body["failed"].([]any)
	if len(succeeded) != 2 {
		t.Fatalf("succeeded = %v, want the two that exist", succeeded)
	}
	if len(failed) != 1 {
		t.Fatalf("failed = %v, want the one that does not", failed)
	}
	if entry := failed[0].(map[string]any); entry["code"] != "not_found" {
		t.Fatalf("failure code = %v, want not_found", entry["code"])
	}

	// The two that succeeded actually moved.
	_, after := c.do(http.MethodGet, "/api/v1/monitors/"+first, nil)
	if after["enabled"] != false {
		t.Fatalf("enabled = %v, want false", after["enabled"])
	}
}

func TestBulkAddTagsIsIdempotent(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	_, tag := c.do(http.MethodPost, "/api/v1/tags", map[string]any{"name": "Edge"})
	tagID := tag["id"].(string)
	id := createHTTPMonitor(t, c, "Gateway")

	for range 2 {
		resp, body := c.do(http.MethodPost, "/api/v1/monitors/bulk", map[string]any{
			"monitor_ids": []string{id}, "operation": "add_tags", "tag_ids": []string{tagID},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("bulk = %d (%v)", resp.StatusCode, body)
		}
	}

	_, monitor := c.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	tags := monitor["tag_ids"].([]any)
	if len(tags) != 1 {
		t.Fatalf("tag_ids = %v, want exactly one after two adds", tags)
	}

	// And removing takes it off again.
	c.do(http.MethodPost, "/api/v1/monitors/bulk", map[string]any{
		"monitor_ids": []string{id}, "operation": "remove_tags", "tag_ids": []string{tagID},
	})
	_, monitor = c.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	if tags := monitor["tag_ids"].([]any); len(tags) != 0 {
		t.Fatalf("tag_ids = %v, want none after removal", tags)
	}
}

func TestBulkRefusesAnUnknownOperationBeforeTouchingAnything(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	resp, body := c.do(http.MethodPost, "/api/v1/monitors/bulk", map[string]any{
		"monitor_ids": []string{id}, "operation": "obliterate",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bulk = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if _, after := c.do(http.MethodGet, "/api/v1/monitors/"+id, nil); after["enabled"] != true {
		t.Fatal("the monitor changed despite the request being refused")
	}
}

// Both halves of the membership signal are needed, and this is why: a monitor
// leaving a filter as another enters keeps the count identical while the view
// has changed.
func TestMembershipMovesOnContentAndOnCount(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	_, first := c.do(http.MethodGet, "/api/v1/monitors/membership", nil)
	if first["count"] != 1.0 {
		t.Fatalf("count = %v, want 1", first["count"])
	}

	// A configuration edit that never touches monitor_state still has to move
	// the version, or a rename would leave every open list view showing the old
	// name until something failed.
	c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{"name": "Checkout API"})
	_, renamed := c.do(http.MethodGet, "/api/v1/monitors/membership", nil)
	if renamed["version"] == first["version"] {
		t.Fatalf("version did not move after a rename: %v", renamed["version"])
	}
	if renamed["count"] != 1.0 {
		t.Fatalf("count = %v, want 1 still", renamed["count"])
	}

	createHTTPMonitor(t, c, "Search")
	_, grown := c.do(http.MethodGet, "/api/v1/monitors/membership", nil)
	if grown["count"] != 2.0 {
		t.Fatalf("count = %v, want 2", grown["count"])
	}
}

func TestMembershipAndTheListingAgreeAboutAFilter(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	first := createHTTPMonitor(t, c, "Checkout")
	createHTTPMonitor(t, c, "Search")
	c.do(http.MethodPost, "/api/v1/monitors/"+first+"/pause", nil)

	for _, query := range []string{"?status=paused", "?enabled=false", "?search=Checkout"} {
		_, listed := c.do(http.MethodGet, "/api/v1/monitors"+query, nil)
		data := listed["data"].([]any)

		_, signal := c.do(http.MethodGet, "/api/v1/monitors/membership"+query, nil)
		if float64(len(data)) != signal["count"] {
			t.Fatalf("%s: listing returned %d rows, membership says %v", query, len(data), signal["count"])
		}
		if len(data) != 1 {
			t.Fatalf("%s: returned %d rows, want 1", query, len(data))
		}
	}
}

func TestSearchMatchesTheTargetAsWellAsTheName(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	createHTTPMonitor(t, c, "Checkout")

	// The question that brings somebody to the search box is usually "what else
	// points at this host?", and the answer lives in a field they never named.
	_, body := c.do(http.MethodGet, "/api/v1/monitors?search=example.com", nil)
	if data := body["data"].([]any); len(data) != 1 {
		t.Fatalf("search by target returned %d rows, want 1", len(data))
	}
}

func TestAnUnknownFilterValueIsRefusedRatherThanMatchingNothing(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	// Silently returning an empty page for a typo is how somebody concludes
	// their monitors have been deleted.
	for _, query := range []string{"?status=uP", "?type=htp", "?enabled=maybe"} {
		resp, _ := c.do(http.MethodGet, "/api/v1/monitors"+query, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", query, resp.StatusCode)
		}
	}
}

func TestIncludeIsOptInAndUnknownValuesAreRefused(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	_, tag := c.do(http.MethodPost, "/api/v1/tags", map[string]any{"name": "Edge"})
	_, group := c.do(http.MethodPost, "/api/v1/groups", map[string]any{"name": "Production"})
	id := createHTTPMonitor(t, c, "Checkout")
	c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{
		"tag_ids": []string{tag["id"].(string)}, "group_id": group["id"].(string),
	})

	// Without include=, the response is exactly what it was before the embeds
	// existed. That is what makes adding one a non-breaking change.
	_, plain := c.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	for _, field := range []string{"tags", "group", "uptime", "last_heartbeat"} {
		if _, present := plain[field]; present {
			t.Errorf("%s present without being asked for", field)
		}
	}

	_, embedded := c.do(http.MethodGet, "/api/v1/monitors/"+id+"?include=tags,group,uptime", nil)
	if tags, ok := embedded["tags"].([]any); !ok || len(tags) != 1 {
		t.Errorf("tags = %v, want one", embedded["tags"])
	}
	if embedded["group"] == nil {
		t.Error("group was requested and is absent")
	}
	// Nothing has been rolled up yet, so both windows are null rather than zero.
	// Zero would be a claim of total downtime made by a table that has not run.
	uptime, ok := embedded["uptime"].(map[string]any)
	if !ok {
		t.Fatalf("uptime = %v, want an object", embedded["uptime"])
	}
	if uptime["24h"] != nil || uptime["30d"] != nil {
		t.Errorf("uptime = %v, want nulls before the cache has been computed", uptime)
	}

	resp, _ := c.do(http.MethodGet, "/api/v1/monitors/"+id+"?include=everything", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown include = %d, want 400", resp.StatusCode)
	}

	// certificate is offered on a single monitor and not on a list, where it
	// would be a primary-key read per row for a field almost no row has.
	resp, _ = c.do(http.MethodGet, "/api/v1/monitors?include=certificate", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("include=certificate on a list = %d, want 400", resp.StatusCode)
	}
}

func TestCertificateEndpointIsHonestAboutHavingObservedNothing(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	resp, body := c.do(http.MethodGet, "/api/v1/monitors/"+id+"/certificate", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("certificate = %d, want 404 (%v)", resp.StatusCode, body)
	}
	if body["type"] != errorBase+"certificate-not-observed" {
		t.Fatalf("type = %v, want the not-observed problem rather than a generic 404", body["type"])
	}
}

func firstErrorPointer(body map[string]any) string {
	errs, ok := body["errors"].([]any)
	if !ok || len(errs) == 0 {
		return ""
	}
	pointer, _ := errs[0].(map[string]any)["pointer"].(string)
	return pointer
}

func firstErrorCode(body map[string]any) string {
	errs, ok := body["errors"].([]any)
	if !ok || len(errs) == 0 {
		return ""
	}
	code, _ := errs[0].(map[string]any)["code"].(string)
	return code
}

func mustParseID(t *testing.T, raw string) model.ID {
	t.Helper()

	id, ok := model.ParseID(raw)
	if !ok {
		t.Fatalf("parse id %q", raw)
	}
	return id
}

// Once something has been observed, the endpoint and the include both answer
// with it. This is the read side of the observation path: the checker sees the
// certificate, the result frame carries it, ingest stores it, and these two
// surfaces are what anybody actually looks at.
func TestCertificateEndpointRendersWhatWasObserved(t *testing.T) {
	t.Parallel()

	server, store := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	id := createHTTPMonitor(t, c, "Checkout")

	monitorID, ok := model.ParseID(id)
	if !ok {
		t.Fatalf("parse id %q", id)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	valid := true
	if err := store.SaveCertificate(t.Context(), model.Certificate{
		MonitorID:         monitorID,
		OrgID:             model.SentinelOrgID,
		Subject:           "CN=api.example.com",
		Issuer:            "CN=Example CA R3",
		SerialNumber:      "04a1b2c3",
		ValidTo:           now.Add(45 * 24 * time.Hour),
		FingerprintSHA256: []byte{0xde, 0xad, 0xbe, 0xef},
		SANs:              []string{"api.example.com"},
		ChainValid:        &valid,
		ObservedAt:        now,
	}); err != nil {
		t.Fatalf("save certificate: %v", err)
	}

	resp, body := c.do(http.MethodGet, "/api/v1/monitors/"+id+"/certificate", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("certificate = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["subject"] != "CN=api.example.com" || body["issuer"] != "CN=Example CA R3" {
		t.Errorf("certificate = %v, want the observed subject and issuer", body)
	}
	// Hex on the way out: the wire carries raw bytes, and JSON has nowhere to
	// put them.
	if body["fingerprint_sha256"] != "deadbeef" {
		t.Errorf("fingerprint_sha256 = %v, want the hex encoding", body["fingerprint_sha256"])
	}
	if body["chain_valid"] != true {
		t.Errorf("chain_valid = %v, want true", body["chain_valid"])
	}
	// Counted from now rather than stored, so it counts down between
	// observations instead of going stale for an hour at a time.
	if remaining, ok := body["days_remaining"].(float64); !ok || remaining < 44 || remaining > 45 {
		t.Errorf("days_remaining = %v, want about 45", body["days_remaining"])
	}

	resp, body = c.do(http.MethodGet, "/api/v1/monitors/"+id+"?include=certificate", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("include=certificate = %d (%v)", resp.StatusCode, body)
	}
	embedded, isObject := body["certificate"].(map[string]any)
	if !isObject {
		t.Fatalf("certificate = %v, want the embedded object", body["certificate"])
	}
	if embedded["serial_number"] != "04a1b2c3" {
		t.Errorf("embedded certificate = %v, want the same row the endpoint returned", embedded)
	}

	// And it lands in the overview's expiring-soon count, which is the same
	// index read from the other end.
	_, overview := c.do(http.MethodGet, "/api/v1/overview", nil)
	if overview["certificates_expiring_soon"] != float64(0) {
		t.Errorf("certificates_expiring_soon = %v, want 0 at 45 days out", overview["certificates_expiring_soon"])
	}
}

// The strip a list view draws under each row. The reason it is an embed rather
// than a request per row is the whole point: at a hundred rows a page, one
// request each is exactly the fan-out ADR-004 and the include= design exist to
// prevent.
func TestIncludeHeartbeatsResolvesARunForTheWholePage(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	// Two monitors, so a run landing under the wrong row would show.
	firstToken, first := createPushMonitor(t, c, map[string]any{
		"name": "Nightly backup", "type": "push", "config": map[string]any{},
	})
	secondToken, second := createPushMonitor(t, c, map[string]any{
		"name": "Reporting job", "type": "push", "config": map[string]any{},
	})

	pusher := newClient(t, server)
	for i := 0; i < 4; i++ {
		pusher.do(http.MethodGet, "/api/v1/push/"+firstToken, nil)
	}
	pusher.do(http.MethodGet, "/api/v1/push/"+secondToken+"?status=down", nil)

	_, body := c.do(http.MethodGet, "/api/v1/monitors?include=heartbeats", nil)
	runs := map[string][]any{}
	for _, row := range body["data"].([]any) {
		monitor := row.(map[string]any)
		beats, ok := monitor["heartbeats"].([]any)
		if !ok {
			t.Fatalf("monitor %v carries no heartbeats array", monitor["name"])
		}
		runs[monitor["id"].(string)] = beats
	}

	if got := len(runs[first]); got != 4 {
		t.Errorf("first monitor's run has %d beats, want 4", got)
	}
	if got := len(runs[second]); got != 1 {
		t.Errorf("second monitor's run has %d beats, want 1", got)
	}

	// Most recent first, and each beat belongs to the row it arrived under.
	// UNION ALL promises nothing about row order, so this is asserted rather
	// than assumed — a strip drawn backwards is a bug nobody would look for.
	var previous string
	for i, entry := range runs[first] {
		beat := entry.(map[string]any)
		if beat["monitor_id"] != first {
			t.Fatalf("beat %d belongs to %v, not to the row it was returned under", i, beat["monitor_id"])
		}
		at := beat["time"].(string)
		if previous != "" && at > previous {
			t.Errorf("beat %d is newer than the one before it: %s after %s", i, at, previous)
		}
		previous = at
	}
}

// The embed's cost has to track the viewport rather than whatever a caller
// types, which is the ADR-004 invariant it is on the wrong side of the moment
// the ceiling comes off. Clamped rather than refused, like the page limit.
func TestHeartbeatsLimitIsClampedAndValidated(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	token, id := createPushMonitor(t, c, map[string]any{
		"name": "Nightly backup", "type": "push", "config": map[string]any{},
	})
	pusher := newClient(t, server)
	for i := 0; i < 3; i++ {
		pusher.do(http.MethodGet, "/api/v1/push/"+token, nil)
	}

	_, body := c.do(http.MethodGet, "/api/v1/monitors/"+id+"?include=heartbeats&heartbeats_limit=2", nil)
	if beats := body["heartbeats"].([]any); len(beats) != 2 {
		t.Errorf("heartbeats_limit=2 returned %d beats", len(beats))
	}

	// Above the ceiling is clamped, not rejected: only three beats exist, so
	// what this proves is that the request succeeded rather than 400ing.
	resp, body := c.do(http.MethodGet, "/api/v1/monitors/"+id+"?include=heartbeats&heartbeats_limit=100000", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("heartbeats_limit above the ceiling = %d, want 200 with a clamp", resp.StatusCode)
	}
	if beats := body["heartbeats"].([]any); len(beats) != 3 {
		t.Errorf("clamped request returned %d beats, want the 3 that exist", len(beats))
	}

	// Nonsense is a 400, because a client asking for something it will not get
	// should find out at development time.
	for _, value := range []string{"0", "-1", "twenty"} {
		resp, _ := c.do(http.MethodGet, "/api/v1/monitors/"+id+"?include=heartbeats&heartbeats_limit="+value, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("heartbeats_limit=%s = %d, want 400", value, resp.StatusCode)
		}
	}

	// Not asked for is still absent, which is what makes adding the embed a
	// non-breaking change.
	_, plain := c.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	if _, present := plain["heartbeats"]; present {
		t.Error("heartbeats present without being asked for")
	}
}
