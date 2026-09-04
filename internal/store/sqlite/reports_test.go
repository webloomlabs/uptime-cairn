package sqlite

import (
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The whole point of the read-side contract is that this holds.
var _ report.Store = (*Store)(nil)

// seedDay writes one 1d rollup bucket directly, rather than driving heartbeats
// through the pipeline.
//
// Deliberate: these tests assert what the report queries do with tier rows, not
// what the rollup does to produce them. Going through the pipeline would make a
// failure here ambiguous between the two, and the rollup already has its own
// tests that do exactly that.
func seedDay(t *testing.T, s *Store, id model.ID, day time.Time, up, down, unknown int, rtSum float64, rtCount int) {
	t.Helper()

	_, err := s.db.ExecContext(t.Context(), `
		INSERT INTO heartbeat_1d (bucket_start, monitor_id, org_id,
		    up_count, down_count, pending_count, maintenance_count,
		    unknown_count, skipped_count,
		    response_time_sum, response_time_count, response_time_min, response_time_max)
		VALUES (?, ?, ?, ?, ?, 0, 0, ?, 0, ?, ?, ?, ?)`,
		millis(day), id[:], model.SentinelOrgID[:], up, down, unknown,
		rtSum, rtCount, 10.0, 90.0)
	if err != nil {
		t.Fatalf("seed day: %v", err)
	}
}

// seedHour is the same for the 1h tier, which is what a report over a single day
// draws its charts from.
func seedHour(t *testing.T, s *Store, id model.ID, hour time.Time, up, down, unknown int, rtSum float64, rtCount int) {
	t.Helper()

	_, err := s.db.ExecContext(t.Context(), `
		INSERT INTO heartbeat_1h (bucket_start, monitor_id, org_id,
		    up_count, down_count, pending_count, maintenance_count,
		    unknown_count, skipped_count,
		    response_time_sum, response_time_count, response_time_min, response_time_max)
		VALUES (?, ?, ?, ?, ?, 0, 0, ?, 0, ?, ?, ?, ?)`,
		millis(hour), id[:], model.SentinelOrgID[:], up, down, unknown,
		rtSum, rtCount, 10.0, 90.0)
	if err != nil {
		t.Fatalf("seed hour: %v", err)
	}
}

func mustCreate(t *testing.T, s *Store, m model.Monitor) model.Monitor {
	t.Helper()
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create monitor %s: %v", m.Name, err)
	}
	return m
}

// A report window is a sum over buckets, and the sum has to be exact: the tiers
// carry a response-time sum and count rather than an average precisely so that
// an arbitrary window can be re-weighted without error. An average of daily
// averages would be a different, wrong number whenever the days have different
// check counts — which they always do.
func TestWindowTotalsIsExactAcrossBuckets(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := mustCreate(t, s, testMonitor("api"))
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// Two days with deliberately unequal check counts. The average of the daily
	// averages is (100+200)/2 = 150; the true average is 12000/100 = 120.
	seedDay(t, s, m.ID, day, 90, 10, 0, 10000, 100)               // 100 checks averaging 100ms
	seedDay(t, s, m.ID, day.AddDate(0, 0, 1), 50, 0, 5, 2000, 10) // 10 checks averaging 200ms

	totals, err := s.WindowTotals(t.Context(), []model.ID{m.ID}, day, day.AddDate(0, 0, 7), "1d")
	if err != nil {
		t.Fatalf("window totals: %v", err)
	}

	b, ok := totals[m.ID]
	if !ok {
		t.Fatal("monitor absent from totals")
	}
	if b.Up != 140 || b.Down != 10 {
		t.Errorf("counts = up %d down %d, want up 140 down 10", b.Up, b.Down)
	}
	if b.Unknown != 5 {
		t.Errorf("unknown = %d, want 5 — a gap has to survive to the report, not be folded into down", b.Unknown)
	}
	if b.ResponseTimeCount != 110 || b.ResponseTimeSum != 12000 {
		t.Fatalf("response time = sum %v count %d, want sum 12000 count 110", b.ResponseTimeSum, b.ResponseTimeCount)
	}
	if avg := b.ResponseTimeSum / float64(b.ResponseTimeCount); avg < 109.09 || avg > 109.10 {
		t.Errorf("average = %.4f, want ~109.09 (12000/110) and never 150, the average of the daily averages", avg)
	}
	// Start is the window, not the first day that happened to hold data.
	if !b.Start.Equal(day) {
		t.Errorf("Start = %s, want the window start %s", b.Start, day)
	}
}

