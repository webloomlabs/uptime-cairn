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

	// p95 is what UptimeFromRaw reports, and rawFrom/rawTo capture the window it
	// was asked for — which is a fact the document does not expose but the
	// contract depends on.
	p95            *float64
	rawFrom, rawTo time.Time

	// rawShort makes RawCovers answer false, which is the per-monitor half of
	// the trailing-week gate: retention policy can permit the figure while this
	// particular monitor's raw rows have already been pruned behind the daily
	// tier that summarised them.
	rawShort bool

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
	return !f.rawShort, nil
}

func (f *fakeStore) UptimeFromRaw(_ context.Context, _ model.ID, from, to time.Time) (store.HistoryBucket, error) {
	f.note("UptimeFromRaw")
	f.rawFrom, f.rawTo = from, to
	return store.HistoryBucket{ResponseTimeP95: f.p95}, nil
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

// The percentile is computed for a report small enough to afford it, and it
// carries its own window and method rather than sitting unlabelled beside a
// month-long average.
func TestP95IsComputedForASmallScope(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	value := 940.0
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 100}},
		p95:      &value,
	}

	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	p := doc.Monitors[0].ResponseTime.P95
	if p == nil || !p.Available || p.ValueMs == nil || *p.ValueMs != 940 {
		t.Fatalf("p95 = %+v, want 940 available", p)
	}
	if p.Method != MethodNearestRank {
		t.Errorf("method = %q, want %q", p.Method, MethodNearestRank)
	}

	// The last seven days of the reported period — March — not of the present
	// moment. A March report describing April would be a figure about a month
	// the document is not about.
	wantFrom := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -7)
	if !f.rawFrom.Equal(wantFrom) {
		t.Errorf("raw window starts %s, want %s (seven days back from the period end)", f.rawFrom, wantFrom)
	}
	if !p.WindowEnd.Equal(doc.Meta.PeriodEnd) {
		t.Errorf("p95 window ends %s, want the period end %s", p.WindowEnd, doc.Meta.PeriodEnd)
	}
}

// The bound exists because this is the one figure that cannot be batched. Over
// it, the block says scope_too_large and **no raw query is issued at all** —
// which is the point: the cost is what the bound is protecting against.
func TestLargeScopeSkipsThePercentileWithoutQuerying(t *testing.T) {
	t.Parallel()

	f := &fakeStore{totals: map[model.ID]store.HistoryBucket{}}
	for i := 0; i <= P95MaxMonitors; i++ {
		m := monitorNamed("m")
		f.monitors = append(f.monitors, m)
		f.totals[m.ID] = store.HistoryBucket{Up: 100}
	}

	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	p := doc.Monitors[0].ResponseTime.P95
	if p == nil || p.Available || p.Reason != ReasonScopeTooLarge {
		t.Fatalf("p95 = %+v, want unavailable with %q", p, ReasonScopeTooLarge)
	}
	if f.calls["UptimeFromRaw"] != 0 || f.calls["RawCovers"] != 0 {
		t.Errorf("issued %d raw queries over a large scope; the bound exists to prevent exactly that",
			f.calls["UptimeFromRaw"]+f.calls["RawCovers"])
	}
}

// Short raw retention is answered from policy, without a query, and with the
// reason that distinguishes it from a scope decision.
func TestShortRawRetentionSkipsThePercentileFromPolicy(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	f := &fakeStore{monitors: []model.Monitor{m}, totals: map[model.ID]store.HistoryBucket{m.ID: {Up: 100}}}

	r := defaultRetention()
	r.RawDays = 3
	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, r, model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	p := doc.Monitors[0].ResponseTime.P95
	if p == nil || p.Reason != ReasonInsufficientRaw {
		t.Fatalf("p95 = %+v, want %q", p, ReasonInsufficientRaw)
	}
	if f.calls["RawCovers"] != 0 {
		t.Error("queried raw despite retention policy already answering the question")
	}
}

