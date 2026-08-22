package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/rollup"
)

// The rollup queries.
//
// Every one of them aggregates in SQL rather than reading rows into Go. At 5,000
// monitors on the 20-second floor a day is 21.6 million heartbeats, and pulling
// even a tenth of that across the driver to add it up would make the pipeline the
// most expensive thing in the process.
//
// Units, which are the easiest thing here to get quietly wrong: heartbeats.time
// is microseconds since the epoch and bucket_start is milliseconds, per the data
// model's conventions (§1). The factor of a thousand between them appears in the
// raw query and nowhere else.

// bucketExpr floors a millisecond timestamp to a tier's bucket, in milliseconds.
func bucketExpr(column string, interval time.Duration) string {
	ms := interval.Milliseconds()
	return fmt.Sprintf("(%s / %d) * %d", column, ms, ms)
}

// RollUpRaw builds the 1m tier from heartbeats.
//
// The only tier computed from raw, and the only one that computes a real p95 —
// coarser tiers carry an approximation, per §11.5, because a percentile cannot be
// re-aggregated and the sketches that would allow it have no SQLite-native
// support.
func (s *Store) RollUpRaw(ctx context.Context, from, to time.Time) (int64, error) {
	const query = `
WITH sample AS (
    SELECT monitor_id, org_id, status,
           ` + measuredResponseTime + ` AS response_time_ms,
           (time / 60000000) * 60000 AS bucket_start
    FROM heartbeats
    WHERE time >= ?1 AND time < ?2
),
agg AS (
    SELECT monitor_id, org_id, bucket_start,
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
    FROM sample
    GROUP BY monitor_id, bucket_start
),
-- Nearest-rank p95: order the measurements in the bucket and take the
-- ceil(0.95 * n)-th. Integer arithmetic rather than a float multiply so the
-- rank is exactly reproducible on both backends.
ranked AS (
    SELECT monitor_id, bucket_start, response_time_ms,
           ROW_NUMBER() OVER (PARTITION BY monitor_id, bucket_start ORDER BY response_time_ms) AS rn,
           COUNT(*)     OVER (PARTITION BY monitor_id, bucket_start) AS n
    FROM sample
    WHERE response_time_ms IS NOT NULL
),
p95 AS (
    SELECT monitor_id, bucket_start, response_time_ms AS value
    FROM ranked
    WHERE rn = (n * 95 + 99) / 100
)
INSERT INTO heartbeat_1m (
    bucket_start, monitor_id, org_id,
    up_count, down_count, pending_count, maintenance_count, unknown_count, skipped_count,
    response_time_sum, response_time_count, response_time_min, response_time_max, response_time_p95)
SELECT agg.bucket_start, agg.monitor_id, agg.org_id,
       agg.up_count, agg.down_count, agg.pending_count, agg.maintenance_count,
       agg.unknown_count, agg.skipped_count,
       agg.rt_sum, agg.rt_count, agg.rt_min, agg.rt_max, p95.value
FROM agg LEFT JOIN p95
  ON p95.monitor_id = agg.monitor_id AND p95.bucket_start = agg.bucket_start
ON CONFLICT (monitor_id, bucket_start) DO UPDATE SET
    up_count            = excluded.up_count,
    down_count          = excluded.down_count,
    pending_count       = excluded.pending_count,
    maintenance_count   = excluded.maintenance_count,
    unknown_count       = excluded.unknown_count,
    skipped_count       = excluded.skipped_count,
    response_time_sum   = excluded.response_time_sum,
    response_time_count = excluded.response_time_count,
    response_time_min   = excluded.response_time_min,
    response_time_max   = excluded.response_time_max,
    response_time_p95   = excluded.response_time_p95`

	// A full recount of every bucket in range, upserted. Recomputing rather than
	// incrementing is what makes a re-run harmless: the same range run twice
	// produces the same numbers, which is the property the whole watermark
	// design leans on.
	res, err := s.db.ExecContext(ctx, query, from.UnixMicro(), to.UnixMicro())
	if err != nil {
		return 0, fmt.Errorf("roll up raw into 1m: %w", err)
	}
	return res.RowsAffected()
}

