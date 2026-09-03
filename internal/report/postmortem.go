package report

import (
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The three MTT* intervals are model.Incident.Metrics(), reused rather than
// recomputed. See PostMortem for why.

// The incident post-mortem.
//
// # The rule this file exists to enforce
//
// **MTTD is reported as unknown when it is unknown, and that is the common case
// rather than the edge one.** `AutoOpened` is never set before Phase 3 and
// `DetectedAt` is nil on any incident somebody recorded by hand — which is every
// incident on every install today. A post-mortem that filled the gap by treating
// `started_at` as the detection time would report a time-to-detect of zero on an
// outage nobody noticed for forty minutes, and it would do so with a confident
// number in a document written to be read by the people who were affected.
//
// So all three MTT* figures are pointers, and each is nil for its own reason:
//
//   - MTTD is nil with no `detected_at`. It cannot be reconstructed from the
//     timeline afterwards, which is exactly why the column exists.
//   - MTTA is nil when nobody acknowledged, which is a real outcome and not a
//     missing value: an incident resolved before anybody picked it up has no
//     time-to-acknowledge, and zero would claim somebody was there instantly.
//   - MTTR is nil while the incident is open. An unresolved incident has no
//     time-to-resolve, and clamping it to "so far" would produce a figure that
//     changes every time the report is regenerated.
//
// # A negative interval is refused rather than reported
//
// Every interval runs from `started_at` — when the outage began — to the instant
// being measured, so all three are non-negative on a correctly ordered incident.
// Timestamps are editable, though: `started_at` "may be in the past: an incident
// recorded after the fact is the normal case", so an operator correcting a start
// time to after a detection they had already recorded produces an ordering
// nothing enforces. A negative MTTD is not a fact about the world; it is a fact
// about somebody's typing, and it is reported as unknown for the same reason an
// inferred one would be.

// Incident is one incident as the post-mortem reports it.
//
// Computed rather than passed through: the raw row carries instants and this
// carries the intervals between them, which is what a post-mortem is about.
type Incident struct {
	ID    model.ID
	Title string
	State string

	// Impact is the incident's declared severity, carried because a post-mortem
	// that lists three incidents without saying which one mattered is a list
	// rather than a report.
	Impact string

	StartedAt      time.Time
	DetectedAt     *time.Time
	AcknowledgedAt *time.Time
	ResolvedAt     *time.Time

	// AutoOpened distinguishes an incident the system raised from one a person
	// declared. It is never true before Phase 3, and it is carried now because
	// it is what makes a null MTTD explicable rather than mysterious.
	AutoOpened bool

	MonitorIDs []model.ID

	// The three intervals, in seconds, each nil for its own reason. See the file
	// comment: none of the three is ever inferred.
	MTTDSeconds *int
	MTTASeconds *int
	MTTRSeconds *int

	// AlertsFired is how many notifications this incident produced, from the
	// delivery log — which is retained ninety days precisely because
	// post-mortems cite it. Nil for an incident older than that retention, which
	// is a different statement from zero: "we no longer know" and "nobody was
	// told" are the two answers a post-mortem most needs to keep apart.
	AlertsFired *int
}

// PostMortem builds the incident sections for a window.
//
// The alert counts are supplied by the caller rather than looked up here,
// because the delivery log is not part of the read-side contract this package
// declares and adding a method for one report type would put it in front of
// every consumer. A nil map means "not known", which is what an install whose
// retention has swallowed the rows should say.
func PostMortem(incidents []model.Incident, alerts map[model.ID]int, retainedFrom *time.Time) []Incident {
	out := make([]Incident, 0, len(incidents))
	for _, in := range incidents {
		section := Incident{
			ID:             in.ID,
			Title:          in.Title,
			State:          in.State,
			Impact:         in.Impact,
			StartedAt:      in.StartedAt,
			DetectedAt:     in.DetectedAt,
			AcknowledgedAt: in.AcknowledgedAt,
			ResolvedAt:     in.ResolvedAt,
			AutoOpened:     in.AutoOpened,
			MonitorIDs:     in.MonitorIDs,
		}

		// **The arithmetic is model.Incident.Metrics(), not a second copy of
		// it.** The incident API already publishes these three intervals, and
		// two implementations would eventually disagree — at which point a
		// client comparing the incident screen against the post-mortem would
		// find two different times to resolve for one outage, with nothing to
		// say which was right.
		//
		// What is added here is the refusal below, which Metrics() deliberately
		// does not do: changing it would alter a Phase 1 surface, and the report
		// is where an impossible interval most needs to read as unknown.
		metrics := in.Metrics()
		section.MTTDSeconds = nonNegative(metrics.TimeToDetect)
		section.MTTASeconds = nonNegative(metrics.TimeToAcknowledge)
		section.MTTRSeconds = nonNegative(metrics.TimeToResolve)

		section.AlertsFired = alertsFor(in, alerts, retainedFrom)
		out = append(out, section)
	}
	return out
}

// alertsFor is how many alerts an incident produced, or nil where the answer is
// no longer knowable.
//
// The distinction is the point. Zero means "the delivery log covers this
// incident and holds nothing for it", which in a post-mortem reads as *nobody was
// told* — one of the more serious findings a post-mortem can carry. Nil means
// "the rows have been swept", which is a statement about retention and about
// nothing else. Collapsing them would let a retention policy manufacture that
// finding.
func alertsFor(in model.Incident, alerts map[model.ID]int, retainedFrom *time.Time) *int {
	if retainedFrom != nil && in.StartedAt.Before(*retainedFrom) {
		return nil
	}
	if alerts == nil {
		return nil
	}
	count := alerts[in.ID]
	return &count
}

// nonNegative narrows an interval to one that describes an ordering.
//
// A negative result is nil rather than negative. Incident timestamps are
// editable — "an incident recorded after the fact is the normal case" — so an
// operator correcting a start time to after a detection they had already
// recorded produces an ordering nothing in the schema enforces. The result is a
// fact about somebody's typing rather than about the outage, and "unknown" is
// the honest thing to print beside it.
func nonNegative(seconds *int64) *int {
	if seconds == nil || *seconds < 0 {
		return nil
	}
	out := int(*seconds)
	return &out
}

// MTTSummary is the aggregate across a post-mortem's incidents.
//
// The **means** the phase plan asks for, and each is taken over the incidents
// that actually have the figure rather than over all of them. Treating a null as
// a zero would drag every average towards zero in proportion to how much is
// unknown — which is the opposite of what a reader should conclude from missing
// data — and dividing by the wrong denominator is the same mistake the uptime
// rules refuse one layer down.
type MTTSummary struct {
	// Incidents is how many the window held, which is the denominator nothing
	// else here uses.
	Incidents int

	// Each mean is nil when no incident in the window supplied that figure, and
	// each count says how many did. The count is not decoration: "22 minutes,
	// from one incident of nine" is a very different claim from "22 minutes".
	MeanTimeToDetect      *int
	DetectKnownCount      int
	MeanTimeToAcknowledge *int
	AcknowledgeKnownCount int
	MeanTimeToResolve     *int
	ResolveKnownCount     int
}

// Summarise averages the three intervals over the incidents that have them.
func Summarise(incidents []Incident) MTTSummary {
	summary := MTTSummary{Incidents: len(incidents)}

	for _, block := range []struct {
		pick  func(Incident) *int
		mean  **int
		count *int
	}{
		{func(i Incident) *int { return i.MTTDSeconds }, &summary.MeanTimeToDetect, &summary.DetectKnownCount},
		{func(i Incident) *int { return i.MTTASeconds }, &summary.MeanTimeToAcknowledge, &summary.AcknowledgeKnownCount},
		{func(i Incident) *int { return i.MTTRSeconds }, &summary.MeanTimeToResolve, &summary.ResolveKnownCount},
	} {
		total := 0
		known := 0
		for _, in := range incidents {
			if v := block.pick(in); v != nil {
				total += *v
				known++
			}
		}
		*block.count = known
		if known > 0 {
			mean := total / known
			*block.mean = &mean
		}
	}
	return summary
}
