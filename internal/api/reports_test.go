package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/artifact"
	"github.com/webloomlabs/uptime-cairn/internal/report/runner"
)

// reportingClient is a set-up client against a server with the report worker
// pool actually running.
//
// A real pool rather than a stand-in, for the reason the harness gives about
// every other collaborator in this file: the interesting claim is that a queued
// run reaches an artifact somebody can download, and a stand-in would only prove
// the stand-in agrees with itself.
func reportingClient(t *testing.T) *client {
	t.Helper()
	c, _ := reportingClientWithFiles(t)
	return c
}

// reportingClientWithFiles also hands back the artifact directory, for the one
// test that needs to take a file away behind the server's back.
func reportingClientWithFiles(t *testing.T) (*client, *artifact.Store) {
	t.Helper()

	server, store, api := testAPI(t)
	files := artifact.New(t.TempDir(), artifact.DefaultMaxBytes)
	pool := runner.NewPool(
		runner.New(store, files, runner.Options{}),
		1, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pool.Start(t.Context())
	api.WithReporting(pool, files, time.UTC)

	c := newClient(t, server)
	c.setup()
	return c, files
}

func createTemplate(t *testing.T, c *client, body map[string]any) string {
	t.Helper()

	resp, out := c.do(http.MethodPost, "/api/v1/report-templates", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create template = %d, want 201 (%v)", resp.StatusCode, out)
	}
	return out["id"].(string)
}

// awaitRun polls until the run leaves the queue, which is what a client does
// with the 202 the spec returns.
func awaitRun(t *testing.T, c *client, id string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, body := c.do(http.MethodGet, "/api/v1/report-runs/"+id, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get run = %d (%v)", resp.StatusCode, body)
		}
		switch body["state"] {
		case "succeeded", "partial", "failed":
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never left the queue", id)
	return nil
}

// **The Month 4 checkpoint.** A computed SLA report as JSON, for an arbitrary
// past month, generated through the API and downloaded as a file.
//
// The scope here selects nothing, and that is the interesting half rather than a
// shortcut: a client whose monitors were all deleted still gets a report saying
// so, with the denominator stated and the figures null rather than zero. Zero
// uptime and unknown uptime are different claims and only one of them is an
// accusation.
func TestGenerateAndDownloadAnSLAReport(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	id := createTemplate(t, c, map[string]any{
		"name":       "Retainer SLA",
		"type":       "sla",
		"period":     "month",
		"sla_target": 99.9,
		"formats":    []string{"json"},
	})

	resp, run := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{
		"period_start": "2026-03-01T00:00:00Z",
		"period_end":   "2026-04-01T00:00:00Z",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("generate = %d, want 202 (%v)", resp.StatusCode, run)
	}
	if run["state"] != "queued" {
		t.Errorf("state = %v, want queued — the spec returns a run to poll, not a document", run["state"])
	}
	if run["period_start"] != "2026-03-01T00:00:00Z" {
		t.Errorf("period_start = %v, want the window that was asked for", run["period_start"])
	}

	finished := awaitRun(t, c, run["id"].(string))
	if finished["state"] != "succeeded" {
		t.Fatalf("run = %v (%v)", finished["state"], finished["error"])
	}

	artifacts := finished["artifacts"].([]any)
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(artifacts))
	}
	first := artifacts[0].(map[string]any)
	if first["format"] != "json" || first["state"] != "rendered" {
		t.Fatalf("artifact = %v", first)
	}
	// The digest is what makes an artifact evidence rather than a file.
	if sha, _ := first["sha256"].(string); len(sha) != 64 {
		t.Errorf("sha256 = %q, want 64 hex characters", sha)
	}
	if first["download_url"] == nil {
		t.Fatal("a rendered artifact was offered with no download link")
	}

	body, contentType := c.download(t, first["download_url"].(string))
	if contentType != "application/json" {
		t.Errorf("content type = %q", contentType)
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the downloaded artifact is not JSON: %v", err)
	}
	meta := doc["meta"].(map[string]any)
	if meta["period_start"] != "2026-03-01T00:00:00Z" {
		t.Errorf("meta.period_start = %v", meta["period_start"])
	}
	if meta["schema_version"] == nil {
		t.Error("no schema_version; a BI tool parsing this years from now cannot tell which shape it holds")
	}
	// **Honest nulls.** The scope selected nothing, so there is no estate summary
	// at all — `summary` is null rather than a block of zeroes. Zero uptime and
	// unknown uptime are different claims and only one of them is an accusation,
	// and a client whose monitors were all deleted gets a report saying so rather
	// than a failed run nobody looks at until the invoice goes out.
	if doc["summary"] != nil {
		t.Errorf("summary = %v, want null for a scope that selected nothing", doc["summary"])
	}
	if monitors, _ := doc["monitors"].([]any); len(monitors) != 0 {
		t.Errorf("monitors = %v, want none", monitors)
	}
	scope := doc["scope"].(map[string]any)
	if scope["monitor_count"].(float64) != 0 {
		t.Errorf("monitor_count = %v, want 0 — and stated rather than omitted", scope["monitor_count"])
	}
	// The resolution that actually answered is on the document, so a reader can
	// tell what grain the figures were read at.
	if meta["resolution"] == nil {
		t.Error("no resolution block; the reader cannot tell what grain answered")
	}
}

