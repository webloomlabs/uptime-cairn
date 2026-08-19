// Package rollup computes the heartbeat aggregate tiers and enforces retention.
//
// ADR-002 fixes the tiers — raw → 1m → 5m → 1h → 1d — and the data model (§5.4)
// makes the bucket contract explicit because Timescale will compute these as
// continuous aggregates while SQLite computes them here, and the repository
// interface only holds if both produce the same numbers. So the contract lives in
// this package, backend-independent, and the backend supplies only the queries.
//
// Rollups are not an optimisation of history; they are what makes history exist
// at all. Raw heartbeats are kept for seven days, so a status page's 90-day
// uptime bar reads tiers that nothing populated until now.
package rollup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Tier is one resolution in the chain.
type Tier struct {
	// Name is the suffix used in table names and in the API's resolution
	// parameter: "1m", "5m", "1h", "1d".
	Name string

	// Interval is the bucket width. bucket_start is inclusive,
	// bucket_start+Interval exclusive, and every heartbeat belongs to exactly
	// one bucket at each tier.
	Interval time.Duration

	// Source is the tier this one aggregates from, or nil for 1m, which is the
	// only tier computed from raw heartbeats.
	//
	// Each tier from the tier below rather than all from raw: it bounds
	// recomputation cost, and it is why the rollup columns store a sum and a
	// count instead of an average. An average cannot be re-weighted into a
	// coarser bucket; a sum and a count can.
	Source *Tier

	// Reprocess is how far back each run recomputes, over and above new
	// buckets. Probes buffer and replay on a control-plane outage (ADR-001), so
	// heartbeats arrive out of order, and a bucket computed the moment its
	// interval closed can be wrong minutes later. Recomputing a trailing window
	// with an upsert makes that self-healing up to this bound.
	Reprocess time.Duration
}

// The chain. Reprocess widens as the tiers coarsen, because the cost is measured
// in buckets rather than in wall-clock: ten daily buckets is a smaller query
// than ten one-minute buckets, and it tolerates a far longer probe outage.
var (
	tier1m = Tier{Name: "1m", Interval: time.Minute, Reprocess: 10 * time.Minute}
	tier5m = Tier{Name: "5m", Interval: 5 * time.Minute, Source: &tier1m, Reprocess: time.Hour}
	tier1h = Tier{Name: "1h", Interval: time.Hour, Source: &tier5m, Reprocess: 12 * time.Hour}
	tier1d = Tier{Name: "1d", Interval: 24 * time.Hour, Source: &tier1h, Reprocess: 72 * time.Hour}

	// Tiers is the chain in computation order. Order matters: 5m reads what 1m
	// just wrote, within the same run.
	Tiers = []Tier{tier1m, tier5m, tier1h, tier1d}
)

// grace is how long a bucket must have been closed before it is computed at all.
// It covers the ordinary lateness of in-flight results — the probe's flush
// interval, the batch round trip, the acknowledgement — as distinct from the
// replay-after-outage lateness Tier.Reprocess covers.
const grace = 30 * time.Second

// maxBucketsPerRun bounds catch-up work. A process that was down for a week has
// a week of buckets to build, and doing it in one statement would hold the single
// SQLite writer for the duration — which looks exactly like an outage to every
// monitor trying to record a heartbeat.
const maxBucketsPerRun = 2000

// Store is what the pipeline needs from persistence, named here so the tier
// contract above is the only thing either backend has to agree with.
type Store interface {
	// RollUpRaw aggregates heartbeats into the 1m tier for buckets that start
	// in [from, to), replacing any rows already there.
	RollUpRaw(ctx context.Context, from, to time.Time) (int64, error)

	// RollUpTier aggregates one tier from the tier below over the same range.
	RollUpTier(ctx context.Context, tier, source Tier, from, to time.Time) (int64, error)

	// LatestBucket is the newest bucket_start in a tier, or the zero time when
	// the tier is empty.
	LatestBucket(ctx context.Context, tier Tier) (time.Time, error)

	// EarliestRaw and EarliestBucket bootstrap a tier that has no rows yet.
	EarliestRaw(ctx context.Context) (time.Time, error)
	EarliestBucket(ctx context.Context, tier Tier) (time.Time, error)

	// DeleteHeartbeatsBefore and DeleteBucketsBefore enforce retention, in
	// bounded batches, returning how many rows went.
	DeleteHeartbeatsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	DeleteBucketsBefore(ctx context.Context, tier Tier, cutoff time.Time, limit int) (int64, error)
	DeleteDeliveriesBefore(ctx context.Context, table string, cutoff time.Time, limit int) (int64, error)

	// RefreshUptimeCache recomputes the per-monitor uptime windows from the
	// rollups.
	RefreshUptimeCache(ctx context.Context, now time.Time) (int64, error)

	// PurgeNext deletes one enqueued entity's history in bounded batches,
	// reporting whether the entry is finished.
	PurgeNext(ctx context.Context, limit int) (rows int64, done bool, err error)

	// IncrementalVacuum returns pages to the filesystem. Deleting rows from
	// SQLite does not shrink the file on its own (data model §9.2).
	IncrementalVacuum(ctx context.Context, pages int) error

	// AutoVacuumMode reports the database's auto_vacuum setting, so a database
	// that cannot reclaim space says so once rather than silently growing.
	AutoVacuumMode(ctx context.Context) (int, error)
}

