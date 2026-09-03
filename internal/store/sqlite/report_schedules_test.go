package sqlite

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

var schedNow = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

func testSchedule(templateID model.ID, next *time.Time) model.ReportSchedule {
	return model.ReportSchedule{
		ID:               model.NewID(),
		OrgID:            model.SentinelOrgID,
		ReportTemplateID: templateID,
		Name:             "Monthly to Acme",
		Enabled:          true,
		Frequency:        model.ReportFrequencyMonthly,
		Timezone:         "Australia/Sydney",
		SendAt:           "09:00",
		NextRunAt:        next,
		CreatedAt:        schedNow,
		UpdatedAt:        schedNow,
	}
}

func emailTarget(scheduleID model.ID) model.ReportScheduleDelivery {
	return model.ReportScheduleDelivery{
		ID:               model.NewID(),
		OrgID:            model.SentinelOrgID,
		ReportScheduleID: scheduleID,
		Type:             model.ReportDeliveryEmail,
		Config:           json.RawMessage(`{"recipients":["ops@example.com"]}`),
		Formats:          []string{model.FormatPDF},
		CreatedAt:        schedNow,
		UpdatedAt:        schedNow,
	}
}

func liveScheduleTemplate(t *testing.T, s *Store) model.ReportTemplate {
	t.Helper()
	tpl := testTemplate("Monthly")
	if err := s.CreateReportTemplate(t.Context(), tpl); err != nil {
		t.Fatal(err)
	}
	return tpl
}

