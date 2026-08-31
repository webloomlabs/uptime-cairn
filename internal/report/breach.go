package report

import (
	"math"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Breach is one contiguous period counted against the error budget.
type Breach struct {
	StartedAt time.Time

	// EndedAt is exclusive, the same half-open convention every bucket read in
	// this codebase uses.
	EndedAt time.Time

	// DurationSeconds is the downtime **inside** the period, not the length of
	// the period. A day containing four minutes of downtime is a four-minute
	// breach spanning that day, and reporting 24 hours because the day is the
	// unit would be the same overstatement ADR-006 refuses for percentiles.
	DurationSeconds int

	// IncidentID links the breach to a declared incident where one covers it.
	// Nil is the common case: incidents are declared by a human and most
	// downtime never gets one.
	IncidentID *model.ID
}

// ComputeBreaches finds the periods that consumed budget, from the daily series.
//
// # Why days, when the plan asks for timestamps
//
// Three sources could answer this and only one of them is always available.
//
// Raw heartbeats would give the real minute a service went down and came back,
// and they are bounded by settings.retention.raw_days — seven by default. An SLA
// report covers a completed month, which is outside raw retention on almost
// every install, so a breach log built on raw would be empty for exactly the
// reports that need one. Incidents carry real timestamps and are declared by a
// human; most installs declare few or none, and a log that silently omits
// unrecorded outages is worse than a coarse one.
//
// The 1d tier is kept indefinitely and therefore always answers. So a breach is
// a run of consecutive days that contained downtime, bounded by those days, and
// the duration inside it is the projected downtime rather than the span — which
// keeps the total honest even though the boundaries are coarse.
//
// The coarseness is not hidden: meta.resolution on the document states the tier
// that answered, and it is the same "1d" that produced these dates. A finer log
// arrives when it can be computed without lying, which for a historical month
// means not from this data.
//
// Days the probe could not observe do not break a run and do not start one.
// Downtime either side of a gap is more likely one outage than two, and a gap is
// not evidence of recovery.
func ComputeBreaches(daily []store.HistoryBucket, handling string) []Breach {
	var (
		out     []Breach
		current *Breach
		seconds float64
	)

	// Accumulated as a float and rounded once, at the end of a run. Rounding
	// each day and adding the results loses up to half a second a day — twelve
	// seconds over a month, which is enough to make a breach table and an error
	// budget disagree in the last digit and enough for somebody to ask why.
	flush := func() {
		if current != nil {
			current.DurationSeconds = int(math.Round(seconds))
			out = append(out, *current)
			current = nil
			seconds = 0
		}
	}

	for _, b := range daily {
		down := b.Down
		if handling == MaintenanceCountAsDown {
			down += b.Maintenance
		}

		observed := b.Up + b.Down
		if handling == MaintenanceCountAsUp || handling == MaintenanceCountAsDown {
			observed += b.Maintenance
		}

		if down == 0 {
			// A day with observations and no downtime ends a run. A day with
			// nothing observed at all ends nothing: it is a gap, and a gap is
			// not evidence that the service recovered.
			if observed > 0 {
				flush()
			}
			continue
		}

		seconds += dayDowntimeSeconds(down, observed)

		end := b.Start.AddDate(0, 0, 1)
		if current == nil {
			current = &Breach{StartedAt: b.Start, EndedAt: end}
			continue
		}
		current.EndedAt = end
	}
	flush()

	return out
}

// dayDowntimeSeconds converts one day's counts into seconds of downtime.
//
// **The only conversion in the product from checks to time**, and everything
// that reports a duration goes through it: the breach log, and the error budget
// the breach log is there to explain. Two conversions would let a report say
// "budget used 3h 36m" above a table of breaches summing to 2h 24m, which is the
// kind of internal contradiction an auditor finds in thirty seconds.
//
// Per day rather than per window, and that is the substantive part. Projecting a
// window-level down proportion onto the window's whole length attributes the
// observed failure rate to days **nobody observed** — inventing downtime on a
// day the probe was off, which is precisely what the denominator rules refuse
// one layer up. A day with no observations contributes nothing here, because
// nothing is what is known about it.
func dayDowntimeSeconds(down, observed int) float64 {
	if observed <= 0 || down <= 0 {
		return 0
	}
	return float64(down) / float64(observed) * (24 * time.Hour).Seconds()
}

// DowntimeSeconds totals the downtime the breach log accounts for. It is what
// the error budget is measured against.
//
// Defined as the sum of the breaches rather than as a second pass over the same
// days, so the total under a table can never disagree with the rows in it. Two
// implementations of one sum is how a report ends up contradicting itself in a
// way that survives review, because both halves look right on their own.
func DowntimeSeconds(daily []store.HistoryBucket, handling string) int {
	var total int
	for _, b := range ComputeBreaches(daily, handling) {
		total += b.DurationSeconds
	}
	return total
}
