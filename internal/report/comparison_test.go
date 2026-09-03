package report

import (
	"context"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// A comparative report compares the window against the same length before it,
// and the current period comes first.
//
// Two decisions in one assertion. The **previous window is the same duration
// placed immediately before**, not the previous calendar period: February beside
// March would put 28 days against 31, and every count would differ for reasons
// that are about the calendar rather than about the service. And the **current
// period leads**, which is the opposite of chronological order and the right way
// round for a document whose subject is the period it covers — a reader scanning
// a table should not have to check dates to find out which column is now.
func TestPreviousPeriodIsTheSameLengthImmediatelyBefore(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 990, Down: 10}},
	}

	window := Window{
		From:     time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		Timezone: "UTC",
	}

	comparison, err := BuildComparison(context.Background(), f,
		ComparisonSpec{Mode: CompareToPreviousPeriod}, Scope{}, window, "1d", MaintenanceExclude)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if comparison == nil || len(comparison.Series) != 2 {
		t.Fatalf("comparison = %+v, want two series", comparison)
	}

	if comparison.Series[0].Label != "This period" {
		t.Errorf("first series = %q, want the current period leading", comparison.Series[0].Label)
	}
	current, before := comparison.Series[0], comparison.Series[1]
	if current.PeriodStart == nil || !current.PeriodStart.Equal(window.From) {
		t.Errorf("current period starts %v, want %v", current.PeriodStart, window.From)
	}
	// 31 days back from 1 March, not 1 February — a same-length window rather
	// than the previous calendar month.
	wantFrom := window.From.Add(-window.To.Sub(window.From))
	if before.PeriodStart == nil || !before.PeriodStart.Equal(wantFrom) {
		t.Errorf("previous period starts %v, want %v — the same length placed "+
			"immediately before, so the counts are comparable", before.PeriodStart, wantFrom)
	}
	if before.PeriodEnd == nil || !before.PeriodEnd.Equal(window.From) {
		t.Errorf("previous period ends %v, want it to abut the current one", before.PeriodEnd)
	}
}

// Comparing entities costs two batched reads per series, not two per monitor.
//
// The property the extended load gate measures. A comparative report over a
// large estate that fanned out per monitor would be invisible in every result and
// fatal in the gate.
func TestComparingEntitiesStaysBatched(t *testing.T) {
	t.Parallel()

	first, second := monitorNamed("api"), monitorNamed("web")
	f := &fakeStore{
		monitors: []model.Monitor{first},
		totals:   map[model.ID]store.HistoryBucket{first.ID: {Up: 100}},
	}

	window := Window{
		From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	}
	comparison, err := BuildComparison(context.Background(), f,
		ComparisonSpec{Mode: CompareMonitors, MonitorIDs: []model.ID{first.ID, second.ID}},
		Scope{}, window, "1d", MaintenanceExclude)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(comparison.Series) != 2 {
		t.Fatalf("%d series, want 2", len(comparison.Series))
	}
	// One scope resolution and one totals read per series, and no more: two
	// entities, two of each.
	if f.calls["WindowTotals"] != 2 {
		t.Errorf("WindowTotals called %d times for two series, want 2", f.calls["WindowTotals"])
	}
	if f.calls["MonitorsInScope"] != 2 {
		t.Errorf("MonitorsInScope called %d times, want 2", f.calls["MonitorsInScope"])
	}
	// The label is the monitor's name where the entity resolves to exactly one.
	if comparison.Series[0].Label != "api" {
		t.Errorf("label = %q, want the monitor's name", comparison.Series[0].Label)
	}
}

// A comparative template naming nothing produces an empty block rather than an
// error, following the empty-scope rule one layer up: a client whose compared
// monitors were all deleted still gets a report saying so, which beats a failed
// run nobody looks at until the invoice goes out.
func TestComparingNothingIsADocumentNotAnError(t *testing.T) {
	t.Parallel()

	f := &fakeStore{}
	comparison, err := BuildComparison(context.Background(), f,
		ComparisonSpec{Mode: CompareGroups}, Scope{}, Window{}, "1d", MaintenanceExclude)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if comparison == nil || len(comparison.Series) != 0 {
		t.Errorf("comparison = %+v, want an empty block", comparison)
	}
}

// An unknown mode is refused by name rather than silently producing nothing.
//
// Region-against-region is the mode somebody will try, and it is Phase 4. Being
// told the mode is not recognised is a better answer than a report with no
// comparison on it.
func TestAnUnknownComparisonModeIsRefusedByName(t *testing.T) {
	t.Parallel()

	_, err := BuildComparison(context.Background(), &fakeStore{},
		ComparisonSpec{Mode: "regions"}, Scope{}, Window{}, "1d", MaintenanceExclude)
	if err == nil {
		t.Fatal("an unknown comparison mode was accepted")
	}
}

// **An uptime report carries no SLO vocabulary, even where the monitors have
// targets.**
//
// That is what makes it the default a solo user gets. It has a second use worth
// stating: an agency running an uptime summary for a client does not put the
// internal target it set on that client's monitors onto the client's document.
// Choosing the type is the choice; a target set for the agency's own dashboards
// is not a decision to publish it.
func TestAnUptimeReportOmitsTheSLABlockEvenWithATarget(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 990, Down: 10}},
		targets:  map[model.ID]Target{m.ID: {Percent: 99.9, Source: TargetFromMonitor}},
	}
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	uptime, err := Build(context.Background(), f, Spec{
		Type: model.ReportTypeUptime, Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if uptime.Monitors[0].SLA != nil {
		t.Errorf("an uptime report carried an SLA block: %+v. The target belongs to "+
			"whoever set it, and choosing this type is a decision not to publish it",
			uptime.Monitors[0].SLA)
	}

	// The same monitor under an sla template does get one, so the omission above
	// is about the type rather than about the fixture.
	sla, err := Build(context.Background(), f, Spec{
		Type: model.ReportTypeSLA, Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if sla.Monitors[0].SLA == nil {
		t.Fatal("an sla report over a monitor with a target has no SLA block")
	}
}

// Only the types that want an incident log pay for one.
//
// It is a fifth read, and the four-reads-whatever-the-scope property the load
// gate measures is worth keeping for the two types that make up almost every
// scheduled report.
func TestOnlyThePostMortemReadsTheIncidentLog(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		reportType string
		wantReads  int
	}{
		{model.ReportTypeUptime, 0},
		{model.ReportTypeSLA, 0},
		{model.ReportTypePostMortem, 1},
		{model.ReportTypeCustom, 1},
	} {
		m := monitorNamed("api")
		f := &fakeStore{
			monitors: []model.Monitor{m},
			totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 100}},
		}
		if _, err := Build(context.Background(), f, Spec{
			Type: tc.reportType, Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
		}, defaultRetention(), model.NewID(), now); err != nil {
			t.Fatalf("%s: build: %v", tc.reportType, err)
		}
		if got := f.calls["ListIncidents"]; got != tc.wantReads {
			t.Errorf("%s read the incident log %d times, want %d", tc.reportType, got, tc.wantReads)
		}
	}
}
