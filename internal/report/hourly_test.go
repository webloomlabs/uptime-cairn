package report

import (
	"context"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// hourBucket is one hour of the 1h tier, in the shape the store returns.
func hourBucket(base time.Time, n, up, down int, sum float64, count int) store.HistoryBucket {
	return store.HistoryBucket{
		Start:             base.Add(time.Duration(n) * time.Hour),
		Up:                up,
		Down:              down,
		ResponseTimeSum:   sum,
		ResponseTimeCount: count,
	}
}

// A report covering one day draws a strip of one cell and a line of one point
// from the daily series — a picture of a number printed directly beneath it.
// The hourly series is what makes those two exhibits carry anything, and this is
// the read that supplies it.
func TestADailyReportCarriesAnHourlySeries(t *testing.T) {
	t.Parallel()

	m := monitorNamed("checkout")
	sydney := mustLoad(t, "Australia/Sydney")
	// The day the report covers: 3 September in Sydney, cut in Sydney.
	base := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC) // = 4 Sep 00:00 +10:00

	var hourly []store.HistoryBucket
	for i := 0; i < 24; i++ {
		hourly = append(hourly, hourBucket(base, i, 60, 0, 60*600, 60))
	}

	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 1440, ResponseTimeSum: 1440 * 600, ResponseTimeCount: 1440}},
		series:   map[model.ID][]store.HistoryBucket{m.ID: {day(0, 1440*600, 1440)}},
		hourly:   map[model.ID][]store.HistoryBucket{m.ID: hourly},
		targets:  map[model.ID]Target{},
	}

	doc, err := Build(context.Background(), f, Spec{
		Period:              PeriodDay,
		PeriodStyle:         StyleCalendar,
		Timezone:            "Australia/Sydney",
		MaintenanceHandling: MaintenanceExclude,
	}, defaultRetention(), model.NewID(), time.Date(2026, 9, 4, 9, 0, 0, 0, sydney))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if len(doc.Monitors) != 1 {
		t.Fatalf("sections = %d, want 1", len(doc.Monitors))
	}
	section := doc.Monitors[0]
	if len(section.Hourly) != 24 {
		t.Fatalf("hourly buckets = %d, want 24", len(section.Hourly))
	}
	if section.Hourly[0].AverageMs == nil || *section.Hourly[0].AverageMs != 600 {
		t.Errorf("first hour average = %v, want 600", section.Hourly[0].AverageMs)
	}
	if section.Hourly[0].Uptime.Ratio == nil || *section.Hourly[0].Uptime.Ratio != 1 {
		t.Errorf("first hour uptime = %v, want 1", section.Hourly[0].Uptime.Ratio)
	}

	// The published series is untouched. `daily` is one point per day typed
	// `format: date` in the frozen spec, and the hourly exhibit is a second
	// drawing of the same window rather than a finer reading of that field.
	if len(section.DailyUptime) != 1 || len(section.ResponseTime.Daily) != 1 {
		t.Errorf("daily series changed: %d uptime days, %d latency days, want 1 and 1",
			len(section.DailyUptime), len(section.ResponseTime.Daily))
	}
}

// An hour with no successful check is a gap, exactly as a day is. Zero would
// draw the service answering instantly during a window nobody observed.
func TestAnUnobservedHourIsAGapRatherThanZero(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	base := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 60, ResponseTimeSum: 36000, ResponseTimeCount: 60}},
		series:   map[model.ID][]store.HistoryBucket{m.ID: {day(0, 36000, 60)}},
		hourly: map[model.ID][]store.HistoryBucket{m.ID: {
			hourBucket(base, 0, 60, 0, 36000, 60),
			{Start: base.Add(time.Hour), Unknown: 60},
		}},
		targets: map[model.ID]Target{},
	}

	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodDay, PeriodStyle: StyleCalendar, Timezone: "UTC",
		MaintenanceHandling: MaintenanceExclude,
	}, defaultRetention(), model.NewID(), time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	gap := doc.Monitors[0].Hourly[1]
	if gap.AverageMs != nil {
		t.Errorf("unobserved hour has an average of %v, want none", *gap.AverageMs)
	}
	if gap.Uptime.Ratio != nil {
		t.Errorf("unobserved hour has an uptime of %v, want none", *gap.Uptime.Ratio)
	}
}

// The four-reads property is what fifty concurrent runs on the first of the
// month rest on. The hourly series is a fifth read and must never appear on the
// window those runs cover.
func TestAMonthlyReportDoesNotPayForTheHourlySeries(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 100}},
		series:   map[model.ID][]store.HistoryBucket{},
		targets:  map[model.ID]Target{},
	}

	if _, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := f.calls["HourlySeries"]; got != 0 {
		t.Errorf("HourlySeries called %d times on a monthly report, want 0", got)
	}
	if got := f.calls["DailySeries"]; got != 1 {
		t.Errorf("DailySeries called %d times, want 1", got)
	}
}

// A window short enough to want hourly cells can still have no hourly data
// behind it. Where retention has left only the daily tier, asking would return
// an empty map and draw nothing; not asking says the same thing without the
// round trip.
func TestNoHourlySeriesWhereOnlyTheDailyTierSurvives(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 100}},
		series:   map[model.ID][]store.HistoryBucket{},
		targets:  map[model.ID]Target{},
	}

	// Every sub-daily tier pruned to nothing, which is a legal retention policy
	// and the one an operator short of disk arrives at.
	retention := Retention{RawDays: 1, Rollup1mDays: 1, Rollup5mDays: 1, Rollup1hDays: 1, Rollup1dDays: 0}
	now := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)

	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodDay, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, retention, model.NewID(), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if doc.Meta.Resolution.Tier != "1d" {
		t.Fatalf("tier = %q; this test needs the daily tier to be what answered", doc.Meta.Resolution.Tier)
	}
	if got := f.calls["HourlySeries"]; got != 0 {
		t.Errorf("HourlySeries called %d times with no sub-daily tier left, want 0", got)
	}
}

// The boundary is a property of the window rather than of the period name: a
// custom two-day window has the same one-or-two-cell chart a daily report has,
// and a three-day one does not.
func TestTheHourlySeriesFollowsTheWindowRatherThanThePeriodName(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		span time.Duration
		want bool
	}{
		{"six hours", 6 * time.Hour, true},
		{"one day", 24 * time.Hour, true},
		{"two days", 48 * time.Hour, true},
		{"three days", 72 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := monitorNamed("api")
			f := &fakeStore{
				monitors: []model.Monitor{m},
				totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 100}},
				series:   map[model.ID][]store.HistoryBucket{},
				targets:  map[model.ID]Target{},
			}
			to := now.Truncate(time.Hour)
			if _, err := Build(context.Background(), f, Spec{
				Period: PeriodCustom, Timezone: "UTC", From: to.Add(-tc.span), To: to,
			}, defaultRetention(), model.NewID(), now); err != nil {
				t.Fatalf("build: %v", err)
			}
			if got := f.calls["HourlySeries"] == 1; got != tc.want {
				t.Errorf("hourly series read = %v over %s, want %v", got, tc.span, tc.want)
			}
		})
	}
}
