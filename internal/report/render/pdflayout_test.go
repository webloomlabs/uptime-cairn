package render

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// layoutOf flows an element list and returns the drawn document, so the tests
// below can assert on pages rather than on bytes.
//
// The running footers are deliberately not written: they are the one thing that
// belongs in the bottom margin, and the margin invariant below would have to
// make an exception for them that could hide a real overflow.
func layoutOf(elements []Element) *PDF {
	return flow(elements, testFamily()).pdf
}

func longTable(rows int) Table {
	t := Table{Columns: []Column{{Title: "Started"}, {Title: "Ended"}, {Title: "Downtime", Numeric: true}}}
	for i := range rows {
		t.Rows = append(t.Rows, []string{fmt.Sprintf("row-%03d", i), "2 Mar 2026", "2h 24m"})
	}
	return t
}

// A table longer than a page breaks, and the header repeats on every page it
// reaches. A continuation page of unlabelled columns is a page a reader has to
// flip back from, which is the first of ADR-007's four named table hazards.
func TestLongTableBreaksAndRepeatsItsHeader(t *testing.T) {
	t.Parallel()

	pdf := layoutOf([]Element{longTable(120)})
	texts := pageTexts(pdf)

	if len(texts) < 2 {
		t.Fatalf("120 rows fitted on %d page(s); the table did not break", len(texts))
	}
	for i, text := range texts {
		if !strings.Contains(text, "STARTED") {
			t.Errorf("page %d carries table rows with no header above them", i+1)
		}
	}

	// Nothing is lost at a break, which is the thing a page-breaking bug does
	// most quietly.
	all := strings.Join(texts, "\n")
	for i := range 120 {
		if !strings.Contains(all, fmt.Sprintf("row-%03d", i)) {
			t.Fatalf("row-%03d vanished at a page break", i)
		}
	}
}

// Widow control: a continuation page consisting of a repeated header and a
// single row reads as a printing fault rather than as a table. When the break
// would leave one row alone, a row is pulled back.
func TestATableNeverLeavesASingleRowOnTheLastPage(t *testing.T) {
	t.Parallel()

	// Sweep the sizes around a page boundary rather than guessing one: the row
	// count that produces a lone widow depends on the type scale, and a test
	// pinned to one number stops testing the rule the moment a size changes.
	for rows := 30; rows <= 90; rows++ {
		texts := pageTexts(layoutOf([]Element{longTable(rows)}))
		if len(texts) < 2 {
			continue
		}
		last := texts[len(texts)-1]
		count := strings.Count(last, "row-")
		if count == 1 {
			t.Errorf("%d rows left a single row alone on the last page", rows)
		}
	}
}

// A cell taller than a page is the third hazard and the one with no good answer:
// the row has to either overflow the margin or lose text. It loses text, marked,
// so that a reader can see something was cut rather than reading a shortened
// name as the whole of it.
func TestAnEnormousCellIsClampedRatherThanOverflowing(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("verylongmonitorname ", 400)
	table := Table{
		Columns: []Column{{Title: "Monitor"}, {Title: "Uptime", Numeric: true}},
		Rows:    [][]string{{huge, "99.9%"}, {"second row", "100%"}},
	}

	pdf := layoutOf([]Element{table})
	texts := pageTexts(pdf)
	all := strings.Join(texts, "\n")

	if !strings.Contains(all, "…") {
		t.Error("the clamped cell is not marked as truncated")
	}
	if !strings.Contains(all, "second row") {
		t.Error("the row after the enormous one was lost")
	}
	if len(texts) > 2 {
		t.Errorf("one oversized row produced %d pages; it was not clamped", len(texts))
	}
}

// Column widths come off the content. A numeric column narrowed to make room for
// prose is a column where "2 Mar 2026" wraps onto two lines, so the overflow is
// taken from the text column instead.
func TestOverflowIsTakenFromTheTextColumn(t *testing.T) {
	t.Parallel()

	family := testFamily()
	l := &pdfLayout{pdf: NewPDF(family), family: family}

	table := Table{
		Columns: []Column{{Title: "Monitor"}, {Title: "Downtime", Numeric: true}},
		Rows:    [][]string{{strings.Repeat("wide ", 60), "2h 24m"}},
	}
	widths := l.columnWidths(table)

	natural := Measure(family.Regular, "2h 24m", sizeTable) + 16
	if widths[1] < natural {
		t.Errorf("the numeric column was squeezed to %v, below the %v its content needs", widths[1], natural)
	}
	if total := widths[0] + widths[1]; total > contentW+0.01 {
		t.Errorf("columns total %v, wider than the %v of page available", total, contentW)
	}
}

