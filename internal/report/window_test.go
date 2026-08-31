package report

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

// The rule the plan says will be reported as a bug if it is wrong: a monthly
// report for an Australian agency starts at midnight in Sydney, not at midnight
// UTC — which is 11 hours and one working day apart.
func TestMonthlyWindowIsCutInTheStatedZone(t *testing.T) {
	t.Parallel()

	sydney := mustLoad(t, "Australia/Sydney")
	// 09:00 on 1 April in Sydney, which is 22:00 on 31 March in UTC.
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, sydney)

	w, err := ResolveWindow(PeriodMonth, StyleCalendar, sydney, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	wantFrom := time.Date(2026, 3, 1, 0, 0, 0, 0, sydney)
	wantTo := time.Date(2026, 4, 1, 0, 0, 0, 0, sydney)
	if !w.From.Equal(wantFrom) || !w.To.Equal(wantTo) {
		t.Fatalf("window = %s–%s, want %s–%s", w.From, w.To, wantFrom, wantTo)
	}
	// The same boundary in UTC is the previous day mid-evening. If the window
	// had been cut in UTC it would have started here and swallowed 28 February.
	if got := w.From.UTC(); !got.Equal(time.Date(2026, 2, 28, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("UTC instant = %s, want 2026-02-28T13:00Z", got)
	}
	if w.Timezone != "Australia/Sydney" {
		t.Errorf("timezone = %q, want Australia/Sydney — it cannot be recovered from the instants", w.Timezone)
	}
}

// A calendar window is the last *complete* period, so the same definition run
// twice in the same month covers the same month both times. That is what makes a
// scheduled report re-runnable and an invoice attachment stable.
func TestCalendarMonthIsStableWithinTheMonth(t *testing.T) {
	t.Parallel()

	utc := time.UTC
	early, err := ResolveWindow(PeriodMonth, StyleCalendar, utc, time.Date(2026, 4, 1, 0, 5, 0, 0, utc))
	if err != nil {
		t.Fatal(err)
	}
	late, err := ResolveWindow(PeriodMonth, StyleCalendar, utc, time.Date(2026, 4, 28, 23, 0, 0, 0, utc))
	if err != nil {
		t.Fatal(err)
	}

	if !early.From.Equal(late.From) || !early.To.Equal(late.To) {
		t.Errorf("window moved within the month: %s–%s then %s–%s", early.From, early.To, late.From, late.To)
	}
}

// A daylight-saving transition makes a day 23 or 25 hours long, and the error
// budget is a proportion of the window's real length. Computing 30 × 24 hours
// would be an hour wrong twice a year in a number people check.
func TestWindowDurationFollowsDaylightSaving(t *testing.T) {
	t.Parallel()

	sydney := mustLoad(t, "Australia/Sydney")
	// April 2026 contains the end of Australian daylight saving: the clocks go
	// back, so the month is one hour longer than 30 days.
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, sydney)

	w, err := ResolveWindow(PeriodMonth, StyleCalendar, sydney, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := w.Duration(), 30*24*time.Hour+time.Hour; got != want {
		t.Errorf("April in Sydney = %v, want %v (the clocks go back)", got, want)
	}
}

// Weeks start on Monday, matching ISO-8601 and the invoices this feature exists
// to replace. Sunday is the trap: it is weekday 0 in Go and the *last* day of an
// ISO week.
func TestCalendarWeekStartsMonday(t *testing.T) {
	t.Parallel()

	utc := time.UTC
	// 5 April 2026 is a Sunday. The last complete week is 23–29 March.
	w, err := ResolveWindow(PeriodWeek, StyleCalendar, utc, time.Date(2026, 4, 5, 12, 0, 0, 0, utc))
	if err != nil {
		t.Fatal(err)
	}

	if w.From.Weekday() != time.Monday {
		t.Errorf("week starts on a %s, want Monday", w.From.Weekday())
	}
	if !w.From.Equal(time.Date(2026, 3, 23, 0, 0, 0, 0, utc)) {
		t.Errorf("from = %s, want 23 March — a Sunday belongs to the week that just ended", w.From)
	}
	if w.Duration() != 7*24*time.Hour {
		t.Errorf("duration = %v, want 168h", w.Duration())
	}
}

// Calendar quarters, and the fiscal year is deliberately not guessed at.
func TestCalendarQuarter(t *testing.T) {
	t.Parallel()

	utc := time.UTC
	for _, tc := range []struct {
		now      time.Time
		from, to time.Time
	}{
		{time.Date(2026, 4, 2, 0, 0, 0, 0, utc), time.Date(2026, 1, 1, 0, 0, 0, 0, utc), time.Date(2026, 4, 1, 0, 0, 0, 0, utc)},
		{time.Date(2026, 1, 15, 0, 0, 0, 0, utc), time.Date(2025, 10, 1, 0, 0, 0, 0, utc), time.Date(2026, 1, 1, 0, 0, 0, 0, utc)},
		{time.Date(2026, 9, 30, 0, 0, 0, 0, utc), time.Date(2026, 4, 1, 0, 0, 0, 0, utc), time.Date(2026, 7, 1, 0, 0, 0, 0, utc)},
	} {
		w, err := ResolveWindow(PeriodQuarter, StyleCalendar, utc, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		if !w.From.Equal(tc.from) || !w.To.Equal(tc.to) {
			t.Errorf("from %s: window = %s–%s, want %s–%s", tc.now, w.From, w.To, tc.from, tc.to)
		}
	}
}

// Rolling counts back from the run time and therefore moves every run. Right for
// an operational review, wrong for an invoice — which is why the two styles
// exist and why the document records which was used.
func TestRollingCountsBackFromNow(t *testing.T) {
	t.Parallel()

	utc := time.UTC
	now := time.Date(2026, 4, 15, 14, 30, 0, 0, utc)
	w, err := ResolveWindow(PeriodMonth, StyleRolling, utc, now)
	if err != nil {
		t.Fatal(err)
	}

	if !w.To.Equal(now) {
		t.Errorf("to = %s, want the run time %s", w.To, now)
	}
	if !w.From.Equal(time.Date(2026, 3, 15, 14, 30, 0, 0, utc)) {
		t.Errorf("from = %s, want one month back to the minute", w.From)
	}
}

// A custom period needs dates and says so, rather than silently defaulting to a
// month somebody did not ask for.
func TestCustomAndUnknownPeriodsAreRefusedWithAReason(t *testing.T) {
	t.Parallel()

	if _, err := ResolveWindow(PeriodCustom, StyleCalendar, time.UTC, time.Now()); err == nil {
		t.Error("custom period accepted without dates")
	}
	if _, err := ResolveWindow("fortnight", StyleCalendar, time.UTC, time.Now()); err == nil {
		t.Error("unknown period accepted")
	}
	if _, err := ResolveWindow(PeriodMonth, StyleCalendar, nil, time.Now()); err == nil {
		t.Error("nil location accepted; a window cut in no stated zone is the bug this guards")
	}
}

// §3.2: retention limits resolution, not existence. A month inside 1m retention
// answers at 1m; last March does not, and comes back at a coarser tier *labelled
// as coarser* rather than silently upsampled.
func TestTierIsDowngradedRatherThanSilentlyCoarsened(t *testing.T) {
	t.Parallel()

	r := Retention{RawDays: 7, Rollup1mDays: 30, Rollup5mDays: 90, Rollup1hDays: 365, Rollup1dDays: 0}
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	recent := Window{From: now.AddDate(0, 0, -20), To: now}
	if got := ResolveTier("1m", recent, now, r); got.Tier != "1m" || got.Downgraded {
		t.Errorf("recent month = %+v, want 1m undowngraded", got)
	}

	// Thirteen months back: past 1m, 5m and 1h retention. Only the daily tier,
	// which is kept indefinitely, still holds it.
	old := Window{From: now.AddDate(-1, -1, 0), To: now.AddDate(-1, 0, 0)}
	got := ResolveTier("1m", old, now, r)
	if got.Tier != "1d" {
		t.Errorf("tier = %q, want 1d", got.Tier)
	}
	if !got.Downgraded {
		t.Error("downgrade not reported; the report would present daily figures as minute ones")
	}
	if got.RequestedTier != "1m" {
		t.Errorf("requested tier = %q, want it preserved for the label", got.RequestedTier)
	}
}

// Where even the daily tier has been pruned, the report states the range it
// actually covered rather than returning figures that quietly omit the start of
// the window.
func TestTruncatedHistoryReportsWhereTheDataStarts(t *testing.T) {
	t.Parallel()

	r := Retention{RawDays: 1, Rollup1mDays: 7, Rollup5mDays: 14, Rollup1hDays: 30, Rollup1dDays: 90}
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	old := Window{From: now.AddDate(-1, 0, 0), To: now.AddDate(0, -11, 0)}

	got := ResolveTier("auto", old, now, r)
	if got.Tier != "1d" {
		t.Errorf("tier = %q, want 1d", got.Tier)
	}
	if got.CoveredFrom == nil {
		t.Fatal("covered_from is nil; the report would claim a year it does not have")
	}
	if !got.CoveredFrom.Equal(now.AddDate(0, 0, -90)) {
		t.Errorf("covered from %s, want 90 days back", got.CoveredFrom)
	}
}

// The percentile gate has a policy half as well as a data half: raw_days below
// seven means the figure is omitted, and both checks have to pass.
func TestRawRetentionGate(t *testing.T) {
	t.Parallel()

	if (Retention{RawDays: 3}).RawCoversTrailingWeek() {
		t.Error("three days of raw reported as covering a seven-day percentile")
	}
	if !(Retention{RawDays: 7}).RawCoversTrailingWeek() {
		t.Error("exactly seven days should cover")
	}
	if !(Retention{RawDays: 0}).RawCoversTrailingWeek() {
		t.Error("indefinite raw retention should cover")
	}
}
