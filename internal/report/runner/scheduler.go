package runner

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The scheduler: what turns a saved schedule into a queued run.
//
// It does as little as possible. A tick asks the database for schedules whose
// firing time has passed, and for each one claims it and queues a run in a
// single transaction. Everything after that — computing, rendering, storing,
// delivering — belongs to the pool, which is off the check path and bounded.

// TickInterval is how often the scheduler looks.
//
// A minute, because the finest thing a schedule can express is a minute (the
// cron field is minutes) and looking more often would only find the same nothing
// sooner. The query it runs is a seek on a partial index, so the cost of a tick
// on an install with no schedules is one indexed lookup that returns no rows.
const TickInterval = time.Minute

// LateAfter is how far past its due time a run has to start before it is marked
// late.
//
// A quarter of an hour. Ticks are a minute apart and a busy pool can add a few
// more, so anything under this is ordinary jitter and marking it late would make
// the flag meaningless. Beyond it, something was actually wrong — most often the
// instance was not running when the schedule was due, which is exactly what the
// flag exists to say.
const LateAfter = 15 * time.Minute

// DueBatch bounds one tick. An install with a thousand schedules due at 09:00 on
// the first queues them over several ticks rather than in one burst, which keeps
// a tick's transaction short and the pool's queue from being filled by a single
// pass.
const DueBatch = 100

// ScheduleStore is what the scheduler needs from persistence.
type ScheduleStore interface {
	DueReportSchedules(ctx context.Context, now time.Time, limit int) ([]model.ReportSchedule, error)
	ClaimReportSchedule(ctx context.Context, id model.ID, expected, lastRun time.Time, next *time.Time, run model.ReportRun) error
	ReportTemplateForRun(ctx context.Context, id model.ID) (model.ReportTemplate, error)
}

// Queue is where a claimed run goes.
type Queue interface {
	Submit(run model.ReportRun) error
}

// Scheduler fires report schedules.
type Scheduler struct {
	store ScheduleStore
	queue Queue
	log   *slog.Logger
}

func NewScheduler(store ScheduleStore, queue Queue, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{store: store, queue: queue, log: log}
}

// Run ticks until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx, time.Now().UTC().Truncate(time.Millisecond))
		}
	}
}

// Tick queues everything due, and returns how many runs it started.
//
// `now` is a parameter so a test can drive the clock, and so a tick's whole
// decision set comes from one instant rather than from several reads of a moving
// one.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) int {
	due, err := s.store.DueReportSchedules(ctx, now, DueBatch)
	if err != nil {
		s.log.Error("find due report schedules", "error", err)
		return 0
	}

	var queued int
	for _, schedule := range due {
		if s.fire(ctx, schedule, now) {
			queued++
		}
	}
	return queued
}

func (s *Scheduler) fire(ctx context.Context, schedule model.ReportSchedule, now time.Time) bool {
	if schedule.NextRunAt == nil {
		return false
	}
	due := *schedule.NextRunAt

	template, err := s.store.ReportTemplateForRun(ctx, schedule.ReportTemplateID)
	if err != nil {
		// The schedule survives, so the operator can fix whatever is wrong. Not
		// advancing means the next tick tries again rather than silently
		// skipping a period.
		s.log.Error("scheduled report has no template",
			"schedule_id", schedule.ID.String(), "error", err)
		return false
	}

	spec := report.ScheduleSpec{
		Frequency: schedule.Frequency,
		Cron:      schedule.Cron,
		Timezone:  schedule.Timezone,
		SendAt:    schedule.SendAt,
	}

	// **Computed from now, not from the firing that was missed.** An instance
	// that was down for three days owes a daily client one report, not three: the
	// point of a monthly report is the month, and three copies of yesterday's
	// would be noise arriving as an apology. The one that does go out is marked
	// late, which is how the UI says what happened.
	next, err := report.NextRun(spec, now)
	var nextPtr *time.Time
	if err != nil {
		// A schedule that no longer resolves — an expression that never fires
		// again, a zone removed from the database. It is cleared rather than left
		// due, so it stops spinning, and the error is logged where an operator
		// sees it.
		s.log.Error("schedule has no next firing; it will not run again until edited",
			"schedule_id", schedule.ID.String(), "error", err)
	} else {
		nextPtr = &next
	}

	window, err := s.window(template, schedule, now)
	if err != nil {
		s.log.Error("cannot resolve the window for a scheduled report",
			"schedule_id", schedule.ID.String(), "error", err)
		return false
	}

	scheduleID := schedule.ID
	run := model.ReportRun{
		ID:               model.NewID(),
		OrgID:            schedule.OrgID,
		ReportTemplateID: template.ID,
		ReportScheduleID: &scheduleID,
		State:            model.RunQueued,
		PeriodStart:      window.From,
		PeriodEnd:        window.To,
		Timezone:         window.Timezone,
		Late:             now.Sub(due) > LateAfter,
		CreatedAt:        now,
	}

	if err := s.store.ClaimReportSchedule(ctx, schedule.ID, due, now, nextPtr, run); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Another tick got there first. That is the guard working, not a
			// failure, and it is the reason a restart mid-tick does not send a
			// client two copies of the same report.
			return false
		}
		s.log.Error("claim report schedule", "schedule_id", schedule.ID.String(), "error", err)
		return false
	}

	if err := s.queue.Submit(run); err != nil {
		// The row is committed as queued, so nothing is lost — it is picked up by
		// the recovery pass rather than by this tick. Logged because a full queue
		// at 09:00 on the first is something an operator wants to know about.
		s.log.Warn("report queue is full; the run is recorded and not yet started",
			"run_id", run.ID.String(), "error", err)
	}
	if run.Late {
		s.log.Warn("scheduled report is late",
			"run_id", run.ID.String(), "due", due, "started", now, "behind", now.Sub(due).String())
	}
	return true
}

// window is the period a scheduled run covers: the most recently completed one,
// cut in the schedule's own zone.
//
// The schedule's zone rather than the instance's, because the schedule stores one
// precisely so that changing the instance zone does not silently move the
// boundaries of a report somebody has been receiving for a year.
func (s *Scheduler) window(template model.ReportTemplate, schedule model.ReportSchedule, now time.Time) (report.Window, error) {
	zone := schedule.Timezone
	if zone == "" {
		zone = "UTC"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return report.Window{}, err
	}
	return report.ResolveWindow(template.Period, template.PeriodStyle, loc, now)
}
