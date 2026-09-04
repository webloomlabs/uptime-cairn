package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func createSchedule(t *testing.T, c *client, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	return c.do(http.MethodPost, "/api/v1/report-schedules", body)
}

func scheduleFixtureBody(templateID string) map[string]any {
	return map[string]any{
		"report_template_id": templateID,
		"name":               "Monthly to Acme",
		"frequency":          "monthly",
		"timezone":           "Australia/Sydney",
		"send_at":            "09:00",
		"deliveries": []map[string]any{
			{"type": "email", "recipients": []string{"ops@example.com"}, "formats": []string{"pdf"}},
		},
	}
}

// A schedule round-trips, and `next_run_at` is computed on write rather than
// left for the scheduler to discover.
func TestAScheduleIsCreatedWithItsNextFiring(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})

	resp, body := createSchedule(t, c, scheduleFixtureBody(tpl))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%v)", resp.StatusCode, body)
	}
	if body["next_run_at"] == nil {
		t.Fatal("no next_run_at; the UI cannot say when this fires and the scheduler has nothing to seek on")
	}

	next, err := time.Parse(time.RFC3339, body["next_run_at"].(string))
	if err != nil {
		t.Fatalf("next_run_at is not a timestamp: %v", err)
	}
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skip("no tzdata")
	}
	// **09:00 in Sydney on the first**, which is not 09:00 UTC — the whole
	// reason the schedule stores a zone.
	local := next.In(sydney)
	if local.Day() != 1 || local.Hour() != 9 {
		t.Errorf("next firing is %s local, want 09:00 on the 1st", local.Format(time.RFC3339))
	}

	deliveries := body["deliveries"].([]any)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(deliveries))
	}
	first := deliveries[0].(map[string]any)
	if first["type"] != "email" {
		t.Errorf("delivery type = %v", first["type"])
	}
	if got := first["recipients"].([]any); len(got) != 1 || got[0] != "ops@example.com" {
		t.Errorf("recipients = %v", got)
	}
}

// **A schedule that will never fire is refused at write time rather than
// discovered by its silence.** The 30th of February parses cleanly and matches
// nothing; stored, it would sit forever and the operator would find out when a
// client asked where the report went.
func TestAScheduleThatNeverFiresIsRefused(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})

	body := scheduleFixtureBody(tpl)
	body["frequency"] = "cron"
	body["cron"] = "0 0 30 2 *"

	resp, out := createSchedule(t, c, body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, out)
	}
	if detail := problemMessages(out); !strings.Contains(detail, "never fires") {
		t.Errorf("messages = %q, want one saying the schedule never fires", detail)
	}
}

// A cron on a named frequency is refused rather than stored and ignored, which
// would leave an operator with a schedule they believe they configured.
func TestACronOnANamedFrequencyIsRefusedByTheAPI(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})

	body := scheduleFixtureBody(tpl)
	body["cron"] = "0 3 * * *" // frequency is still monthly

	resp, out := createSchedule(t, c, body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (%v)", resp.StatusCode, out)
	}
}

// An unknown zone is refused by name. Falling back to UTC would send a monthly
// report a working day early for half the world with nothing saying so.
func TestAnUnknownZoneIsRefusedByTheAPI(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})

	body := scheduleFixtureBody(tpl)
	body["timezone"] = "Mars/Olympus"

	resp, out := createSchedule(t, c, body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, out)
	}
	if !strings.Contains(problemMessages(out), "Mars/Olympus") {
		t.Errorf("messages = %q, want the zone named", problemMessages(out))
	}
}

// send_at is read strictly, so "9:00" is a validation error rather than a guess.
func TestABadSendAtIsRefused(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})

	body := scheduleFixtureBody(tpl)
	body["send_at"] = "9:00"

	resp, _ := createSchedule(t, c, body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// A schedule needs a template that exists, and the answer names the field rather
// than arriving as an internal error from a foreign key.
func TestAScheduleNeedsARealTemplate(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	body := scheduleFixtureBody("01a05bc1-8bd5-736a-9f3f-8424439051ed")

	resp, out := createSchedule(t, c, body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, out)
	}
	item := out["errors"].([]any)[0].(map[string]any)
	if item["pointer"] != "/report_template_id" {
		t.Errorf("pointer = %v", item["pointer"])
	}
}

// A schedule needs somewhere to send its output.
func TestAScheduleNeedsADeliveryTarget(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})

	body := scheduleFixtureBody(tpl)
	body["deliveries"] = []map[string]any{}

	resp, _ := createSchedule(t, c, body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// **An incomplete s3 target is refused with the missing field named**, rather
// than stored with a credential that authenticates as nobody.
//
// `region` is the one an operator on MinIO will reasonably leave blank, never
// having needed one — so the message says why it is required rather than merely
// that it is.
func TestAnIncompleteS3TargetIsRefusedWithTheMissingFieldNamed(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})

	body := scheduleFixtureBody(tpl)
	body["deliveries"] = []map[string]any{
		{"type": "s3", "s3": map[string]any{"bucket": "reports", "secret_access_key": "hunter2"}},
	}

	resp, out := createSchedule(t, c, body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, out)
	}
	messages := problemMessages(out)
	for _, want := range []string{"region is required for the request signature", "access_key_id is required"} {
		if !strings.Contains(messages, want) {
			t.Errorf("messages = %q, want it to contain %q", messages, want)
		}
	}
}