// Retention is the per-tier policy, in days, from Settings.retention. Zero means
// keep indefinitely.
type Retention struct {
	RawDays             int
	Rollup1mDays        int
	Rollup5mDays        int
	Rollup1hDays        int
	Rollup1dDays        int
	WebhookDeliveryDays int

	// NotificationDeliveryDays has no field in the frozen Settings schema; the
	// data model (§9.1) sets it at 90 days because post-mortems cite these rows.
	// It is here so the number is stated once rather than buried in a query.
	NotificationDeliveryDays int
}

// DefaultRetention matches the defaults in the OpenAPI Settings schema.
func DefaultRetention() Retention {
	return Retention{
		RawDays:                  7,
		Rollup1mDays:             30,
		Rollup5mDays:             90,
		Rollup1hDays:             365,
		Rollup1dDays:             0, // indefinite: the long history the reporting engine sells
		WebhookDeliveryDays:      30,
		NotificationDeliveryDays: 90,
	}
}

// Validate enforces the one rule that makes the chain coherent: a coarser tier
// must be kept at least as long as a finer one. Reversed, history would develop
// a hole in the middle — detail retained past the summary that replaced it.
func (r Retention) Validate() error {
	// Indefinite (0) is longer than any finite value, so it compares as +inf.
	longer := func(coarse, fine int) bool { return coarse == 0 || (fine != 0 && coarse >= fine) }

	pairs := []struct {
		coarseName, fineName string
		coarse, fine         int
	}{
		{"rollup_1m_days", "raw_days", r.Rollup1mDays, r.RawDays},
		{"rollup_5m_days", "rollup_1m_days", r.Rollup5mDays, r.Rollup1mDays},
		{"rollup_1h_days", "rollup_5m_days", r.Rollup1hDays, r.Rollup5mDays},
		{"rollup_1d_days", "rollup_1h_days", r.Rollup1dDays, r.Rollup1hDays},
	}
	for _, p := range pairs {
		if !longer(p.coarse, p.fine) {
			return fmt.Errorf("%s (%d) must be at least %s (%d): a coarser tier kept for less time than a finer one leaves a hole in history",
				p.coarseName, p.coarse, p.fineName, p.fine)
		}
	}
	if r.RawDays < 1 {
		return errors.New("raw_days must be at least 1")
	}
	return nil
}

func (r Retention) forTier(t Tier) int {
	switch t.Name {
	case "1m":
		return r.Rollup1mDays
	case "5m":
		return r.Rollup5mDays
	case "1h":
		return r.Rollup1hDays
	default:
		return r.Rollup1dDays
	}
}

// Bucket floors a time to its bucket start.
//
// Computed from the Unix epoch rather than with time.Truncate, which rounds
// against year 1. The two agree for every interval here, and stating the epoch
// explicitly is what makes the contract in §5.4 literally true rather than
// true by coincidence.
func Bucket(t time.Time, interval time.Duration) time.Time {
	step := int64(interval / time.Second)
	secs := t.UTC().Unix()
	floored := secs / step * step
	// Integer division truncates toward zero, so pre-epoch times need nudging
	// down to keep this a floor rather than a round-toward-1970.
	if secs < 0 && floored != secs {
		floored -= step
	}
	return time.Unix(floored, 0).UTC()
}

// Runner drives one pass of the pipeline: build the tiers, enforce retention,
// refresh the uptime cache, advance any pending purge, and hand freed pages back
// to the filesystem.
type Runner struct {
	store     Store
	log       *slog.Logger
	retention Retention

	// warnedNoVacuum keeps the "this database cannot reclaim space" warning to
	// once per process. It is a real problem and it is also unchanging, so
	// repeating it hourly forever would train people to ignore the log.
	warnedNoVacuum bool

	lastCacheRefresh time.Time
}

