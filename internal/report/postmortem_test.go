package report

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

var incidentStart = time.Date(2026, 3, 14, 2, 0, 0, 0, time.UTC)

func at(offset time.Duration) *time.Time {
	t := incidentStart.Add(offset)
	return &t
}

// **The rule this whole file exists for.** MTTD is unknown when it is unknown,
// and that is the common case rather than the edge one.
//
// `AutoOpened` is never set before Phase 3 and `DetectedAt` is nil on any
// incident somebody recorded by hand — which is every incident on every install
// today. A post-mortem that filled the gap by treating `started_at` as the
// detection time would report a time-to-detect of zero on an outage nobody
// noticed for forty minutes: a confident wrong number, in a document written to
// be read by the people who were affected.
func TestAnUndetectedIncidentReportsUnknownRatherThanZero(t *testing.T) {
	t.Parallel()

	sections := PostMortem([]model.Incident{{
		ID: model.NewID(), Title: "Checkout down", State: "resolved",
		StartedAt: incidentStart, ResolvedAt: at(90 * time.Minute),
	}}, nil, nil)

	if len(sections) != 1 {
		t.Fatalf("%d sections, want 1", len(sections))
	}
	if sections[0].MTTDSeconds != nil {
		t.Errorf("mttd = %d seconds on an incident with no detected_at. Zero here "+
			"would claim the outage was noticed the moment it began",
			*sections[0].MTTDSeconds)
	}
	// Never acknowledged is also a real outcome rather than an instant one.
	if sections[0].MTTASeconds != nil {
		t.Errorf("mtta = %d on an incident nobody acknowledged", *sections[0].MTTASeconds)
	}
	// And the one that is known is computed.
	if sections[0].MTTRSeconds == nil || *sections[0].MTTRSeconds != 5400 {
		t.Errorf("mttr = %v, want 5400", sections[0].MTTRSeconds)
	}
}

// An open incident has no time to resolve.
//
// Clamping it to "so far" would produce a figure that changes every time the
// report is regenerated — which would make two runs of the same definition over
// the same window disagree, breaking the determinism the whole subsystem rests
// on for a number that was never a fact.
func TestAnOpenIncidentHasNoTimeToResolve(t *testing.T) {
	t.Parallel()

	sections := PostMortem([]model.Incident{{
		ID: model.NewID(), Title: "Ongoing", State: "investigating",
		StartedAt: incidentStart, AcknowledgedAt: at(4 * time.Minute),
	}}, nil, nil)

	if sections[0].MTTRSeconds != nil {
		t.Error("an unresolved incident reported a time to resolve")
	}
	if sections[0].MTTASeconds == nil || *sections[0].MTTASeconds != 240 {
		t.Errorf("mtta = %v, want 240", sections[0].MTTASeconds)
	}
}

// Every interval runs from the start of the outage, and a detection recorded
// before it is unknown rather than negative.
//
// The direction is worth pinning down because it is easy to get backwards:
// `started_at` is when the outage began and `detected_at` is when the system
// first saw it, so time-to-detect is detection minus start. The first assertion
// holds that direction; the second holds the refusal.
//
// Incident timestamps are editable — an incident recorded after the fact is the
// normal case — so an operator correcting a start time to *after* a detection
// they had already recorded produces an ordering nothing in the schema enforces.
// The result is a fact about somebody's typing, not about the outage.
func TestIntervalsRunFromTheStartAndRefuseAnImpossibleOrdering(t *testing.T) {
	t.Parallel()

	ordinary := PostMortem([]model.Incident{{
		ID: model.NewID(), Title: "Noticed ten minutes in", State: "resolved",
		StartedAt: incidentStart, DetectedAt: at(10 * time.Minute),
		ResolvedAt: at(time.Hour),
	}}, nil, nil)
	if ordinary[0].MTTDSeconds == nil || *ordinary[0].MTTDSeconds != 600 {
		t.Fatalf("mttd = %v, want 600 — detection ten minutes after the outage "+
			"began is a ten-minute time to detect", ordinary[0].MTTDSeconds)
	}

	// The same incident with its start corrected to a point after the detection
	// that was already recorded.
	corrected := PostMortem([]model.Incident{{
		ID: model.NewID(), Title: "Corrected", State: "resolved",
		StartedAt: incidentStart, DetectedAt: at(-20 * time.Minute),
		ResolvedAt: at(time.Hour),
	}}, nil, nil)
	if corrected[0].MTTDSeconds != nil {
		t.Errorf("mttd = %d — a detection recorded before the declared start is a "+
			"typo, and a negative time-to-detect is not a fact about the world",
			*corrected[0].MTTDSeconds)
	}
}

