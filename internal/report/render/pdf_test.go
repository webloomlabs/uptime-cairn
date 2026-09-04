package render

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// tjRun matches a text-showing operator in a content stream. Identity-H means
// the operand is glyph ids, two bytes each, hex encoded.
var tjRun = regexp.MustCompile(`<([0-9A-F]*)> Tj`)

// -pdf writes the sample report to a path, so it can be opened.
//
// This is how the layout in this package was actually checked: rendered, run
// through poppler, rasterised, and looked at. The test face draws a filled box
// per character rather than a letterform, so what it shows is placement, and
// placement is what a layout engine gets wrong.
var writePDF = flag.String("pdf", "", "write the sample report to this path")

func TestWriteSamplePDF(t *testing.T) {
	if *writePDF == "" {
		t.Skip("pass -pdf <path> to write the sample report")
	}
	out, err := PDFDocument(sample(), brandFixture(), testFamily())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*writePDF, out, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d bytes to %s", len(out), *writePDF)
}

// pageTexts decodes what each page draws, back through the face's cmap.
//
// This is the only way to assert on a PDF's words without a parser, and it
// checks something worth checking on the way: that the glyph ids in the stream
// map back to the characters that were composed. A document that draws the right
// shapes with the wrong ids looks correct and copies out as nonsense.
func pageTexts(p *PDF) []string {
	out := make([]string, len(p.pages))
	for i, page := range p.pages {
		var b strings.Builder
		for _, m := range tjRun.FindAllStringSubmatch(page.content.String(), -1) {
			for j := 0; j+4 <= len(m[1]); j += 4 {
				gid, err := strconv.ParseUint(m[1][j:j+4], 16, 16)
				if err != nil {
					continue
				}
				b.WriteRune(testRune(uint16(gid)))
			}
			b.WriteString("\n")
		}
		out[i] = b.String()
	}
	return out
}

func renderPDF(t *testing.T, doc report.Document, brand Brand) ([]byte, *PDF) {
	t.Helper()

	family := testFamily()
	out, err := PDFDocument(doc, brand, family)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	drawn, err := pdfFor(doc, brand, family, nil)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	return out, drawn
}

// The file is a PDF: the header a reader sniffs, a cross-reference table whose
// offsets actually point at their objects, and a trailer naming the catalogue.
// A malformed xref is the failure that opens fine in one reader and not in the
// one the client uses.
func TestPDFStructureIsWellFormed(t *testing.T) {
	t.Parallel()

	out, _ := renderPDF(t, sample(), brandFixture())

	if !bytes.HasPrefix(out, []byte("%PDF-1.7\n")) {
		t.Fatalf("no PDF header: %q", out[:min(16, len(out))])
	}
	if !bytes.HasSuffix(out, []byte("%%EOF\n")) {
		t.Error("file does not end with the end-of-file marker")
	}
	for _, want := range []string{"/Type /Catalog", "/Type /Pages", "/Type /Page ", "trailer", "startxref"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("missing %q", want)
		}
	}

	// Every xref entry must land on its own object header. This is the check
	// that catches an offset computed before a stream was written.
	xref := bytes.LastIndex(out, []byte("\nxref\n"))
	if xref < 0 {
		t.Fatal("no xref table")
	}
	lines := strings.Split(string(out[xref+6:]), "\n")
	count, err := strconv.Atoi(strings.Fields(lines[0])[1])
	if err != nil {
		t.Fatalf("xref subsection header %q: %v", lines[0], err)
	}
	for i := 1; i < count; i++ {
		off, err := strconv.Atoi(strings.TrimLeft(strings.Fields(lines[i+1])[0], "0") + "")
		if err != nil {
			t.Fatalf("xref entry %d: %v", i, err)
		}
		want := fmt.Sprintf("%d 0 obj", i)
		if !bytes.HasPrefix(out[off:], []byte(want)) {
			t.Errorf("xref entry %d points at %q, want %q", i, out[off:off+len(want)], want)
		}
	}
}

// ADR-007 item 6. Nothing in the writer reads a clock or a random source, so the
// same model rendered twice is the same file — which is what makes the plan's
// re-runnable generation a property rather than a hope.
func TestPDFIsByteIdentical(t *testing.T) {
	t.Parallel()

	first, _ := renderPDF(t, sample(), brandFixture())
	for i := range 3 {
		again, _ := renderPDF(t, sample(), brandFixture())
		if !bytes.Equal(first, again) {
			t.Fatalf("render %d differs from the first (%d vs %d bytes)", i, len(again), len(first))
		}
	}
}

// The /ID and the creation date come from the run, not from now. Two runs of the
// same definition are the same file; two different runs are distinguishable.
func TestIdentityComesFromTheRunNotTheClock(t *testing.T) {
	t.Parallel()

	doc := sample()
	first, _ := renderPDF(t, doc, brandFixture())

	other := sample()
	other.Meta.GeneratedAt = doc.Meta.GeneratedAt.Add(72 * time.Hour)
	second, _ := renderPDF(t, other, brandFixture())

	if bytes.Equal(first, second) {
		t.Error("two runs generated at different times produced identical files")
	}
	if !bytes.Contains(first, []byte("/CreationDate (D:"+doc.Meta.GeneratedAt.UTC().Format("20060102150405"))) {
		t.Error("the creation date is not the run's")
	}
	if bytes.Contains(first, []byte(time.Now().UTC().Format("20060102"))) &&
		!strings.HasPrefix(doc.Meta.GeneratedAt.UTC().Format("20060102"), time.Now().UTC().Format("20060102")) {
		t.Error("today's date appears in a document generated in the past")
	}
}

