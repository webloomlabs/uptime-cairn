package render

import (
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// The response-time SLI says what it can prove and no more.
//
// Phase 1 marks a check down when it breaches its configured response-time
// threshold, and `Class` is not persisted — so a `down` in stored history is
// "did not meet the requirement", with no way back to which requirement. A
// report cannot therefore say a service "was slow" on a given day: the row that
// would support the claim was collapsed before it was stored, and the sentence
// would be an inference presented as an observation.
//
// The wording this constrains is the wording that costs a dispute. "The site was
// slow on the 14th" is a claim about experience; "the 14th was over the 250 ms
// target" is a claim about a rule somebody agreed to, and only the second is
// something this product watched happen.
//
// Two assertions, and the second is the one that would otherwise rot: the note
// has to *appear* where a target exists, or the first assertion passes trivially
// on a document that says nothing about response time at all.
func TestTheResponseTimeSLIIsAboutATargetAndNeverAboutSlowness(t *testing.T) {
	t.Parallel()

	doc := documentWithResponseTarget(250)
	text := composedText(t, doc)

	for _, banned := range []string{"slow", "sluggish", "laggy", "poor performance"} {
		if strings.Contains(strings.ToLower(text), banned) {
			t.Errorf("the composed report contains %q. A threshold breach is stored as "+
				"`down` with no class beside it, so the document cannot distinguish "+
				"'too slow' from 'did not answer' and must not imply that it can", banned)
		}
	}

	if !strings.Contains(text, "over target") {
		t.Error("no 'over target' figure on a document that has a response-time target")
	}
	if !strings.Contains(text, "250 ms") {
		t.Error("the methodology note does not name the target the days were counted against")
	}
	if !strings.Contains(text, "is recorded as down and is not separable") {
		t.Error("the note does not admit that a threshold breach and a failure to answer " +
			"are the same row in stored history — which is the fact that makes the " +
			"careful wording necessary rather than merely tasteful")
	}
}

// And a document with no target says nothing about one. A sentence explaining a
// rule nobody set is noise on a page kept short on purpose, and it would also be
// the only place in the product asserting a default threshold exists.
func TestNoResponseTargetMeansNoSentenceAboutOne(t *testing.T) {
	t.Parallel()

	doc := documentWithResponseTarget(0)
	text := composedText(t, doc)

	if strings.Contains(text, "over target") || strings.Contains(text, "recorded as down and is not separable") {
		t.Error("the methodology note describes a response-time target on a report that has none")
	}
}

// composedText flattens the composed elements to the words a reader sees, which
// is the level this rule is about — not the HTML, and not the PDF, because the
// wording is decided once in Compose and both backends draw what it says.
func composedText(t *testing.T, doc report.Document) string {
	t.Helper()

	var b strings.Builder
	for _, el := range Compose(doc, Brand{}) {
		switch e := el.(type) {
		case Cover:
			b.WriteString(e.Title + " " + e.Period + " ")
		case Heading:
			b.WriteString(e.Text + " ")
		case Paragraph:
			b.WriteString(e.Text + " ")
		case KeyValues:
			for _, kv := range e.Items {
				b.WriteString(kv.Key + " " + kv.Value + " " + kv.Note + " ")
			}
		case Chart:
			b.WriteString(e.Title + " " + e.Caption + " ")
		case Table:
			for _, col := range e.Columns {
				b.WriteString(col.Title + " ")
			}
			for _, row := range e.Rows {
				b.WriteString(strings.Join(row, " ") + " ")
			}
		case Footer:
			b.WriteString(e.Text + " ")
		}
	}
	return b.String()
}

// documentWithResponseTarget builds the smallest document that exercises the
// rule. A target of zero means none at all.
func documentWithResponseTarget(targetMs int) report.Document {
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	ratio := 0.999
	average := 180.0
	over := 2

	latency := report.Latency{AverageMs: &average, SampleCount: 8000}
	if targetMs > 0 {
		latency.TargetMs = &targetMs
		latency.DaysOverTarget = &over
	}

	return report.Document{
		Meta: report.Meta{
			SchemaVersion: report.SchemaVersion,
			TemplateName:  "Acme monthly",
			PeriodStart:   start,
			PeriodEnd:     start.AddDate(0, 1, 0),
			Timezone:      "UTC",
			GeneratedAt:   start.AddDate(0, 1, 0),
			Resolution:    report.Resolution{Tier: "1h", RequestedTier: "1h"},
		},
		Monitors: []report.MonitorSection{{
			MonitorID:    model.NewID(),
			Name:         "checkout",
			Uptime:       report.Uptime{Ratio: &ratio, ObservedChecks: 8000, UpChecks: 7992, DownChecks: 8, MaintenanceHandling: report.MaintenanceExclude},
			ResponseTime: latency,
		}},
	}
}