// RollUpTier builds one tier from the tier below.
func (s *Store) RollUpTier(ctx context.Context, tier, source rollup.Tier, from, to time.Time) (int64, error) {
	query := fmt.Sprintf(`
INSERT INTO heartbeat_%s (
    bucket_start, monitor_id, org_id,
    up_count, down_count, pending_count, maintenance_count, unknown_count, skipped_count,
    response_time_sum, response_time_count, response_time_min, response_time_max, response_time_p95)
SELECT %s AS b, monitor_id, org_id,
       SUM(up_count), SUM(down_count), SUM(pending_count), SUM(maintenance_count),
       SUM(unknown_count), SUM(skipped_count),
       SUM(response_time_sum), SUM(response_time_count),
       MIN(response_time_min), MAX(response_time_max),
       -- An approximation, and it must be labelled as one wherever it surfaces
       -- (§11.5). The largest sub-bucket p95 is the conservative choice: for
       -- latency, overstating is the safer direction to be wrong in, and a
       -- weighted mean would flatten exactly the spikes anyone cares about.
       MAX(response_time_p95)
FROM heartbeat_%s
WHERE bucket_start >= ?1 AND bucket_start < ?2
GROUP BY monitor_id, b
ON CONFLICT (monitor_id, bucket_start) DO UPDATE SET
    up_count            = excluded.up_count,
    down_count          = excluded.down_count,
    pending_count       = excluded.pending_count,
    maintenance_count   = excluded.maintenance_count,
    unknown_count       = excluded.unknown_count,
    skipped_count       = excluded.skipped_count,
    response_time_sum   = excluded.response_time_sum,
    response_time_count = excluded.response_time_count,
    response_time_min   = excluded.response_time_min,
    response_time_max   = excluded.response_time_max,
    response_time_p95   = excluded.response_time_p95`,
		tier.Name, bucketExpr("bucket_start", tier.Interval), source.Name)

	res, err := s.db.ExecContext(ctx, query, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("roll up %s from %s: %w", tier.Name, source.Name, err)
	}
	return res.RowsAffected()
}

// LatestBucket is the newest bucket in a tier, or the zero time when empty.
func (s *Store) LatestBucket(ctx context.Context, tier rollup.Tier) (time.Time, error) {
	return s.bucketBound(ctx, "MAX", tier)
}

// EarliestBucket is the oldest bucket in a tier.
func (s *Store) EarliestBucket(ctx context.Context, tier rollup.Tier) (time.Time, error) {
	return s.bucketBound(ctx, "MIN", tier)
}

func (s *Store) bucketBound(ctx context.Context, fn string, tier rollup.Tier) (time.Time, error) {
	query := fmt.Sprintf(`SELECT %s(bucket_start) FROM heartbeat_%s`, fn, tier.Name)

	var value sql.NullInt64
	if err := s.ro.QueryRowContext(ctx, query).Scan(&value); err != nil {
		return time.Time{}, fmt.Errorf("%s bucket in %s: %w", fn, tier.Name, err)
	}
	if !value.Valid {
		return time.Time{}, nil
	}
	return time.UnixMilli(value.Int64).UTC(), nil
}

// EarliestRaw is the oldest heartbeat, used to bootstrap the 1m tier.
func (s *Store) EarliestRaw(ctx context.Context) (time.Time, error) {
	var value sql.NullInt64
	if err := s.ro.QueryRowContext(ctx, `SELECT MIN(time) FROM heartbeats`).Scan(&value); err != nil {
		return time.Time{}, fmt.Errorf("earliest heartbeat: %w", err)
	}
	if !value.Valid {
		return time.Time{}, nil
	}
	return time.UnixMicro(value.Int64).UTC(), nil
}