// NewRunner returns a runner over one store.
func NewRunner(store Store, retention Retention, log *slog.Logger) *Runner {
	return &Runner{store: store, retention: retention, log: log}
}

// Batch sizes. Bounded because SQLite has one writer: an unbounded DELETE over a
// week of heartbeats holds it for the duration, and every monitor trying to
// record a result during that window looks like an outage.
const (
	deleteBatch = 5000
	purgeBatch  = 5000
	vacuumPages = 1000
)

// cacheInterval is how often the uptime cache is recomputed, independently of
// the rollup cadence. Five minutes matches the finest tier it reads, so
// refreshing more often would recompute from buckets that had not changed.
const cacheInterval = 5 * time.Minute

// Run performs one pass. Errors from a stage are logged and the pass continues:
// a failing retention query must not stop rollups, because rollups are what the
// history the user is looking at right now is made of.
func (r *Runner) Run(ctx context.Context, now time.Time) {
	if err := r.Rollup(ctx, now); err != nil {
		r.log.Error("rollup", "error", err)
	}
	if err := r.Retain(ctx, now); err != nil {
		r.log.Error("retention", "error", err)
	}
	if err := r.Purge(ctx); err != nil {
		r.log.Error("purge", "error", err)
	}

	// The cache is refreshed on its own, slower cadence. It reads a few hundred
	// buckets per monitor per window, which at 5,000 monitors is real work for a
	// figure nobody needs to the minute — and its staleness is bounded and
	// reported through computed_at, which is the contract §5.5 states.
	if now.Sub(r.lastCacheRefresh) >= cacheInterval {
		if _, err := r.store.RefreshUptimeCache(ctx, now); err != nil {
			r.log.Error("refresh uptime cache", "error", err)
		} else {
			r.lastCacheRefresh = now
		}
	}
}

// Rollup builds every tier, finest first.
func (r *Runner) Rollup(ctx context.Context, now time.Time) error {
	for _, tier := range Tiers {
		from, to, err := r.window(ctx, tier, now)
		if err != nil {
			return err
		}
		if !from.Before(to) {
			continue
		}

		var rows int64
		if tier.Source == nil {
			rows, err = r.store.RollUpRaw(ctx, from, to)
		} else {
			rows, err = r.store.RollUpTier(ctx, tier, *tier.Source, from, to)
		}
		if err != nil {
			return fmt.Errorf("roll up %s: %w", tier.Name, err)
		}
		if rows > 0 {
			r.log.Debug("rolled up", "tier", tier.Name, "buckets", rows,
				"from", from.Format(time.RFC3339), "to", to.Format(time.RFC3339))
		}
	}
	return nil
}

// window decides which buckets this run computes.
//
// The watermark is derived from the data rather than stored: the newest bucket
// in the tier, wound back by Tier.Reprocess. Deriving it means a tier that was
// dropped, truncated, or restored from an older backup rebuilds itself with no
// separate state to have gone stale — and it means the recomputation is
// idempotent, because every write is an upsert over a full recount of the bucket.
func (r *Runner) window(ctx context.Context, tier Tier, now time.Time) (from, to time.Time, err error) {
	// Only closed buckets, and only once the grace period has passed. A bucket
	// computed before its interval ends is a partial figure that looks final.
	to = Bucket(now.Add(-grace), tier.Interval)

	latest, err := r.store.LatestBucket(ctx, tier)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("latest %s bucket: %w", tier.Name, err)
	}

	switch {
	case !latest.IsZero():
		from = latest.Add(-tier.Reprocess)
	default:
		// Nothing in this tier yet: start from the oldest thing there is to
		// aggregate. Absent that, there is nothing to do at all.
		var earliest time.Time
		if tier.Source == nil {
			earliest, err = r.store.EarliestRaw(ctx)
		} else {
			earliest, err = r.store.EarliestBucket(ctx, *tier.Source)
		}
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("earliest source for %s: %w", tier.Name, err)
		}
		if earliest.IsZero() {
			return to, to, nil
		}
		from = earliest
	}
	from = Bucket(from, tier.Interval)

	// Bound catch-up. What is left over is picked up by the next pass, so a
	// long backfill makes progress every tick instead of blocking one of them.
	if limit := from.Add(time.Duration(maxBucketsPerRun) * tier.Interval); limit.Before(to) {
		to = limit
	}
	return from, to, nil
}

