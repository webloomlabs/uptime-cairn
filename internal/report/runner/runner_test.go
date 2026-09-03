package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/artifact"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

var (
	now     = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	march   = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	aprilOn = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

// fakeStore is the store seam, in memory. A fake rather than the real SQLite
// one, and deliberately: these tests are about what the runner does when a
// renderer fails or a disk is full, and driving that through a database would
// make every failure ambiguous between the two layers.
type fakeStore struct {
	// The pool test shares one of these across goroutines, so the fields the
	// runner writes are guarded. A fake that races is a fake that fails -race
	// for a reason that has nothing to do with the code under test.
	mu sync.Mutex

	template model.ReportTemplate
	brand    model.BrandProfile
	logo     []byte

	// settings is the instance row the runner reads per run. brandErr makes the
	// profile lookup fail, which is the path an install with no brand profile at
	// all takes and the one the appearance fallback exists for.
	settings model.Settings
	brandErr error

	monitors []model.Monitor
	daily    map[model.ID][]store.HistoryBucket
	totals   map[model.ID]store.HistoryBucket

	startErr error

	started   bool
	finished  string
	failure   string
	artifacts []model.ReportArtifact
}

func (f *fakeStore) StartReportRun(context.Context, model.ID, time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	return nil
}

func (f *fakeStore) FinishReportRun(_ context.Context, _ model.ID, state, failure string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished, f.failure = state, failure
	return nil
}

func (f *fakeStore) CreateReportArtifact(_ context.Context, a model.ReportArtifact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.artifacts = append(f.artifacts, a)
	return nil
}

// outcome is how a test reads what happened, under the same lock the runner
// wrote it with.
func (f *fakeStore) outcome() (state, failure string, artifacts []model.ReportArtifact) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished, f.failure, append([]model.ReportArtifact(nil), f.artifacts...)
}

func (f *fakeStore) ReportTemplateForRun(context.Context, model.ID) (model.ReportTemplate, error) {
	return f.template, nil
}

func (f *fakeStore) GetBrandProfile(context.Context, model.ID) (model.BrandProfile, error) {
	return f.brand, f.brandErr
}

func (f *fakeStore) DefaultBrandProfile(context.Context) (model.BrandProfile, error) {
	return f.brand, f.brandErr
}

func (f *fakeStore) GetSettings(context.Context, model.ID) (model.Settings, error) {
	return f.settings, nil
}

func (f *fakeStore) BrandLogo(context.Context, model.ID) ([]byte, string, error) {
	return f.logo, model.LogoPNG, nil
}

func (f *fakeStore) MonitorsInScope(context.Context, report.Scope) ([]model.Monitor, error) {
	return f.monitors, nil
}

func (f *fakeStore) WindowTotals(context.Context, []model.ID, time.Time, time.Time, string) (map[model.ID]store.HistoryBucket, error) {
	return f.totals, nil
}

func (f *fakeStore) DailySeries(context.Context, []model.ID, time.Time, time.Time) (map[model.ID][]store.HistoryBucket, error) {
	return f.daily, nil
}

func (f *fakeStore) RawCovers(context.Context, model.ID, time.Time, string) (bool, error) {
	return false, nil
}

func (f *fakeStore) UptimeFromRaw(context.Context, model.ID, time.Time, time.Time) (store.HistoryBucket, error) {
	return store.HistoryBucket{}, nil
}

func (f *fakeStore) SLOTargets(context.Context, []model.ID) (map[model.ID]report.Target, error) {
	return map[model.ID]report.Target{}, nil
}

func (f *fakeStore) ListIncidents(context.Context, *store.Cursor, int, store.IncidentFilter) ([]model.Incident, bool, error) {
	return nil, false, nil
}

// fakeFiles records what was written and can be made to fail, which is how the
// full-disk path gets demonstrated rather than reasoned about.
type fakeFiles struct {
	mu      sync.Mutex
	written map[string][]byte
	failOn  map[string]error
}