// A complete s3 target is stored, and **the credential never comes back out**.
//
// The split is at the storage boundary rather than the API boundary: the secret
// is sealed onto its own column, so the map the read path serialises has no
// credential in it to leak. This asserts the consequence rather than the
// mechanism — a read of the schedule returns the bucket and not the key.
func TestACompleteS3TargetIsStoredAndItsSecretIsNotReadableBack(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})

	body := scheduleFixtureBody(tpl)
	body["deliveries"] = []map[string]any{{
		"type": "s3",
		"s3": map[string]any{
			"bucket": "client-reports", "prefix": "acme", "region": "ap-southeast-2",
			"endpoint": "https://minio.example.com:9000", "path_style": true,
			"access_key_id": "AKIAIOSFODNN7EXAMPLE", "secret_access_key": "hunter2",
		},
	}}

	resp, out := createSchedule(t, c, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%v)", resp.StatusCode, out)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Errorf("the secret access key came back in the response: %s", raw)
	}
	if !strings.Contains(string(raw), "client-reports") {
		t.Errorf("the bucket did not round-trip: %s", raw)
	}
	if !strings.Contains(string(raw), "minio.example.com") {
		t.Errorf("the endpoint did not round-trip: %s", raw)
	}
}

// An s3 target with no s3 block is refused. Accepting one and storing a delivery
// that names no bucket would produce a schedule that fails on every firing.
func TestAnS3TargetNeedsAnS3Block(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})

	body := scheduleFixtureBody(tpl)
	body["deliveries"] = []map[string]any{{"type": "s3"}}

	resp, out := createSchedule(t, c, body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, out)
	}
	if !strings.Contains(problemMessages(out), "needs an s3 block") {
		t.Errorf("messages = %q, want the reason stated", problemMessages(out))
	}
}

// Changing the frequency recomputes the firing time. A schedule edited from
// monthly to daily that kept next month's date would go quiet for a month.
func TestChangingTheFrequencyRecomputesTheFiring(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})
	_, created := createSchedule(t, c, scheduleFixtureBody(tpl))
	id := created["id"].(string)

	_, updated := c.do(http.MethodPatch, "/api/v1/report-schedules/"+id, map[string]any{"frequency": "daily"})
	if updated["next_run_at"] == created["next_run_at"] {
		t.Error("the firing time did not move when the frequency changed")
	}

	before, _ := time.Parse(time.RFC3339, created["next_run_at"].(string))
	after, _ := time.Parse(time.RFC3339, updated["next_run_at"].(string))
	if !after.Before(before) {
		t.Errorf("daily fires at %s, no sooner than the monthly %s", after, before)
	}
}

// An absent deliveries field leaves the targets alone. The store replaces
// wholesale, so a PATCH that did not mention them would otherwise silently
// remove every recipient.
func TestPatchingWithoutDeliveriesKeepsThem(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})
	_, created := createSchedule(t, c, scheduleFixtureBody(tpl))
	id := created["id"].(string)

	_, updated := c.do(http.MethodPatch, "/api/v1/report-schedules/"+id, map[string]any{"name": "Renamed"})
	if deliveries := updated["deliveries"].([]any); len(deliveries) != 1 {
		t.Fatalf("deliveries = %d after a rename, want 1 — a PATCH that did not mention them removed them", len(deliveries))
	}
	if updated["name"] != "Renamed" {
		t.Errorf("name = %v", updated["name"])
	}
}

// Deleting a schedule stops it and leaves it out of the list.
func TestDeletingASchedule(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	tpl := createTemplate(t, c, map[string]any{"name": "Monthly", "type": "sla", "formats": []string{"pdf"}})
	_, created := createSchedule(t, c, scheduleFixtureBody(tpl))
	id := created["id"].(string)

	resp, _ := c.do(http.MethodDelete, "/api/v1/report-schedules/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
	if resp, _ := c.do(http.MethodGet, "/api/v1/report-schedules/"+id, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", resp.StatusCode)
	}

	_, list := c.do(http.MethodGet, "/api/v1/report-schedules", nil)
	if data := list["data"].([]any); len(data) != 0 {
		t.Errorf("a deleted schedule is still listed")
	}
}

// The list filters by template, which is how the UI shows "what does this
// definition send, and to whom".
func TestScheduleListFiltersByTemplate(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	first := createTemplate(t, c, map[string]any{"name": "A", "type": "uptime", "formats": []string{"json"}})
	second := createTemplate(t, c, map[string]any{"name": "B", "type": "uptime", "formats": []string{"json"}})
	createSchedule(t, c, scheduleFixtureBody(first))
	createSchedule(t, c, scheduleFixtureBody(second))

	_, body := c.do(http.MethodGet, "/api/v1/report-schedules?report_template_id="+first, nil)
	if data := body["data"].([]any); len(data) != 1 {
		t.Errorf("template filter returned %d schedules, want 1", len(data))
	}
}

// problemMessages joins every validation message, so a test can assert on the
// reason without knowing which field carried it.
func problemMessages(body map[string]any) string {
	items, _ := body["errors"].([]any)
	var out []string
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			if msg, ok := item["message"].(string); ok {
				out = append(out, msg)
			}
		}
	}
	if detail, ok := body["detail"].(string); ok {
		out = append(out, detail)
	}
	return strings.Join(out, " | ")
}