// The download is also addressable by format, which is the path the spec's
// /download?format= operation resolves.
func TestDownloadByFormat(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	id := createTemplate(t, c, map[string]any{
		"name": "Monthly", "type": "uptime", "formats": []string{"html", "csv"},
	})

	_, run := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{})
	finished := awaitRun(t, c, run["id"].(string))
	if finished["state"] != "succeeded" {
		t.Fatalf("run = %v (%v)", finished["state"], finished["error"])
	}

	runID := run["id"].(string)
	for format, wantType := range map[string]string{
		"html": "text/html; charset=utf-8",
		"csv":  "text/csv; charset=utf-8",
	} {
		body, contentType := c.download(t, "/api/v1/report-runs/"+runID+"/download?format="+format)
		if contentType != wantType {
			t.Errorf("%s content type = %q, want %q", format, contentType, wantType)
		}
		if len(body) == 0 {
			t.Errorf("%s artifact is empty", format)
		}
	}

	// A format this run did not produce is not found, rather than served empty.
	resp, _ := c.do(http.MethodGet, "/api/v1/report-runs/"+runID+"/download?format=pdf", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing format = %d, want 404", resp.StatusCode)
	}
}

// **A format that cannot be produced degrades the run rather than failing it**,
// and the API shows both halves: the run is partial, the failed artifact carries
// its reason, and asking for that format is a 409 rather than a 404 — the file
// was not written, and saying "no such thing" would hide why.
//
// The case is real on every build until a TrueType family is vendored, which is
// what makes it worth an end-to-end test rather than a unit one.
func TestAFailedFormatIsPartialAndExplained(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	id := createTemplate(t, c, map[string]any{
		"name": "Branded", "type": "uptime", "formats": []string{"pdf", "json"},
	})

	_, run := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{})
	finished := awaitRun(t, c, run["id"].(string))

	if finished["state"] != "partial" {
		t.Fatalf("state = %v, want partial (%v)", finished["state"], finished["error"])
	}
	if reason, _ := finished["error"].(string); reason == "" {
		t.Error("a partial run carries no reason")
	}

	var failed map[string]any
	for _, raw := range finished["artifacts"].([]any) {
		a := raw.(map[string]any)
		if a["state"] == "failed" {
			failed = a
		}
	}
	if failed == nil {
		t.Fatal("the failed format has no artifact row; the run's state is unexplained")
	}
	if failed["format"] != "pdf" || failed["error"] == nil {
		t.Errorf("failed artifact = %v", failed)
	}
	if failed["download_url"] != nil {
		t.Error("a failed artifact was offered a download link")
	}

	resp, body := c.do(http.MethodGet, "/api/v1/report-runs/"+run["id"].(string)+"/download?format=pdf", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("download of a failed format = %d, want 409 (%v)", resp.StatusCode, body)
	}
}

