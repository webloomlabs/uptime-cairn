package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The SQLite half of report.Store. The other three methods it needs —
// RawCovers, UptimeFromRaw and ListIncidents — already exist in history.go and
// incidents.go with the signatures that interface names, which is why they are
// not restated here.
//
// Naming report.Scope from a backend follows RollUpTier taking a rollup.Tier:
// the computation package owns the vocabulary and the backend supplies the
// queries, which is ADR-002's arrangement rather than an inversion of it.
//
// Both batched methods take a slice of monitors and return a map. That is the
// whole point of them. A report over an estate is one document over the whole
// set, and the per-monitor version of either query passes every correctness test
// and then fails the load gate at 5,000 monitors — which is the same fan-out
// that made a page of twenty-five monitors read a quarter of a million heartbeat
// rows before `include=heartbeats` was rewritten.

// MonitorsInScope resolves a report's scope to the monitors it covers.
//
// The three sources combine as a union, evaluated in one predicate rather than
// as a UNION of three queries, so a monitor named explicitly *and* carrying a
// named tag appears once. A group reaches its children, for the same reason the
// monitor list's group filter does: a report scoped to a parent that returned
// nothing while the child underneath held every monitor is a report nobody
// trusts twice.
//
// Disabled and paused monitors are included, deliberately. A monitor somebody
// turned off last week still has three weeks of history in the window, and
// omitting it would silently change what a client's monthly report covers on the
// day an operator paused something.
//
// Unpaginated, unlike ListMonitors, because a document assembled a page at a
// time would be a function of page size. Ordered by id, which is stable and
// arbitrary: presentation order is the report model's decision, and sorting by
// name here would quietly settle the collation question ADR-009 is still
// proposing.
func (s *Store) MonitorsInScope(ctx context.Context, scope report.Scope) ([]model.Monitor, error) {
	if scope.Empty() {
		return nil, nil
	}

	query := `
		SELECT ` + monitorColumns + `
		FROM monitors m JOIN monitor_state s ON s.monitor_id = m.id
		WHERE m.org_id = ? AND (`
	args := []any{model.SentinelOrgID[:]}

	var clauses string
	add := func(sql string) {
		if clauses != "" {
			clauses += " OR "
		}
		clauses += sql
	}

	if len(scope.MonitorIDs) > 0 {
		add(`m.id IN (` + placeholders(len(scope.MonitorIDs)) + `)`)
		for _, id := range scope.MonitorIDs {
			args = append(args, id[:])
		}
	}
	if len(scope.GroupIDs) > 0 {
		list := placeholders(len(scope.GroupIDs))
		add(`m.group_id IN (` + list + `)
		     OR m.group_id IN (SELECT c.id FROM groups c WHERE c.parent_group_id IN (` + list + `))`)
		for range 2 {
			for _, id := range scope.GroupIDs {
				args = append(args, id[:])
			}
		}
	}
	if len(scope.TagIDs) > 0 {
		add(`EXISTS (SELECT 1 FROM monitor_tags mt
		             WHERE mt.monitor_id = m.id AND mt.tag_id IN (` + placeholders(len(scope.TagIDs)) + `))`)
		for _, id := range scope.TagIDs {
			args = append(args, id[:])
		}
	}
	query += clauses + `) ORDER BY m.id`

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("monitors in scope: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m.Monitor)
	}
	return out, rows.Err()
}