// ADR-006 removes the window percentile rather than policing it. There is no
// tier this can come from, so the query selects NULL at every tier including 1m,
// where a real per-minute p95 is actually stored and is still not mergeable.
func TestWindowTotalsNeverReturnsAPercentile(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := mustCreate(t, s, testMonitor("api"))
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	_, err := s.db.ExecContext(t.Context(), `
		INSERT INTO heartbeat_1m (bucket_start, monitor_id, org_id, up_count, down_count,
		    response_time_sum, response_time_count, response_time_p95)
		VALUES (?, ?, ?, 1, 0, 100, 1, 4200)`,
		millis(day), m.ID[:], model.SentinelOrgID[:])
	if err != nil {
		t.Fatalf("seed 1m: %v", err)
	}

	for _, tier := range []string{"1m", "1d"} {
		totals, err := s.WindowTotals(t.Context(), []model.ID{m.ID}, day, day.AddDate(0, 0, 1), tier)
		if err != nil {
			t.Fatalf("window totals %s: %v", tier, err)
		}
		if b := totals[m.ID]; b.ResponseTimeP95 != nil {
			t.Errorf("tier %s returned p95 %v, want nil — a quantile does not merge over a window", tier, *b.ResponseTimeP95)
		}
	}
}

// Zero up and zero down is a real state meaning "observed nothing". A monitor
// with no buckets at all is a different thing, and a report has to be able to
// tell them apart — so an absent monitor is absent rather than zero-valued.
func TestWindowTotalsOmitsMonitorsWithNoBuckets(t *testing.T) {
	t.Parallel()

	s := open(t)
	seen := mustCreate(t, s, testMonitor("seen"))
	unseen := mustCreate(t, s, testMonitor("unseen"))
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	seedDay(t, s, seen.ID, day, 10, 0, 0, 1000, 10)

	totals, err := s.WindowTotals(t.Context(), []model.ID{seen.ID, unseen.ID}, day, day.AddDate(0, 0, 1), "1d")
	if err != nil {
		t.Fatalf("window totals: %v", err)
	}
	if _, ok := totals[unseen.ID]; ok {
		t.Error("monitor with no buckets present in the map; a caller cannot then tell a gap from a hundred per cent of nothing")
	}
	if len(totals) != 1 {
		t.Errorf("len = %d, want 1", len(totals))
	}
}

// The window is half-open — start inclusive, end exclusive — the same contract
// every bucket read in this codebase uses. A month that included the first
// instant of the next month would double-count a day across two reports.
func TestWindowTotalsWindowIsHalfOpen(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := mustCreate(t, s, testMonitor("api"))
	first := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	next := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	seedDay(t, s, m.ID, first, 10, 0, 0, 100, 10)
	seedDay(t, s, m.ID, last, 10, 0, 0, 100, 10)
	seedDay(t, s, m.ID, next, 99, 0, 0, 100, 10)

	totals, err := s.WindowTotals(t.Context(), []model.ID{m.ID}, first, next, "1d")
	if err != nil {
		t.Fatalf("window totals: %v", err)
	}
	if up := totals[m.ID].Up; up != 20 {
		t.Errorf("up = %d, want 20 — March 1 included, April 1 excluded", up)
	}
}

// The daily series is ADR-006's primary latency exhibit, so its order and its
// gaps both matter: best and worst day are read straight off it, and a missing
// day has to stay missing rather than arriving as a zero that reads as an outage.
func TestDailySeriesIsOrderedAndKeepsGapsAbsent(t *testing.T) {
	t.Parallel()

	s := open(t)
	a := mustCreate(t, s, testMonitor("a"))
	b := mustCreate(t, s, testMonitor("b"))
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// Seeded out of order on purpose: the query orders, the caller does not.
	seedDay(t, s, a.ID, day.AddDate(0, 0, 2), 10, 0, 0, 3000, 10)
	seedDay(t, s, a.ID, day, 10, 0, 0, 1000, 10)
	seedDay(t, s, b.ID, day, 5, 5, 0, 500, 5)

	series, err := s.DailySeries(t.Context(), []model.ID{a.ID, b.ID}, day, day.AddDate(0, 0, 7))
	if err != nil {
		t.Fatalf("daily series: %v", err)
	}

	if got := len(series[a.ID]); got != 2 {
		t.Fatalf("monitor a has %d days, want 2 — the day with no bucket must be absent, not zero", got)
	}
	if !series[a.ID][0].Start.Equal(day) || !series[a.ID][1].Start.Equal(day.AddDate(0, 0, 2)) {
		t.Errorf("days out of order: %s then %s", series[a.ID][0].Start, series[a.ID][1].Start)
	}
	// Each monitor's series is its own; one query for many monitors must not
	// braid them together.
	if got := len(series[b.ID]); got != 1 {
		t.Errorf("monitor b has %d days, want 1", got)
	}
	if avg := series[a.ID][1].ResponseTimeSum / float64(series[a.ID][1].ResponseTimeCount); avg != 300 {
		t.Errorf("day 3 average = %v, want 300", avg)
	}
}