// A target of exactly 100 is refused with the reason, not merely rejected. Its
// error budget is zero seconds, which makes burn rate undefined and every report
// a breach report — and the API is the layer where that can be said.
func TestATargetOfExactlyOneHundredIsRefusedWithTheReason(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/report-templates", map[string]any{
		"name": "Impossible", "type": "sla", "sla_target": 100, "formats": []string{"json"},
	})

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, body)
	}
	errs, _ := body["errors"].([]any)
	if len(errs) == 0 {
		t.Fatalf("no validation items in %v", body)
	}
	item := errs[0].(map[string]any)
	if item["pointer"] != "/sla_target" {
		t.Errorf("pointer = %v, want /sla_target", item["pointer"])
	}
	if msg, _ := item["message"].(string); msg == "" || !strings.Contains(msg, "zero seconds") {
		t.Errorf("message = %q, want it to explain why", msg)
	}
}

// A template must name a format. One that names none would produce nothing and
// call it success.
func TestATemplateMustNameAFormat(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/report-templates", map[string]any{
		"name": "Formatless", "type": "uptime", "formats": []string{},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, body)
	}
}

// PATCH leaves absent fields alone and clears explicit nulls. Those are two
// different requests an operator makes on purpose, and a handler that cannot
// tell them apart leaves them unable to undo their own change.
func TestPatchDistinguishesAbsentFromNull(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	id := createTemplate(t, c, map[string]any{
		"name": "Retainer", "type": "sla", "sla_target": 99.9,
		"response_time_target_ms": 500, "formats": []string{"json"},
	})

	// Absent: the target survives a rename.
	resp, body := c.do(http.MethodPatch, "/api/v1/report-templates/"+id, map[string]any{"name": "Retainer, renamed"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d (%v)", resp.StatusCode, body)
	}
	if body["sla_target"] == nil {
		t.Error("an absent field cleared the stored value")
	}
	if body["name"] != "Retainer, renamed" {
		t.Errorf("name = %v", body["name"])
	}

	// Explicit null: the target goes.
	_, body = c.do(http.MethodPatch, "/api/v1/report-templates/"+id, map[string]any{"sla_target": nil})
	if body["sla_target"] != nil {
		t.Errorf("sla_target = %v, want null after an explicit null", body["sla_target"])
	}
	if body["response_time_target_ms"] == nil {
		t.Error("clearing one field cleared another")
	}
}

// **Deleting a template keeps the runs it produced.** The spec says so on the
// operation itself, and the store honours it with a soft delete rather than a
// cascade — a run is the record of what a client was sent.
func TestDeletingATemplateKeepsItsRuns(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	id := createTemplate(t, c, map[string]any{
		"name": "Monthly", "type": "uptime", "formats": []string{"json"},
	})

	_, run := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{})
	runID := run["id"].(string)
	awaitRun(t, c, runID)

	resp, _ := c.do(http.MethodDelete, "/api/v1/report-templates/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", resp.StatusCode)
	}
	if resp, _ := c.do(http.MethodGet, "/api/v1/report-templates/"+id, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", resp.StatusCode)
	}

	resp, kept := c.do(http.MethodGet, "/api/v1/report-runs/"+runID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the run went with its template: %d", resp.StatusCode)
	}
	if kept["report_template_id"] != id {
		t.Error("the surviving run no longer names the definition it was produced from")
	}

	// And it is still downloadable, which is the half that matters to whoever is
	// disputing a figure.
	artifacts := kept["artifacts"].([]any)
	if len(artifacts) == 0 {
		t.Fatal("the run's artifacts went with the template")
	}
	if body, _ := c.download(t, artifacts[0].(map[string]any)["download_url"].(string)); len(body) == 0 {
		t.Error("the artifact of a deleted template's run cannot be downloaded")
	}
}

