package render

import (
	"encoding/binary"
	"strings"
	"testing"
)

// The metrics are read at the scale PDF wants — 1/1000 em — rather than in font
// units. Converting once at parse time is what keeps measurement, the /W array
// and the descriptor from each applying their own factor, which is the classic
// way a document comes out with text at the right size and the wrong widths.
func TestMetricsAreScaledToGlyphSpace(t *testing.T) {
	t.Parallel()

	f, err := ParseTrueType(buildTestFont(false))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got := f.Advance('A'); got != testAdvance {
		t.Errorf("advance = %v, want %d per 1000 em", got, testAdvance)
	}
	if got := f.Ascent(); got != 800 {
		t.Errorf("ascent = %v, want 800", got)
	}
	if got := f.CapHeight(); got != 700 {
		t.Errorf("cap height = %v, want 700 from OS/2 sCapHeight", got)
	}
	if got := f.BBox(); got != [4]float64{-100, -250, 1000, 900} {
		t.Errorf("bbox = %v", got)
	}
}

// Measurement is the arithmetic the layout pass depends on for wrapping, column
// widths and every anchored label. At 600/1000 em a character is 0.6 of the size.
func TestMeasureIsAdvanceTimesSize(t *testing.T) {
	t.Parallel()

	f, _ := ParseTrueType(buildTestFont(false))
	if got := Measure(f, "ABCDE", 10); got != 30 {
		t.Errorf("Measure(5 chars, 10pt) = %v, want 30", got)
	}
	if got := Measure(f, "", 10); got != 0 {
		t.Errorf("Measure(empty) = %v, want 0", got)
	}
}

// The cmap is what turns text into the glyph ids the content stream carries.
// Both segments resolve, and a rune the face does not cover maps to .notdef
// rather than to whatever happens to be at that index.
func TestCmapResolvesBothSegmentsAndRefusesToGuess(t *testing.T) {
	t.Parallel()

	f, _ := ParseTrueType(buildTestFont(false))

	for _, r := range []rune{' ', 'A', 'z', '~', '–', '—', '…'} {
		if got := f.GlyphID(r); got != testGID(r) {
			t.Errorf("GlyphID(%q) = %d, want %d", r, got, testGID(r))
		}
	}
	if got := f.GlyphID('あ'); got != 0 {
		t.Errorf("GlyphID(uncovered rune) = %d, want 0 (.notdef)", got)
	}
}

// hmtx's tail shares the last long metric's advance. A face that ends its long
// metrics early and a face that does not must measure identically.
func TestShortHmtxSharesTheLastAdvance(t *testing.T) {
	t.Parallel()

	f, err := ParseTrueType(buildTestFontMetrics(false, 10))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := f.AdvanceGID(uint16(testNumGlyphs - 1)); got != testAdvance {
		t.Errorf("tail advance = %v, want %d — the last long metric is shared", got, testAdvance)
	}
}

// The weight is detected, because the PDF descriptor's StemV and the layout's
// choice of face both depend on it and neither has another source.
func TestBoldIsDetected(t *testing.T) {
	t.Parallel()

	regular, _ := ParseTrueType(buildTestFont(false))
	bold, _ := ParseTrueType(buildTestFont(true))

	if regular.Bold() {
		t.Error("the regular face reports bold")
	}
	if !bold.Bold() {
		t.Error("the bold face does not report bold")
	}
	if bold.StemV() <= regular.StemV() {
		t.Error("the bold face has no heavier stem than the regular one")
	}
}

// The PostScript name reaches /BaseFont. It is an identifier inside the file
// rather than something a reader resolves, but two faces sharing one name is a
// document where the bold text is not bold.
func TestPostScriptNameIsRead(t *testing.T) {
	t.Parallel()

	regular, _ := ParseTrueType(buildTestFont(false))
	bold, _ := ParseTrueType(buildTestFont(true))

	if regular.PostScriptName() != "CairnTest-Regular" {
		t.Errorf("name = %q", regular.PostScriptName())
	}
	if bold.PostScriptName() == regular.PostScriptName() {
		t.Error("both faces carry the same PostScript name")
	}
}

// A font file is a file, and a broken one must fail the render rather than the
// process. Each of these is a real thing somebody hands a font loader.
func TestMalformedFontsAreRefusedByName(t *testing.T) {
	t.Parallel()

	otto := buildTestFont(false)
	binary.BigEndian.PutUint32(otto[0:], 0x4F54544F)

	collection := buildTestFont(false)
	binary.BigEndian.PutUint32(collection[0:], 0x74746366)

	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, "truncated"},
		{"noise", []byte("this is not a font at all, it is a readme"), "not a TrueType font"},
		{"cff outlines", otto, "TrueType outlines, not CFF"},
		{"collection", collection, "collection"},
		{"cut short", buildTestFont(false)[:200], ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := ParseTrueType(c.in)
			if err == nil {
				t.Fatalf("parsed %s without error (face %v)", c.name, f)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}