// The hourly series is the same query one tier down, and it has to stay that
// way: two implementations would be two places for a column to be added to one
// and forgotten in the other, and the symptom is two exhibits of one window that
// disagree by a number nobody can trace.
func TestHourlySeriesReadsTheHourTierAndNotTheDaily(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := mustCreate(t, s, testMonitor("checkout"))
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	// A day's worth of hours, seeded out of order, with one hour missing.
	for _, n := range []int{2, 0, 3} {
		seedHour(t, s, m.ID, day.Add(time.Duration(n)*time.Hour), 60, 0, 0, 60*float64(600+n), 60)
	}
	// And the daily bucket that summarises them, which this query must not read.
	seedDay(t, s, m.ID, day, 180, 0, 0, 999999, 180)

	series, err := s.HourlySeries(t.Context(), []model.ID{m.ID}, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("hourly series: %v", err)
	}

	got := series[m.ID]
	if len(got) != 3 {
		t.Fatalf("hours = %d, want 3 — the unobserved hour must be absent, not zero", len(got))
	}
	for i, want := range []int{0, 2, 3} {
		if !got[i].Start.Equal(day.Add(time.Duration(want) * time.Hour)) {
			t.Errorf("hour %d is %s, want %02d:00", i, got[i].Start, want)
		}
	}
	if avg := got[0].ResponseTimeSum / float64(got[0].ResponseTimeCount); avg != 600 {
		t.Errorf("first hour average = %v, want 600 — this is reading the daily tier", avg)
	}
}

// Both batched reads must seek per monitor rather than scan the tier. This is
// the invariant that does not show up in any result, and the one that regressed
// once already: the embeds' MAX(time) GROUP BY monitor_id read 26ms of index at
// 500 monitors and 489ms at 5,000 while returning identical rows.
func TestBatchedReadsSeekRatherThanScan(t *testing.T) {
	t.Parallel()

	s := open(t)
	ids := make([]model.ID, 3)
	for i := range ids {
		ids[i] = mustCreate(t, s, testMonitor("m")).ID
	}
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	plan := func(query string, args ...any) string {
		t.Helper()
		rows, err := s.ro.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer func() { _ = rows.Close() }()

		var out []string
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			out = append(out, detail)
		}
		return strings.Join(out, " | ")
	}

	args := []any{millis(day)}
	for _, id := range ids {
		args = append(args, id[:])
	}
	args = append(args, millis(day), millis(day.AddDate(0, 0, 31)))

	// The property, not the index's name: SQLite implements the (monitor_id,
	// bucket_start) primary key of a rowid table as sqlite_autoindex_…_1, so
	// asserting on "PRIMARY KEY" would fail against a correct plan. What has to
	// hold is that the tier is searched with monitor_id bound and the range
	// applied — one bounded seek per monitor — and never scanned.
	assertSeeks := func(label, detail string) {
		t.Helper()
		switch {
		case strings.Contains(detail, "SCAN heartbeat_1d"):
			t.Errorf("%s scans the tier rather than seeking: %s", label, detail)
		case !strings.Contains(detail, "SEARCH heartbeat_1d"),
			!strings.Contains(detail, "monitor_id=?"),
			!strings.Contains(detail, "bucket_start>"):
			t.Errorf("%s is not a bounded per-monitor seek: %s", label, detail)
		}
	}

	assertSeeks("WindowTotals", plan(`
		SELECT monitor_id, SUM(up_count), SUM(down_count), SUM(pending_count),
		       SUM(maintenance_count), SUM(unknown_count), SUM(skipped_count),
		       SUM(response_time_sum), SUM(response_time_count),
		       MIN(response_time_min), MAX(response_time_max), NULL, ?
		FROM heartbeat_1d
		WHERE monitor_id IN (`+placeholders(len(ids))+`) AND bucket_start >= ? AND bucket_start < ?
		GROUP BY monitor_id`, args...))

	assertSeeks("DailySeries", plan(`
		SELECT monitor_id, up_count, down_count, pending_count, maintenance_count,
		       unknown_count, skipped_count,
		       response_time_sum, response_time_count, response_time_min, response_time_max,
		       NULL, bucket_start
		FROM heartbeat_1d
		WHERE monitor_id IN (`+placeholders(len(ids))+`) AND bucket_start >= ? AND bucket_start < ?
		ORDER BY monitor_id, bucket_start`, args[1:]...))

	// The hourly series is the same shape against heartbeat_1h, and it is read
	// on a window where the daily tier holds one row per monitor — so a scan
	// here would be twenty-four times as expensive on the exhibit that replaced
	// the cheap one.
	hourly := plan(`
		SELECT monitor_id, up_count, down_count, pending_count, maintenance_count,
		       unknown_count, skipped_count,
		       response_time_sum, response_time_count, response_time_min, response_time_max,
		       NULL, bucket_start
		FROM heartbeat_1h
		WHERE monitor_id IN (`+placeholders(len(ids))+`) AND bucket_start >= ? AND bucket_start < ?
		ORDER BY monitor_id, bucket_start`, args[1:]...)
	switch {
	case strings.Contains(hourly, "SCAN heartbeat_1h"):
		t.Errorf("HourlySeries scans the tier rather than seeking: %s", hourly)
	case !strings.Contains(hourly, "SEARCH heartbeat_1h"),
		!strings.Contains(hourly, "monitor_id=?"),
		!strings.Contains(hourly, "bucket_start>"):
		t.Errorf("HourlySeries is not a bounded per-monitor seek: %s", hourly)
	}
}

