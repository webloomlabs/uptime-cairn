package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

var runNow = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

func liveTemplate(t *testing.T, s *Store) model.ReportTemplate {
	t.Helper()
	tpl := testTemplate("Monthly")
	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatalf("create template: %v", err)
	}
	return tpl
}

func queuedRun(t *testing.T, s *Store, templateID model.ID) model.ReportRun {
	t.Helper()
	run := model.ReportRun{
		ID:               model.NewID(),
		OrgID:            model.SentinelOrgID,
		ReportTemplateID: templateID,
		State:            model.RunQueued,
		PeriodStart:      runNow.AddDate(0, -1, 0),
		PeriodEnd:        runNow,
		Timezone:         "Australia/Sydney",
		CreatedAt:        runNow,
	}
	if err := s.CreateReportRun(t.Context(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

func artifact(runID model.ID, format, state string) model.ReportArtifact {
	return model.ReportArtifact{
		ID:          model.NewID(),
		OrgID:       model.SentinelOrgID,
		ReportRunID: runID,
		Format:      format,
		State:       state,
		Path:        "2026/04/" + format + ".out",
		SizeBytes:   1024,
		SHA256:      "abc123",
		CreatedAt:   runNow,
	}
}

// The zone the boundaries were cut in is recorded, not assumed. It is the
// difference between a month and a month minus a working day, and there is no
// way to recover it after the fact.
func TestARunRecordsTheZoneItsWindowWasCutIn(t *testing.T) {
	t.Parallel()

	s := open(t)
	run := queuedRun(t, s, liveTemplate(t, s).ID)

	got, err := s.GetReportRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Timezone != "Australia/Sydney" {
		t.Errorf("timezone = %q, want the one the window was cut in", got.Timezone)
	}
	if !got.PeriodStart.Equal(run.PeriodStart) || !got.PeriodEnd.Equal(run.PeriodEnd) {
		t.Errorf("period = %s–%s, want %s–%s", got.PeriodStart, got.PeriodEnd, run.PeriodStart, run.PeriodEnd)
	}
	if got.State != model.RunQueued {
		t.Errorf("state = %q, want queued", got.State)
	}
}

// **The property a bounded worker pool rests on.** Starting a run is conditional
// on it still being queued, so two workers that pick up the same run cannot both
// render it: one updates a row, the other gets ErrConflict and moves on. Without
// this the pool needs a lock, and a lock is a thing to get wrong at 09:00 on the
// first of the month.
func TestOnlyOneWorkerCanStartARun(t *testing.T) {
	t.Parallel()

	s := open(t)
	run := queuedRun(t, s, liveTemplate(t, s).ID)

	if err := s.StartReportRun(t.Context(), run.ID, runNow); err != nil {
		t.Fatalf("first worker: %v", err)
	}
	if err := s.StartReportRun(t.Context(), run.ID, runNow); !errors.Is(err, ErrConflict) {
		t.Errorf("second worker: %v, want ErrConflict", err)
	}

	got, err := s.GetReportRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.RunRunning || got.StartedAt == nil {
		t.Errorf("run = %q started %v, want running with a start time", got.State, got.StartedAt)
	}
}

// partial is a real terminal state and survives the round trip with its reason.
// Collapsing it into succeeded or failed is how somebody concludes a delivery
// went out whole when one format of it did not.
func TestPartialIsATerminalStateWithAReason(t *testing.T) {
	t.Parallel()

	s := open(t)
	run := queuedRun(t, s, liveTemplate(t, s).ID)

	if err := s.StartReportRun(t.Context(), run.ID, runNow); err != nil {
		t.Fatal(err)
	}
	reason := "pdf: no embedded font family"
	if err := s.FinishReportRun(t.Context(), run.ID, model.RunPartial, reason, runNow.Add(time.Minute)); err != nil {
		t.Fatalf("finish: %v", err)
	}

	got, err := s.GetReportRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.RunPartial {
		t.Errorf("state = %q, want partial", got.State)
	}
	if got.Error != reason {
		t.Errorf("error = %q, want %q", got.Error, reason)
	}
	if got.FinishedAt == nil {
		t.Error("a finished run has no finish time")
	}
}

// One artifact per run per format, enforced by the database rather than by
// whichever code path remembered. A second PDF for one run is a re-render that
// escaped, and two rows would make the download path pick one arbitrarily.
func TestASecondArtifactInOneFormatIsRefused(t *testing.T) {
	t.Parallel()

	s := open(t)
	run := queuedRun(t, s, liveTemplate(t, s).ID)

	if err := s.CreateReportArtifact(t.Context(), artifact(run.ID, model.FormatPDF, model.ArtifactRendered)); err != nil {
		t.Fatal(err)
	}
	err := s.CreateReportArtifact(t.Context(), artifact(run.ID, model.FormatPDF, model.ArtifactRendered))
	if !errors.Is(err, ErrConflict) {
		t.Errorf("second pdf: %v, want ErrConflict", err)
	}
}

// A failed format is listed beside the ones that rendered. It is why the run is
// partial, and hiding it leaves the run's own state unexplained on the page that
// shows it.
func TestAFailedFormatIsListedWithTheRest(t *testing.T) {
	t.Parallel()

	s := open(t)
	run := queuedRun(t, s, liveTemplate(t, s).ID)

	failed := artifact(run.ID, model.FormatPDF, model.ArtifactFailed)
	failed.Path, failed.SizeBytes, failed.SHA256 = "", 0, ""
	failed.Error = "no embedded font family"
	if err := s.CreateReportArtifact(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateReportArtifact(t.Context(), artifact(run.ID, model.FormatHTML, model.ArtifactRendered)); err != nil {
		t.Fatal(err)
	}

	got, err := s.ArtifactsForRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("artifacts = %d, want 2 including the failure", len(got))
	}

	var sawFailure bool
	for _, a := range got {
		if a.State == model.ArtifactFailed {
			sawFailure = true
			if a.Error == "" {
				t.Error("a failed artifact carries no reason")
			}
			if a.Path != "" {
				t.Error("a failed artifact has a path; nothing was written")
			}
		}
	}
	if !sawFailure {
		t.Error("the failed format is missing from the run's artifacts")
	}
}

// A page of runs costs one query for its artifacts, not one per run. Fifty round
// trips is what an operator pays to see what went out this morning otherwise.
func TestArtifactsForAPageOfRunsComeBackTogether(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveTemplate(t, s)

	var ids []model.ID
	for range 3 {
		run := queuedRun(t, s, tpl.ID)
		ids = append(ids, run.ID)
		if err := s.CreateReportArtifact(t.Context(), artifact(run.ID, model.FormatJSON, model.ArtifactRendered)); err != nil {
			t.Fatal(err)
		}
	}

	byRun, err := s.ArtifactsForRuns(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if len(byRun[id]) != 1 {
			t.Errorf("run %s got %d artifacts, want 1", id, len(byRun[id]))
		}
	}
}

// Expiry is a tombstone, not a delete. The row stays so a bookmarked link
// answers "this existed and is gone" with a 410 rather than "no such thing" with
// a 404 — different facts, and a client chasing a missing report deserves the
// first. The path goes with the bytes, because a path that no longer resolves
// invites the next reader to go looking.
func TestExpiryLeavesATombstoneRatherThanDeletingTheRow(t *testing.T) {
	t.Parallel()

	s := open(t)
	run := queuedRun(t, s, liveTemplate(t, s).ID)

	a := artifact(run.ID, model.FormatPDF, model.ArtifactRendered)
	expires := runNow.Add(-time.Hour)
	a.ExpiresAt = &expires
	if err := s.CreateReportArtifact(t.Context(), a); err != nil {
		t.Fatal(err)
	}

	due, err := s.ExpirableArtifacts(t.Context(), runNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != a.ID {
		t.Fatalf("expirable = %d rows, want the one past its expiry", len(due))
	}

	if err := s.MarkArtifactExpired(t.Context(), a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetReportArtifact(t.Context(), a.ID)
	if err != nil {
		t.Fatalf("the row was deleted rather than tombstoned: %v", err)
	}
	if got.State != model.ArtifactExpired {
		t.Errorf("state = %q, want expired", got.State)
	}
	if got.Path != "" || got.SizeBytes != 0 || got.SHA256 != "" {
		t.Errorf("a tombstone still points at bytes that are gone: %+v", got)
	}

	// And it is not offered to the sweeper a second time.
	again, err := s.ExpirableArtifacts(t.Context(), runNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("an already-expired artifact is still expirable")
	}
}

// An artifact kept indefinitely — which is what report_artifact_days = 0 selects
// — is never swept. A null expiry that read as "expired at the epoch" would
// reclaim the whole estate on the first sweep.
func TestAnArtifactWithNoExpiryIsNeverSwept(t *testing.T) {
	t.Parallel()

	s := open(t)
	run := queuedRun(t, s, liveTemplate(t, s).ID)
	if err := s.CreateReportArtifact(t.Context(), artifact(run.ID, model.FormatCSV, model.ArtifactRendered)); err != nil {
		t.Fatal(err)
	}

	due, err := s.ExpirableArtifacts(t.Context(), runNow.AddDate(10, 0, 0), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Errorf("%d artifact(s) with no expiry were offered to the sweeper", len(due))
	}
}

// The orphan sweeper's half: every path the database believes in, so a directory
// walk can find the files it does not. ADR-008 writes the file before the row,
// so bytes with no row are the expected residue of a crash.
func TestLiveArtifactPathsIsWhatTheOrphanSweeperComparesAgainst(t *testing.T) {
	t.Parallel()

	s := open(t)
	run := queuedRun(t, s, liveTemplate(t, s).ID)

	kept := artifact(run.ID, model.FormatPDF, model.ArtifactRendered)
	kept.Path = "2026/04/kept.pdf"
	if err := s.CreateReportArtifact(t.Context(), kept); err != nil {
		t.Fatal(err)
	}
	failed := artifact(run.ID, model.FormatHTML, model.ArtifactFailed)
	failed.Path = ""
	if err := s.CreateReportArtifact(t.Context(), failed); err != nil {
		t.Fatal(err)
	}

	paths, err := s.LiveArtifactPaths(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !paths["2026/04/kept.pdf"] {
		t.Error("a live artifact's path is missing; the sweeper would delete it")
	}
	if len(paths) != 1 {
		t.Errorf("paths = %v, want only the one file that exists", paths)
	}
}

// skipped is recorded rather than left as an absence. Silence with no row behind
// it is indistinguishable from a system that is not running, which is the same
// reason a suppressed notification gets a row.
func TestASkippedDeliveryIsRecordedRatherThanOmitted(t *testing.T) {
	t.Parallel()

	s := open(t)
	run := queuedRun(t, s, liveTemplate(t, s).ID)

	for i, outcome := range []string{model.DeliverySucceeded, model.DeliverySkipped, model.DeliveryFailed} {
		d := model.ReportDelivery{
			ID:          model.NewID(),
			OrgID:       model.SentinelOrgID,
			ReportRunID: run.ID,
			Type:        model.ReportDeliveryEmail,
			Outcome:     outcome,
			Attempt:     i + 1,
			Target:      "ops@example.com",
			CreatedAt:   runNow.Add(time.Duration(i) * time.Second),
		}
		if outcome == model.DeliveryFailed {
			d.Error = "smtp: connection refused"
		}
		if err := s.RecordReportDelivery(t.Context(), d); err != nil {
			t.Fatalf("record %s: %v", outcome, err)
		}
	}

	log, err := s.DeliveriesForRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 3 {
		t.Fatalf("log = %d rows, want 3", len(log))
	}
	if log[0].Outcome != model.DeliverySucceeded || log[1].Outcome != model.DeliverySkipped {
		t.Errorf("the log is not in attempt order: %v", []string{log[0].Outcome, log[1].Outcome})
	}
	if log[2].Error == "" {
		t.Error("a failed delivery carries no reason")
	}
}

// The run list filters the way the spec's query parameters say, and pages on the
// same keyset shape every other collection uses.
func TestRunListFiltersAndPages(t *testing.T) {
	t.Parallel()

	s := open(t)
	first, second := liveTemplate(t, s), testTemplate("Other")
	if err := s.CreateReportTemplate(t.Context(), second); err != nil {
		t.Fatal(err)
	}

	for i := range 4 {
		run := model.ReportRun{
			ID: model.NewID(), OrgID: model.SentinelOrgID, ReportTemplateID: first.ID,
			State: model.RunSucceeded, PeriodStart: runNow.AddDate(0, -1, 0), PeriodEnd: runNow,
			Timezone: "UTC", CreatedAt: runNow.Add(time.Duration(i) * time.Minute),
		}
		if i == 0 {
			run.State = model.RunFailed
		}
		if err := s.CreateReportRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}
	queuedRun(t, s, second.ID)

	mine, _, err := s.ListReportRuns(t.Context(), nil, 10, ReportRunFilter{ReportTemplateID: &first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 4 {
		t.Errorf("template filter returned %d runs, want 4", len(mine))
	}

	failed, _, err := s.ListReportRuns(t.Context(), nil, 10, store.ReportRunFilter{States: []string{model.RunFailed}})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].State != model.RunFailed {
		t.Errorf("state filter returned %d rows", len(failed))
	}

	page, more, err := s.ListReportRuns(t.Context(), nil, 2, ReportRunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || !more {
		t.Fatalf("page = %d rows, more = %v", len(page), more)
	}
	if page[0].CreatedAt.Before(page[1].CreatedAt) {
		t.Error("runs are not newest-first")
	}

	next, _, err := s.ListReportRuns(t.Context(),
		&Cursor{UpdatedAt: page[1].CreatedAt, ID: page[1].ID}, 2, ReportRunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range page {
		for _, b := range next {
			if a.ID == b.ID {
				t.Errorf("run %s appears on both pages", a.ID)
			}
		}
	}
}

// Missing rows are ErrNotFound on every read, and finishing a run that is not
// there is not a silent success.
func TestMissingRunsAndArtifactsAreNotFound(t *testing.T) {
	t.Parallel()

	s := open(t)
	missing := model.NewID()

	if _, err := s.GetReportRun(t.Context(), missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("get run: %v, want ErrNotFound", err)
	}
	if _, err := s.GetReportArtifact(t.Context(), missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("get artifact: %v, want ErrNotFound", err)
	}
	if _, err := s.ArtifactByFormat(t.Context(), missing, model.FormatPDF); !errors.Is(err, ErrNotFound) {
		t.Errorf("artifact by format: %v, want ErrNotFound", err)
	}
	if err := s.FinishReportRun(t.Context(), missing, model.RunFailed, "", runNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("finish run: %v, want ErrNotFound", err)
	}
	if err := s.MarkArtifactExpired(t.Context(), missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("expire artifact: %v, want ErrNotFound", err)
	}
	if err := s.StartReportRun(t.Context(), missing, runNow); !errors.Is(err, ErrConflict) {
		t.Errorf("start missing run: %v, want ErrConflict — it is not queued", err)
	}
}

// A restart finishes the runs it interrupted, and does not touch the ones it did
// not.
//
// The state a stuck run leaves behind is the point. `running` means "a worker has
// this", and after a crash nobody has it and nobody ever will — so the row sits
// on the run-history screen forever, looking exactly like a report that is still
// being produced. A person reading that screen cannot tell whether to wait, and
// the answer is that they should never have been asked to.
//
// Doing it at start-up rather than on a timer is what makes it safe: no worker
// has started, so every `running` row is from a process that is gone. A threshold
// sweep would have to be long enough not to kill a genuinely slow CSV over five
// thousand monitors, which is exactly how long the ambiguous state would last.
func TestARestartFinishesTheRunsItInterrupted(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveTemplate(t, s)

	// One that got as far as producing an artifact, one that did not, and two
	// that must be left exactly as they are.
	withArtifact := queuedRun(t, s, tpl.ID)
	empty := queuedRun(t, s, tpl.ID)
	queued := queuedRun(t, s, tpl.ID)
	done := queuedRun(t, s, tpl.ID)

	for _, run := range []model.ReportRun{withArtifact, empty, done} {
		if err := s.StartReportRun(t.Context(), run.ID, runNow); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	if err := s.CreateReportArtifact(t.Context(), artifact(withArtifact.ID, model.FormatJSON, model.ArtifactRendered)); err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if err := s.FinishReportRun(t.Context(), done.ID, model.RunSucceeded, "", runNow); err != nil {
		t.Fatalf("finish: %v", err)
	}

	at := runNow.Add(time.Hour)
	recovered, err := s.RecoverInterruptedReportRuns(t.Context(), at)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered != 2 {
		t.Errorf("recovered = %d, want 2", recovered)
	}

	// Something arrived, so the run says so. Calling it a failure would claim
	// nothing was produced when a real artifact is on disk with a digest beside
	// it.
	got := mustGetRun(t, s, withArtifact.ID)
	if got.State != model.RunPartial {
		t.Errorf("state = %q, want %q — one format landed before the interruption",
			got.State, model.RunPartial)
	}
	if got.Error == "" || got.FinishedAt == nil {
		t.Error("a recovered run carries neither a reason nor a finish time")
	}

	// Nothing arrived, so it failed. Partial means "some of it arrived", and a
	// run with no artifacts has nothing to be partial about.
	if got := mustGetRun(t, s, empty.ID); got.State != model.RunFailed {
		t.Errorf("state = %q, want %q on a run that produced nothing", got.State, model.RunFailed)
	}

	// A queued run is a run nothing has claimed yet. Recovery must leave it
	// alone or a restart during a monthly burst would fail every report waiting
	// in the queue rather than running them.
	if got := mustGetRun(t, s, queued.ID); got.State != model.RunQueued {
		t.Errorf("a queued run became %q; a restart must not fail the backlog", got.State)
	}

	// And a finished run keeps its outcome and its timestamps.
	if got := mustGetRun(t, s, done.ID); got.State != model.RunSucceeded || got.Error != "" {
		t.Errorf("a completed run was rewritten: state = %q, error = %q", got.State, got.Error)
	}
}

// The artifacts a recovered run did produce are kept.
//
// They are complete files with committed rows and digests. Deleting them to make
// the run's state tidier would be destroying the only record of what a client was
// actually sent — which is the one thing ADR-008 says an artifact exists to be.
func TestRecoveryKeepsTheArtifactsThatLanded(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveTemplate(t, s)
	run := queuedRun(t, s, tpl.ID)

	if err := s.StartReportRun(t.Context(), run.ID, runNow); err != nil {
		t.Fatalf("start: %v", err)
	}
	kept := artifact(run.ID, model.FormatJSON, model.ArtifactRendered)
	if err := s.CreateReportArtifact(t.Context(), kept); err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	if _, err := s.RecoverInterruptedReportRuns(t.Context(), runNow.Add(time.Hour)); err != nil {
		t.Fatalf("recover: %v", err)
	}

	artifacts, err := s.ArtifactsForRuns(t.Context(), []model.ID{run.ID})
	if err != nil {
		t.Fatalf("artifacts: %v", err)
	}
	rows := artifacts[run.ID]
	if len(rows) != 1 {
		t.Fatalf("%d artifacts after recovery, want 1", len(rows))
	}
	if rows[0].State != model.ArtifactRendered || rows[0].SHA256 != kept.SHA256 {
		t.Errorf("the surviving artifact was altered: %+v", rows[0])
	}
}

// A failed artifact row is not something that landed.
//
// The distinction is the whole reason `partial` exists: a run whose only format
// failed to render produced nothing, and calling it partial would tell somebody
// a document is available when none is.
func TestAFailedArtifactDoesNotMakeARunPartial(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveTemplate(t, s)
	run := queuedRun(t, s, tpl.ID)

	if err := s.StartReportRun(t.Context(), run.ID, runNow); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.CreateReportArtifact(t.Context(),
		artifact(run.ID, model.FormatPDF, model.ArtifactFailed)); err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	if _, err := s.RecoverInterruptedReportRuns(t.Context(), runNow.Add(time.Hour)); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := mustGetRun(t, s, run.ID); got.State != model.RunFailed {
		t.Errorf("state = %q, want %q — a failed artifact row is a format that did "+
			"not arrive, not one that did", got.State, model.RunFailed)
	}
}

func mustGetRun(t *testing.T, s *Store, id model.ID) model.ReportRun {
	t.Helper()
	run, err := s.GetReportRun(t.Context(), id)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return run
}
