// Package report computes the report model that every renderer consumes.
//
// The discipline is the one internal/rollup already follows and for the same
// reason: ADR-002's repository interface only holds if both backends produce the
// same numbers, so the contract lives here, backend-independent, and the backend
// supplies only the queries. Timescale will answer these with continuous
// aggregates and SQLite answers them with the rollup tiers; an SLA figure that
// differed between them would be a compatibility break disguised as a rounding
// error.
//
// The rule that shapes everything below, from ADR-007: renderers are siblings,
// not a chain. This package produces the document; HTML, PDF, CSV and JSON each
// consume it, and none consumes another's output. Nothing here knows what a page
// looks like.
//
// Two constraints from ADR-006 are load-bearing on the interface itself rather
// than on the computation, which is why they are named this far down the stack:
//
//   - Reads go through the rollup tiers, never raw, except inside raw retention
//     where a real percentile is being computed. A report that scans a year of
//     heartbeats is the failure ADR-004 exists to prevent, one layer up.
//   - Sum and count are carried to the edge, never an average. Both are additive,
//     which is what makes an aggregate over an arbitrary window exact rather than
//     estimated — and it is why there is no method here that returns a mean.
package report

import (
	"context"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Scope is the monitor selection a report covers, as a query rather than as a
// saved list.
//
// Selection is by rule wherever possible and is resolved at run time: an agency
// that adds a monitor to a client's tag expects it in that client's next report
// without editing the report, and a list flattened at save time cannot do that.
// The three sources combine as a union.
//
// This describes the query, not the stored template — the persisted form is JSON
// on report_templates.scope and belongs to the type that reads it.
type Scope struct {
	MonitorIDs []model.ID
	GroupIDs   []model.ID
	TagIDs     []model.ID
}

// Empty reports whether the scope selects nothing at all, which is a report
// definition somebody should be told about rather than an empty document.
func (s Scope) Empty() bool {
	return len(s.MonitorIDs)+len(s.GroupIDs)+len(s.TagIDs) == 0
}

// Target is an SLO target and the level it was found at.
//
// The source travels with the number because §4.3 requires the report to print
// it: a monitor silently inheriting its group's target is otherwise invisible to
// whoever reads the report, and "99.9%" means something different when nobody
// set it on this monitor.
type Target struct {
	Percent float64
	Source  string
}

// Where a target was resolved from. The template's own override is applied by
// the caller — it is not stored per monitor and the store cannot see it.
const (
	TargetFromMonitor = "monitor"
	TargetFromGroup   = "group"
)

// Store is what computing a report needs from persistence.
//
// Named by the consumer, following internal/api and internal/rollup, and for the
// reason store.go gives for declaring no interfaces up front: a method set
// invented before the code that uses it is a method set shaped by guesswork.
// Everything here answers a question §4.3 of the phase plan actually asks.
//
// No method takes an organisation. That is the existing convention rather than a
// tenancy decision — the store resolves it — and Phase 3 changes it everywhere at
// once or nowhere.
type Store interface {
	// MonitorsInScope resolves a Scope to the monitors it covers, at the instant
	// the run starts.
	//
	// Unpaginated, unlike ListMonitors, and deliberately: a report over an
	// estate is one document over the whole set, and paging through a cursor to
	// assemble it would make the document a function of page size. The cost is
	// bounded by the scope rather than by the install.
	MonitorsInScope(ctx context.Context, s Scope) ([]model.Monitor, error)

	// WindowTotals is one aggregate per monitor over the whole report window,
	// read from the named tier.
	//
	// Batched rather than per-monitor because this is the query the extended
	// load gate exercises — fifty concurrent runs across 5,000 monitors — and a
	// call per monitor is the fan-out that gate exists to catch.
	//
	// The returned buckets carry counts and response-time sum and count. The
	// caller divides, applies its maintenance policy, and decides its own
	// denominator; nothing here computes a ratio, because what counts against an
	// SLO is a choice the report states on its face rather than a property of a
	// query.
	WindowTotals(ctx context.Context, ids []model.ID, from, to time.Time, tier string) (map[model.ID]store.HistoryBucket, error)

	// DailySeries is the per-day series over the window, from the 1d tier — the
	// exhibit ADR-006 makes primary, and the source of the daily average, the
	// best and worst day, and the days over target.
	//
	// Batched for the same reason as WindowTotals. A month is 31 buckets per
	// monitor, which is small per monitor and is not small at 5,000 of them.
	DailySeries(ctx context.Context, ids []model.ID, from, to time.Time) (map[model.ID][]store.HistoryBucket, error)

	// RawCovers reports whether raw heartbeats reach back far enough to answer a
	// range completely. It is the gate on the trailing-seven-day p95 and it is
	// not advisory: RawDays is operator-configurable down to one, and a p95 over
	// three days printed under a seven-day heading is the kind of wrong figure
	// this phase exists to avoid. Short coverage omits the block, labelled.
	RawCovers(ctx context.Context, id model.ID, from time.Time, tier string) (bool, error)

	// UptimeFromRaw computes a real nearest-rank percentile over raw heartbeats.
	//
	// The only percentile in the product, and the only one there can be: a
	// quantile is a rank statistic and does not merge, so no coarser tier holds
	// one. Per-monitor rather than batched because it is confined to seven days
	// and because raw is the one place a report is allowed to read a wide range
	// of rows at all.
	UptimeFromRaw(ctx context.Context, id model.ID, from, to time.Time) (store.HistoryBucket, error)

	// SLOTargets resolves each monitor's uptime target: its own if it has one,
	// otherwise its group's, and it reports which of the two answered.
	//
	// A separate method rather than a field on model.Monitor, deliberately.
	// Phase 2 only *reads* this number — nothing alerts on it and no monitor's
	// status is affected by it — and putting it on the domain type would hand it
	// to every consumer in the product, through both write paths, ahead of the
	// phase that gives it meaning. It is also the resolution that a field could
	// not express: the fallback and its provenance are one answer, and computing
	// them in the caller would mean every caller getting the order right.
	//
	// A monitor with no target at either level is **absent from the map** rather
	// than present with a zero. Zero is a real target percentage and an absurd
	// one, and the report's SLA block is omitted where there is no target instead
	// of being computed against a number nobody chose.
	SLOTargets(ctx context.Context, ids []model.ID) (map[model.ID]Target, error)

	// ListIncidents supplies the incident log and the post-mortem, filtered by
	// the window through IncidentFilter's From and To.
	//
	// Reused verbatim from the API's own store rather than restated, so that a
	// report and the incident screen cannot come to disagree about what "in this
	// window" means.
	ListIncidents(ctx context.Context, after *store.Cursor, limit int, filter store.IncidentFilter) ([]model.Incident, bool, error)
}

// Deliberately absent, so that the gaps are decisions rather than omissions:
//
// A percentile over the report window. There is none, at any tier, and ADR-006
// removes the mechanism rather than policing it — HistoryFromTier already
// substitutes NULL for the stored maximum-of-p95, and nothing here asks for it.
//
// Window-level minimum and maximum. Over a month they are extreme-value
// statistics: the single slowest successful check out of tens of thousands,
// which reads as alarming and carries no signal. The daily series gives the same
// shape of information from a statistic that only moves on sustained
// degradation, which is why DailySeries is here and a MinMax method is not.
//
// Certificate and domain expiries. The document has a place for them and the
// data exists, but the query that answers /api/v1/expiries returns rows this
// package has no type for yet, and inventing one before that endpoint is written
// is exactly what store.go warns against. It joins when the endpoint does.
//
// Anything that writes. A run's lifecycle — templates, schedules, artifacts,
// deliveries, share links — is the API's half of the surface and belongs to the
// handlers that use it. This interface computes; it does not record.
