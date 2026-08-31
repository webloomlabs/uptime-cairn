package report

import (
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
	)

	flush := func() {
		if current != nil {
			out = append(out, *current)
			current = nil
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

		// The same projection the error budget uses, applied to one day: the
		// observed down proportion over the day's length. It is the only
		// conversion in the product from checks to seconds, and using a second
		// one here would let the breach durations disagree with the budget they
		// are supposed to explain.
		seconds := int(float64(down) / float64(observed) * (24 * time.Hour).Seconds())

		end := b.Start.AddDate(0, 0, 1)
		if current == nil {
			current = &Breach{StartedAt: b.Start, EndedAt: end, DurationSeconds: seconds}
			continue
		}
		current.EndedAt = end
		current.DurationSeconds += seconds
	}
	flush()

	return out
}