// The font is embedded whole as a CID-keyed Type0 face. ADR-007 item 4 rejects
// the base fourteen precisely because they lock encoding to WinAnsi; Identity-H
// is what lifts that limit, and /FontFile2 is what makes the document render on
// a machine that has never heard of the face.
func TestFontIsEmbeddedAndAddressedByGlyph(t *testing.T) {
	t.Parallel()

	out, _ := renderPDF(t, sample(), brandFixture())

	for _, want := range []string{
		"/Subtype /Type0", "/Encoding /Identity-H", "/Subtype /CIDFontType2",
		"/FontFile2", "/CIDToGIDMap /Identity", "/ToUnicode",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("missing %q", want)
		}
	}

	// The whole file, byte for byte, so a reader can rasterise it.
	raw := testFamily().Regular.Raw()
	if !bytes.Contains(out, raw) {
		t.Error("the face's bytes are not in the document")
	}
	if !bytes.Contains(out, []byte(fmt.Sprintf("/Length1 %d", len(raw)))) {
		t.Error("/Length1 does not state the font file's length")
	}
}

// Without a ToUnicode CMap, Identity-H glyph ids are opaque and a report copied
// out of a reader comes back as mojibake. An auditor pasting a figure into a
// spreadsheet is a real use of these artifacts, so this is not optional metadata.
func TestTextIsExtractable(t *testing.T) {
	t.Parallel()

	out, _ := renderPDF(t, sample(), brandFixture())

	cmap := out[bytes.Index(out, []byte("/Adobe-Identity-UCS")):]
	if !bytes.Contains(cmap, []byte("beginbfchar")) {
		t.Fatal("the ToUnicode CMap has no character mappings")
	}
	// 'A' must map back to U+0041 through its glyph id.
	want := fmt.Sprintf("<%04X> <0041>", testGID('A'))
	if !bytes.Contains(out, []byte(want)) {
		t.Errorf("no mapping %s in the ToUnicode CMap", want)
	}
}

// **The drift guard ADR-007 calls required rather than optional.** Two renderers
// over one model is two layouts that can drift, and the drift is invisible until
// a client sees a PDF that disagrees with the page they were shown. Every figure
// the composition produced has to appear in both documents.
func TestPDFAndHTMLShowTheSameFigures(t *testing.T) {
	t.Parallel()

	doc, brand := sample(), brandFixture()
	page, err := HTML(doc, brand)
	if err != nil {
		t.Fatal(err)
	}
	_, pdf := renderPDF(t, doc, brand)
	drawn := strings.Join(pageTexts(pdf), "\n")

	var checked int
	for _, el := range Compose(doc, brand) {
		kv, ok := el.(KeyValues)
		if !ok {
			continue
		}
		for _, item := range kv.Items {
			checked++
			if !strings.Contains(string(page), esc(item.Value)) {
				t.Errorf("HTML is missing the figure %q (%s)", item.Value, item.Key)
			}
			if !strings.Contains(drawn, item.Value) {
				t.Errorf("PDF is missing the figure %q (%s)", item.Value, item.Key)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no figures were compared; the guard is not guarding anything")
	}
}

// §4.3's two obligations reach the PDF's face as well as the HTML's. A number
// that is explained on one artifact and bare on the other is a number the client
// reads without its denominator on whichever they were sent.
func TestDenominatorAndPolicyAreOnThePDFFace(t *testing.T) {
	t.Parallel()

	_, pdf := renderPDF(t, sample(), brandFixture())
	drawn := strings.Join(pageTexts(pdf), " ")
	drawn = strings.ReplaceAll(drawn, "\n", " ")

	for _, want := range []string{"share of observed checks", "not an outage", "Declared maintenance is excluded"} {
		if !strings.Contains(drawn, want) {
			t.Errorf("the PDF face is missing %q", want)
		}
	}
}

// Every character the report draws has a glyph in the face. A missing glyph is
// drawn as .notdef — a visible box — and this is the test that turns that from
// something a client notices into something the suite does.
func TestEveryComposedCharacterHasAGlyph(t *testing.T) {
	t.Parallel()

	_, pdf := renderPDF(t, sample(), brandFixture())
	for i, page := range pdf.pages {
		for _, m := range tjRun.FindAllStringSubmatch(page.content.String(), -1) {
			for j := 0; j+4 <= len(m[1]); j += 4 {
				if m[1][j:j+4] == "0000" {
					t.Fatalf("page %d draws .notdef: some composed character has no glyph", i+1)
				}
			}
		}
	}
}

// The cover is its own page. It is what an agency's client sees first and the
// whole reason the white-label feature exists.
func TestCoverIsItsOwnPage(t *testing.T) {
	t.Parallel()

	_, pdf := renderPDF(t, sample(), brandFixture())
	texts := pageTexts(pdf)

	if len(texts) < 2 {
		t.Fatalf("document has %d page(s); the cover has nothing after it", len(texts))
	}
	if !strings.Contains(texts[0], "Prepared for") {
		t.Error("the cover page does not name who the report is for")
	}
	if strings.Contains(texts[0], "Error budget") {
		t.Error("a figure block is sharing the cover page")
	}
}

// Every page is numbered, and the numbering knows the total — which is why it is
// written in a second pass. A page with no number can be removed from the middle
// of a printed report without anybody noticing.
func TestEveryPageIsNumbered(t *testing.T) {
	t.Parallel()

	_, pdf := renderPDF(t, sample(), brandFixture())
	texts := pageTexts(pdf)

	for i, text := range texts {
		want := fmt.Sprintf("Page %d of %d", i+1, len(texts))
		if !strings.Contains(text, want) {
			t.Errorf("page %d is not numbered %q", i+1, want)
		}
	}
}
