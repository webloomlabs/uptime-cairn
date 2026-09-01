package render

import (
	"strings"
	"testing"
)

// The acceptance tests for the vendored face.
//
// They exist so that swapping the family is a deliberate act with a checklist
// rather than a file replacement whose consequences show up on a client's PDF.
// Each one encodes a property the choice was made on.

func embeddedOrSkip(t *testing.T) Family {
	t.Helper()
	family, err := Embedded()
	if err != nil {
		t.Fatalf("the embedded family does not parse: %v", err)
	}
	return family
}

// It parses, both weights are present, and they are distinguishable. Two faces
// sharing one PostScript name is a document where the bold text is not bold.
func TestTheEmbeddedFamilyParses(t *testing.T) {
	t.Parallel()

	family := embeddedOrSkip(t)
	if family.Regular == nil || family.Bold == nil {
		t.Fatal("the family is missing a weight")
	}
	if family.Regular.Bold() {
		t.Error("the regular face reports itself bold")
	}
	if !family.Bold.Bold() {
		t.Error("the bold face does not report itself bold")
	}
	if family.Regular.PostScriptName() == family.Bold.PostScriptName() {
		t.Errorf("both faces are named %q; the bold text would not be bold",
			family.Regular.PostScriptName())
	}
	if !strings.Contains(family.Regular.PostScriptName(), "Roboto") {
		t.Errorf("PostScript name = %q, want the vendored family",
			family.Regular.PostScriptName())
	}
}

// **Tabular figures, checked rather than assumed.**
//
// The PDF backend applies no OpenType features, so `tnum` is unreachable and
// what matters is the face's *default* advance for each digit. A face with
// proportional figures would give every table in every report a drifting decimal
// point — right-aligned columns would still end flush and the numbers inside
// them would not line up. The HTML report asks for tabular numerals in CSS; this
// is what makes the PDF agree with it.
func TestTheEmbeddedFiguresAreTabular(t *testing.T) {
	t.Parallel()

	for name, face := range map[string]Font{"regular": embeddedOrSkip(t).Regular, "bold": embeddedOrSkip(t).Bold} {
		widths := map[float64]bool{}
		for r := '0'; r <= '9'; r++ {
			widths[face.Advance(r)] = true
		}
		if len(widths) != 1 {
			t.Errorf("%s face has %d distinct digit widths, want 1 — a column of figures will not line up",
				name, len(widths))
		}
	}
}

// Every character the report can draw has a glyph. A missing one renders as
// .notdef — a visible box — and this is what turns that from something a client
// notices into something the suite does.
//
// The set is the punctuation Compose actually reaches for, not a general
// alphabet: the `×` of the burn rate, the en dash of a period range, the em dash
// of a footer, the ellipsis of a truncated table cell.
func TestTheEmbeddedFamilyCoversWhatTheReportDraws(t *testing.T) {
	t.Parallel()

	const drawn = "0123456789" +
		"abcdefghijklmnopqrstuvwxyz" +
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		" .,:;'\"()[]{}%-+/&<>#@_=*|\\~^`$?!" +
		"–—…×°"

	family := embeddedOrSkip(t)
	for name, face := range map[string]Font{"regular": family.Regular, "bold": family.Bold} {
		var missing []rune
		for _, r := range drawn {
			if face.GlyphID(r) == 0 {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s face has no glyph for %q", name, string(missing))
		}
	}
}

// The descriptor entries a PDF /FontDescriptor requires are all present and
// sane. A zero ascent or cap height produces a document that opens and lays out
// its text on top of itself.
func TestTheEmbeddedMetricsAreUsable(t *testing.T) {
	t.Parallel()

	family := embeddedOrSkip(t)
	for name, face := range map[string]Font{"regular": family.Regular, "bold": family.Bold} {
		if face.Ascent() <= 0 || face.Descent() >= 0 {
			t.Errorf("%s: ascent %v, descent %v — want a positive ascent and a negative descent",
				name, face.Ascent(), face.Descent())
		}
		if face.CapHeight() <= 0 {
			t.Errorf("%s: cap height %v", name, face.CapHeight())
		}
		bbox := face.BBox()
		if bbox[2] <= bbox[0] || bbox[3] <= bbox[1] {
			t.Errorf("%s: degenerate bounding box %v", name, bbox)
		}
		if got := Measure(face, "99.884%", 9.5); got <= 0 || got > 200 {
			t.Errorf("%s: %q measures %v points, which is not a plausible width", name, "99.884%", got)
		}
	}
}

// The bold face is heavier than the regular one. Vendoring the same file twice
// by mistake is the obvious way to get a report with no emphasis anywhere, and
// it is invisible until somebody looks at a rendered page.
func TestTheBoldFaceIsActuallyHeavier(t *testing.T) {
	t.Parallel()

	family := embeddedOrSkip(t)
	regular := Measure(family.Regular, "Uptime report", 12)
	bold := Measure(family.Bold, "Uptime report", 12)

	if bold <= regular {
		t.Errorf("bold measures %v against the regular face's %v; the two files may be the same", bold, regular)
	}
	if family.Bold.StemV() <= family.Regular.StemV() {
		t.Error("the bold face declares no heavier stem")
	}
}

// A PDF built with the embedded family renders every composed character rather
// than falling back to .notdef, which is the end-to-end version of the coverage
// test above and the one that would catch a face that covers the alphabet but
// not the punctuation Compose emits.
func TestTheEmbeddedFamilyDrawsTheWholeSampleReport(t *testing.T) {
	t.Parallel()

	family := embeddedOrSkip(t)
	drawn, err := pdfFor(sample(), brandFixture(), family)
	if err != nil {
		t.Fatal(err)
	}

	for i, page := range drawn.pages {
		for _, m := range tjRun.FindAllStringSubmatch(page.content.String(), -1) {
			for j := 0; j+4 <= len(m[1]); j += 4 {
				if m[1][j:j+4] == "0000" {
					t.Fatalf("page %d draws .notdef: the embedded face is missing a character the report composes", i+1)
				}
			}
		}
	}
}
