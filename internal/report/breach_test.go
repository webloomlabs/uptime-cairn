package report

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/store"
)

func downDay(n, up, down int) store.HistoryBucket {
	return store.HistoryBucket{Start: day0.AddDate(0, 0, n), Up: up, Down: down}
}

// Consecutive bad days are one breach, not three. A client reading "three
// breaches" counts three separate incidents; the outage was one.
func TestConsecutiveBadDaysAreOneBreach(t *testing.T) {
	t.Parallel()

	daily := []store.HistoryBucket{
		downDay(0, 100, 0),
		downDay(1, 90, 10),
		downDay(2, 50, 50),
		downDay(3, 100, 0),
		downDay(4, 99, 1),
	}

	got := ComputeBreaches(daily, MaintenanceExclude)

	if len(got) != 2 {
		t.Fatalf("breaches = %d, want 2 (a two-day run, then a separate one)", len(got))
	}
	if !got[0].StartedAt.Equal(day0.AddDate(0, 0, 1)) || !got[0].EndedAt.Equal(day0.AddDate(0, 0, 3)) {
		t.Errorf("first breach spans %s–%s, want 2–4 March exclusive", got[0].StartedAt, got[0].EndedAt)
	}
	if !got[1].StartedAt.Equal(day0.AddDate(0, 0, 4)) {
		t.Errorf("second breach starts %s, want 5 March", got[1].StartedAt)
	}
}

// The duration is the downtime inside the period, never the length of it. A day
// containing four minutes of downtime is a four-minute breach, and reporting 24
// hours because the day is the unit would overstate by a factor of 360.
func TestBreachDurationIsDowntimeNotSpan(t *testing.T) {
	t.Parallel()

	// 10% of one day down: 8,640 seconds.
	got := ComputeBreaches([]store.HistoryBucket{downDay(0, 90, 10)}, MaintenanceExclude)

	if len(got) != 1 {
		t.Fatalf("breaches = %d, want 1", len(got))
	}
	if got[0].DurationSeconds != 8640 {
		t.Errorf("duration = %ds, want 8640 (10%% of a day) and never 86400 (the whole day)", got[0].DurationSeconds)
	}
	if span := got[0].EndedAt.Sub(got[0].StartedAt); span != 24*time.Hour {
		t.Errorf("span = %v, want a full day; the boundaries are the day, the duration is not", span)
	}
}

// A gap is not a recovery. Downtime either side of a day the probe could not
// observe is more likely one outage than two, and splitting it would invent a
// recovery nobody witnessed.
func TestAnUnobservedDayDoesNotEndABreach(t *testing.T) {
	t.Parallel()

	daily := []store.HistoryBucket{
		downDay(0, 90, 10),
		{Start: day0.AddDate(0, 0, 1), Unknown: 100}, // the probe was down
		downDay(2, 90, 10),
	}

	got := ComputeBreaches(daily, MaintenanceExclude)

	if len(got) != 1 {
		t.Fatalf("breaches = %d, want 1 — a gap is not evidence the service recovered", len(got))
	}
	if !got[0].EndedAt.Equal(day0.AddDate(0, 0, 3)) {
		t.Errorf("breach ends %s, want 4 March — it spans the gap", got[0].EndedAt)
	}
	// The unobserved day contributes no seconds: nothing was seen, so nothing is
	// counted, which is the same rule the denominator follows.
	if got[0].DurationSeconds != 2*8640 {
		t.Errorf("duration = %d, want %d — the gap day adds nothing", got[0].DurationSeconds, 2*8640)
	}
}

// A clean observed day does end one. That is the difference between "we looked
// and it was fine" and "we could not look".
func TestACleanObservedDayEndsABreach(t *testing.T) {
	t.Parallel()

	daily := []store.HistoryBucket{downDay(0, 90, 10), downDay(1, 100, 0), downDay(2, 90, 10)}

	if got := ComputeBreaches(daily, MaintenanceExclude); len(got) != 2 {
		t.Errorf("breaches = %d, want 2", len(got))
	}
}

// The maintenance policy reaches the breach log, because it reaches the budget
// the log is there to explain. Declared maintenance is not a breach by default
// and becomes one under count_as_down.
func TestMaintenancePolicyDecidesWhetherMaintenanceIsABreach(t *testing.T) {
	t.Parallel()

	daily := []store.HistoryBucket{{Start: day0, Up: 90, Maintenance: 10}}

	if got := ComputeBreaches(daily, MaintenanceExclude); len(got) != 0 {
		t.Errorf("excluded maintenance produced %d breaches, want 0", len(got))
	}
	got := ComputeBreaches(daily, MaintenanceCountAsDown)
	if len(got) != 1 {
		t.Fatalf("count_as_down produced %d breaches, want 1", len(got))
	}
	if got[0].DurationSeconds != 8640 {
		t.Errorf("duration = %d, want 8640", got[0].DurationSeconds)
	}
}

// A perfect month has an empty log rather than a nil-versus-empty distinction
// anybody has to think about.
func TestNoDowntimeIsNoBreaches(t *testing.T) {
	t.Parallel()

	daily := []store.HistoryBucket{downDay(0, 100, 0), downDay(1, 100, 0)}
	if got := ComputeBreaches(daily, MaintenanceExclude); len(got) != 0 {
		t.Errorf("breaches = %v, want none", got)
	}
}

// The breach durations and the error budget must agree: both are the same
// projection of the same counts, and two conversions would let the log
// contradict the total it is supposed to explain.
func TestBreachTotalAgreesWithConsumedBudget(t *testing.T) {
	t.Parallel()

	daily := []store.HistoryBucket{downDay(0, 90, 10), downDay(1, 100, 0), downDay(2, 95, 5)}

	var total int
	for _, b := range ComputeBreaches(daily, MaintenanceExclude) {
		total += b.DurationSeconds
	}

	u := ComputeUptime(Sum(daily), MaintenanceExclude)
	sla := ComputeSLA(u, Target{Percent: 99, Source: TargetFromMonitor}, 3*24*time.Hour)

	// Both are the observed down proportion projected onto time; over equal-length
	// days with equal check counts they are the same number.
	if diff := total - sla.ErrorBudgetConsumedSeconds; diff > 1 || diff < -1 {
		t.Errorf("breach total %ds disagrees with consumed budget %ds", total, sla.ErrorBudgetConsumedSeconds)
	}
}
