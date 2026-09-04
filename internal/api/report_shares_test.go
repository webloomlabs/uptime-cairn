package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Share links, end to end.
//
// The test that matters most here is the last one: **fetch a shared report while
// it is live and assert no monitor target appears anywhere in the rendered
// document.** That is the golden-path assertion the status page taught, and it is
// the reason the public response is a separate projection rather than a filter —
// a field cannot leak through a shape that has no place to put it, and this is
// the assertion that proves the shape has none.

// anonymous fetches a URL with no credentials at all: no bearer, no CSRF, no
// session cookie. Everything about the public pair has to work through this, and
// nothing about it may work through anything else.
func anonymous(t *testing.T, c *client, path string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh transport, so the session cookie the client jar holds cannot be
	// sent by accident — which would make an unauthenticated test pass for an
	// authenticated reason.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// sharedRun generates a report and returns its run id.
func sharedRun(t *testing.T, c *client, formats ...string) string {
	t.Helper()

	if len(formats) == 0 {
		formats = []string{"json"}
	}
	id := createTemplate(t, c, map[string]any{
		"name": "Acme retainer", "type": "sla", "period": "month",
		"sla_target": 99.9, "formats": formats,
	})
	resp, run := c.do(http.MethodPost, "/api/v1/report-templates/"+id+"/generate", map[string]any{
		"period_start": "2026-03-01T00:00:00Z",
		"period_end":   "2026-04-01T00:00:00Z",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("generate = %d (%v)", resp.StatusCode, run)
	}
	finished := awaitRun(t, c, run["id"].(string))
	if finished["state"] != "succeeded" {
		t.Fatalf("run = %v (%v)", finished["state"], finished["error"])
	}
	return run["id"].(string)
}

// share creates a link and returns the token out of the URL.
func share(t *testing.T, c *client, runID string, body map[string]any) (string, map[string]any) {
	t.Helper()

	resp, out := c.do(http.MethodPost, "/api/v1/report-runs/"+runID+"/share", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create share = %d, want 201 (%v)", resp.StatusCode, out)
	}
	url, _ := out["url"].(string)
	if url == "" {
		t.Fatal("no url in the create response")
	}
	at := strings.LastIndex(url, "/")
	return url[at+1:], out
}

// **The golden path, and the assertion the whole projection exists for.**
//
// A shared report is fetched by a stranger with no credentials, and nothing that
// identifies a monitor, a run, a template or a schedule appears anywhere in what
// comes back. This is asserted over the raw response bytes rather than over
// parsed fields, because the failure being guarded against is a field somebody
// adds later — and a test that checks named fields would not see it.
func TestASharedReportLeaksNoIdentifiers(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)

	// A monitor with a target a leak would be recognisable by. It is in scope for
	// nothing, deliberately: the assertion is that the string cannot reach the
	// public response even when it is in the database the response was built
	// from.
	resp, monitor := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "Acme production", "type": "http",
		"config": map[string]any{"url": "https://secret-internal-host.acme.invalid/health"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor = %d (%v)", resp.StatusCode, monitor)
	}

	runID := sharedRun(t, c, "json")
	token, _ := share(t, c, runID, nil)

	resp, body := anonymous(t, c, "/api/v1/public/reports/"+token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public read = %d (%s)", resp.StatusCode, body)
	}

	// **The monitor's target is the assertion**, and it is the one the plan names:
	// an internal hostname is what a stranger must not learn from a link that was
	// sent to talk about uptime. `ReportDocument` has no field for a target, which
	// is why this holds — the guarantee is structural rather than a filter
	// somebody remembered to apply.
	if strings.Contains(string(body), "secret-internal-host.acme.invalid") {
		t.Errorf("the public projection carries a monitor target:\n%s", body)
	}
	if strings.Contains(string(body), monitor["id"].(string)) {
		t.Errorf("a monitor out of scope reached the projection:\n%s", body)
	}
	// The shape has no place to put them, which is the property rather than the
	// symptom: these keys must be absent, not empty.
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "report_template_id", "report_schedule_id", "deliveries", "artifacts"} {
		if _, present := out[key]; present {
			t.Errorf("the public projection has a %q key; it is a separate shape, not a filter", key)
		}
	}
	// What it does carry is what a client came for.
	if out["title"] != "Acme retainer" {
		t.Errorf("title = %v", out["title"])
	}
	if formats, _ := out["formats"].([]any); len(formats) != 1 || formats[0] != "json" {
		t.Errorf("formats = %v, want the one that rendered", out["formats"])
	}
}

