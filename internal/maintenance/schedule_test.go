package maintenance

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

func at(t *testing.T, layout, value, zone string) time.Time {
	t.Helper()

	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("zone %s: %v", zone, err)
	}
	parsed, err := time.ParseInLocation(layout, value, location)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

func local(t *testing.T, value, zone string) time.Time {
	t.Helper()
	return at(t, "2006-01-02 15:04", value, zone)
}

func utc(t *testing.T, value string) time.Time {
	t.Helper()
	return at(t, "2006-01-02 15:04", value, "UTC")
}

func window(strategy string) model.MaintenanceWindow {
	return model.MaintenanceWindow{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Title: "Patching", Strategy: strategy, Timezone: "UTC",
		SuppressNotifications: true,
	}
}

func next(t *testing.T, w model.MaintenanceWindow, after time.Time) Occurrence {
	t.Helper()

	occurrence, ok, err := Next(w, after)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !ok {
		t.Fatalf("no occurrence after %s", after)
	}
	return occurrence
}

func TestSingleWindow(t *testing.T) {
	t.Parallel()

	ends := utc(t, "2026-08-19 03:00")
	w := window(model.StrategySingle)
	w.StartsAt = utc(t, "2026-08-19 02:00")
	w.EndsAt = &ends

	cases := []struct {
		name  string
		at    time.Time
		state string
	}{
		{"before", utc(t, "2026-08-19 01:59"), model.MaintenanceScheduled},
		{"at the start", utc(t, "2026-08-19 02:00"), model.MaintenanceActive},
		{"inside", utc(t, "2026-08-19 02:30"), model.MaintenanceActive},
		// End-exclusive, matching the half-open convention the rollup buckets
		// use. One convention for intervals, not two.
		{"at the end", utc(t, "2026-08-19 03:00"), model.MaintenanceEnded},
		{"after", utc(t, "2026-08-19 04:00"), model.MaintenanceEnded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := State(w, tc.at); got != tc.state {
				t.Errorf("state at %s = %s, want %s", tc.at, got, tc.state)
			}
		})
	}
}

func TestSingleWindowFromDuration(t *testing.T) {
	t.Parallel()

	w := window(model.StrategySingle)
	w.StartsAt = utc(t, "2026-08-19 02:00")
	w.Duration = 90 * time.Minute

	occurrence := next(t, w, utc(t, "2026-08-19 00:00"))
	if !occurrence.End.Equal(utc(t, "2026-08-19 03:30")) {
		t.Errorf("end = %s", occurrence.End)
	}
}

// The headline claim: "02:00 every Sunday" survives a daylight-saving
// transition still meaning 02:00 local. An offset cannot express that, which is
// why the column holds an IANA name.
func TestRecurrenceFollowsLocalTimeAcrossDST(t *testing.T) {
	t.Parallel()

	// Sydney leaves daylight saving on 2026-04-05 and enters it on 2026-10-04.
	w := window(model.StrategyRecurringDaily)
	w.Timezone = "Australia/Sydney"
	w.StartsAt = local(t, "2026-03-01 02:00", "Australia/Sydney")
	w.Duration = time.Hour

	summer := next(t, w, local(t, "2026-03-10 00:00", "Australia/Sydney"))
	winter := next(t, w, local(t, "2026-06-10 00:00", "Australia/Sydney"))

	sydney, _ := time.LoadLocation("Australia/Sydney")
	if hour := summer.Start.In(sydney).Hour(); hour != 2 {
		t.Errorf("summer occurrence starts at %02d:00 local, want 02:00", hour)
	}
	if hour := winter.Start.In(sydney).Hour(); hour != 2 {
		t.Errorf("winter occurrence starts at %02d:00 local, want 02:00", hour)
	}

	// And the two are at different UTC offsets, which is the whole point — a
	// stored offset would have made one of them wrong.
	_, summerOffset := summer.Start.In(sydney).Zone()
	_, winterOffset := winter.Start.In(sydney).Zone()
	if summerOffset == winterOffset {
		t.Fatalf("both occurrences are at offset %d; the test is not exercising a transition", summerOffset)
	}
}

func TestRecurringWeeklyOnlyOnItsDays(t *testing.T) {
	t.Parallel()

	w := window(model.StrategyRecurringWeekly)
	w.StartsAt = utc(t, "2026-08-01 02:00")
	w.Duration = time.Hour
	w.Recurrence.Weekdays = []int{0, 3} // Sunday and Wednesday

	// 2026-08-19 is a Wednesday.
	occurrence := next(t, w, utc(t, "2026-08-17 00:00"))
	if occurrence.Start.Weekday() != time.Wednesday {
		t.Errorf("first occurrence is a %s", occurrence.Start.Weekday())
	}
	if !occurrence.Start.Equal(utc(t, "2026-08-19 02:00")) {
		t.Errorf("start = %s", occurrence.Start)
	}

	following := next(t, w, occurrence.End)
	if following.Start.Weekday() != time.Sunday {
		t.Errorf("second occurrence is a %s, want Sunday", following.Start.Weekday())
	}
}