// The estate block has no percentile object at all, rather than one reporting
// itself unavailable. A quantile does not merge across monitors, so there is no
// reason to give: the figure does not exist rather than being withheld, and the
// contract requires a reason whenever the object is present and unavailable.
func TestEstateSummaryHasNoPercentileObject(t *testing.T) {
	t.Parallel()

	m := monitorNamed("api")
	value := 940.0
	f := &fakeStore{monitors: []model.Monitor{m}, totals: map[model.ID]store.HistoryBucket{m.ID: {Up: 100}}, p95: &value}

	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, defaultRetention(), model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if doc.Summary.ResponseTime.P95 != nil {
		t.Errorf("estate p95 = %+v, want nil", doc.Summary.ResponseTime.P95)
	}
}

// Every present-and-unavailable percentile states a reason. The contract says
// so, and a figure absent without one reads as a defect in the product rather
// than as a decision about honesty.
func TestUnavailablePercentileAlwaysCarriesAReason(t *testing.T) {
	t.Parallel()

	short := defaultRetention()
	short.RawDays = 1

	for _, tc := range []struct {
		name      string
		monitors  int
		retention Retention
	}{
		{"scope", P95MaxMonitors + 1, defaultRetention()},
		{"retention", 1, short},
	} {
		f := &fakeStore{totals: map[model.ID]store.HistoryBucket{}}
		for i := 0; i < tc.monitors; i++ {
			m := monitorNamed("m")
			f.monitors = append(f.monitors, m)
			f.totals[m.ID] = store.HistoryBucket{Up: 100}
		}

		doc, err := Build(context.Background(), f, Spec{
			Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
		}, tc.retention, model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("%s: build: %v", tc.name, err)
		}
		for _, section := range doc.Monitors {
			p := section.ResponseTime.P95
			if p == nil {
				t.Fatalf("%s: p95 object absent on a monitor section, want present with a reason", tc.name)
			}
			if !p.Available && p.Reason == "" {
				t.Errorf("%s: p95 unavailable with no reason", tc.name)
			}
		}
	}
}

// The second of ADR-006's two guard tests, and the one that holds the gate the
// ADR is actually about.
//
// The first — in internal/store/sqlite — keeps the approximate percentile out of
// the coarse tiers. This one keeps the *real* percentile from being quoted over
// a window it does not cover. The distinction matters because the two failures
// look nothing alike from the outside: an approximation is wrong by some amount,
// while an uncovered window is a correct percentile under a heading that lies
// about which days it describes.
//
// The case is a live one rather than a hypothetical. `RawCovers` is compared
// against the daily tier rather than asked in the absolute, so it goes false
// exactly when retention pruned raw rows that the 1d tier had already summarised
// — an ordinary install that has been running longer than raw_days. Retention
// policy permits the figure here (raw_days is seven), and the monitor still
// cannot supply it. Nothing but this per-monitor check stands between "the
// operator configured seven days of raw" and "these 940 ms are a p95 over the
// last seven days", which is the sentence the report would otherwise print.
func TestShortCoverageOnOneMonitorOmitsThePercentile(t *testing.T) {
	t.Parallel()

	value := 940.0
	m := monitorNamed("api")
	f := &fakeStore{
		monitors: []model.Monitor{m},
		totals:   map[model.ID]store.HistoryBucket{m.ID: {Up: 100}},
		p95:      &value,
		rawShort: true,
	}

	// A retention policy that permits the figure, so the only thing that can
	// withhold it is the per-monitor check.
	retention := defaultRetention()
	if !retention.RawCoversTrailingWeek() {
		t.Fatal("the fixture's retention policy already withholds the p95; " +
			"this test would then pass for the wrong reason")
	}

	doc, err := Build(context.Background(), f, Spec{
		Period: PeriodMonth, PeriodStyle: StyleCalendar, Timezone: "UTC",
	}, retention, model.NewID(), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// The check was actually made rather than skipped by an earlier gate, which
	// is what makes the assertion below about coverage.
	if f.calls["RawCovers"] == 0 {
		t.Fatal("coverage was never checked; an earlier gate answered instead")
	}

	p := doc.Monitors[0].ResponseTime.P95
	if p == nil {
		t.Fatal("no percentile object at all, want one reporting itself unavailable")
	}
	if p.Available || p.ValueMs != nil {
		t.Fatalf("p95 = %+v — raw does not reach back seven days for this monitor, "+
			"so the figure would be a percentile over a shorter window printed under "+
			"a seven-day heading", p)
	}
	if p.Reason != ReasonInsufficientRaw {
		t.Errorf("reason = %q, want %q", p.Reason, ReasonInsufficientRaw)
	}
}