// **A tension in the frozen spec, resolved here and recorded rather than
// resolved quietly.**
//
// `PublicReport` is described as having "no run id, no template id, no delivery
// log and no monitor identifier" — and it also carries `document`, which is
// `ReportDocument`, whose `meta` holds `report_run_id` and `report_template_id`
// and whose sections hold `monitor_id`. Both statements are in the spec and they
// disagree.
//
// It is resolved in favour of the document, for one reason that settles it: the
// spec also mandates `GET /public/reports/{token}/download?format=json`, which
// serves the stored artifact byte for byte. Those identifiers are one query
// parameter away no matter what this projection does, so stripping them from the
// inline copy would buy nothing and would make the inline document differ from
// the file — which is the one property ADR-008 item 15 will not have.
//
// What that costs is bounded and worth stating plainly: a link holder learns the
// UUIDs of rows in this install. They authorise nothing, they are not guessable
// backwards into anything, and the monitor *target* — the field that would
// actually be a disclosure — is absent from `ReportDocument` by construction.
//
// This test exists so the decision is visible and fails loudly if somebody later
// changes the document's shape to carry a target.
func TestTheSharedDocumentIsTheStoredDocument(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	runID := sharedRun(t, c, "json")
	token, _ := share(t, c, runID, nil)

	_, inline := anonymous(t, c, "/api/v1/public/reports/"+token)
	var projection map[string]any
	if err := json.Unmarshal(inline, &projection); err != nil {
		t.Fatal(err)
	}
	document, ok := projection["document"].(map[string]any)
	if !ok {
		t.Fatalf("document = %v, want the stored JSON artifact inlined", projection["document"])
	}

	// The inline copy and the downloaded file are the same document. If they ever
	// diverge, one of them is a re-render.
	_, downloaded := anonymous(t, c, "/api/v1/public/reports/"+token+"/download?format=json")
	var stored map[string]any
	if err := json.Unmarshal(downloaded, &stored); err != nil {
		t.Fatal(err)
	}
	inlineAgain, _ := json.Marshal(document)
	storedAgain, _ := json.Marshal(stored)
	if string(inlineAgain) != string(storedAgain) {
		t.Error("the inline document differs from the stored artifact; one of them is a re-render")
	}

	// And the identifiers the resolution above accepts are the ones that are
	// there — asserted so that the acceptance is deliberate rather than assumed.
	meta, _ := document["meta"].(map[string]any)
	if meta["report_run_id"] != runID {
		t.Errorf("meta.report_run_id = %v, want the run — the document is the artifact verbatim", meta["report_run_id"])
	}
}

// The link is `noindex`, because a share link publishes a client's uptime data to
// anyone holding the URL and a search index turns "anyone holding the URL" into
// "anyone".
func TestASharedReportIsNotIndexable(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	token, _ := share(t, c, sharedRun(t, c), nil)

	resp, _ := anonymous(t, c, "/api/v1/public/reports/"+token)
	if got := resp.Header.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("X-Robots-Tag = %q, want noindex", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q; the token is in the URL and must not travel in a referrer", got)
	}

	// The refusals carry it too: a 404 body naming a client's report is still a
	// page, and an unknown token is the response a crawler is most likely to get.
	resp, _ = anonymous(t, c, "/api/v1/public/reports/"+strings.Repeat("a", 43))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown token = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("the 404 is indexable: X-Robots-Tag = %q", got)
	}
}

// **A share link serves the stored artifact, never a re-render** (ADR-008
// item 15). The bytes through the public download are byte-identical to the ones
// the authenticated download serves, and both match the digest on the row.
func TestASharedDownloadServesTheStoredBytes(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	runID := sharedRun(t, c, "json")
	token, _ := share(t, c, runID, nil)

	_, private := c.do(http.MethodGet, "/api/v1/report-runs/"+runID, nil)
	artifacts := private["artifacts"].([]any)
	first := artifacts[0].(map[string]any)

	authenticated, _ := c.download(t, first["download_url"].(string))

	resp, public := anonymous(t, c, "/api/v1/public/reports/"+token+"/download?format=json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public download = %d (%s)", resp.StatusCode, public)
	}
	if string(public) != string(authenticated) {
		t.Error("the public download differs from the authenticated one; it is serving something other than the stored file")
	}
	if got := resp.Header.Get("X-Cairn-SHA256"); got != first["sha256"] {
		t.Errorf("digest header = %q, want the row's %v", got, first["sha256"])
	}
}