// A day past the end of a short month is skipped, not clamped. "The 31st"
// meaning the 28th of February is a guess about intent, and a maintenance
// window is a poor place to guess.
func TestRecurringMonthlySkipsShortMonths(t *testing.T) {
	t.Parallel()

	w := window(model.StrategyRecurringMonthly)
	w.StartsAt = utc(t, "2026-01-31 02:00")
	w.Duration = time.Hour
	w.Recurrence.DaysOfMonth = []int{31}

	occurrence := next(t, w, utc(t, "2026-02-01 00:00"))
	if !occurrence.Start.Equal(utc(t, "2026-03-31 02:00")) {
		t.Errorf("after January the next 31st is %s, want 2026-03-31 (February is skipped, not clamped)",
			occurrence.Start)
	}
}

func TestUntilStopsTheRecurrence(t *testing.T) {
	t.Parallel()

	until := utc(t, "2026-08-21 00:00")
	w := window(model.StrategyRecurringDaily)
	w.StartsAt = utc(t, "2026-08-19 02:00")
	w.Duration = time.Hour
	w.Recurrence.Until = &until

	// until falls before the 21st's 02:00 occurrence, so the 20th is the last
	// one. "Stop recurring after this instant" is about the occurrence's start,
	// not about the day it falls on.
	last, ok, err := Next(w, utc(t, "2026-08-19 12:00"))
	if err != nil || !ok {
		t.Fatalf("the 20th should occur: ok=%v err=%v", ok, err)
	}
	if !last.Start.Equal(utc(t, "2026-08-20 02:00")) {
		t.Errorf("last occurrence starts %s", last.Start)
	}
	if _, ok, err := Next(w, utc(t, "2026-08-20 12:00")); err != nil || ok {
		t.Errorf("recurrence continued past until: ok=%v err=%v", ok, err)
	}
	if got := State(w, utc(t, "2026-08-22 00:00")); got != model.MaintenanceEnded {
		t.Errorf("state = %s, want ended", got)
	}
}

func TestCancelledWindowNeverOccurs(t *testing.T) {
	t.Parallel()

	cancelled := utc(t, "2026-08-18 00:00")
	w := window(model.StrategyRecurringDaily)
	w.StartsAt = utc(t, "2026-08-19 02:00")
	w.Duration = time.Hour
	w.CancelledAt = &cancelled

	if _, ok, _ := Next(w, utc(t, "2026-08-19 02:30")); ok {
		t.Error("a cancelled window produced an occurrence")
	}
	if got := State(w, utc(t, "2026-08-19 02:30")); got != model.MaintenanceCancelled {
		t.Errorf("state = %s", got)
	}
}

// An occurrence already running is more urgent than the next one to start, so
// Next returns it rather than skipping ahead.
func TestNextReturnsTheOccurrenceInProgress(t *testing.T) {
	t.Parallel()

	w := window(model.StrategyRecurringDaily)
	w.StartsAt = utc(t, "2026-08-01 23:00")
	w.Duration = 3 * time.Hour // crosses midnight

	occurrence := next(t, w, utc(t, "2026-08-20 00:30"))
	if !occurrence.Start.Equal(utc(t, "2026-08-19 23:00")) {
		t.Errorf("start = %s, want yesterday's occurrence which is still running", occurrence.Start)
	}
	if !occurrence.Covers(utc(t, "2026-08-20 00:30")) {
		t.Error("the returned occurrence does not cover the instant asked about")
	}
}

func TestActiveAt(t *testing.T) {
	t.Parallel()

	w := window(model.StrategyRecurringDaily)
	w.StartsAt = utc(t, "2026-08-01 02:00")
	w.Duration = time.Hour

	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{utc(t, "2026-08-19 01:59"), false},
		{utc(t, "2026-08-19 02:00"), true},
		{utc(t, "2026-08-19 02:59"), true},
		{utc(t, "2026-08-19 03:00"), false},
	} {
		got, err := ActiveAt(w, tc.at)
		if err != nil {
			t.Fatalf("active: %v", err)
		}
		if got != tc.want {
			t.Errorf("active at %s = %v, want %v", tc.at, got, tc.want)
		}
	}
}

func TestUnknownTimezoneIsAnError(t *testing.T) {
	t.Parallel()

	w := window(model.StrategyRecurringDaily)
	w.Timezone = "Mars/Olympus_Mons"
	w.StartsAt = utc(t, "2026-08-19 02:00")
	w.Duration = time.Hour

	if _, _, err := Next(w, utc(t, "2026-08-19 00:00")); err == nil {
		t.Error("an unknown zone was accepted")
	}
}

// The embedded zone database is the reason this works on a FROM scratch image.
// Asserting a non-UTC zone loads is asserting that the import is still there.
func TestZoneDatabaseIsEmbedded(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"Australia/Sydney", "Europe/London", "America/New_York"} {
		if _, err := time.LoadLocation(name); err != nil {
			t.Errorf("%s does not load: the tzdata import has gone missing (%v)", name, err)
		}
	}
}
