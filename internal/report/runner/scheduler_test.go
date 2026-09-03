package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// fakeSchedules is the schedule half of the store, in memory, with the
// conditional claim the real one enforces — because that condition is the thing
// most of these tests are about.
type fakeSchedules struct {
	template  model.ReportTemplate
	schedules []model.ReportSchedule

	claims []claim
	fail   error
}

type claim struct {
	id       model.ID
	expected time.Time
	next     *time.Time
	run      model.ReportRun
}

func (f *fakeSchedules) DueReportSchedules(_ context.Context, now time.Time, limit int) ([]model.ReportSchedule, error) {
	var out []model.ReportSchedule
	for _, s := range f.schedules {
		if s.Enabled && s.NextRunAt != nil && !s.NextRunAt.After(now) {
			out = append(out, s)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeSchedules) ClaimReportSchedule(_ context.Context, id model.ID, expected, _ time.Time, next *time.Time, run model.ReportRun) error {
	if f.fail != nil {
		return f.fail
	}
	for i, s := range f.schedules {
		if s.ID != id {
			continue
		}
		// The conditional update, faithfully: a claim against a firing time the
		// row no longer holds loses.
		if s.NextRunAt == nil || !s.NextRunAt.Equal(expected) {
			return store.ErrConflict
		}
		f.schedules[i].NextRunAt = next
		f.claims = append(f.claims, claim{id: id, expected: expected, next: next, run: run})
		return nil
	}
	return store.ErrNotFound
}

func (f *fakeSchedules) ReportTemplateForRun(context.Context, model.ID) (model.ReportTemplate, error) {
	return f.template, nil
}

type fakeQueue struct {
	submitted []model.ReportRun
	err       error
}

func (q *fakeQueue) Submit(run model.ReportRun) error {
	if q.err != nil {
		return q.err
	}
	q.submitted = append(q.submitted, run)
	return nil
}

func scheduleFixture(next time.Time, frequency string) (*fakeSchedules, model.ReportSchedule) {
	template := model.ReportTemplate{
		ID: model.NewID(), Name: "Monthly", Type: model.ReportTypeSLA,
		Period: model.ReportPeriodMonth, PeriodStyle: model.ReportStyleCalendar,
		MaintenanceHandling: "exclude", Formats: []string{model.FormatJSON},
	}
	schedule := model.ReportSchedule{
		ID: model.NewID(), OrgID: model.SentinelOrgID, ReportTemplateID: template.ID,
		Enabled: true, Frequency: frequency, Timezone: "UTC", SendAt: "09:00",
		NextRunAt: &next,
	}
	return &fakeSchedules{template: template, schedules: []model.ReportSchedule{schedule}}, schedule
}

func TestADueScheduleQueuesARunAndAdvances(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	fake, schedule := scheduleFixture(due, model.ReportFrequencyMonthly)
	queue := &fakeQueue{}

	now := due.Add(30 * time.Second)
	if got := NewScheduler(fake, queue, discardLog()).Tick(t.Context(), now); got != 1 {
		t.Fatalf("queued %d runs, want 1", got)
	}

	if len(queue.submitted) != 1 {
		t.Fatalf("submitted %d runs", len(queue.submitted))
	}
	run := queue.submitted[0]
	if run.ReportScheduleID == nil || *run.ReportScheduleID != schedule.ID {
		t.Error("the run does not name the schedule that produced it")
	}
	if run.State != model.RunQueued {
		t.Errorf("state = %q, want queued", run.State)
	}
	if run.Late {
		t.Error("a run half a minute behind its due time was marked late")
	}
	// The window is the period that just closed, not the one in progress.
	if !run.PeriodStart.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("period start = %s, want 1 March", run.PeriodStart)
	}
	if !run.PeriodEnd.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("period end = %s, want 1 April", run.PeriodEnd)
	}

	// And the schedule moved, so the next tick does not fire it again.
	if next := fake.schedules[0].NextRunAt; next == nil || !next.Equal(time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("next run = %v, want 1 May 09:00", next)
	}
	if NewScheduler(fake, queue, discardLog()).Tick(t.Context(), now) != 0 {
		t.Error("the same schedule fired twice")
	}
}

