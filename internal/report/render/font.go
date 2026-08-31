package render

// Fonts for the PDF backend (ADR-007 item 4).
//
// The fourteen standard PDF base fonts are rejected by that item and the reason
// is a product one rather than a technical one: they cost no bytes, but they
// lock encoding to WinAnsi and produce a document that looks generic — which
// defeats a white-label feature whose entire purpose is that the client believes
// their agency made it. So a face is a real TrueType file, embedded whole.
//
// # Why this is an interface rather than a package-level face
//
// The family is a **vendored binary asset and a visual identity commitment**,
// and ADR-007 says so in as many words: changing it reflows every future report.
// That choice is the maintainer's, not this file's, so the code takes a Family
// and does not name one. Wiring a specific face is a one-line decision at the
// call site rather than a change in the writer.
//
// Metrics are in 1/1000 em throughout, which is PDF's glyph space and what the
// /Widths and /FontDescriptor entries want. Converting once at parse time keeps
// every consumer — measurement, layout, the content stream — in one unit.
type Font interface {
	// PostScriptName is the /BaseFont value.
	PostScriptName() string

	// GlyphID maps a rune to a glyph index, or 0 (.notdef) where the face has
	// no glyph for it. A missing glyph is drawn rather than dropped: a visible
	// box tells a reader something is wrong with the font, whereas silently
	// omitting the character tells them something is wrong with their data.
	GlyphID(r rune) uint16

	// Advance is the advance width of a rune in 1/1000 em.
	Advance(r rune) float64

	// AdvanceGID is the same, by glyph index, for building the /W array.
	AdvanceGID(gid uint16) float64

	NumGlyphs() int

	// The vertical metrics a /FontDescriptor requires, in 1/1000 em.
	Ascent() float64
	Descent() float64
	CapHeight() float64
	ItalicAngle() float64
	StemV() float64

	// BBox is the font bounding box: xMin, yMin, xMax, yMax.
	BBox() [4]float64

	// Bold reports whether this face is the bold one, which the descriptor flags
	// and the PDF's synthetic-bold decision both need.
	Bold() bool

	// Raw is the whole font file, for the /FontFile2 stream. Whole rather than
	// subset, which is ADR-007's stated first cut: a subsetter is a correctness
	// risk measured in silently missing glyphs, and the saving is a few hundred
	// kilobytes on an artifact nobody stores a million of.
	Raw() []byte
}

// Family is the regular and bold pair. Two weights, because the primitive set
// has two (see Weight in draw.go) and a third would be a third file in the
// binary.
type Family struct {
	Regular Font
	Bold    Font
}

// Face picks the member for a weight.
func (f Family) Face(w Weight) Font {
	if w == Bold && f.Bold != nil {
		return f.Bold
	}
	return f.Regular
}

// Measure is the width of a string at a size, in points.
//
// The one measurement function in the product, used by the PDF backend for text
// anchoring and by the layout pass for wrapping and column widths. Advances are
// summed per rune with no kerning: the face's `kern` table is not read, because
// kerning changes measurement and drawing together and getting only one of them
// right is worse than getting neither.
func Measure(f Font, s string, sizePt float64) float64 {
	if f == nil {
		return 0
	}
	var total float64
	for _, r := range s {
		total += f.Advance(r)
	}
	return total * sizePt / 1000
}