func newFiles() *fakeFiles {
	return &fakeFiles{written: map[string][]byte{}, failOn: map[string]error{}}
}

func (f *fakeFiles) Write(id model.ID, format string, when time.Time, data []byte) (artifact.Written, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[format]; err != nil {
		return artifact.Written{}, err
	}
	path := artifact.RelPath(id, format, when)
	f.written[format] = data
	return artifact.Written{Path: path, SizeBytes: int64(len(data)), SHA256: "digest"}, nil
}

func (f *fakeFiles) bytes(format string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.written[format]
	return data, ok
}

func fixture(formats ...string) (*fakeStore, model.ReportRun) {
	monitor := model.Monitor{ID: model.NewID(), Name: "checkout", Type: "http"}
	return &fakeStore{
			template: model.ReportTemplate{
				ID: model.NewID(), Name: "Monthly", Type: model.ReportTypeSLA,
				Period: model.ReportPeriodMonth, PeriodStyle: model.ReportStyleCalendar,
				MaintenanceHandling: "exclude", Formats: formats,
			},
			brand:    model.BrandProfile{ID: model.NewID(), CompanyName: "Smith & Co"},
			monitors: []model.Monitor{monitor},
			totals: map[model.ID]store.HistoryBucket{
				monitor.ID: {Start: march, Up: 1900, Down: 100, ResponseTimeSum: 250000, ResponseTimeCount: 1900},
			},
			daily: map[model.ID][]store.HistoryBucket{
				monitor.ID: {
					{Start: march, Up: 950, Down: 50, ResponseTimeSum: 100000, ResponseTimeCount: 950},
					{Start: march.AddDate(0, 0, 1), Up: 950, Down: 50, ResponseTimeSum: 150000, ResponseTimeCount: 950},
				},
			},
		}, model.ReportRun{
			ID: model.NewID(), OrgID: model.SentinelOrgID,
			State:       model.RunQueued,
			PeriodStart: march, PeriodEnd: aprilOn, Timezone: "UTC",
			CreatedAt: now,
		}
}

