package report

import "github.com/webloomlabs/uptime-cairn/internal/store"

// How declared maintenance is counted. Stated on the figure rather than only in
// the template, because the same monitor over the same month produces three
// different uptime percentages under these three settings, and a report that
// does not say which one it used is a number nobody can check.
const (
	// MaintenanceExclude leaves maintenance out of the denominator entirely, and
	// is the default. Planned work is not an outage the customer is owed for.
	MaintenanceExclude = "exclude"

	// MaintenanceCountAsUp puts it in the denominator as time the service met
	// its obligation, which is what a contract stating "excluding scheduled
	// windows" usually means in practice — the window still consumes the month.
	MaintenanceCountAsUp = "count_as_up"

	// MaintenanceCountAsDown counts it against the figure. Rare and deliberate:
	// an internal SLO that refuses to let planned downtime be free.
	MaintenanceCountAsDown = "count_as_down"
)

// Uptime is the computed uptime block for one monitor or for an estate,
// mirroring ReportUptimeBlock in the API contract.
//
// The whole point of the type is that the denominator travels with the ratio.
// §4.3: an SLA computed over 60% observation is not an SLA, so what was counted,
// what was excluded, and how much of the window observed nothing are all part of
// the figure rather than context somebody has to go and find.
type Uptime struct {
	// Ratio is nil when the denominator is zero. A window in which nothing was
	// observed has no uptime percentage, and reporting one — in either direction
	// — invents a fact. Zero would claim total downtime; one would claim perfect
	// service from a probe that never ran.
	Ratio *float64

	// MaintenanceHandling is the policy this figure was computed under.
	MaintenanceHandling string

	// ObservedChecks is the denominator actually used, after the maintenance
	// policy has been applied.
	ObservedChecks int

	UpChecks          int
	DownChecks        int
	MaintenanceChecks int

	// UnknownChecks is the probe reporting that it could not perform the check —
	// a statement about the probe, never about the target. SkippedChecks is the
	// scheduler shedding under overload, which is the same kind of fact about
	// the system rather than about the service.
	//
	// Neither is ever downtime and neither ever enters the denominator
	// (ADR-005). They are reported so that a null or a surprising ratio is
	// explicable rather than mysterious.
	UnknownChecks int
	SkippedChecks int

	// UnobservedShare is unknown plus skipped over everything that was scheduled.
	// It is the figure that says how much to trust the ratio above, and it is nil
	// only when there was nothing scheduled at all.
	UnobservedShare *float64
}

// ComputeUptime applies the denominator rules of §4.3 to one aggregated bucket.
//
// The rules, in the order they bite:
//
//   - down counts against the figure. That is the only thing that does.
//   - unknown and skipped leave the denominator entirely. The probe could not
//     look; that is not evidence the service was down, and counting it as
//     downtime is the lie internal/status/doc.go names as the one this phase
//     must not inherit.
//   - maintenance is excluded by default, and the policy is recorded on the
//     result either way.
//
// Nothing here divides by wall-clock. The denominator is observed checks, which
// is what the contract's `denominator` field defaults to and what every count in
// the rollup tiers actually measures.
func ComputeUptime(b store.HistoryBucket, handling string) Uptime {
	if handling == "" {
		handling = MaintenanceExclude
	}

	u := Uptime{
		MaintenanceHandling: handling,
		UpChecks:            b.Up,
		DownChecks:          b.Down,
		MaintenanceChecks:   b.Maintenance,
		UnknownChecks:       b.Unknown,
		SkippedChecks:       b.Skipped,
	}

	up, down := b.Up, b.Down
	switch handling {
	case MaintenanceCountAsUp:
		up += b.Maintenance
	case MaintenanceCountAsDown:
		down += b.Maintenance
	}

	// pending is not counted either, and is not configurable. A monitor that has
	// never reported has no verdict yet, which is a third thing again from "the
	// probe could not look".
	u.ObservedChecks = up + down
	if u.ObservedChecks > 0 {
		ratio := float64(up) / float64(u.ObservedChecks)
		u.Ratio = &ratio
	}

	// The share is taken over everything that was scheduled, including the
	// maintenance the policy may have just removed from the denominator —
	// otherwise excluding maintenance would quietly improve the apparent quality
	// of the observation as well as the uptime figure.
	scheduled := b.Up + b.Down + b.Maintenance + b.Unknown + b.Skipped
	if scheduled > 0 {
		share := float64(b.Unknown+b.Skipped) / float64(scheduled)
		u.UnobservedShare = &share
	}
	return u
}

// Sum totals buckets into one, for an estate-wide figure across every monitor in
// scope.
//
// Counts are additive at every tier, so this is an exact aggregation rather than
// an average of averages — which is the same property that makes a window total
// exact, applied across monitors instead of across time. There is deliberately
// no percentile here and there cannot be one (ADR-006).
func Sum(buckets []store.HistoryBucket) store.HistoryBucket {
	var total store.HistoryBucket
	for i, b := range buckets {
		if i == 0 {
			total.Start = b.Start
		} else if b.Start.Before(total.Start) {
			total.Start = b.Start
		}
		total.Up += b.Up
		total.Down += b.Down
		total.Pending += b.Pending
		total.Maintenance += b.Maintenance
		total.Unknown += b.Unknown
		total.Skipped += b.Skipped
		total.ResponseTimeSum += b.ResponseTimeSum
		total.ResponseTimeCount += b.ResponseTimeCount
	}
	// Min, max and p95 are deliberately not carried across monitors. The fastest
	// check in an estate is not a fact about the estate, and a percentile does
	// not merge any better across monitors than it does across time.
	return total
}