// The schedule and its targets round-trip, including the zone — which is stored
// on the schedule precisely so that changing the instance zone does not silently
// move the boundaries of a report somebody has been receiving for a year.
func TestAScheduleRoundTripsWithItsTargets(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveScheduleTemplate(t, s)
	next := schedNow.AddDate(0, 1, 0)
	sched := testSchedule(tpl.ID, &next)

	if err := s.CreateReportSchedule(t.Context(), sched, []model.ReportScheduleDelivery{emailTarget(sched.ID)}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetReportSchedule(t.Context(), sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Timezone != "Australia/Sydney" || got.SendAt != "09:00" {
		t.Errorf("schedule = %+v", got)
	}
	if got.NextRunAt == nil || !got.NextRunAt.Equal(next) {
		t.Errorf("next run = %v, want %v", got.NextRunAt, next)
	}

	targets, err := s.DeliveriesForSchedule(t.Context(), sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Type != model.ReportDeliveryEmail {
		t.Fatalf("targets = %+v", targets)
	}
	if string(targets[0].Config) != `{"recipients":["ops@example.com"]}` {
		t.Errorf("config = %s", targets[0].Config)
	}
}

// Targets are replaced wholesale, so the request is the state. A diff would have
// to reconcile position, membership and identity at once for a handful of rows.
func TestUpdatingASchedulesTargetsReplacesThem(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveScheduleTemplate(t, s)
	sched := testSchedule(tpl.ID, nil)
	if err := s.CreateReportSchedule(t.Context(), sched, []model.ReportScheduleDelivery{
		emailTarget(sched.ID), emailTarget(sched.ID),
	}); err != nil {
		t.Fatal(err)
	}

	replacement := emailTarget(sched.ID)
	replacement.Config = json.RawMessage(`{"recipients":["finance@example.com"]}`)
	if err := s.UpdateReportSchedule(t.Context(), sched, []model.ReportScheduleDelivery{replacement}); err != nil {
		t.Fatalf("update: %v", err)
	}

	targets, err := s.DeliveriesForSchedule(t.Context(), sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	if string(targets[0].Config) != `{"recipients":["finance@example.com"]}` {
		t.Errorf("config = %s", targets[0].Config)
	}
}

// The due query finds what is due and nothing else. It is the scheduler's only
// query, so everything it wrongly returns becomes a report somebody did not ask
// for.
func TestDueSchedulesAreTheOnesActuallyDue(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveScheduleTemplate(t, s)

	past, future := schedNow.Add(-time.Hour), schedNow.Add(time.Hour)
	dueOne := testSchedule(tpl.ID, &past)
	notYet := testSchedule(tpl.ID, &future)
	disabled := testSchedule(tpl.ID, &past)
	disabled.Enabled = false
	unscheduled := testSchedule(tpl.ID, nil)

	for _, sched := range []model.ReportSchedule{dueOne, notYet, disabled, unscheduled} {
		if err := s.CreateReportSchedule(t.Context(), sched, nil); err != nil {
			t.Fatal(err)
		}
	}

	due, err := s.DueReportSchedules(t.Context(), schedNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != dueOne.ID {
		t.Fatalf("due = %d schedules, want only the one past its firing time", len(due))
	}
}

// **A claim advances the schedule and queues the run in one transaction**, and
// only the caller holding the firing time the row still has wins. Separately,
// a crash between them would either lose a report or send it twice.
func TestClaimingIsAtomicAndConditional(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveScheduleTemplate(t, s)
	due := schedNow.Add(-time.Minute)
	sched := testSchedule(tpl.ID, &due)
	if err := s.CreateReportSchedule(t.Context(), sched, nil); err != nil {
		t.Fatal(err)
	}

	scheduleID := sched.ID
	run := model.ReportRun{
		ID: model.NewID(), OrgID: model.SentinelOrgID, ReportTemplateID: tpl.ID,
		ReportScheduleID: &scheduleID, State: model.RunQueued,
		PeriodStart: schedNow.AddDate(0, -1, 0), PeriodEnd: schedNow,
		Timezone: "UTC", CreatedAt: schedNow,
	}
	next := schedNow.AddDate(0, 1, 0)

	if err := s.ClaimReportSchedule(t.Context(), sched.ID, due, schedNow, &next, run); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// The run exists...
	if _, err := s.GetReportRun(t.Context(), run.ID); err != nil {
		t.Fatalf("the run was not queued by the claim: %v", err)
	}
	// ...and the schedule moved.
	got, err := s.GetReportSchedule(t.Context(), sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NextRunAt == nil || !got.NextRunAt.Equal(next) {
		t.Errorf("next run = %v, want %v", got.NextRunAt, next)
	}
	if got.LastRunAt == nil {
		t.Error("last run was not recorded")
	}

	// A second claim against the firing time the row no longer holds loses, and
	// queues nothing.
	second := run
	second.ID = model.NewID()
	if err := s.ClaimReportSchedule(t.Context(), sched.ID, due, schedNow, &next, second); !errors.Is(err, ErrConflict) {
		t.Fatalf("second claim: %v, want ErrConflict", err)
	}
	if _, err := s.GetReportRun(t.Context(), second.ID); !errors.Is(err, ErrNotFound) {
		t.Error("a losing claim still queued a run; the transaction did not roll back")
	}
}

// Deleting a schedule stops it firing and keeps the runs it produced — a run is
// the record of what a client was sent.
func TestDeletingAScheduleStopsItAndKeepsItsRuns(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveScheduleTemplate(t, s)
	due := schedNow.Add(-time.Minute)
	sched := testSchedule(tpl.ID, &due)
	if err := s.CreateReportSchedule(t.Context(), sched, []model.ReportScheduleDelivery{emailTarget(sched.ID)}); err != nil {
		t.Fatal(err)
	}
	run := seedRun(t, s, tpl.ID, &sched.ID)

	if err := s.DeleteReportSchedule(t.Context(), sched.ID, schedNow); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := s.GetReportSchedule(t.Context(), sched.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
	due2, err := s.DueReportSchedules(t.Context(), schedNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due2) != 0 {
		t.Error("a deleted schedule is still due")
	}

	got, err := s.GetReportRun(t.Context(), run)
	if err != nil {
		t.Fatalf("the run went with its schedule: %v", err)
	}
	if got.ReportScheduleID == nil || *got.ReportScheduleID != sched.ID {
		t.Error("the run no longer names the schedule it came from")
	}
}

// Deleting a template takes its schedules with it, in the same transaction —
// a schedule with no template to render has nothing to do, and one left enabled
// would keep firing.
func TestDeletingATemplateAlsoStopsItsSchedules(t *testing.T) {
	t.Parallel()

	s := open(t)
	tpl := liveScheduleTemplate(t, s)
	due := schedNow.Add(-time.Minute)
	sched := testSchedule(tpl.ID, &due)
	if err := s.CreateReportSchedule(t.Context(), sched, nil); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteReportTemplate(t.Context(), tpl.ID, schedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetReportSchedule(t.Context(), sched.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the schedule survived its template: %v", err)
	}
	due2, _ := s.DueReportSchedules(t.Context(), schedNow, 10)
	if len(due2) != 0 {
		t.Error("a schedule under a deleted template is still due")
	}
}
