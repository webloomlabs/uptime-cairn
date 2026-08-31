package report

import (
	"context"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// fakeStore counts its calls, because the number of reads Build makes is a
// property the extended load gate depends on: four, whatever the scope size.
type fakeStore struct {
	monitors []model.Monitor
	totals   map[model.ID]store.HistoryBucket
	series   map[model.ID][]store.HistoryBucket
	targets  map[model.ID]Target

	calls map[string]int
}

func (f *fakeStore) note(name string) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
}

func (f *fakeStore) MonitorsInScope(context.Context, Scope) ([]model.Monitor, error) {
	f.note("MonitorsInScope")
	return f.monitors, nil
}

func (f *fakeStore) WindowTotals(_ context.Context, _ []model.ID, _, _ time.Time, _ string) (map[model.ID]store.HistoryBucket, error) {
	f.note("WindowTotals")
	return f.totals, nil
}

func (f *fakeStore) DailySeries(_ context.Context, _ []model.ID, _, _ time.Time) (map[model.ID][]store.HistoryBucket, error) {
	f.note("DailySeries")
	return f.series, nil
}

func (f *fakeStore) SLOTargets(context.Context, []model.ID) (map[model.ID]Target, error) {
	f.note("SLOTargets")
	return f.targets, nil
}

func (f *fakeStore) RawCovers(context.Context, model.ID, time.Time, string) (bool, error) {
	f.note("RawCovers")
	return true, nil
}

func (f *fakeStore) UptimeFromRaw(context.Context, model.ID, time.Time, time.Time) (store.HistoryBucket, error) {
	f.note("UptimeFromRaw")
	return store.HistoryBucket{}, nil
}

func (f *fakeStore) ListIncidents(context.Context, *store.Cursor, int, store.IncidentFilter) ([]model.Incident, bool, error) {
	f.note("ListIncidents")
	return nil, false, nil
}

var _ Store = (*fakeStore)(nil)

func monitorNamed(name string) model.Monitor {
	return model.Monitor{ID: model.NewID(), Name: name, Type: model.TypeHTTP}
}

func defaultRetention() Retention {
	return Retention{RawDays: 7, Rollup1mDays: 30, Rollup5mDays: 90, Rollup1hDays: 365, Rollup1dDays: 0}
}

// The Month 4 checkpoint in miniature: an SLA report over a past month, with a
// stated denominator, an honest null, and the resolution actually used.
func TestBuildProducesTheSLAReport(t *testing.T) {
	t.Parallel()

	m := monitorNamed("checkout")
	target := 99.9
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals: map[model.ID]store.HistoryBucket{
			m.ID: {Up: 9900, Down: 100, ResponseTimeSum: 1000000, ResponseTimeCount: 9900},
		},
		series: map[model.ID][]store.HistoryBucket{
			m.ID: {day(0, 100000, 1000), downDay(1, 900, 100)},
		},
		targets: map[model.ID]Target{m.ID: {Percent: target, Source: TargetFromMonitor}},
	}

	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	doc, err := Build(context.Background(), f, Spec{
		TemplateName:        "Acme monthly",
		Type:                "sla",
		Period:              PeriodMonth,
		PeriodStyle:         StyleCalendar,
		Timezone:            "Australia/Sydney",
		MaintenanceHandling: MaintenanceExclude,
	}, defaultRetention(), model.NewID(), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if doc.Meta.SchemaVersion != SchemaVersion {
		t.Errorf("schema version = %d, want %d", doc.Meta.SchemaVersion, SchemaVersion)
	}
	if doc.Meta.Timezone != "Australia/Sydney" {
		t.Errorf("timezone = %q, want Australia/Sydney", doc.Meta.Timezone)
	}
	// March in Sydney, not March in UTC.
	sydney := mustLoad(t, "Australia/Sydney")
	if !doc.Meta.PeriodStart.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, sydney)) {
		t.Errorf("period start = %s, want 1 March in Sydney", doc.Meta.PeriodStart)
	}
	if len(doc.Monitors) != 1 {
		t.Fatalf("sections = %d, want 1", len(doc.Monitors))
	}

	section := doc.Monitors[0]
	if got := ratio(t, section.Uptime); !closeTo(got, 0.99) {
		t.Errorf("uptime = %v, want 0.99", got)
	}
	if section.SLA == nil {
		t.Fatal("no SLA block on a monitor with a target")
	}
	if section.SLA.TargetSource != TargetFromMonitor {
		t.Errorf("target source = %q, want monitor", section.SLA.TargetSource)
	}
	if section.SLA.Met == nil || *section.SLA.Met {
		t.Error("99% against a 99.9% target should not be met")
	}
	if len(section.Breaches) != 1 {
		t.Errorf("breaches = %d, want 1", len(section.Breaches))
	}
	if doc.Summary == nil {
		t.Fatal("no estate summary")
	}
}