// **A missed schedule is late, not lost — and it is one report, not a backlog.**
// An instance down for three days owes a daily client one report: three copies of
// yesterday's would be noise arriving as an apology. The next firing is computed
// from now rather than from the firing that was missed, so nothing cascades.
func TestAMissedScheduleFiresOnceAndIsMarkedLate(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	fake, _ := scheduleFixture(due, model.ReportFrequencyDaily)
	queue := &fakeQueue{}

	// The instance comes back three days later.
	now := due.AddDate(0, 0, 3).Add(2 * time.Hour)
	NewScheduler(fake, queue, discardLog()).Tick(t.Context(), now)

	if len(queue.submitted) != 1 {
		t.Fatalf("queued %d runs for three missed days, want 1", len(queue.submitted))
	}
	if !queue.submitted[0].Late {
		t.Error("a run three days behind was not marked late")
	}
	// The next firing is tomorrow, not one of the days that were missed.
	next := fake.schedules[0].NextRunAt
	if next == nil || !next.Equal(time.Date(2026, 4, 5, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("next run = %v, want 5 April — computed from now, not from the missed firing", next)
	}
}

// Ordinary scheduling jitter is not lateness. Ticks are a minute apart and a busy
// pool adds a few more; marking those late would make the flag meaningless on the
// screen where it matters.
func TestOrdinaryJitterIsNotLate(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	fake, _ := scheduleFixture(due, model.ReportFrequencyDaily)
	queue := &fakeQueue{}

	NewScheduler(fake, queue, discardLog()).Tick(t.Context(), due.Add(LateAfter-time.Second))
	if queue.submitted[0].Late {
		t.Error("a run just inside the lateness threshold was marked late")
	}
}

// **Two ticks cannot both fire one schedule.** The claim is conditional on the
// firing time the caller read, so a restart mid-tick does not send a client two
// copies of the same report.
func TestASecondTickCannotClaimTheSameFiring(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	fake, schedule := scheduleFixture(due, model.ReportFrequencyDaily)
	queue := &fakeQueue{}
	scheduler := NewScheduler(fake, queue, discardLog())

	// Both ticks see the schedule as due, because the second reads the list
	// before the first has advanced it.
	first := scheduler.Tick(t.Context(), due.Add(time.Minute))
	fake.schedules[0].NextRunAt = &due // as if the second tick read the old value
	fake.fail = store.ErrConflict      // and the database refuses its claim
	second := scheduler.Tick(t.Context(), due.Add(time.Minute))

	if first != 1 || second != 0 {
		t.Errorf("ticks queued %d and %d runs, want 1 and 0", first, second)
	}
	if len(fake.claims) != 1 || fake.claims[0].id != schedule.ID {
		t.Errorf("claims = %d, want exactly one", len(fake.claims))
	}
}

// A full queue does not lose the run. The row is committed as queued inside the
// claim's transaction, so a recovery pass finds it — the tick's job was to
// record that the schedule owed a report, and it did.
func TestAFullQueueStillRecordsTheRun(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	fake, _ := scheduleFixture(due, model.ReportFrequencyDaily)
	queue := &fakeQueue{err: ErrBusy}

	if got := NewScheduler(fake, queue, discardLog()).Tick(t.Context(), due.Add(time.Minute)); got != 1 {
		t.Errorf("tick queued %d, want 1 — the run is recorded even when the pool refuses it", got)
	}
	if len(fake.claims) != 1 {
		t.Fatal("the run was not recorded")
	}
	if fake.claims[0].run.State != model.RunQueued {
		t.Errorf("recorded state = %q, want queued", fake.claims[0].run.State)
	}
}

// A disabled schedule is never due, and one with no firing time is not either.
func TestDisabledAndUnscheduledRowsAreNeverDue(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	fake, _ := scheduleFixture(due, model.ReportFrequencyDaily)
	fake.schedules[0].Enabled = false
	fake.schedules = append(fake.schedules, model.ReportSchedule{
		ID: model.NewID(), Enabled: true, Frequency: model.ReportFrequencyDaily,
	})

	queue := &fakeQueue{}
	if got := NewScheduler(fake, queue, discardLog()).Tick(t.Context(), due.Add(time.Hour)); got != 0 {
		t.Errorf("tick queued %d runs, want 0", got)
	}
}

// A schedule whose template has gone is left due rather than advanced, so the
// next tick tries again once an operator has fixed it — silently skipping a
// period would be the worse failure.
func TestAScheduleWithNoTemplateIsLeftForTheOperator(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	fake, _ := scheduleFixture(due, model.ReportFrequencyDaily)
	broken := &brokenTemplates{fakeSchedules: fake}

	queue := &fakeQueue{}
	if got := NewScheduler(broken, queue, discardLog()).Tick(t.Context(), due.Add(time.Minute)); got != 0 {
		t.Errorf("tick queued %d runs, want 0", got)
	}
	if next := fake.schedules[0].NextRunAt; next == nil || !next.Equal(due) {
		t.Errorf("next run = %v, want it left at the due time so the next tick retries", next)
	}
}

type brokenTemplates struct{ *fakeSchedules }

func (b *brokenTemplates) ReportTemplateForRun(context.Context, model.ID) (model.ReportTemplate, error) {
	return model.ReportTemplate{}, errors.New("no such template")
}

// One tick is bounded. A thousand schedules due at 09:00 on the first are queued
// over several ticks rather than in one burst that fills the pool and holds a
// transaction open.
func TestATickIsBounded(t *testing.T) {
	t.Parallel()

	due := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	fake, _ := scheduleFixture(due, model.ReportFrequencyDaily)
	for range DueBatch + 50 {
		fake.schedules = append(fake.schedules, model.ReportSchedule{
			ID: model.NewID(), OrgID: model.SentinelOrgID, ReportTemplateID: fake.template.ID,
			Enabled: true, Frequency: model.ReportFrequencyDaily, Timezone: "UTC",
			SendAt: "09:00", NextRunAt: &due,
		})
	}

	queue := &fakeQueue{}
	if got := NewScheduler(fake, queue, discardLog()).Tick(t.Context(), due.Add(time.Minute)); got > DueBatch {
		t.Errorf("one tick queued %d runs, want at most %d", got, DueBatch)
	}
}
