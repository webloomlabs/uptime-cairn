package report

import (
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Why a p95 is absent. Reported rather than left blank, because a missing
// percentile with no reason beside it reads as a bug in the product.
const (
	// ReasonInsufficientRaw is settings.retention.raw_days being shorter than
	// the seven-day window. The figure is omitted rather than computed over
	// whatever raw happens to hold, which would print a three-day number under a
	// seven-day heading.
	ReasonInsufficientRaw = "insufficient_raw_retention"

	// ReasonNoSuccessfulChecks is raw covering the window and containing no
	// successful check to rank. A percentile of nothing is not zero.
	ReasonNoSuccessfulChecks = "no_successful_checks"

	// ReasonScopeTooLarge is the report covering more monitors than the instance
	// computes this figure for.
	//
	// It is the one statistic in the block that cannot be batched — a rank over
	// raw heartbeats, one query per monitor across roughly ten thousand rows —
	// so an estate-wide report would spend longer here than on the whole of the
	// rest of the document. A client report over a handful of monitors is the
	// case that wants it and the case that gets it.
	ReasonScopeTooLarge = "scope_too_large"
)

// MethodNearestRank is the only method this product computes, stated on the
// figure because a percentile quoted without its method is worse than none.
const MethodNearestRank = "nearest_rank"

// DayLatency is one point on the daily average series.
type DayLatency struct {
	Date time.Time

	// AverageMs is nil for a day with no successful checks, which renders as a
	// gap rather than as zero. A day the probe could not run is not a day the
	// service answered instantly.
	AverageMs   *float64
	SampleCount int
}

// P95 is the trailing-seven-day percentile and the circumstances of it. It is
// always present as a structure and often absent as a number; Available is what
// separates the two, so a caller cannot mistake "not computed" for "zero".
type P95 struct {
	Available   bool
	ValueMs     *float64
	WindowStart *time.Time
	WindowEnd   *time.Time
	Method      string
	Reason      string
}

// Latency is the five figures of ADR-006 and no others, mirroring
// ReportResponseTimeBlock.
//
// There is deliberately no window minimum or maximum. Over a month those are the
// single fastest and single slowest successful check out of tens of thousands;
// the maximum in particular reads as alarming while carrying no signal, and one
// garbage-collection pause moves it. BestDay and WorstDay give the same shape of
// information from a statistic that only moves on sustained degradation.
type Latency struct {
	AverageMs   *float64
	SampleCount int

	Daily             []DayLatency
	BestDay, WorstDay *DayLatency

	TargetMs        *int
	DaysOverTarget  *int
	DatesOverTarget []time.Time

	// P95 is nil where the figure does not apply at all rather than being
	// unavailable for a reason — which is the estate summary, because a
	// percentile merges no better across monitors than across time. An absent
	// object and an unavailable one are different statements, and the contract
	// requires a reason whenever the object is present and unavailable.
	P95 *P95
}

// ComputeLatency derives the whole block from the window total and the daily
// series — the same two reads every other figure in the report comes from.
//
// The window average is SUM(response_time_sum) / SUM(response_time_count), which
// is exact at any tier because both are additive. It is emphatically not the
// mean of the daily averages: a day with ten checks and a day with ten thousand
// weigh the same in that calculation and should not.
//
// target is the template's response_time_target_ms, which classifies days after
// the fact and is a different thing from a monitor's own
// response_time_threshold_ms — that one marks a check down when breached and has
// already had its effect on the uptime figures above.
func ComputeLatency(total store.HistoryBucket, daily []store.HistoryBucket, target *int) Latency {
	l := Latency{
		SampleCount: total.ResponseTimeCount,
		TargetMs:    target,
	}
	if total.ResponseTimeCount > 0 {
		avg := total.ResponseTimeSum / float64(total.ResponseTimeCount)
		l.AverageMs = &avg
	}

	var overTarget int
	for _, b := range daily {
		point := DayLatency{Date: b.Start, SampleCount: b.ResponseTimeCount}
		if b.ResponseTimeCount > 0 {
			avg := b.ResponseTimeSum / float64(b.ResponseTimeCount)
			point.AverageMs = &avg

			// Extremes are taken over observed days only. A day with no
			// successful checks is not the best day the service ever had.
			if l.BestDay == nil || avg < *l.BestDay.AverageMs {
				best := point
				l.BestDay = &best
			}
			if l.WorstDay == nil || avg > *l.WorstDay.AverageMs {
				worst := point
				l.WorstDay = &worst
			}
			if target != nil && avg > float64(*target) {
				overTarget++
				l.DatesOverTarget = append(l.DatesOverTarget, b.Start)
			}
		}
		l.Daily = append(l.Daily, point)
	}

	// Null rather than zero where no target is set: "no days exceeded the
	// target" and "there is no target" are different statements, and only one of
	// them is a compliment.
	if target != nil {
		l.DaysOverTarget = &overTarget
	}
	return l
}

// TrailingP95 packages the one real percentile in the product.
//
// covered is the answer from RawCovers, and it is a gate rather than a hint:
// raw_days is operator-configurable down to one, and a p95 over three days
// printed under a seven-day heading is exactly the unlabelled approximation
// ADR-006 exists to prevent. value is nil when raw held no successful check to
// rank, which is a different absence and says so.
//
// The window is carried on the figure because it is *not* the report window, and
// the two sitting side by side without labels — "average, 30 days: 180 ms" next
// to "p95: 940 ms" — reads as a contradiction.
func TrailingP95(covered bool, value *float64, from, to time.Time) *P95 {
	if !covered {
		return &P95{Reason: ReasonInsufficientRaw}
	}
	if value == nil {
		return &P95{Reason: ReasonNoSuccessfulChecks}
	}
	return &P95{
		Available:   true,
		ValueMs:     value,
		WindowStart: &from,
		WindowEnd:   &to,
		Method:      MethodNearestRank,
	}
}