// The run list filters by template and by state, the two parameters the spec
// gives it.
func TestRunListFilters(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	first := createTemplate(t, c, map[string]any{"name": "A", "type": "uptime", "formats": []string{"json"}})
	second := createTemplate(t, c, map[string]any{"name": "B", "type": "uptime", "formats": []string{"json"}})

	for _, id := range []string{first, first, second} {
		_, run := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{})
		awaitRun(t, c, run["id"].(string))
	}

	_, body := c.do(http.MethodGet, "/api/v1/report-runs?report_template_id="+first, nil)
	if data, _ := body["data"].([]any); len(data) != 2 {
		t.Errorf("template filter returned %d runs, want 2", len(data))
	}

	_, body = c.do(http.MethodGet, "/api/v1/report-runs?state=succeeded", nil)
	if data, _ := body["data"].([]any); len(data) != 3 {
		t.Errorf("state filter returned %d runs, want 3", len(data))
	}
	_, body = c.do(http.MethodGet, "/api/v1/report-runs?state=failed", nil)
	if data, _ := body["data"].([]any); len(data) != 0 {
		t.Errorf("state filter returned %d failed runs, want 0", len(data))
	}
}

// An artifact addressed under the wrong run is not found. The pair is the
// address, and honouring a mismatch would make the run id decorative on a path
// a share link resolves.
func TestAnArtifactUnderTheWrongRunIsNotFound(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	id := createTemplate(t, c, map[string]any{"name": "A", "type": "uptime", "formats": []string{"json"}})

	_, first := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{})
	firstRun := awaitRun(t, c, first["id"].(string))
	_, second := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{})
	awaitRun(t, c, second["id"].(string))

	artifactID := firstRun["artifacts"].([]any)[0].(map[string]any)["id"].(string)
	resp, _ := c.do(http.MethodGet,
		"/api/v1/report-runs/"+second["id"].(string)+"/artifacts/"+artifactID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-run artifact = %d, want 404", resp.StatusCode)
	}
}

// A malformed id in a scope is reported at the field that carries it, rather
// than as a generic 422 the caller has to guess at.
func TestABadScopeIdIsReportedAtItsPointer(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/report-templates", map[string]any{
		"name": "Scoped", "type": "uptime", "formats": []string{"json"},
		"scope": map[string]any{"monitor_ids": []string{"not-a-uuid"}},
	})

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, body)
	}
	errs := body["errors"].([]any)
	if errs[0].(map[string]any)["pointer"] != "/scope/monitor_ids/0" {
		t.Errorf("pointer = %v, want the element that was wrong", errs[0])
	}
}

// A window whose end precedes its start is refused rather than producing a
// report over negative time.
func TestAnInvertedWindowIsRefused(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	id := createTemplate(t, c, map[string]any{"name": "A", "type": "uptime", "formats": []string{"json"}})

	resp, body := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{
		"period_start": "2026-04-01T00:00:00Z",
		"period_end":   "2026-03-01T00:00:00Z",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, body)
	}
}

// Generating against an instance with no report worker says so rather than
// queueing a run into a void. A row stuck at `queued` forever reads as a hung
// report rather than as a missing feature.
func TestGeneratingWithNoWorkerIsReportedRatherThanQueued(t *testing.T) {
	t.Parallel()

	server, _, _ := testAPI(t)
	c := newClient(t, server)
	c.setup()

	id := createTemplate(t, c, map[string]any{"name": "A", "type": "uptime", "formats": []string{"json"}})
	resp, body := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (%v)", resp.StatusCode, body)
	}

	_, runs := c.do(http.MethodGet, "/api/v1/report-runs", nil)
	if data, _ := runs["data"].([]any); len(data) != 0 {
		t.Errorf("%d run(s) were recorded with nothing to execute them", len(data))
	}
}