// DeleteHeartbeatsBefore removes one bounded batch of expired raw heartbeats.
//
// Bounded by rowid rather than by a bare `WHERE time < ?`: SQLite would have to
// scan the whole table for that, because the only index on heartbeats leads with
// org_id. The subquery finds a batch and the outer delete removes exactly those.
func (s *Store) DeleteHeartbeatsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM heartbeats
		WHERE rowid IN (SELECT rowid FROM heartbeats WHERE time < ? LIMIT ?)`,
		cutoff.UnixMicro(), limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired heartbeats: %w", err)
	}
	return res.RowsAffected()
}

// DeleteBucketsBefore removes one bounded batch of expired rollup buckets.
func (s *Store) DeleteBucketsBefore(ctx context.Context, tier rollup.Tier, cutoff time.Time, limit int) (int64, error) {
	query := fmt.Sprintf(`
		DELETE FROM heartbeat_%s
		WHERE rowid IN (SELECT rowid FROM heartbeat_%s WHERE bucket_start < ? LIMIT ?)`,
		tier.Name, tier.Name)

	res, err := s.db.ExecContext(ctx, query, cutoff.UnixMilli(), limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired %s buckets: %w", tier.Name, err)
	}
	return res.RowsAffected()
}

// deliveryTables is an allow-list, because the table name is interpolated. The
// caller is internal today; an allow-list makes it stay safe when it is not.
var deliveryTables = map[string]bool{
	"webhook_deliveries":      true,
	"notification_deliveries": true,
}

// DeleteDeliveriesBefore trims the append-only delivery logs.
func (s *Store) DeleteDeliveriesBefore(ctx context.Context, table string, cutoff time.Time, limit int) (int64, error) {
	if !deliveryTables[table] {
		return 0, fmt.Errorf("retention: %q is not a delivery log", table)
	}
	query := fmt.Sprintf(`
		DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE created_at < ? LIMIT ?)`,
		table, table)

	res, err := s.db.ExecContext(ctx, query, cutoff.UnixMilli(), limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired %s: %w", table, err)
	}
	return res.RowsAffected()
}

// uptimeWindows are the cache's windows and the tier each reads.
//
// The tier is chosen so the range is a few hundred buckets per monitor at most.
// Reading 24 hours from the 1m tier would be 1,440 rows per monitor, which at
// 5,000 monitors is seven million rows per refresh — the kind of convenience
// that quietly fails the load gate (§5.5).
var uptimeWindows = []struct {
	name string
	span time.Duration
	tier string
}{
	// 24h reads the 5m tier rather than 1h for one reason that only shows up on
	// a fresh install: the 1h tier has nothing in it for the first hour, so a
	// new install would show a null uptime for an hour after its first check.
	// 288 buckets per monitor is a small enough seek to be worth that.
	{"24h", 24 * time.Hour, "5m"},
	{"7d", 7 * 24 * time.Hour, "1h"},
	{"30d", 30 * 24 * time.Hour, "1d"},
	{"90d", 90 * 24 * time.Hour, "1d"},
	{"365d", 365 * 24 * time.Hour, "1d"},
}

// RefreshUptimeCache recomputes every monitor's uptime windows from the rollups.
//
// Driven from `monitors` so each window is a primary-key range seek per monitor
// rather than a scan of a tier holding a year of buckets. It is a performance
// structure and nothing else: always reconstructible, always droppable, and
// stamped with computed_at so a reader knows how stale it is.
func (s *Store) RefreshUptimeCache(ctx context.Context, now time.Time) (int64, error) {
	var total int64

	for _, w := range uptimeWindows {
		query := fmt.Sprintf(`
INSERT INTO monitor_uptime_cache (
    monitor_id, org_id, window, uptime_ratio, total_checks, down_checks, downtime_seconds, computed_at)
SELECT m.id, m.org_id, ?3,
       -- Null whenever up + down is zero, row or no row: a bucket of nothing but
       -- unknown or skipped checks is a gap in observation, and rendering a gap
       -- as downtime is a lie a status page must not tell (§5.3).
       CASE WHEN COALESCE(SUM(h.up_count + h.down_count), 0) > 0
            THEN CAST(SUM(h.up_count) AS REAL) / SUM(h.up_count + h.down_count)
            ELSE NULL END,
       COALESCE(SUM(h.up_count + h.down_count), 0),
       COALESCE(SUM(h.down_count), 0),
       -- A failing check stands for one interval of unavailability, which is the
       -- same arithmetic /uptime does. Deriving it from the check count rather
       -- than from a share of the bucket means a monitor checked for only part
       -- of the window does not have the rest attributed to it either way.
       COALESCE(SUM(h.down_count), 0) * m.interval_seconds,
       ?2
FROM monitors m
LEFT JOIN heartbeat_%s h
       ON h.monitor_id = m.id AND h.bucket_start >= ?1