// Scope is a union evaluated in one predicate, so a monitor selected twice over
// appears once. Three UNIONed queries would return it three times and the report
// would count its downtime three times with it.
func TestMonitorsInScopeUnionsWithoutDuplicating(t *testing.T) {
	t.Parallel()

	s := open(t)

	parent := model.Group{ID: model.NewID(), OrgID: model.SentinelOrgID, Name: "client"}
	if err := s.CreateGroup(t.Context(), parent); err != nil {
		t.Fatalf("create group: %v", err)
	}
	child := model.Group{ID: model.NewID(), OrgID: model.SentinelOrgID, Name: "client-api", ParentGroupID: &parent.ID}
	if err := s.CreateGroup(t.Context(), child); err != nil {
		t.Fatalf("create child group: %v", err)
	}
	tag := model.Tag{ID: model.NewID(), OrgID: model.SentinelOrgID, Name: "production", Slug: "production"}
	if err := s.CreateTag(t.Context(), tag); err != nil {
		t.Fatalf("create tag: %v", err)
	}

	inGroup := testMonitor("in-group")
	inGroup.GroupID = &parent.ID
	mustCreate(t, s, inGroup)

	inChild := testMonitor("in-child-group")
	inChild.GroupID = &child.ID
	mustCreate(t, s, inChild)

	tagged := mustCreate(t, s, testMonitor("tagged"))
	if err := s.SetMonitorTags(t.Context(), tagged.ID, model.SentinelOrgID, []model.ID{tag.ID}); err != nil {
		t.Fatalf("tag monitor: %v", err)
	}

	// Selected three ways at once: by id, by its group, and by its tag.
	overlapping := testMonitor("overlapping")
	overlapping.GroupID = &parent.ID
	mustCreate(t, s, overlapping)
	if err := s.SetMonitorTags(t.Context(), overlapping.ID, model.SentinelOrgID, []model.ID{tag.ID}); err != nil {
		t.Fatalf("tag monitor: %v", err)
	}

	mustCreate(t, s, testMonitor("unrelated"))

	got, err := s.MonitorsInScope(t.Context(), report.Scope{
		MonitorIDs: []model.ID{overlapping.ID},
		GroupIDs:   []model.ID{parent.ID},
		TagIDs:     []model.ID{tag.ID},
	})
	if err != nil {
		t.Fatalf("monitors in scope: %v", err)
	}

	names := map[string]int{}
	for _, m := range got {
		names[m.Name]++
	}
	if names["overlapping"] != 1 {
		t.Errorf("overlapping monitor appears %d times, want 1", names["overlapping"])
	}
	if names["in-child-group"] != 1 {
		t.Error("a group scope did not reach its child group; a report scoped to a parent must not come back empty")
	}
	if names["unrelated"] != 0 {
		t.Error("unrelated monitor in scope")
	}
	if len(got) != 4 {
		t.Errorf("scope resolved %d monitors (%v), want 4", len(got), names)
	}
}

