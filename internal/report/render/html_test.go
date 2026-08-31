package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// -update rewrites the golden file. Run it, then **read the diff** — the whole
// value of a golden test is that somebody looks at what changed, and a flag that
// gets run reflexively turns it into a rubber stamp.
var update = flag.Bool("update", false, "rewrite the golden report")

func brandFixture() Brand {
	return Brand{
		CompanyName: `Smith & Co <Ltd>`,
		FooterText:  "Confidential — prepared under MSA 2024/17.",
		CoverText:   "Monthly availability summary for the services covered by your agreement.",
	}
}

// The golden report. ADR-007 requires this to stand up **alongside the first
// renderer, not after the second**: two renderers over one model is two layouts
// that can drift, and the drift is invisible until a client sees a PDF that
// disagrees with the page they were shown. Having it now means the PDF backend
// arrives with something to be measured against on its first day.
func TestGoldenReport(t *testing.T) {
	got, err := HTML(sample(), brandFixture())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	path := filepath.Join("testdata", "golden_report.html")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden rewritten; read the diff before committing it")
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("rendered report differs from the golden file.\n"+
			"Run: go test ./internal/report/render -run TestGoldenReport -update\n"+
			"then read the diff rather than accepting it.\ngot %d bytes, want %d",
			len(got), len(want))
	}
}

// The two obligations §4.3 puts on the face of the report, checked on the face
// rather than in the model. A number that is right in the JSON and unexplained
// in the document is still a number an auditor cannot check.
func TestDenominatorAndMaintenancePolicyAreOnTheFace(t *testing.T) {
	t.Parallel()

	out, err := HTML(sample(), brandFixture())
	if err != nil {
		t.Fatal(err)
	}
	page := string(out)

	for _, want := range []string{
		"share of observed checks",
		"not an outage",
		"Declared maintenance is excluded",
		"observed checks",
		"Not observed",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("report face is missing %q", want)
		}
	}
}

// The resolution is stated when it was forced, because §3.2's promise is that
// retention limits resolution rather than existence — and the reader has to be
// told which they are holding.
func TestDowngradedResolutionIsStatedOnTheFace(t *testing.T) {
	t.Parallel()

	out, err := HTML(sample(), brandFixture())
	if err != nil {
		t.Fatal(err)
	}
	page := string(out)

	if !strings.Contains(page, "daily resolution") {
		t.Error("the resolution actually used is not stated")
	}
	if !strings.Contains(page, "coarser than requested") {
		t.Error("a downgraded resolution is not disclosed")
	}
}

// A missing percentile always says why. An absent figure with no reason reads as
// a defect in the product rather than as a decision about honesty — which is the
// same rule the spec's enum now carries.
func TestAbsentPercentileExplainsItselfOnTheFace(t *testing.T) {
	t.Parallel()

	out, err := HTML(sample(), brandFixture())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "not computed for a report of this size") {
		t.Error("the p95 is missing from the face with no reason beside it")
	}
}

// User-supplied text reaches the page escaped. The brand name here is the one
// from the fixture, and a client actually called `Smith & Co <Ltd>` must get a
// report rather than a broken document.
func TestBrandTextIsEscaped(t *testing.T) {
	t.Parallel()

	out, err := HTML(sample(), brandFixture())
	if err != nil {
		t.Fatal(err)
	}
	page := string(out)

	if strings.Contains(page, "<Ltd>") {
		t.Error("unescaped user text reached the document")
	}
	if !strings.Contains(page, "Smith &amp; Co &lt;Ltd&gt;") {
		t.Error("brand name not present escaped")
	}
}

// Self-contained: no stylesheet link, no script, no remote image. An artifact is
// a record, and a record that needs a network to render is not one.
func TestReportIsSelfContained(t *testing.T) {
	t.Parallel()

	out, err := HTML(sample(), brandFixture())
	if err != nil {
		t.Fatal(err)
	}
	page := string(out)

	// The SVG namespace declaration is a URI that identifies rather than one
	// that loads — no agent ever fetches it, and SVG is invalid without it. It
	// is the one exception, and it is removed before the scan rather than
	// allowed for by a looser pattern that would also let a real remote asset
	// through.
	scanned := strings.ReplaceAll(page, `xmlns="http://www.w3.org/2000/svg"`, "")

	for _, forbidden := range []string{"<script", "<link", "http://", "https://", "url("} {
		if strings.Contains(scanned, forbidden) {
			t.Errorf("report is not self-contained: contains %q", forbidden)
		}
	}
	if !strings.Contains(page, `name="robots" content="noindex`) {
		t.Error("no noindex; a shared report on a URL must not be indexable")
	}
}

// The charts are inline SVG drawn through the primitives, not an <img> pointing
// somewhere. Same reason as above, and the reason the PDF backend will be able
// to draw them at all.
func TestChartsAreInline(t *testing.T) {
	t.Parallel()

	out, err := HTML(sample(), brandFixture())
	if err != nil {
		t.Fatal(err)
	}
	page := string(out)

	if strings.Count(page, "<svg") < 2 {
		t.Error("expected an uptime strip and a latency chart inline")
	}
	if !strings.Contains(page, gapColor.Hex()) {
		t.Error("the unobserved day is not drawn as a gap on the page")
	}
}

// A period with nothing in it still produces a document. The report says so
// rather than failing, which is the same rule Build follows for an empty scope.
func TestEmptyDocumentStillRenders(t *testing.T) {
	t.Parallel()

	doc := report.Document{Meta: sample().Meta}
	out, err := HTML(doc, Brand{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "<h1>") {
		t.Error("an empty report produced no cover")
	}
}

// Nothing measured draws a chart that says so rather than an empty frame the
// reader has to interpret.
func TestChartWithNoMeasurementsSaysSo(t *testing.T) {
	t.Parallel()

	doc := sample()
	doc.Monitors[0].ResponseTime = report.ComputeLatency(
		store.HistoryBucket{},
		[]store.HistoryBucket{{Start: march, Unknown: 10}},
		nil,
	)

	out, err := HTML(doc, Brand{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "No measurements in this period") {
		t.Error("an empty chart is drawn as an empty box with no explanation")
	}
}

// Composition is deterministic, which is what makes the golden test meaningful
// and what ADR-007 item 6 asks of every renderer.
func TestHTMLIsDeterministic(t *testing.T) {
	t.Parallel()

	first, err := HTML(sample(), brandFixture())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := HTML(sample(), brandFixture())
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("render %d differs from the first", i)
		}
	}
}

// The element set is bounded (ADR-007 item 3). Compose may only emit the seven,
// and this fails if an eighth appears — which is the difference between a list
// that grows by decision and one that grows by accident.
func TestComposeEmitsOnlyTheSevenElements(t *testing.T) {
	t.Parallel()

	for _, el := range Compose(sample(), brandFixture()) {
		switch el.(type) {
		case Cover, Heading, Paragraph, KeyValues, Table, Chart, Footer:
		default:
			t.Errorf("composed an element outside the bounded set: %T", el)
		}
	}
}
