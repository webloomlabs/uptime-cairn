package render

import (
	"strings"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// Content selection: what makes `custom` a builder rather than a synonym for
// "everything".
//
// The property under test throughout is that a template gets the blocks it named
// and no others — and, in the first test below, that a template naming nothing
// gets exactly what it got before any of this existed.

// kinds reduces the element list to the shape a reader would describe, which is
// what these assertions are actually about. Comparing whole elements would make
// every test fail on a caption edit.
func kinds(elements []Element) []string {
	var out []string
	for _, e := range elements {
		switch v := e.(type) {
		case Cover:
			out = append(out, "cover")
		case Heading:
			out = append(out, "heading:"+v.Text)
		case Chart:
			out = append(out, "chart:"+chartName(v.Kind))
		case KeyValues:
			out = append(out, "keyvalues")
		case Table:
			out = append(out, "table")
		case Paragraph:
			out = append(out, "paragraph")
		case Footer:
			out = append(out, "footer")
		}
	}
	return out
}

func chartName(k ChartKind) string {
	if k == ChartUptimeStrip {
		return "uptime"
	}
	return "latency"
}

func has(elements []Element, kind string) bool {
	for _, k := range kinds(elements) {
		if k == kind {
			return true
		}
	}
	return false
}

// **The compatibility assertion, and the most important one here.**
//
// Every template on every install today names no sections. All of them have to
// render exactly what they rendered before selection existed, and the way to be
// sure is to compare the two compositions rather than to reason about the
// defaults being right.
func TestAnEmptySelectionComposesExactlyTheDefaults(t *testing.T) {
	t.Parallel()

	doc := sample()
	brand := brandFixture()

	before := kinds(Compose(doc, brand))
	for _, selection := range [][]string{nil, {}, {"not_a_section"}} {
		after := kinds(ComposeSections(doc, brand, selection))
		if strings.Join(before, "|") != strings.Join(after, "|") {
			t.Errorf("selection %v changed the default document:\n before: %v\n  after: %v",
				selection, before, after)
		}
	}
}

// A narrow selection produces a narrow document — the whole point.
func TestASelectionEmitsOnlyWhatItNames(t *testing.T) {
	t.Parallel()

	elements := ComposeSections(sample(), brandFixture(), []string{model.SectionUptimeTable})

	// The monitor is still introduced, because there is something under it.
	if !has(elements, "heading:checkout") {
		t.Error("the monitor heading is missing from a document that has its uptime table")
	}
	if !has(elements, "keyvalues") {
		t.Error("the uptime table is missing")
	}
	// And everything not named is gone.
	for _, unwanted := range []string{
		"heading:Summary", "heading:Service level", "heading:Response time",
		"chart:uptime", "chart:latency",
	} {
		if has(elements, unwanted) {
			t.Errorf("%q was emitted by a template that named only uptime_table", unwanted)
		}
	}

	// The three that are not sections survive regardless, which is deliberate:
	// the methodology paragraph carries the denominator and the maintenance
	// policy, and a figure whose policy can be switched off cannot be checked.
	for _, required := range []string{"cover", "paragraph", "footer"} {
		if !has(elements, required) {
			t.Errorf("%q is not a section and must not be removable", required)
		}
	}
}

// **Order is honoured**, because the spec says "ordered content blocks" and
// treating the list as a set would quietly narrow the contract.
func TestSelectionOrderIsHonoured(t *testing.T) {
	t.Parallel()

	doc := sample()
	brand := brandFixture()

	// Response time before the uptime table, inside each monitor.
	reversed := kinds(ComposeSections(doc, brand,
		[]string{model.SectionResponseTime, model.SectionUptimeTable}))
	responseAt, tableAt := -1, -1
	for i, k := range reversed {
		if k == "heading:Response time" && responseAt < 0 {
			responseAt = i
		}
		// The uptime table is the KeyValues that follows the response-time block
		// here, so the assertion is positional rather than by name.
		if k == "keyvalues" && responseAt >= 0 && i > responseAt {
			tableAt = i
			break
		}
	}
	if responseAt < 0 || tableAt < 0 {
		t.Fatalf("both blocks should be present: %v", reversed)
	}

	// And the document-level order: incidents named before the summary come out
	// before it.
	withIncidents := sample()
	withIncidents.Incidents = []report.Incident{{Title: "Checkout degraded", State: "resolved"}}
	got := kinds(ComposeSections(withIncidents, brand,
		[]string{model.SectionIncidentLog, model.SectionSummary}))

	summaryAt, incidentAt := -1, -1
	for i, k := range got {
		if k == "heading:Summary" {
			summaryAt = i
		}
		if strings.HasPrefix(k, "heading:") && strings.Contains(strings.ToLower(k), "incident") && incidentAt < 0 {
			incidentAt = i
		}
	}
	if incidentAt < 0 || summaryAt < 0 {
		t.Fatalf("both blocks should be present: %v", got)
	}
	if incidentAt > summaryAt {
		t.Errorf("incident_log was named first and came out after the summary: %v", got)
	}
}

// The per-monitor group is emitted **once**, at the position of the first
// per-monitor section named.
//
// The alternative — scattering a monitor's blocks across the document — would
// repeat its heading for each one and turn a three-monitor report into fifteen
// headings.
func TestThePerMonitorGroupIsEmittedOnce(t *testing.T) {
	t.Parallel()

	doc := sample()
	doc.Monitors = append(doc.Monitors, doc.Monitors[0])
	doc.Monitors[1].Name = "api"

	got := kinds(ComposeSections(doc, brandFixture(), []string{
		model.SectionUptimeTable, model.SectionSummary, model.SectionResponseTime,
	}))

	var checkout, api int
	for _, k := range got {
		switch k {
		case "heading:checkout":
			checkout++
		case "heading:api":
			api++
		}
	}
	if checkout != 1 || api != 1 {
		t.Errorf("monitor headings appeared %d and %d times, want once each: %v", checkout, api, got)
	}
}

// A monitor heading is not emitted with nothing under it. A selection of
// document-level blocks alone should not produce a page of monitor names, which
// reads as a rendering fault rather than as a choice somebody made.
func TestAMonitorWithNoSelectedBlocksIsNotIntroduced(t *testing.T) {
	t.Parallel()

	elements := ComposeSections(sample(), brandFixture(), []string{model.SectionSummary})
	if has(elements, "heading:checkout") {
		t.Errorf("the monitor was introduced with none of its blocks selected: %v", kinds(elements))
	}
	if !has(elements, "heading:Summary") {
		t.Error("the summary was not emitted")
	}
}

// sla_breakdown and error_budget are two names for one block, and naming both
// must not draw it twice.
//
// The spec separates them; ADR-006 does not. The error budget is computed from
// the same up and down counts as the target-versus-actual line and is rendered
// beside it, so splitting the block would put a budget on one page and the
// target it is a budget against on another.
func TestNamingBothServiceLevelSectionsDrawsTheBlockOnce(t *testing.T) {
	t.Parallel()

	got := kinds(ComposeSections(sample(), brandFixture(),
		[]string{model.SectionSLABreakdown, model.SectionErrorBudget}))

	var n int
	for _, k := range got {
		if k == "heading:Service level" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the service level block was drawn %d times: %v", n, got)
	}
}

// The two sections the frozen enum names and nothing composes yet contribute no
// block, and — this is the part worth asserting — do not fall back to the
// defaults by looking like an empty selection.
func TestUncomposedSectionsProduceNoBlockRatherThanTheDefaults(t *testing.T) {
	t.Parallel()

	elements := ComposeSections(sample(), brandFixture(),
		[]string{model.SectionMaintenanceLog, model.SectionCertificateExpiry})
	got := kinds(elements)

	for _, unwanted := range []string{"heading:Summary", "heading:checkout"} {
		if has(elements, unwanted) {
			t.Errorf("selecting only uncomposed sections fell back to the defaults: %v", got)
		}
	}
	// The cover, the brand's cover text, the methodology note and the footer —
	// the four that are not sections and cannot be deselected.
	if want := "cover|paragraph|paragraph|footer"; strings.Join(got, "|") != want {
		t.Errorf("got %v, want only the four non-section elements (%s)", got, want)
	}
}
