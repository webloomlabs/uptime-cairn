package sqlite

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/rollup"
)

// rollUpClosed runs the pipeline the way the runner does: every tier, only over
// buckets that have closed and waited out the grace period. It is what makes
// these tests reproduce the real gap rather than an artificial one — rolling up
// an open bucket by hand would paper over exactly the bug being asserted.
func rollUpClosed(t *testing.T, s *Store, now time.Time, back time.Duration) {
	t.Helper()

	for _, tier := range rollup.Tiers {
		to := rollup.Bucket(now.Add(-30*time.Second), tier.Interval)
		from := rollup.Bucket(now.Add(-back), tier.Interval)
		if !from.Before(to) {
			continue
		}
		var err error
		if tier.Source == nil {
			_, err = s.RollUpRaw(t.Context(), from, to)
		} else {
			_, err = s.RollUpTier(t.Context(), tier, *tier.Source, from, to)
		}
		if err != nil {
			t.Fatalf("roll up %s: %v", tier.Name, err)
		}
	}
}

// barWindow is the range a status page asks for: whole UTC days, ending after
// today so the day in progress is inside it.
func barWindow(now time.Time, days int) (from, to time.Time) {
	to = now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	return to.AddDate(0, 0, -days), to
}

// The 1d tier only ever holds *closed* days, so a bar read from it alone is
// missing today — and on an instance younger than a day it is empty, which is a
// status page with no uptime bar at all.
func TestDailyUptimeIncludesTheDayInProgress(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("today")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC()
	// Three up and one down, this morning: a ratio no rollup has computed yet.
	beats := []model.Heartbeat{
		{Status: model.StatusUp}, {Status: model.StatusUp},
		{Status: model.StatusUp}, {Status: model.StatusDown},
	}
	writeBeats(t, s, m, now.Truncate(24*time.Hour).Add(time.Minute), time.Minute, beats)
	rollUpClosed(t, s, now, 48*time.Hour)

	from, to := barWindow(now, 90)
	got, err := s.DailyUptime(t.Context(), []model.ID{m.ID}, from, to)
	if err != nil {
		t.Fatalf("daily uptime: %v", err)
	}

	days := got[m.ID]
	if len(days) == 0 {
		t.Fatal("no days returned: the bar would render empty")
	}
	last := days[len(days)-1]
	if want := now.Truncate(24 * time.Hour); !last.Date.Equal(want) {
		t.Fatalf("newest day = %s, want today (%s)", last.Date, want)
	}
	if last.Ratio == nil || *last.Ratio != 0.75 {
		t.Errorf("today's ratio = %v, want 0.75", last.Ratio)
	}
}

// Days the tier has answered are left alone. It matters at the edge of raw
// retention, where the tier's row is complete and the raw rows behind it have
// been half deleted — taking raw there would report an outage that was really a
// deletion.
func TestDailyUptimePrefersTheRolledUpDay(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("yesterday")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC()
	yesterday := now.Truncate(24*time.Hour).AddDate(0, 0, -1)

	writeBeats(t, s, m, yesterday.Add(time.Hour), time.Minute,
		[]model.Heartbeat{{Status: model.StatusUp}, {Status: model.StatusUp}})
	rollUpClosed(t, s, now, 72*time.Hour)

	// Arrives after the rollup and stays unrolled: the tier still says 100%.
	writeBeats(t, s, m, yesterday.Add(2*time.Hour), time.Minute,
		[]model.Heartbeat{{Status: model.StatusDown}, {Status: model.StatusDown}})

	from, to := barWindow(now, 90)
	got, err := s.DailyUptime(t.Context(), []model.ID{m.ID}, from, to)
	if err != nil {
		t.Fatalf("daily uptime: %v", err)
	}

	var found bool
	for _, day := range got[m.ID] {
		if !day.Date.Equal(yesterday) {
			continue
		}
		found = true
		if day.Ratio == nil || *day.Ratio != 1 {
			t.Errorf("yesterday's ratio = %v, want 1 (the tier's own figure)", day.Ratio)
		}
	}
	if !found {
		t.Fatal("yesterday missing from the bar")
	}
}

// A day nothing observed stays absent, so the caller can draw it as a gap. This
// is the null that must survive to the page: filling it with zero draws an
// outage that never happened.
func TestDailyUptimeLeavesUnobservedDaysOut(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("gap")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC()
	writeBeats(t, s, m, now.Truncate(24*time.Hour).Add(time.Minute), time.Minute,
		[]model.Heartbeat{{Status: model.StatusUp}})
	rollUpClosed(t, s, now, 72*time.Hour)

	from, to := barWindow(now, 90)
	got, err := s.DailyUptime(t.Context(), []model.ID{m.ID}, from, to)
	if err != nil {
		t.Fatalf("daily uptime: %v", err)
	}
	if len(got[m.ID]) != 1 {
		t.Fatalf("days = %d, want 1: only the day with observations", len(got[m.ID]))
	}
}