// A paused monitor still has history in the window. Dropping it would silently
// change what a client's monthly report covers on the day somebody paused it.
func TestMonitorsInScopeIncludesDisabled(t *testing.T) {
	t.Parallel()

	s := open(t)
	off := testMonitor("paused")
	off.Enabled = false
	mustCreate(t, s, off)

	got, err := s.MonitorsInScope(t.Context(), report.Scope{MonitorIDs: []model.ID{off.ID}})
	if err != nil {
		t.Fatalf("monitors in scope: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("disabled monitor resolved to %d monitors, want 1", len(got))
	}
}

// An empty scope selects nothing and must not degenerate into "everything",
// which is what a bare WHERE with no clauses would do.
func TestMonitorsInScopeEmptySelectsNothing(t *testing.T) {
	t.Parallel()

	s := open(t)
	mustCreate(t, s, testMonitor("api"))

	got, err := s.MonitorsInScope(t.Context(), report.Scope{})
	if err != nil {
		t.Fatalf("monitors in scope: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty scope resolved %d monitors, want 0", len(got))
	}
}

// Guards the seam rather than the query: report.Store is satisfied by the same
// concrete store that serves the API, so a report and the dashboard cannot come
// to read from two different implementations.
func TestStoreSatisfiesReportContract(t *testing.T) {
	t.Parallel()

	var s store.HeartbeatStore = (*Store)(nil)
	_ = s
}

// setTarget writes an SLO target directly. There is no store method for it and
// deliberately so: model.Monitor has no such field, because Phase 2 only reads
// this number and putting it on the domain type would hand it to every consumer
// ahead of the phase that gives it meaning.
func setTarget(t *testing.T, s *Store, table string, id model.ID, percent float64) {
	t.Helper()

	if _, err := s.db.ExecContext(t.Context(),
		`UPDATE `+table+` SET slo_target_percent = ? WHERE id = ?`, percent, id[:]); err != nil {
		t.Fatalf("set target on %s: %v", table, err)
	}
}

// The precedence is a rule of the product — template, then monitor, then group,
// then none — and the level that answered has to come back with the number,
// because a monitor silently inheriting its group's target is otherwise
// invisible on the report face.
func TestSLOTargetsResolveMonitorBeforeGroup(t *testing.T) {
	t.Parallel()

	s := open(t)
	group := model.Group{ID: model.NewID(), OrgID: model.SentinelOrgID, Name: "client"}
	if err := s.CreateGroup(t.Context(), group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	setTarget(t, s, "groups", group.ID, 99.0)

	own := testMonitor("has-its-own")
	own.GroupID = &group.ID
	mustCreate(t, s, own)
	setTarget(t, s, "monitors", own.ID, 99.95)

	inherits := testMonitor("inherits")
	inherits.GroupID = &group.ID
	mustCreate(t, s, inherits)

	none := mustCreate(t, s, testMonitor("ungrouped"))

	targets, err := s.SLOTargets(t.Context(), []model.ID{own.ID, inherits.ID, none.ID})
	if err != nil {
		t.Fatalf("slo targets: %v", err)
	}

	if got := targets[own.ID]; got.Percent != 99.95 || got.Source != report.TargetFromMonitor {
		t.Errorf("own = %+v, want 99.95 from monitor", got)
	}
	if got := targets[inherits.ID]; got.Percent != 99.0 || got.Source != report.TargetFromGroup {
		t.Errorf("inherited = %+v, want 99 from group", got)
	}
	if _, ok := targets[none.ID]; ok {
		t.Error("monitor with no target present; the SLA block must be omitted, not computed against zero")
	}
}

// Groups nest one level, and the resolution order has no fourth step in it. A
// monitor in a child group does not quietly acquire the parent group's target —
// the report would print "inherited from group" for a number set two levels up.
func TestSLOTargetsDoNotClimbToAParentGroup(t *testing.T) {
	t.Parallel()

	s := open(t)
	parent := model.Group{ID: model.NewID(), OrgID: model.SentinelOrgID, Name: "client"}
	if err := s.CreateGroup(t.Context(), parent); err != nil {
		t.Fatalf("create group: %v", err)
	}
	setTarget(t, s, "groups", parent.ID, 99.9)

	child := model.Group{ID: model.NewID(), OrgID: model.SentinelOrgID, Name: "client-api", ParentGroupID: &parent.ID}
	if err := s.CreateGroup(t.Context(), child); err != nil {
		t.Fatalf("create child group: %v", err)
	}

	m := testMonitor("in-child")
	m.GroupID = &child.ID
	mustCreate(t, s, m)

	targets, err := s.SLOTargets(t.Context(), []model.ID{m.ID})
	if err != nil {
		t.Fatalf("slo targets: %v", err)
	}
	if got, ok := targets[m.ID]; ok {
		t.Errorf("monitor inherited %+v from a grandparent group; resolution stops at its own group", got)
	}
}