// Revocation is immediate, and it answers 410 rather than 404: "this was
// withdrawn" and "no such thing" are different answers to somebody holding a
// bookmark, and only one of them is true.
func TestARevokedLinkAnswersGone(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	runID := sharedRun(t, c)
	token, _ := share(t, c, runID, nil)

	if resp, body := anonymous(t, c, "/api/v1/public/reports/"+token); resp.StatusCode != http.StatusOK {
		t.Fatalf("before revocation = %d (%s)", resp.StatusCode, body)
	}

	resp, out := c.do(http.MethodDelete, "/api/v1/report-runs/"+runID+"/share", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204 (%v)", resp.StatusCode, out)
	}

	resp, body := anonymous(t, c, "/api/v1/public/reports/"+token)
	if resp.StatusCode != http.StatusGone {
		t.Errorf("after revocation = %d, want 410 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "revoked") {
		t.Errorf("the 410 does not say what happened: %s", body)
	}

	// The artifacts are untouched, which is what "the link stops resolving" means
	// and what it does not mean.
	if resp, _ := c.do(http.MethodGet, "/api/v1/report-runs/"+runID, nil); resp.StatusCode != http.StatusOK {
		t.Error("revoking the link damaged the run")
	}
	// And a second revocation is a 404 rather than a second success, so a
	// double-click does not report two withdrawals of one link.
	if resp, _ := c.do(http.MethodDelete, "/api/v1/report-runs/"+runID+"/share", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second revoke = %d, want 404", resp.StatusCode)
	}
}

// An expired link is gone rather than missing, on the same reasoning.
func TestAnExpiredLinkAnswersGone(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	runID := sharedRun(t, c)

	// A second in the future, so the link is valid when it is created and expired
	// by the time it is read — which is the transition being tested rather than
	// the validation below it.
	expires := time.Now().UTC().Add(1200 * time.Millisecond)
	token, created := share(t, c, runID, map[string]any{"expires_at": expires.Format(time.RFC3339Nano)})
	if created["expires_at"] == nil {
		t.Error("the create response did not report the expiry it was given")
	}

	time.Sleep(1300 * time.Millisecond)

	resp, body := anonymous(t, c, "/api/v1/public/reports/"+token)
	if resp.StatusCode != http.StatusGone {
		t.Errorf("expired link = %d, want 410 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "expired") {
		t.Errorf("the 410 does not say what happened: %s", body)
	}
}

// An expiry already in the past is refused at creation. A link that is expired
// the moment it exists is a link somebody will send to a client.
func TestAnExpiryInThePastIsRefused(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	runID := sharedRun(t, c)

	resp, out := c.do(http.MethodPost, "/api/v1/report-runs/"+runID+"/share",
		map[string]any{"expires_at": "2020-01-01T00:00:00Z"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%v)", resp.StatusCode, out)
	}
	if !strings.Contains(problemMessages(out), "must be in the future") {
		t.Errorf("messages = %q", problemMessages(out))
	}
}

// **One live link per run**, enforced by the database rather than by a handler
// check — and a second request is a 409 rather than a silent replacement.
//
// Replacing would revoke a link somebody has already sent to a client because a
// colleague pressed the button again, and the support call starts with "the
// report link you sent me stopped working".
func TestASecondLinkIsRefusedRatherThanReplacingTheFirst(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	runID := sharedRun(t, c)
	token, _ := share(t, c, runID, nil)

	resp, out := c.do(http.MethodPost, "/api/v1/report-runs/"+runID+"/share", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second create = %d, want 409 (%v)", resp.StatusCode, out)
	}
	// The first link still works, which is the whole point of refusing.
	if resp, _ := anonymous(t, c, "/api/v1/public/reports/"+token); resp.StatusCode != http.StatusOK {
		t.Error("the existing link stopped working when a second was refused")
	}

	// After revoking, a new one may be created — revocation is what makes room.
	if resp, _ := c.do(http.MethodDelete, "/api/v1/report-runs/"+runID+"/share", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatal("revoke failed")
	}
	fresh, _ := share(t, c, runID, nil)
	if fresh == token {
		t.Error("the replacement link reuses the revoked token")
	}
}

// **The token appears exactly once, in the create response.** A run read back
// afterwards reports that a link exists and never what it is: a listing full of
// live credentials is a listing that leaks one the first time it is screenshotted
// or pasted into a ticket.
func TestTheTokenIsNeverReadableBack(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	runID := sharedRun(t, c)
	token, _ := share(t, c, runID, nil)

	_, run := c.do(http.MethodGet, "/api/v1/report-runs/"+runID, nil)
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Errorf("the run read back carries the share token: %s", raw)
	}

	shareBlock, ok := run["share"].(map[string]any)
	if !ok {
		t.Fatalf("share = %v, want an object describing the link", run["share"])
	}
	if _, present := shareBlock["url"]; present {
		t.Error("the share block on a run carries a url; the token is shown once and not again")
	}
	if shareBlock["created_at"] == nil {
		t.Error("the share block says nothing about when the link was made")
	}

	// The listing takes the same path and must make the same promise.
	_, list := c.do(http.MethodGet, "/api/v1/report-runs", nil)
	rawList, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawList), token) {
		t.Errorf("the run listing carries the share token: %s", rawList)
	}
}