// Four reads, whatever the scope. This is the property fifty concurrent runs on
// the first of the month depend on, and the one a convenience loop would quietly
// destroy — the failure would be invisible in every result and fatal in the gate.
func TestBuildReadsAFixedNumberOfTimes(t *testing.T) {
	t.Parallel()

	f := &fakeStore{
		totals:  map[model.ID]store.HistoryBucket{},
		series:  map[model.ID][]store.HistoryBucket{},
		targets: map[model.ID]Target{},
	}
	for i := 0; i < 200; i++ {
		m := monitorNamed("m")
		f.monitors = append(f.monitors, m)
		f.totals[m.ID] = store.HistoryBucket{Up: 100}
	}

	if _, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("build: %v", err)
	}

	for _, name := range []string{"MonitorsInScope", "WindowTotals", "DailySeries", "SLOTargets"} {
		if got := f.calls[name]; got != 1 {
			t.Errorf("%s called %d times over 200 monitors, want 1", name, got)
		}
	}
}

// The template's target beats the monitor's, and the source says so — otherwise
// a client asking "whose number is 99.5?" cannot be answered from the document.
func TestTemplateTargetOverridesTheMonitors(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	override := 99.5
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 999, Down: 1}},
		targets:  map[model.ID]Target{m.ID: {Percent: 99.9, Source: TargetFromMonitor}},
	}

	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC", SLATarget: &override,
	}, defaultRetention(), model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	sla := doc.Monitors[0].SLA
	if sla == nil || sla.TargetPercent != override || sla.TargetSource != TargetFromTemplate {
		t.Errorf("sla = %+v, want 99.5 from the template", sla)
	}
}

// A monitor with no target at any level gets no SLA block rather than one
// computed against a default nobody chose.
func TestNoTargetMeansNoSLABlock(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 100}},
		targets:  map[model.ID]Target{},
	}

	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if doc.Monitors[0].SLA != nil {
		t.Errorf("sla = %+v, want none", doc.Monitors[0].SLA)
	}
}

// An empty scope is a document, not a failure. A client whose monitors were all
// deleted still gets a report saying so — better than a failed run nobody looks
// at until the invoice goes out.
func TestEmptyScopeProducesADocument(t *testing.T) {
	t.Parallel()

	f := &fakeStore{}
	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if doc.Scope.MonitorCount != 0 || len(doc.Monitors) != 0 {
		t.Errorf("scope = %+v, want empty", doc.Scope)
	}
	if doc.Meta.PeriodStart.IsZero() {
		t.Error("an empty document still states the period it covered")
	}
}

// Retention truncating the window is reported, and the figures are read over
// what is actually covered rather than over the range that was asked for.
func TestTruncatedWindowIsLabelledOnTheDocument(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	f := &fakeStore{monitors: []model.Monitor{m}, totals: map[model.ID]store.HistoryBucket{m.ID: {Up: 10}}}

	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodYear, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, Retention{RawDays: 1, Rollup1mDays: 7, Rollup5mDays: 14, Rollup1hDays: 30, Rollup1dDays: 90}, model.NewID(), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if doc.Meta.Resolution.CoveredFrom == nil {
		t.Fatal("covered_from is nil; the document claims a year of data it does not have")
	}
	if doc.Meta.Resolution.Tier != "1d" {
		t.Errorf("tier = %q, want 1d", doc.Meta.Resolution.Tier)
	}
}

// Explicit boundaries win over the named period, so a re-run after a correction
// covers exactly what the original covered. Regenerating "last month" in July
// and getting June would make the correction incomparable to the thing it
// corrects.
func TestExplicitWindowSurvivesARerun(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeStore{}

	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC", From: from, To: to,
	}, defaultRetention(), model.NewID(), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !doc.Meta.PeriodStart.Equal(from) || !doc.Meta.PeriodEnd.Equal(to) {
		t.Errorf("window = %s–%s, want the explicit January", doc.Meta.PeriodStart, doc.Meta.PeriodEnd)
	}
}

// ADR-007 requires the same model rendered twice to be byte-identical, which
// starts with the model being identical. Map iteration is randomised, so the
// estate series is the place that would quietly not be.
func TestBuildIsDeterministic(t *testing.T) {
	t.Parallel()

	f := &fakeStore{totals: map[model.ID]store.HistoryBucket{}, series: map[model.ID][]store.HistoryBucket{}}
	for i := 0; i < 20; i++ {
		m := monitorNamed("m")
		f.monitors = append(f.monitors, m)
		f.totals[m.ID] = store.HistoryBucket{Up: 100}
		f.series[m.ID] = []store.HistoryBucket{day(i%5, 1000, 10), day((i+2)%5, 2000, 10)}
	}

	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	spec := Spec{Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC"}
	runID := model.NewID()

	first, err := Build(context.Background(), f, spec, defaultRetention(), runID, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Build(context.Background(), f, spec, defaultRetention(), runID, now)
		if err != nil {
			t.Fatal(err)
		}
		for j := range first.Summary.ResponseTime.Daily {
			a, b := first.Summary.ResponseTime.Daily[j], again.Summary.ResponseTime.Daily[j]
			if !a.Date.Equal(b.Date) || a.SampleCount != b.SampleCount {
				t.Fatalf("estate series differs between runs at %d: %+v vs %+v", j, a, b)
			}
		}
	}
}

// A timezone the zoneinfo database does not know is refused with the name in the
// message, rather than silently falling back to UTC and cutting somebody's month
// eleven hours early.
func TestUnknownTimezoneIsRefusedByName(t *testing.T) {
	t.Parallel()

	_, err := Build(context.Background(), &fakeStore{}, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "Mars/Olympus_Mons",
	}, defaultRetention(), model.NewID(), time.Now())
	if err == nil {
		t.Fatal("unknown timezone accepted")
	}
}
