package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// History reads bucketed history from whichever source can answer honestly.
//
// Two sources, and the choice between them is the interesting part. Rollup tiers
// exist because raw heartbeats are kept for seven days and history is not — but
// a tier lags, by its own bucket width plus the grace period the pipeline waits
// out for late probe data. Serve a 24-hour chart from the 5m tier and its last
// few minutes are simply missing, which is exactly the part of the chart someone
// looking at an incident is staring at.
//
// So: when raw heartbeats cover the whole requested range, bucket them directly
// at the requested resolution. The answer is then current to the last check, and
// the p95 is a real percentile rather than an approximation. Beyond raw
// retention the tiers take over, which is what they are for.

// RawCovers reports whether raw heartbeats can answer a range as completely as
// the given tier can.
//
// The obvious predicate — "does raw reach back to `from`?" — is wrong, and
// wrong in the case that matters most: a monitor created an hour ago has no raw
// data from twenty-four hours ago, but it has no rolled-up data from then
// either, so raw is not missing anything. Answering "no" there sends a chart of
// a brand-new monitor to an empty tier and draws nothing.
//
// So the real question is whether the tier reaches further back than raw does.
// It only ever does once retention has deleted raw rows the tier summarised,
// which is exactly the point at which the tier should take over.
func (s *Store) RawCovers(ctx context.Context, id model.ID, from time.Time, tier string) (bool, error) {
	if _, ok := tierIntervals[tier]; !ok {
		// No tier to compare against — the caller asked for raw resolution, and
		// raw is the only thing that can answer it.
		return true, nil
	}

	var rawMin, tierMin sql.NullInt64
	err := s.ro.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT (SELECT MIN(time) FROM heartbeats WHERE org_id = ?1 AND monitor_id = ?2),
		       (SELECT MIN(bucket_start) FROM heartbeat_%s WHERE monitor_id = ?2)`, tier),
		model.SentinelOrgID[:], id[:]).Scan(&rawMin, &tierMin)
	if err != nil {
		return false, fmt.Errorf("compare raw and %s coverage for %s: %w", tier, id, err)
	}

	if !rawMin.Valid {
		// No raw rows at all. Whatever the tier holds, it holds more.
		return false, nil
	}
	earliestRaw := time.UnixMicro(rawMin.Int64).UTC()

	switch {
	case earliestRaw.Compare(from) <= 0:
		// Raw spans the whole requested range.
		return true, nil
	case !tierMin.Valid:
		// Nothing rolled up yet, so raw is all there is.
		return true, nil
	default:
		// Compared against the bucket the earliest raw heartbeat falls into, not
		// against the heartbeat itself. A bucket_start is by definition at or
		// before every heartbeat it summarises, so comparing directly would make
		// the tier look older than raw by up to one bucket width — every time,
		// and raw would never be chosen.
		floor := earliestRaw.Truncate(tierIntervals[tier])
		return !time.UnixMilli(tierMin.Int64).UTC().Before(floor), nil
	}
}

// tierIntervals is the allow-list for the interpolated tier name and the widths
// the comparison above needs.
var tierIntervals = map[string]time.Duration{
	"1m": time.Minute,
	"5m": 5 * time.Minute,
	"1h": time.Hour,
	"1d": 24 * time.Hour,
}

// HistoryFromRaw buckets raw heartbeats at an arbitrary interval.
//
// The same aggregation the rollup pipeline performs, with the interval as a
// parameter instead of fixed at a minute — so a chart drawn from raw and the
// same chart drawn from a tier are computed the same way and agree.
func (s *Store) HistoryFromRaw(ctx context.Context, id model.ID, from, to time.Time, interval time.Duration) ([]store.HistoryBucket, error) {
	// Microseconds in, milliseconds out: heartbeats.time is µs and bucket_start
	// is ms, per the data model's conventions (§1).
	micros := interval.Microseconds()
	query := fmt.Sprintf(`
WITH sample AS (
    SELECT status, response_time_ms, ((time / %d) * %d) / 1000 AS bucket_start
    FROM heartbeats
    WHERE org_id = ?1 AND monitor_id = ?2 AND time >= ?3 AND time < ?4
),
agg AS (
    SELECT bucket_start,
           SUM(status = 1) AS up_count,
           SUM(status = 0) AS down_count,
           SUM(status = 2) AS pending_count,
           SUM(status = 3) AS maintenance_count,
           SUM(status = 4) AS unknown_count,
           SUM(status = 5) AS skipped_count,
           SUM(response_time_ms)   AS rt_sum,
           COUNT(response_time_ms) AS rt_count,
           MIN(response_time_ms)   AS rt_min,
           MAX(response_time_ms)   AS rt_max
    FROM sample GROUP BY bucket_start
),
ranked AS (
    SELECT bucket_start, response_time_ms,
           ROW_NUMBER() OVER (PARTITION BY bucket_start ORDER BY response_time_ms) AS rn,
           COUNT(*)     OVER (PARTITION BY bucket_start) AS n
    FROM sample WHERE response_time_ms IS NOT NULL
),
p95 AS (
    SELECT bucket_start, response_time_ms AS value FROM ranked WHERE rn = (n * 95 + 99) / 100
)
SELECT agg.bucket_start, agg.up_count, agg.down_count, agg.pending_count,
       agg.maintenance_count, agg.unknown_count, agg.skipped_count,
       agg.rt_sum, agg.rt_count, agg.rt_min, agg.rt_max, p95.value
FROM agg LEFT JOIN p95 ON p95.bucket_start = agg.bucket_start
ORDER BY agg.bucket_start`, micros, micros)

	rows, err := s.ro.QueryContext(ctx, query,
		model.SentinelOrgID[:], id[:], from.UnixMicro(), to.UnixMicro())
	if err != nil {
		return nil, fmt.Errorf("history from raw: %w", err)
	}
	return scanHistory(rows)
}

// HistoryFromTier reads a rollup tier.
//
// No p95 comes back from anything coarser than 1m, and that is deliberate: the
// coarse tiers store an approximation, the API schema has no field in which to
// label it as one, and §11.5 is explicit that an unlabelled p95 is worse than
// none.
func (s *Store) HistoryFromTier(ctx context.Context, id model.ID, from, to time.Time, tier string) ([]store.HistoryBucket, error) {
	p95 := "response_time_p95"
	if tier != "1m" {
		p95 = "NULL"
	}
	query := fmt.Sprintf(`
		SELECT bucket_start, up_count, down_count, pending_count, maintenance_count,
		       unknown_count, skipped_count,
		       response_time_sum, response_time_count, response_time_min, response_time_max,
		       %s
		FROM heartbeat_%s
		WHERE monitor_id = ? AND bucket_start >= ? AND bucket_start < ?
		ORDER BY bucket_start`, p95, tier)

	rows, err := s.ro.QueryContext(ctx, query, id[:], from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("history from %s: %w", tier, err)
	}
	return scanHistory(rows)
}

func scanHistory(rows *sql.Rows) ([]store.HistoryBucket, error) {
	defer func() { _ = rows.Close() }()

	var out []store.HistoryBucket
	for rows.Next() {
		var (
			b             store.HistoryBucket
			startMs       int64
			sum           sql.NullFloat64
			count         sql.NullInt64
			min, max, p95 sql.NullFloat64
		)
		if err := rows.Scan(&startMs, &b.Up, &b.Down, &b.Pending, &b.Maintenance,
			&b.Unknown, &b.Skipped, &sum, &count, &min, &max, &p95); err != nil {
			return nil, fmt.Errorf("scan history bucket: %w", err)
		}

		b.Start = time.UnixMilli(startMs).UTC()
		b.ResponseTimeSum = sum.Float64
		b.ResponseTimeCount = int(count.Int64)
		if min.Valid {
			v := min.Float64
			b.ResponseTimeMin = &v
		}
		if max.Valid {
			v := max.Float64
			b.ResponseTimeMax = &v
		}
		// Only ever a real percentile: the coarse-tier query selects NULL here
		// rather than the stored approximation.
		if p95.Valid {
			v := p95.Float64
			b.ResponseTimeP95 = &v
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UptimeFromRaw totals one window straight from heartbeats, with a real p95 over
// the whole window rather than a maximum of per-bucket percentiles.
func (s *Store) UptimeFromRaw(ctx context.Context, id model.ID, from, to time.Time) (store.HistoryBucket, error) {
	const query = `
WITH sample AS (
    SELECT status, response_time_ms FROM heartbeats
    WHERE org_id = ?1 AND monitor_id = ?2 AND time >= ?3 AND time < ?4
),
ranked AS (
    SELECT response_time_ms,
           ROW_NUMBER() OVER (ORDER BY response_time_ms) AS rn,
           COUNT(*)     OVER () AS n
    FROM sample WHERE response_time_ms IS NOT NULL
)
SELECT SUM(status = 1), SUM(status = 0), SUM(status = 2), SUM(status = 3),
       SUM(status = 4), SUM(status = 5),
       SUM(response_time_ms), COUNT(response_time_ms),
       MIN(response_time_ms), MAX(response_time_ms),
       (SELECT response_time_ms FROM ranked WHERE rn = (n * 95 + 99) / 100)
FROM sample`

	row := s.ro.QueryRowContext(ctx, query,
		model.SentinelOrgID[:], id[:], from.UnixMicro(), to.UnixMicro())
	return scanUptime(row, from)
}

// UptimeFromTier totals one window from a rollup tier.
//
// p95 is deliberately absent. The tiers below 1m hold an approximation, the 1m
// tier holds a per-minute percentile that cannot be re-aggregated into a
// window-level one, and neither is a number to hand an auditor unlabelled.
func (s *Store) UptimeFromTier(ctx context.Context, id model.ID, from, to time.Time, tier string) (store.HistoryBucket, error) {
	query := fmt.Sprintf(`
		SELECT SUM(up_count), SUM(down_count), SUM(pending_count), SUM(maintenance_count),
		       SUM(unknown_count), SUM(skipped_count),
		       SUM(response_time_sum), SUM(response_time_count),
		       MIN(response_time_min), MAX(response_time_max), NULL
		FROM heartbeat_%s
		WHERE monitor_id = ? AND bucket_start >= ? AND bucket_start < ?`, tier)

	row := s.ro.QueryRowContext(ctx, query, id[:], from.UnixMilli(), to.UnixMilli())
	return scanUptime(row, from)
}

func scanUptime(row *sql.Row, start time.Time) (store.HistoryBucket, error) {
	var (
		b                                       store.HistoryBucket
		up, down, pending, maint, unknown, skip sql.NullInt64
		sum                                     sql.NullFloat64
		count                                   sql.NullInt64
		min, max, p95                           sql.NullFloat64
	)
	if err := row.Scan(&up, &down, &pending, &maint, &unknown, &skip,
		&sum, &count, &min, &max, &p95); err != nil {
		return store.HistoryBucket{}, fmt.Errorf("scan uptime window: %w", err)
	}

	b.Start = start
	b.Up, b.Down = int(up.Int64), int(down.Int64)
	b.Pending, b.Maintenance = int(pending.Int64), int(maint.Int64)
	b.Unknown, b.Skipped = int(unknown.Int64), int(skip.Int64)
	b.ResponseTimeSum, b.ResponseTimeCount = sum.Float64, int(count.Int64)
	if min.Valid {
		v := min.Float64
		b.ResponseTimeMin = &v
	}
	if max.Valid {
		v := max.Float64
		b.ResponseTimeMax = &v
	}
	if p95.Valid {
		v := p95.Float64
		b.ResponseTimeP95 = &v
	}
	return b, nil
}

// DailyUptime returns one ratio per day per monitor over a range, for a status
// page's uptime bar.
//
// Read from the 1d tier rather than computed from raw: the bar spans up to a
// year, and the whole point of the rollup pipeline is that a year-long read is
// 365 rows per monitor rather than several million. Days with no observations
// are simply absent from the result, and the caller renders them as gaps —
// filling them with zero would draw a year of downtime for a monitor created
// last week.
//
// The tier alone is not enough, for the same reason History prefers raw over a
// lagging tier: the pipeline only computes *closed* buckets, so the 1d tier
// never holds today. Reading it on its own drops the rightmost stone from every
// bar — and on an instance younger than a day it returns nothing at all, which
// renders as a status page with no uptime bar rather than one with a short one.
// So the days the tier cannot answer are aggregated from raw heartbeats, which
// are always current, and the tier wins wherever it has a row: a day at the edge
// of raw retention is complete in the tier and half-deleted in raw.
func (s *Store) DailyUptime(ctx context.Context, ids []model.ID, from, to time.Time) (map[model.ID][]store.DailyUptime, error) {
	if len(ids) == 0 {
		return map[model.ID][]store.DailyUptime{}, nil
	}

	args := []any{millis(from), millis(to)}
	for _, id := range ids {
		args = append(args, id[:])
	}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT monitor_id, bucket_start, up_count, down_count
		FROM heartbeat_1d
		WHERE bucket_start >= ? AND bucket_start < ? AND monitor_id IN (`+placeholders(len(ids))+`)
		ORDER BY bucket_start`, args...)
	if err != nil {
		return nil, fmt.Errorf("daily uptime: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[model.ID][]store.DailyUptime, len(ids))
	for rows.Next() {
		var (
			id       []byte
			bucket   int64
			up, down int
		)
		if err := rows.Scan(&id, &bucket, &up, &down); err != nil {
			return nil, err
		}
		var key model.ID
		copy(key[:], id)

		day := store.DailyUptime{Date: fromMillis(bucket)}
		// unknown and skipped are excluded from the denominator here as
		// everywhere else: a day where the probe could not run is a day with no
		// observation, not a day of downtime (ADR-005 decision 16).
		if observed := up + down; observed > 0 {
			ratio := float64(up) / float64(observed)
			day.Ratio = &ratio
		}
		out[key] = append(out[key], day)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := s.fillDailyFromRaw(ctx, ids, from, to, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fillDailyFromRaw adds the days the 1d tier has no row for — today, always, and
// anything else the pipeline has not caught up on — aggregating raw heartbeats
// the same way the rollup would, and leaves the days the tier answered alone.
func (s *Store) fillDailyFromRaw(ctx context.Context, ids []model.ID, from, to time.Time, out map[model.ID][]store.DailyUptime) error {
	const day = 24 * time.Hour

	// Raw is kept for days, not months, so only the tail of the window can be
	// answered from it. Starting the scan at the newest day the tier holds — or
	// at `from` when it holds none — keeps this off the heartbeats table for the
	// bulk of a 90-day bar.
	scanFrom := from
	if newest := newestBucket(out); !newest.IsZero() && newest.After(scanFrom) {
		scanFrom = newest
	}
	if !scanFrom.Before(to) {
		return nil
	}

	// Microseconds, matching heartbeats.time, and out again as milliseconds to
	// match bucket_start — the conventions in data model §1.
	args := []any{model.SentinelOrgID[:], scanFrom.UnixMicro(), to.UnixMicro()}
	for _, id := range ids {
		args = append(args, id[:])
	}
	rows, err := s.ro.QueryContext(ctx, fmt.Sprintf(`
		SELECT monitor_id, ((time / %d) * %d) / 1000 AS bucket_start,
		       SUM(status = 1) AS up_count,
		       SUM(status = 0) AS down_count
		FROM heartbeats
		WHERE org_id = ?1 AND time >= ?2 AND time < ?3 AND monitor_id IN (`+placeholders(len(ids))+`)
		GROUP BY monitor_id, bucket_start
		ORDER BY bucket_start`, day.Microseconds(), day.Microseconds()), args...)
	if err != nil {
		return fmt.Errorf("daily uptime from raw: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id       []byte
			bucket   int64
			up, down int
		)
		if err := rows.Scan(&id, &bucket, &up, &down); err != nil {
			return err
		}
		var key model.ID
		copy(key[:], id)

		date := fromMillis(bucket)
		if hasDay(out[key], date) {
			continue
		}
		entry := store.DailyUptime{Date: date}
		// unknown and skipped stay out of the denominator here too, so a day
		// filled from raw and the same day once the tier catches up agree.
		if observed := up + down; observed > 0 {
			ratio := float64(up) / float64(observed)
			entry.Ratio = &ratio
		}
		out[key] = append(out[key], entry)
	}
	return rows.Err()
}

// newestBucket is the latest day any monitor's tier rows reach — the point past
// which the pipeline has not caught up and raw has to answer. Zero when nothing
// was rolled up at all, which is the case this whole path exists for: an
// instance younger than a day.
func newestBucket(out map[model.ID][]store.DailyUptime) time.Time {
	var newest time.Time
	for _, days := range out {
		if len(days) == 0 {
			continue
		}
		// Ordered by bucket_start, so the last entry is the newest.
		if last := days[len(days)-1].Date; last.After(newest) {
			newest = last
		}
	}
	return newest
}

func hasDay(days []store.DailyUptime, date time.Time) bool {
	for _, d := range days {
		if d.Date.Equal(date) {
			return true
		}
	}
	return false
}