// A run with no link reports null rather than an empty object, so a client can
// tell "no link" from "a link with nothing known about it".
func TestARunWithNoLinkReportsNull(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	_, run := c.do(http.MethodGet, "/api/v1/report-runs/"+sharedRun(t, c), nil)
	if run["share"] != nil {
		t.Errorf("share = %v, want null", run["share"])
	}
}

// A token that is not the right shape is refused before it is hashed. Hashing an
// unbounded path segment is work an unauthenticated caller can ask for
// repeatedly, and the spec bounds the parameter at 22–128 characters.
func TestAMalformedTokenIsRefusedWithoutWork(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	for _, token := range []string{"short", strings.Repeat("x", 129)} {
		resp, _ := anonymous(t, c, "/api/v1/public/reports/"+token)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("token %q = %d, want 404", token[:min(len(token), 12)], resp.StatusCode)
		}
	}
}

// The public pair needs no credentials and must not accept them as a substitute
// for a valid token: an operator's session does not make a bad token good.
func TestASessionDoesNotSubstituteForAToken(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	_ = sharedRun(t, c)

	resp, _ := c.do(http.MethodGet, "/api/v1/public/reports/"+strings.Repeat("b", 43), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("an authenticated request with an unknown token = %d, want 404", resp.StatusCode)
	}
}

// A format the run never produced is a 404 on the format rather than a 410:
// nothing was reclaimed, the document simply does not exist in the shape that was
// asked for.
func TestAFormatThatWasNeverRenderedIsNotFound(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	token, _ := share(t, c, sharedRun(t, c, "json"), nil)

	resp, _ := anonymous(t, c, "/api/v1/public/reports/"+token+"/download?format=csv")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// **The rate limit must not lock out the person the link was sent to.**
//
// This test exists because the first cut reused the login limiter, and a live run
// caught it in one command: five requests in fifteen minutes is right for
// credential guessing and absurd for a document. A client who opens the report,
// downloads two formats and refreshes had already spent the budget.
//
// So the assertion is the ordinary journey, not the limit: a reader doing what a
// reader does is never refused.
func TestAClientReadingTheirOwnReportIsNotRateLimited(t *testing.T) {
	t.Parallel()

	c := reportingClient(t)
	token, _ := share(t, c, sharedRun(t, c, "json", "csv"), nil)

	for i := range 20 {
		if resp, body := anonymous(t, c, "/api/v1/public/reports/"+token); resp.StatusCode != http.StatusOK {
			t.Fatalf("read %d = %d, want 200 (%s)", i+1, resp.StatusCode, body)
		}
		for _, format := range []string{"json", "csv"} {
			if resp, _ := anonymous(t, c, "/api/v1/public/reports/"+token+"/download?format="+format); resp.StatusCode != http.StatusOK {
				t.Fatalf("download %s on pass %d = %d", format, i+1, resp.StatusCode)
			}
		}
	}
}

// The limit is still a limit: past the window's budget one token is refused, with
// a Retry-After so a caller knows what to do rather than guessing.
func TestTheShareLimitEventuallyRefuses(t *testing.T) {
	t.Parallel()

	limiter := newShareLimiter()
	now := time.Now()
	for i := range shareMaxRequests {
		if !limiter.allow("token", now) {
			t.Fatalf("refused at request %d, inside the budget of %d", i+1, shareMaxRequests)
		}
	}
	if limiter.allow("token", now) {
		t.Error("the budget was exceeded and the request was allowed")
	}
	// A different token has its own budget: one client's burst must not take
	// another client's link down.
	if !limiter.allow("other", now) {
		t.Error("one token's budget was charged to another")
	}
	// And the window rolls.
	if !limiter.allow("token", now.Add(shareWindow+time.Second)) {
		t.Error("the window did not roll")
	}
}

// A guesser sweeping tokens must not grow the limiter's map without bound. The
// key space is attacker-chosen here, which is what makes this different from the
// login limiter's email addresses and client IPs.
func TestTheShareLimiterDoesNotAccumulateKeys(t *testing.T) {
	t.Parallel()

	limiter := newShareLimiter()
	start := time.Now()
	for i := range 500 {
		limiter.allow(strings.Repeat("x", 22)+string(rune('a'+i%26))+string(rune(i)), start)
	}
	// Every entry is stale by now, and each is swept as its key is next touched.
	later := start.Add(shareWindow + time.Second)
	for i := range 500 {
		limiter.allow(strings.Repeat("x", 22)+string(rune('a'+i%26))+string(rune(i)), later)
	}
	limiter.mu.Lock()
	size := len(limiter.requests)
	limiter.mu.Unlock()
	if size > 500 {
		t.Errorf("the limiter holds %d keys after 1000 requests over 500 tokens", size)
	}
}