// Wrapping is greedy and space-separated, and a word wider than the measure is
// broken rather than left to run off the page. That word is a URL or a monitor
// named without spaces, and both are ordinary.
func TestWrapBreaksLinesAndOverlongWords(t *testing.T) {
	t.Parallel()

	f := testFamily().Regular

	// Six points a character at 10pt: ten characters to a 60-point line.
	lines := wrap(f, "aaaa bbbb cccc dddd", 10, 60)
	if len(lines) != 2 || lines[0] != "aaaa bbbb" {
		t.Errorf("wrap = %q, want two lines of two words", lines)
	}

	long := wrap(f, strings.Repeat("x", 40), 10, 60)
	if len(long) < 4 {
		t.Errorf("an unbreakable 40-character word produced %d lines, want at least 4", len(long))
	}
	for _, line := range long {
		if w := Measure(f, line, 10); w > 60.01 {
			t.Errorf("line %q measures %v, wider than the 60 asked for", line, w)
		}
	}

	if got := wrap(f, "", 10, 60); got != nil {
		t.Errorf("wrap(empty) = %q, want nil", got)
	}
}

// A heading is never the last thing on a page. A section title with its content
// overleaf is the most common bad break in a generated document and the cheapest
// to prevent.
//
// The cursor is driven directly across the bottom hundred points of the page,
// rather than through a fixture that happens to land near the boundary. An
// earlier version of this test flowed a table of varying length and passed with
// the rule deleted — the row height quantised the cursor so that the boundary
// was never reached, and a test that cannot reach the case it names is not
// testing anything.
func TestAHeadingIsNotOrphanedAtAPageFoot(t *testing.T) {
	t.Parallel()

	for offset := 1; offset <= 140; offset++ {
		family := testFamily()
		l := &pdfLayout{pdf: NewPDF(family), family: family}
		l.newPage()
		l.y = contentB - float64(offset)

		l.heading(Heading{Text: "Response time", Level: 1})
		l.paragraph(Paragraph{Text: "Body text follows the heading."})

		texts := pageTexts(l.pdf)
		for i, text := range texts {
			if strings.Contains(text, "Response time") && !strings.Contains(text, "Body text follows") {
				t.Fatalf("%.0f points from the foot: page %d ends on the heading", float64(offset), i+1)
			}
		}
	}
}

// Nothing is drawn in the bottom margin.
//
// This is the invariant every element's break decision exists to keep, and it is
// tested as one rather than element by element: a figure cell whose note falls
// past the margin, a chart drawn half off the page and a table row that ignored
// the page foot are the same defect, and a per-element assertion only catches
// the element somebody remembered to write one for.
//
// The running footer is the one thing deliberately in the margin, and it is
// written by a separate pass that this fixture does not run.
func TestNothingIsDrawnBelowTheBottomMargin(t *testing.T) {
	t.Parallel()

	figures := KeyValues{Items: []KeyValue{
		{Key: "Uptime", Value: "99.87%", Note: "over 8,905 observed checks in the period"},
		{Key: "Error budget", Value: "43m 12s", Note: "for the period"},
		{Key: "Budget used", Value: "2h 24m"},
	}}
	chart := Chart{Kind: ChartUptimeStrip, Title: "Daily uptime", Caption: "Grey days were not observed."}

	for filler := 0; filler <= 60; filler++ {
		elements := []Element{
			longTable(filler),
			figures,
			chart,
			Paragraph{Text: strings.Repeat("Methodology sentence. ", 12), Muted: true},
			Table{Columns: []Column{{Title: "Day"}, {Title: "Downtime", Numeric: true}},
				Rows: [][]string{{"1 Mar 2026", "2h 24m"}, {"2 Mar 2026", "0s"}}},
		}
		pdf := layoutOf(elements)

		for i, page := range pdf.pages {
			if low, ok := lowestBaseline(page); ok && low > contentB+0.01 {
				t.Fatalf("filler %d: page %d draws text at y=%.1f, past the %.1f bottom margin",
					filler, i+1, low, contentB)
			}
		}
	}
}

var tmOp = regexp.MustCompile(`1 0 0 1 (-?[0-9.]+) (-?[0-9.]+) Tm`)

// lowestBaseline is the furthest-down text baseline on a page, converted back
// into the top-down coordinates the layout thinks in.
func lowestBaseline(page *pdfPage) (float64, bool) {
	var lowest float64
	var found bool
	for _, m := range tmOp.FindAllStringSubmatch(page.content.String(), -1) {
		y, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		if top := page.height - y; !found || top > lowest {
			lowest, found = top, true
		}
	}
	return lowest, found
}

// A figure keeps its label and its qualifying note. A value on its own is the
// failure §4.3 spends its whole length preventing.
func TestAFigureCellIsNotSplitAcrossPages(t *testing.T) {
	t.Parallel()

	figures := KeyValues{Items: []KeyValue{
		{Key: "Uptime", Value: "99.87%", Note: "over 8,905 observed checks"},
	}}

	for offset := 1; offset <= 140; offset++ {
		family := testFamily()
		l := &pdfLayout{pdf: NewPDF(family), family: family}
		l.newPage()
		l.y = contentB - float64(offset)
		l.keyValues(figures)

		for i, text := range pageTexts(l.pdf) {
			if strings.Contains(text, "99.87%") && !strings.Contains(text, "Uptime") {
				t.Fatalf("%d points from the foot: page %d has the value without its label", offset, i+1)
			}
			if strings.Contains(text, "over 8,905 observed checks") && !strings.Contains(text, "99.87%") {
				t.Fatalf("%d points from the foot: page %d has the note without its figure", offset, i+1)
			}
		}
	}
}