// SLOTargets resolves the uptime target for each monitor: its own where it has
// one, otherwise its group's, with the level that answered reported alongside.
//
// One COALESCE and one LEFT JOIN rather than a second round trip per monitor,
// and rather than the caller applying the precedence itself — the order is a
// rule of the product, and a rule implemented at each call site is a rule that
// drifts.
//
// A monitor whose group is nested inside another group does **not** inherit the
// outer group's target. The spec's order is template, monitor, group, none, and
// there is no fourth level in it; adding one silently would mean a report
// printing "inherited from group" for a number set two levels up.
//
// Monitors with no target at either level are absent from the result. The SLA
// block is then omitted rather than computed against a default nobody chose.
func (s *Store) SLOTargets(ctx context.Context, ids []model.ID) (map[model.ID]report.Target, error) {
	if len(ids) == 0 {
		return map[model.ID]report.Target{}, nil
	}

	args := make([]any, 0, len(ids)+1)
	args = append(args, model.SentinelOrgID[:])
	for _, id := range ids {
		args = append(args, id[:])
	}

	rows, err := s.ro.QueryContext(ctx, `
		SELECT m.id,
		       COALESCE(m.slo_target_percent, g.slo_target_percent),
		       CASE WHEN m.slo_target_percent IS NOT NULL THEN 'monitor' ELSE 'group' END
		FROM monitors m LEFT JOIN groups g ON g.id = m.group_id
		WHERE m.org_id = ? AND m.id IN (`+placeholders(len(ids))+`)
		  AND COALESCE(m.slo_target_percent, g.slo_target_percent) IS NOT NULL`, args...)
	if err != nil {
		return nil, fmt.Errorf("slo targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[model.ID]report.Target, len(ids))
	for rows.Next() {
		var (
			raw    []byte
			target report.Target
		)
		if err := rows.Scan(&raw, &target.Percent, &target.Source); err != nil {
			return nil, fmt.Errorf("scan slo target: %w", err)
		}
		var id model.ID
		copy(id[:], raw)
		out[id] = target
	}
	return out, rows.Err()
}

// WindowTotals aggregates one bucket per monitor over the whole report window.
//
// Exact at any tier, and that is the property the whole latency block rests on:
// the tiers store a sum and a count rather than an average, both of which are
// additive, so SUM over any set of buckets is the true total rather than an
// average of averages. Nothing here divides.
//
// p95 comes back NULL unconditionally, at every tier including 1m. ADR-006 is
// the reason and it is not a limitation of this query: a quantile is a rank
// statistic and does not merge, so there is no window-level percentile to be had
// from stored buckets. The 1m tier holds a real per-minute p95 and a maximum of
// those is a provable upper bound that can overstate by a hundredfold. The
// trailing-seven-day figure comes from UptimeFromRaw instead.
//
// Reads closed buckets only, which is what the rollup pipeline writes. A window
// ending inside the current bucket is therefore short by that bucket, and it is
// the caller that decides where a period ends — period_start and period_end are
// the report's to state, and a calendar month cut in the configured zone has no
// partial bucket in it at all.
func (s *Store) WindowTotals(ctx context.Context, ids []model.ID, from, to time.Time, tier string) (map[model.ID]store.HistoryBucket, error) {
	if len(ids) == 0 {
		return map[model.ID]store.HistoryBucket{}, nil
	}

	// bucket_start is the second column of the primary key and monitor_id the
	// first, so IN plus a range is one bounded seek per monitor rather than a
	// scan of the tier. Asserted by a query-plan test, because this is exactly
	// the shape that regressed once already: the plan is the invariant, and it is
	// not visible in the results.
	// The trailing bound parameter is the window start echoed back as the
	// bucket's own Start, so that this query and DailySeries return the same
	// thirteen columns in the same order and share one scanner. An aggregate row
	// has no bucket_start of its own, and MIN(bucket_start) would be the first
	// day that happened to have data rather than the window the report states.
	query := fmt.Sprintf(`
		SELECT monitor_id,
		       SUM(up_count), SUM(down_count), SUM(pending_count), SUM(maintenance_count),
		       SUM(unknown_count), SUM(skipped_count),
		       SUM(response_time_sum), SUM(response_time_count),
		       MIN(response_time_min), MAX(response_time_max), NULL, ?
		FROM heartbeat_%s
		WHERE monitor_id IN (%s) AND bucket_start >= ? AND bucket_start < ?
		GROUP BY monitor_id`, tier, placeholders(len(ids)))

	args := make([]any, 0, len(ids)+3)
	args = append(args, millis(from))
	for _, id := range ids {
		args = append(args, id[:])
	}
	args = append(args, millis(from), millis(to))

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("window totals from %s: %w", tier, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[model.ID]store.HistoryBucket, len(ids))
	for rows.Next() {
		id, b, err := scanKeyedBucket(rows)
		if err != nil {
			return nil, err
		}
		out[id] = b
	}
	// A monitor with no buckets in range is absent rather than zero-valued. Zero
	// up and zero down is a real state that means "observed nothing", and a
	// report has to be able to tell it from "this monitor did not exist yet" —
	// the difference between a gap and a hundred per cent of nothing.
	return out, rows.Err()
}

// DailySeries is the per-day series over the window, from the 1d tier.
//
// ADR-006 makes this the primary latency exhibit: the daily average series is
// where the average over the window, the best and worst day, and the days over
// target all come from. One query rather than one per monitor, for the same
// reason as WindowTotals — a month is 31 rows per monitor, which is small until
// there are five thousand of them.
//
// Days with no observations are absent rather than zero, and the caller renders
// the gap. Filling them would draw a month of downtime for a monitor created
// last week, which is the single most common way a status page lies and would be
// a worse lie on an invoice attachment.
func (s *Store) DailySeries(ctx context.Context, ids []model.ID, from, to time.Time) (map[model.ID][]store.HistoryBucket, error) {
	return s.seriesFromTier(ctx, "1d", ids, from, to)
}

// HourlySeries is the same series at the 1h tier, for a window too short for the
// daily one to be a chart.
//
// A report over a single day has exactly one daily bucket: a strip of one cell
// and a line of one point, which is a drawing that carries no information the
// figures beside it do not already state. The caller decides when to ask —
// nothing here judges the window — and it asks only for short ones, which is
// what keeps this from being a fifth query on the monthly runs the load gate
// measures.
//
// Hours with no observations are absent for the same reason days are.
func (s *Store) HourlySeries(ctx context.Context, ids []model.ID, from, to time.Time) (map[model.ID][]store.HistoryBucket, error) {
	return s.seriesFromTier(ctx, "1h", ids, from, to)
}

// seriesFromTier is the per-bucket series over the window, from a named tier.
//
// One implementation for both grains rather than two, so that a column added to
// the daily series cannot be forgotten in the hourly one — the same reasoning
// scanKeyedBucket is written down for below, and the symptom would be the same:
// two exhibits of one window that disagree.
//
// The tier is interpolated into the table name and is never user input: the two
// callers above pass literals. That is the same arrangement WindowTotals has,
// where the tier comes from ResolveTier's own fixed list.
func (s *Store) seriesFromTier(ctx context.Context, tier string, ids []model.ID, from, to time.Time) (map[model.ID][]store.HistoryBucket, error) {
	if len(ids) == 0 {
		return map[model.ID][]store.HistoryBucket{}, nil
	}

	query := fmt.Sprintf(`
		SELECT monitor_id, up_count, down_count, pending_count, maintenance_count,
		       unknown_count, skipped_count,
		       response_time_sum, response_time_count, response_time_min, response_time_max,
		       NULL, bucket_start
		FROM heartbeat_%s
		WHERE monitor_id IN (%s) AND bucket_start >= ? AND bucket_start < ?
		ORDER BY monitor_id, bucket_start`, tier, placeholders(len(ids)))

	args := make([]any, 0, len(ids)+2)
	for _, id := range ids {
		args = append(args, id[:])
	}
	args = append(args, millis(from), millis(to))

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s series: %w", tier, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[model.ID][]store.HistoryBucket, len(ids))
	for rows.Next() {
		id, b, err := scanKeyedBucket(rows)
		if err != nil {
			return nil, err
		}
		out[id] = append(out[id], b)
	}
	return out, rows.Err()
}

// scanKeyedBucket reads the thirteen columns both queries above select, in that
// order: the monitor, the six status counts, the four response-time aggregates,
// the percentile slot, and the bucket start.
//
// One scanner rather than two because the queries differ only in whether they
// group. Two would be two places for a column to be added to one and forgotten
// in the other, and the symptom of that is a report whose totals and whose daily
// series disagree — which is exactly the kind of defect that survives review by
// looking like a rounding difference.
func scanKeyedBucket(rows *sql.Rows) (model.ID, store.HistoryBucket, error) {
	var (
		raw                                     []byte
		b                                       store.HistoryBucket
		up, down, pending, maint, unknown, skip sql.NullInt64
		sum                                     sql.NullFloat64
		count                                   sql.NullInt64
		low, high, p95                          sql.NullFloat64
		startMs                                 int64
	)
	if err := rows.Scan(&raw, &up, &down, &pending, &maint, &unknown, &skip,
		&sum, &count, &low, &high, &p95, &startMs); err != nil {
		return model.ID{}, store.HistoryBucket{}, fmt.Errorf("scan report bucket: %w", err)
	}

	var id model.ID
	copy(id[:], raw)

	b.Start = fromMillis(startMs)
	b.Up, b.Down = int(up.Int64), int(down.Int64)
	b.Pending, b.Maintenance = int(pending.Int64), int(maint.Int64)
	b.Unknown, b.Skipped = int(unknown.Int64), int(skip.Int64)
	b.ResponseTimeSum, b.ResponseTimeCount = sum.Float64, int(count.Int64)
	if low.Valid {
		v := low.Float64
		b.ResponseTimeMin = &v
	}
	if high.Valid {
		v := high.Float64
		b.ResponseTimeMax = &v
	}
	// p95 is selected as a literal NULL by both queries and is never set. The
	// column is scanned rather than omitted so that the shape matches
	// store.HistoryBucket exactly, and so that anybody who later selects a real
	// percentile into it has nothing to wire up. See ADR-006.
	if p95.Valid {
		v := p95.Float64
		b.ResponseTimeP95 = &v
	}
	return id, b, nil
}