GROUP BY m.id
ON CONFLICT (monitor_id, window) DO UPDATE SET
    uptime_ratio     = excluded.uptime_ratio,
    total_checks     = excluded.total_checks,
    down_checks      = excluded.down_checks,
    downtime_seconds = excluded.downtime_seconds,
    computed_at      = excluded.computed_at`, w.tier)

		res, err := s.db.ExecContext(ctx, query, now.Add(-w.span).UnixMilli(), now.UnixMilli(), w.name)
		if err != nil {
			return total, fmt.Errorf("refresh %s uptime cache: %w", w.name, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// EnqueuePurge marks an entity's history for asynchronous deletion.
func (s *Store) EnqueuePurge(ctx context.Context, tx *sql.Tx, entityType string, id [16]byte, orgID [16]byte, at time.Time) error {
	exec := func(query string, args ...any) error {
		if tx != nil {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		}
		_, err := s.db.ExecContext(ctx, query, args...)
		return err
	}
	// Already queued is not an error: two deletions of the same monitor should
	// leave one purge, not two competing ones.
	return exec(`
		INSERT INTO pending_purges (entity_type, entity_id, org_id, requested_at)
		VALUES (?,?,?,?) ON CONFLICT DO NOTHING`,
		entityType, id[:], orgID[:], at.UnixMilli())
}

// purgeTables is every table holding history keyed by monitor_id with no
// foreign key back to monitors.
//
// None of them cascade, and that is deliberate: the time series is exactly what
// cannot be deleted inside a request (§9.3). A DELETE endpoint that cascaded over
// a week of heartbeats plus a year of buckets would make its own 204 a lie about
// how long the work takes. So the rows are orphaned on purpose and collected
// here — invisible to every API query, all of which filter through a live
// monitor, which is why a lagging purge is a disk-space concern and never a
// correctness one.
var purgeTables = []string{
	"heartbeats",
	"heartbeat_1m",
	"heartbeat_5m",
	"heartbeat_1h",
	"heartbeat_1d",
}

// PurgeNext deletes one batch of the oldest queued entity's history.
//
// Returns done=true when the queue is empty.
//
// The head read stays on the writer while every other read in this file moved to
// the reader pool. It is a dequeue, not a query: the row it picks is the row the
// deletes below act on, and reading it on the same connection that does the
// deleting is what keeps "which entity am I purging" from drifting between the
// two. It costs nothing — one indexed row, once per batch — where the scans
// above cost a table.
func (s *Store) PurgeNext(ctx context.Context, limit int) (int64, bool, error) {
	var (
		entityType string
		entityID   []byte
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT entity_type, entity_id FROM pending_purges
		ORDER BY requested_at, entity_id LIMIT 1`).Scan(&entityType, &entityID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read purge queue: %w", err)
	}

	// One table at a time, raw first because it is by far the largest. The entry
	// is only dequeued once every table comes back empty.
	for _, table := range purgeTables {
		query := fmt.Sprintf(`
			DELETE FROM %s
			WHERE rowid IN (SELECT rowid FROM %s WHERE monitor_id = ? LIMIT ?)`, table, table)

		res, err := s.db.ExecContext(ctx, query, entityID, limit)
		if err != nil {
			return 0, false, fmt.Errorf("purge %s: %w", table, err)
		}
		if rows, _ := res.RowsAffected(); rows > 0 {
			return rows, false, nil
		}
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_purges WHERE entity_type = ? AND entity_id = ?`,
		entityType, entityID); err != nil {
		return 0, false, fmt.Errorf("dequeue purge: %w", err)
	}
	return 0, false, nil
}

// IncrementalVacuum returns freed pages to the filesystem, in bounded steps so
// the write lock is never held for long.
func (s *Store) IncrementalVacuum(ctx context.Context, pages int) error {
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", pages)); err != nil {
		return fmt.Errorf("incremental vacuum: %w", err)
	}
	return nil
}

// AutoVacuumMode reports the database's auto_vacuum setting: 0 none,
// 1 full, 2 incremental.
//
// On the writer deliberately. This exists to confirm that the writer's DSN took
// effect, and asking a different connection whether the first one is configured
// correctly answers a question nobody asked.
func (s *Store) AutoVacuumMode(ctx context.Context) (int, error) {
	var mode int
	if err := s.db.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return 0, fmt.Errorf("read auto_vacuum: %w", err)
	}
	return mode, nil
}