// Retain deletes past the configured horizon and gives the space back.
func (r *Runner) Retain(ctx context.Context, now time.Time) error {
	var freed int64

	if days := r.retention.RawDays; days > 0 {
		n, err := r.deleteAll(ctx, "heartbeats", func(ctx context.Context, cutoff time.Time) (int64, error) {
			return r.store.DeleteHeartbeatsBefore(ctx, cutoff, deleteBatch)
		}, now.AddDate(0, 0, -days))
		if err != nil {
			return err
		}
		freed += n
	}

	for _, tier := range Tiers {
		days := r.retention.forTier(tier)
		if days == 0 {
			// Indefinite. The 1d tier is the long history the reporting engine
			// sells, so this is the normal case for it.
			continue
		}
		n, err := r.deleteAll(ctx, "heartbeat_"+tier.Name, func(ctx context.Context, cutoff time.Time) (int64, error) {
			return r.store.DeleteBucketsBefore(ctx, tier, cutoff, deleteBatch)
		}, now.AddDate(0, 0, -days))
		if err != nil {
			return err
		}
		freed += n
	}

	logs := []struct {
		table string
		days  int
	}{
		{"webhook_deliveries", r.retention.WebhookDeliveryDays},
		{"notification_deliveries", r.retention.NotificationDeliveryDays},
		// audit_log is deliberately absent. Deleting an audit log defeats its
		// purpose (§9.1), so retention never touches it.
	}
	for _, l := range logs {
		if l.days == 0 {
			continue
		}
		table := l.table
		n, err := r.deleteAll(ctx, table, func(ctx context.Context, cutoff time.Time) (int64, error) {
			return r.store.DeleteDeliveriesBefore(ctx, table, cutoff, deleteBatch)
		}, now.AddDate(0, 0, -l.days))
		if err != nil {
			return err
		}
		freed += n
	}

	if freed > 0 {
		r.log.Info("retention deleted rows", "rows", freed)
		r.reclaim(ctx)
	}
	return nil
}

// deleteAll runs one bounded delete repeatedly until the table is clean of rows
// past the cutoff, or the context ends.
func (r *Runner) deleteAll(ctx context.Context, what string, batch func(context.Context, time.Time) (int64, error), cutoff time.Time) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, nil //nolint:nilerr // shutdown is not a retention failure; the next start resumes
		}
		n, err := batch(ctx, cutoff)
		if err != nil {
			return total, fmt.Errorf("delete from %s: %w", what, err)
		}
		total += n
		if n < deleteBatch {
			return total, nil
		}
	}
}

// reclaim hands freed pages back to the filesystem. Without it the database file
// stays at its high-water mark forever, which on a Pi with a 32GB card is the
// difference between working and not (§9.2).
func (r *Runner) reclaim(ctx context.Context) {
	mode, err := r.store.AutoVacuumMode(ctx)
	if err != nil {
		r.log.Warn("read auto_vacuum", "error", err)
		return
	}
	if mode != 2 { // 2 = INCREMENTAL
		if !r.warnedNoVacuum {
			r.warnedNoVacuum = true
			r.log.Warn("this database cannot reclaim disk space: auto_vacuum is not INCREMENTAL, "+
				"so deleted heartbeats free pages inside the file but never shrink it. "+
				"Fix with: PRAGMA auto_vacuum=INCREMENTAL; VACUUM; — which rewrites the whole "+
				"file and needs free space equal to its size",
				"auto_vacuum", mode)
		}
		return
	}
	if err := r.store.IncrementalVacuum(ctx, vacuumPages); err != nil {
		r.log.Warn("incremental vacuum", "error", err)
	}
}

// Purge advances the deletion of history belonging to monitors that are already
// gone. Deleting a monitor returns 204 immediately and enqueues its heartbeats
// here, because a cascade over millions of rows cannot run inside a request.
//
// One entity per pass: orphaned heartbeats are invisible to every API query,
// which all filter through a live monitor, so a purge that lags is a disk-space
// concern and never a correctness one.
func (r *Runner) Purge(ctx context.Context) error {
	for {
		rows, done, err := r.store.PurgeNext(ctx, purgeBatch)
		if err != nil {
			return err
		}
		if rows > 0 {
			r.log.Debug("purged rows", "rows", rows)
		}
		if done {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return nil //nolint:nilerr // shutdown; the queue is durable and the next start resumes
		}
	}
}