// --- images ----------------------------------------------------------------

func pngBytes(t *testing.T, w, h int, alpha uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{R: 0x20, G: 0x60, B: 0xc0, A: alpha})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A logo with a transparent background must not composite to a white box. PDF
// composites through a separate soft mask, so the alpha channel travels as one.
func TestATransparentLogoKeepsItsAlpha(t *testing.T) {
	t.Parallel()

	pdf := NewPDF(testFamily())
	pdf.NewPage(pageW, pageH)
	pdf.Image(Rect{X: 10, Y: 10, W: 100, H: 40}, "image/png", pngBytes(t, 20, 10, 0x80))

	out, err := pdf.Bytes("run", sample().Meta.GeneratedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("/SMask")) {
		t.Error("a semi-transparent PNG was embedded with no soft mask")
	}
	if !bytes.Contains(out, []byte("/Subtype /Image")) {
		t.Error("no image XObject was written")
	}

	opaque := NewPDF(testFamily())
	opaque.NewPage(pageW, pageH)
	opaque.Image(Rect{X: 10, Y: 10, W: 100, H: 40}, "image/png", pngBytes(t, 20, 10, 0xff))
	out, err = opaque.Bytes("run", sample().Meta.GeneratedAt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("/SMask")) {
		t.Error("an opaque PNG carries a soft mask it does not need")
	}
}

// JPEG passes through as /DCTDecode. Re-encoding would cost quality and bytes to
// arrive at the same picture.
func TestJPEGIsEmbeddedWithoutReEncoding(t *testing.T) {
	t.Parallel()

	img := image.NewRGBA(image.Rect(0, 0, 24, 12))
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, img, nil); err != nil {
		t.Fatal(err)
	}

	pdf := NewPDF(testFamily())
	pdf.NewPage(pageW, pageH)
	pdf.Image(Rect{X: 0, Y: 0, W: 50, H: 25}, "image/jpeg", raw.Bytes())
	out, err := pdf.Bytes("run", sample().Meta.GeneratedAt)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(out, []byte("/DCTDecode")) {
		t.Error("JPEG was not embedded through /DCTDecode")
	}
	if !bytes.Contains(out, raw.Bytes()) {
		t.Error("the JPEG bytes were re-encoded rather than passed through")
	}
}

// A logo that cannot be decoded does not fail the report. ADR-007 item 7 is
// about formats, but the same judgement holds inside one: a client would rather
// have an unbranded report than none.
func TestAnUndecodableLogoDoesNotFailTheDocument(t *testing.T) {
	t.Parallel()

	pdf := NewPDF(testFamily())
	pdf.NewPage(pageW, pageH)
	pdf.Image(Rect{X: 0, Y: 0, W: 50, H: 25}, "image/png", []byte("not a png"))
	pdf.Text(50, 50, Run{Text: "Report"}, TextStyle{SizePt: 10, Fill: inkColor})

	out, err := pdf.Bytes("run", sample().Meta.GeneratedAt)
	if err != nil {
		t.Fatalf("an undecodable logo failed the whole document: %v", err)
	}
	if bytes.Contains(out, []byte("/Subtype /Image")) {
		t.Error("an image XObject was written for bytes that did not decode")
	}
}

// The same logo on five pages is one object. A cover mark repeated per page
// would multiply a megabyte by the page count for no visible difference.
func TestARepeatedImageIsStoredOnce(t *testing.T) {
	t.Parallel()

	logo := pngBytes(t, 40, 20, 0xff)
	pdf := NewPDF(testFamily())
	for range 5 {
		pdf.NewPage(pageW, pageH)
		pdf.Image(Rect{X: 0, Y: 0, W: 50, H: 25}, "image/png", logo)
	}
	out, err := pdf.Bytes("run", sample().Meta.GeneratedAt)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(out, []byte("/Subtype /Image")); n != 1 {
		t.Errorf("the same logo produced %d image objects, want 1", n)
	}
}

// A stroked polyline must not be closed by its painting operator. `f` on an open
// path silently closes it, and a latency line that joins its last point back to
// its first says something untrue about the data.
func TestAnOpenPathIsNotClosed(t *testing.T) {
	t.Parallel()

	pdf := NewPDF(testFamily())
	pdf.NewPage(200, 100)
	pdf.Path([]Point{{X: 0, Y: 0}, {X: 10, Y: 10}, {X: 20, Y: 5}}, false, Stroke(lineColor, 1))

	content := pdf.pages[0].content.String()
	if strings.Contains(content, " h ") {
		t.Error("an open path was closed")
	}
	if !strings.HasSuffix(strings.TrimSpace(content), " S") {
		t.Errorf("an open stroked path was painted with %q, want S", content)
	}
}