// download fetches a binary body, which `do` cannot: it decodes JSON, and an
// artifact is a file rather than a response object.
func (c *client) download(t *testing.T, path string) ([]byte, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.authorise(req)

	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("download %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download %s = %d (%s)", path, resp.StatusCode, body)
	}
	return body, resp.Header.Get("Content-Type")
}

// **A row whose file is not on disk answers 410, not 500.**
//
// This test exists because a backup drill produced a 500. The state it covers is
// the one ADR-008's Consequences name — "a restore of the database against a
// stale reports directory yields rows whose files are missing" — and require to
// render "as a missing file rather than an error page". It is also the only state
// in which an operator has restored `cairn.db` without `<data-dir>/reports/`,
// which is the silent half of the backup procedure.
//
// 500 was wrong on two counts: the frozen spec declares 200, 401, 404 and 410 for
// this operation and no 500, and "Internal error, the cause has been logged"
// sends an operator to a log where naming the missing file sends them to their
// backup.
func TestAnArtifactWhoseFileIsGoneAnswersGoneNotInternalError(t *testing.T) {
	t.Parallel()

	c, files := reportingClientWithFiles(t)
	runID := sharedRun(t, c, "json")

	_, run := c.do(http.MethodGet, "/api/v1/report-runs/"+runID, nil)
	artifact := run["artifacts"].([]any)[0].(map[string]any)
	downloadURL := artifact["download_url"].(string)

	// It downloads before the file is removed, so the test cannot pass by the
	// artifact never having existed.
	if body, _ := c.download(t, downloadURL); len(body) == 0 {
		t.Fatal("the artifact did not download before its file was removed")
	}

	// The reports directory, emptied — the restore-without-artifacts case,
	// reproduced by taking the files away behind the server's back exactly as an
	// incomplete restore does.
	if err := os.RemoveAll(files.Root()); err != nil {
		t.Fatal(err)
	}

	resp, problem := c.do(http.MethodGet, downloadURL, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410 (%v)", resp.StatusCode, problem)
	}
	// The detail has to name the cause an operator can act on. A 410 that says
	// only "gone" is indistinguishable from retention, which is a different fact
	// and needs no action at all.
	detail, _ := problem["detail"].(string)
	if !strings.Contains(detail, "reports/") {
		t.Errorf("detail = %q, want it to name the reports directory", detail)
	}

	// The listing still loads, which is the half ADR-008 states outright: the
	// artifact list must render this rather than error on it.
	if resp, body := c.do(http.MethodGet, "/api/v1/report-runs?limit=50", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("the run listing = %d with files missing (%v)", resp.StatusCode, body)
	}
}

// **A rendered artifact whose file is gone is not offered for download.**
//
// The row still says `rendered` — the state enum is frozen and there is no fourth
// value for "the bytes are not here" — so the wire says it by withholding
// `download_url`. ADR-008's Consequences require the artifact list to render this
// as a missing file rather than as something to click, and a state-based check
// gets it exactly wrong: the row says rendered, so it would offer a link that
// answers 410.
func TestAMissingFileIsNotOfferedForDownload(t *testing.T) {
	t.Parallel()

	c, files := reportingClientWithFiles(t)
	runID := sharedRun(t, c, "json", "csv")

	// Offered while the files are there, so the test cannot pass by never having
	// offered anything.
	_, before := c.do(http.MethodGet, "/api/v1/report-runs/"+runID, nil)
	for _, raw := range before["artifacts"].([]any) {
		if a := raw.(map[string]any); a["download_url"] == nil {
			t.Fatalf("%v artifact was not offered before its file was removed", a["format"])
		}
	}

	if err := os.RemoveAll(files.Root()); err != nil {
		t.Fatal(err)
	}

	// The detail read, which is what the expanded row in the UI shows.
	_, after := c.do(http.MethodGet, "/api/v1/report-runs/"+runID, nil)
	for _, raw := range after["artifacts"].([]any) {
		a := raw.(map[string]any)
		if a["download_url"] != nil {
			t.Errorf("%v artifact is still offered for download with no file behind it", a["format"])
		}
		// The row keeps saying what it was. The digest and size survive the file,
		// which is what lets somebody find it in a backup.
		if a["state"] != "rendered" {
			t.Errorf("state = %v, want rendered — the row is a record and did not change", a["state"])
		}
		if a["sha256"] == nil || a["size_bytes"] == nil {
			t.Errorf("the digest or size was dropped along with the file: %v", a)
		}
	}

	// The listing has to agree with the detail. Two screens disagreeing about
	// whether a file exists is worse than either answer on its own.
	_, list := c.do(http.MethodGet, "/api/v1/report-runs?limit=50", nil)
	for _, rawRun := range list["data"].([]any) {
		for _, raw := range rawRun.(map[string]any)["artifacts"].([]any) {
			if a := raw.(map[string]any); a["download_url"] != nil {
				t.Errorf("the listing still offers %v with no file behind it", a["format"])
			}
		}
	}
}