// The intervals are model.Incident.Metrics(), not a second implementation.
//
// Two copies would eventually disagree, and the symptom would be a client
// comparing the incident screen against the post-mortem and finding two different
// times to resolve for one outage — with nothing in either document to say which
// was right. This asserts they agree today, which is what makes a future
// divergence a failing test rather than a support conversation.
func TestThePostMortemAgreesWithTheIncidentAPI(t *testing.T) {
	t.Parallel()

	in := model.Incident{
		ID: model.NewID(), StartedAt: incidentStart,
		DetectedAt: at(3 * time.Minute), AcknowledgedAt: at(11 * time.Minute),
		ResolvedAt: at(76 * time.Minute),
	}
	metrics := in.Metrics()
	section := PostMortem([]model.Incident{in}, nil, nil)[0]

	for _, pair := range []struct {
		name   string
		report *int
		api    *int64
	}{
		{"mttd", section.MTTDSeconds, metrics.TimeToDetect},
		{"mtta", section.MTTASeconds, metrics.TimeToAcknowledge},
		{"mttr", section.MTTRSeconds, metrics.TimeToResolve},
	} {
		if pair.report == nil || pair.api == nil {
			t.Fatalf("%s: report = %v, api = %v", pair.name, pair.report, pair.api)
		}
		if int64(*pair.report) != *pair.api {
			t.Errorf("%s: the post-mortem says %d and the incident API says %d",
				pair.name, *pair.report, *pair.api)
		}
	}
}

// The means are taken over the incidents that have each figure, not over all of
// them.
//
// Treating a null as a zero would drag every average towards zero in proportion
// to how much is unknown — the opposite of what a reader should conclude from
// missing data, and the same wrong denominator the uptime rules refuse one layer
// down. The count travels because "22 minutes, from one incident of nine" is a
// very different claim from "22 minutes".
func TestTheMeansSkipUnknownsRatherThanCountingThemAsZero(t *testing.T) {
	t.Parallel()

	sections := PostMortem([]model.Incident{
		{ID: model.NewID(), StartedAt: incidentStart, ResolvedAt: at(20 * time.Minute)},
		{ID: model.NewID(), StartedAt: incidentStart, ResolvedAt: at(40 * time.Minute)},
		// Still open: contributes nothing rather than zero.
		{ID: model.NewID(), StartedAt: incidentStart},
	}, nil, nil)

	summary := Summarise(sections)
	if summary.Incidents != 3 {
		t.Errorf("incidents = %d, want 3", summary.Incidents)
	}
	if summary.ResolveKnownCount != 2 {
		t.Errorf("resolve known count = %d, want 2", summary.ResolveKnownCount)
	}
	// (1200 + 2400) / 2 = 1800. Counting the open one as zero would give 1200.
	if summary.MeanTimeToResolve == nil || *summary.MeanTimeToResolve != 1800 {
		t.Errorf("mean time to resolve = %v, want 1800 — the open incident must not "+
			"be averaged in as a zero", summary.MeanTimeToResolve)
	}
	// Nothing supplied a detection time, so there is no mean rather than a mean
	// of zero.
	if summary.MeanTimeToDetect != nil {
		t.Errorf("mean time to detect = %d with nothing to average", *summary.MeanTimeToDetect)
	}
	if summary.DetectKnownCount != 0 {
		t.Errorf("detect known count = %d, want 0", summary.DetectKnownCount)
	}
}

// alerts_fired distinguishes "nobody was told" from "we no longer know".
//
// Zero means the delivery log covers this incident and holds nothing for it,
// which in a post-mortem is one of the more serious findings it can carry. Null
// means the rows have been swept — a statement about retention and nothing else.
// A retention policy must not be able to manufacture the first finding.
func TestAlertsFiredTellsSilenceFromForgetting(t *testing.T) {
	t.Parallel()

	recent := model.Incident{ID: model.NewID(), StartedAt: incidentStart}
	old := model.Incident{ID: model.NewID(), StartedAt: incidentStart.AddDate(0, -6, 0)}

	retainedFrom := incidentStart.AddDate(0, 0, -90)
	sections := PostMortem([]model.Incident{recent, old},
		map[model.ID]int{recent.ID: 0}, &retainedFrom)

	if sections[0].AlertsFired == nil || *sections[0].AlertsFired != 0 {
		t.Errorf("alerts_fired = %v for an incident inside the retention window; "+
			"zero is the finding that nobody was told", sections[0].AlertsFired)
	}
	if sections[1].AlertsFired != nil {
		t.Errorf("alerts_fired = %d for an incident older than the delivery log's "+
			"retention; the rows are gone, which is not the same as no alerts",
			*sections[1].AlertsFired)
	}
}