// The ordinary path: everything requested renders, everything is stored, the run
// succeeds.
func TestASuccessfulRunStoresEveryFormat(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON, model.FormatCSV, model.FormatHTML)
	run.ReportTemplateID = s.template.ID
	files := newFiles()

	if err := New(s, files, Options{}).Execute(t.Context(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	state, failure, artifacts := s.outcome()
	if state != model.RunSucceeded {
		t.Errorf("state = %q (%s), want succeeded", state, failure)
	}
	if len(artifacts) != 3 {
		t.Fatalf("artifacts = %d, want 3", len(artifacts))
	}
	for _, a := range artifacts {
		if a.State != model.ArtifactRendered {
			t.Errorf("%s artifact state = %q", a.Format, a.State)
		}
		if a.Path == "" || a.SHA256 == "" || a.SizeBytes == 0 {
			t.Errorf("%s artifact is missing its path, digest or size: %+v", a.Format, a)
		}
	}
	html, _ := files.bytes(model.FormatHTML)
	if !strings.Contains(string(html), "Smith &amp; Co") {
		t.Error("the brand did not reach the rendered HTML")
	}
}

// **ADR-007 item 7, demonstrated.** One format that cannot be produced degrades
// the run to partial and delivers the rest. The case is not hypothetical: it is
// what happens on every build until a TrueType family is vendored, which is
// exactly why the failure path had to exist before the font did.
func TestAFormatThatCannotRenderDegradesTheRunToPartial(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatPDF, model.FormatHTML, model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	files := newFiles()

	// Options carries no font family, which is a supported configuration.
	if err := New(s, files, Options{}).Execute(t.Context(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	state, failure, artifacts := s.outcome()
	if state != model.RunPartial {
		t.Fatalf("state = %q, want partial", state)
	}
	if !strings.Contains(failure, "pdf") || !strings.Contains(failure, "font") {
		t.Errorf("run error = %q, want it to name the format and the reason", failure)
	}

	var pdf, ok int
	for _, a := range artifacts {
		switch a.State {
		case model.ArtifactFailed:
			pdf++
			if a.Format != model.FormatPDF {
				t.Errorf("the wrong format failed: %s", a.Format)
			}
			if a.Error == "" {
				t.Error("the failed artifact carries no reason")
			}
			if a.Path != "" {
				t.Error("a failed artifact has a path; nothing was written")
			}
		case model.ArtifactRendered:
			ok++
		}
	}
	if pdf != 1 || ok != 2 {
		t.Errorf("artifacts: %d failed, %d rendered; want 1 and 2", pdf, ok)
	}
	if _, wrote := files.bytes(model.FormatPDF); wrote {
		t.Error("a failed format still wrote bytes to disk")
	}
}

// A full disk degrades the run and records the reason, rather than aborting.
// Storage failure and render failure take the same path on purpose: from the
// client's side they are the same event — one of the formats did not arrive.
func TestAWriteFailureDegradesTheRunWithItsReason(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON, model.FormatCSV)
	run.ReportTemplateID = s.template.ID
	files := newFiles()
	files.failOn[model.FormatCSV] = errors.New("write artifact: no space left on device")

	if err := New(s, files, Options{}).Execute(t.Context(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	state, failure, _ := s.outcome()
	if state != model.RunPartial {
		t.Errorf("state = %q, want partial", state)
	}
	if !strings.Contains(failure, "no space left") {
		t.Errorf("run error = %q, want the real cause", failure)
	}
	if _, ok := files.bytes(model.FormatJSON); !ok {
		t.Error("the format that could be written was not delivered")
	}
}

// Every format failing is a failed run, not a partial one. Partial means "some
// of it arrived", and saying that when nothing did is the misreading the state
// exists to prevent.
func TestEveryFormatFailingIsAFailedRun(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatPDF)
	run.ReportTemplateID = s.template.ID

	if err := New(s, newFiles(), Options{}).Execute(t.Context(), run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if state, _, _ := s.outcome(); state != model.RunFailed {
		t.Errorf("state = %q, want failed", state)
	}
}

// **The run's recorded window wins over the template's period.** That is what
// makes re-running a first-class action: a report regenerated after a correction
// covers the same window as the one it replaces, rather than whatever "last
// month" resolves to on the day somebody presses the button.
func TestTheRunsWindowWinsOverTheTemplatesPeriod(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	// A window from a year ago, re-run today. The template says "this month".
	run.PeriodStart = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	run.PeriodEnd = time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)

	files := newFiles()
	if err := New(s, files, Options{}).Execute(t.Context(), run, now); err != nil {
		t.Fatal(err)
	}

	raw, _ := files.bytes(model.FormatJSON)
	body := string(raw)
	if !strings.Contains(body, "2025-06-01") {
		t.Errorf("the document does not cover the run's own window:\n%s", firstLines(body, 20))
	}
	if strings.Contains(body, "2026-04-01T00:00:00Z\",\n    \"period_end") {
		t.Error("the template's period overrode the run's recorded window")
	}
}

// The artifact is filed under the month it reports on, not the month it was
// generated in. A March report regenerated in December belongs under March, so
// that a backup restored by month contains what its name says.
func TestArtifactsAreFiledUnderTheReportedMonth(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID

	if err := New(s, newFiles(), Options{}).Execute(t.Context(), run, time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	_, _, artifacts := s.outcome()
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %d", len(artifacts))
	}
	if !strings.HasPrefix(artifacts[0].Path, "2026/04/") {
		t.Errorf("path = %q, want it filed under the reported period", artifacts[0].Path)
	}
}

// Retention of zero keeps an artifact indefinitely, which is the convention that
// settings section already uses. A zero read as "expires at the epoch" would
// reclaim the whole estate on the first sweep.
func TestZeroRetentionMeansNoExpiry(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	if err := New(s, newFiles(), Options{ArtifactDays: 0}).Execute(t.Context(), run, now); err != nil {
		t.Fatal(err)
	}
	if _, _, artifacts := s.outcome(); artifacts[0].ExpiresAt != nil {
		t.Errorf("expiry = %v, want none", artifacts[0].ExpiresAt)
	}

	s2, run2 := fixture(model.FormatJSON)
	run2.ReportTemplateID = s2.template.ID
	if err := New(s2, newFiles(), Options{ArtifactDays: 365}).Execute(t.Context(), run2, now); err != nil {
		t.Fatal(err)
	}
	_, _, kept := s2.outcome()
	if kept[0].ExpiresAt == nil || !kept[0].ExpiresAt.Equal(now.AddDate(0, 0, 365)) {
		t.Errorf("expiry = %v, want a year out", kept[0].ExpiresAt)
	}
}

// A run another worker already has is not this worker's to execute, and it is
// not a failure either. That distinction is what lets the pool be bounded
// without a lock.
func TestARunAnotherWorkerHasIsLeftAlone(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	s.startErr = errors.New("conflict")

	err := New(s, newFiles(), Options{}).Execute(t.Context(), run, now)
	if err == nil {
		t.Fatal("execute reported success for a run it did not claim")
	}
	state, _, artifacts := s.outcome()
	if state != "" {
		t.Errorf("a run this worker did not claim was finished as %q", state)
	}
	if len(artifacts) != 0 {
		t.Error("a run this worker did not claim produced artifacts")
	}
}

// A template requesting no formats fails with a reason rather than succeeding
// having produced nothing. The API refuses an empty list; this is the row that
// predates the rule or was written by hand.
func TestATemplateWithNoFormatsFailsRatherThanSucceedingEmpty(t *testing.T) {
	t.Parallel()

	s, run := fixture()
	run.ReportTemplateID = s.template.ID

	if err := New(s, newFiles(), Options{}).Execute(t.Context(), run, now); err != nil {
		t.Fatal(err)
	}
	state, failure, _ := s.outcome()
	if state != model.RunFailed {
		t.Errorf("state = %q, want failed", state)
	}
	if !strings.Contains(failure, "formats") {
		t.Errorf("reason = %q, want it to name the problem", failure)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// --- the pool ---------------------------------------------------------------

// A full queue is refused rather than grown. An unbounded backlog turns a bad
// morning into memory pressure and then into an OOM kill, which takes the
// monitoring down with the reporting — and the monitoring is the part somebody
// is paying for.
func TestAFullQueueIsRefusedRatherThanGrown(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	pool := NewPool(New(s, newFiles(), Options{}), 1, 2, discardLog())

	// Not started, so nothing drains: two fit, the third is refused.
	if err := pool.Submit(run); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if err := pool.Submit(run); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if err := pool.Submit(run); !errors.Is(err, ErrBusy) {
		t.Errorf("third submit: %v, want ErrBusy", err)
	}
	if pool.Depth() != 2 {
		t.Errorf("depth = %d, want 2", pool.Depth())
	}
}

// Submitting does not block, which is what makes the 202 the spec asks for
// honest: the handler queues and answers, rather than rendering while the client
// waits.
func TestSubmitDoesNotBlock(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	pool := NewPool(New(s, newFiles(), Options{}), 1, 1, discardLog())

	done := make(chan struct{})
	go func() {
		_ = pool.Submit(run)
		_ = pool.Submit(run) // refused, and must not wait for a worker
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked; a request handler would have been held open")
	}
}

// The pool actually runs what it is given, and stops when its context does.
func TestThePoolExecutesAndStops(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON)
	run.ReportTemplateID = s.template.ID
	files := newFiles()

	ctx, cancel := context.WithCancel(t.Context())
	pool := NewPool(New(s, files, Options{}), 2, 4, discardLog())
	pool.Start(ctx)

	if err := pool.Submit(run); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		if state, _, _ = s.outcome(); state != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if state != model.RunSucceeded {
		t.Fatalf("run finished as %q, want succeeded", state)
	}

	cancel()
	stopped := make(chan struct{})
	go func() { pool.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("workers did not stop on cancellation; shutdown would hang")
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