// A shared report offers only the formats a client can actually fetch, and says
// so honestly when none of them can be.
func TestASharedReportDoesNotOfferMissingFormats(t *testing.T) {
	t.Parallel()

	c, files := reportingClientWithFiles(t)
	runID := sharedRun(t, c, "json")
	token, _ := share(t, c, runID, nil)

	if resp, _ := anonymous(t, c, "/api/v1/public/reports/"+token); resp.StatusCode != http.StatusOK {
		t.Fatal("the link did not resolve before the files were removed")
	}

	if err := os.RemoveAll(files.Root()); err != nil {
		t.Fatal(err)
	}

	resp, body := anonymous(t, c, "/api/v1/public/reports/"+token)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410 (%s)", resp.StatusCode, body)
	}
	// **It must not blame retention.** An earlier version asserted the retention
	// policy here, which would be a confident lie to a client whose report is
	// missing because somebody restored a backup without the reports directory.
	if strings.Contains(string(body), "retention") {
		t.Errorf("the 410 blames retention for a missing file: %s", body)
	}
}

// **`sections` is validated, which it was not until it did something.**
//
// The field was stored and round-tripped while nothing read it, so an unknown
// name was harmless. Now that it selects content, a typo is a block silently
// missing from every report the template produces — and the composer drops what
// it cannot recognise rather than failing a queued run, so nothing downstream
// would ever report it. This is the only place it can be caught while somebody
// is looking at the form.
func TestAnUnknownSectionIsRefused(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/report-templates", map[string]any{
		"name": "Custom", "type": "custom", "formats": []string{"json"},
		"sections": []string{"summary", "uptime_tables"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, body)
	}
	// The pointer names the offending element rather than the array, so a form
	// with ten chips can highlight the one that is wrong.
	if pointer := firstErrorPointer(body); pointer != "/sections/1" {
		t.Errorf("pointer = %q, want /sections/1", pointer)
	}
	// And the message lists the alternatives, because "invalid" on a closed
	// vocabulary is a trip to the specification.
	if !strings.Contains(problemMessages(body), "uptime_table") {
		t.Errorf("messages = %q, want the vocabulary listed", problemMessages(body))
	}
}

// A valid selection round-trips in the order it was given. Order is part of the
// contract — the spec calls them "ordered content blocks" — so a store that
// sorted or de-duplicated them would be changing what was asked for.
func TestSectionsRoundTripInOrder(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	want := []any{"response_time", "summary", "uptime_table"}

	resp, body := c.do(http.MethodPost, "/api/v1/report-templates", map[string]any{
		"name": "Custom", "type": "custom", "formats": []string{"json"},
		"sections": want,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%v)", resp.StatusCode, body)
	}
	got, _ := body["sections"].([]any)
	if len(got) != len(want) {
		t.Fatalf("sections = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sections[%d] = %v, want %v (order is part of the contract)", i, got[i], want[i])
		}
	}
}
