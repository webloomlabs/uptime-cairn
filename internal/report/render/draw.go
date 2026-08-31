package render

import "fmt"

// The drawing primitive set (ADR-007 item 2).
//
// Five operations and no more: a text run, a rectangle, a line, a path, an
// image. Charts are written once against this interface and drawn by both
// backends — SVG for the HTML report and, when it lands, a PDF content stream —
// so the two cannot drift into two different-looking charts of the same data.
//
// **Every primitive added here is one both backends must implement forever**,
// which is the whole reason the set stays this small. A gradient, a dash array
// or a clip path is a decision, not a convenience: it doubles in cost the moment
// the PDF backend exists, and it is exactly the kind of thing that gets added
// for one chart and then has to be honoured for the life of the project.
type Backend interface {
	// Text draws a shaped run with its baseline starting at (x, y).
	//
	// A Run rather than a string, per ADR-007 item 5: non-Latin scripts are out
	// of scope for Phase 2 and the design does not foreclose them, so the
	// primitive already accepts the output of a shaping step even though today
	// nothing shapes. A shaper inserts later without changing a single caller —
	// which is the difference between deferring a feature and preventing one.
	Text(x, y float64, run Run, style TextStyle)

	Rect(r Rect, style ShapeStyle)
	Line(x1, y1, x2, y2 float64, style StrokeStyle)

	// Path draws a polyline. Points are absolute; an empty or single-point path
	// draws nothing rather than erroring, because a chart over a monitor with
	// one day of history is a real case and not a failure.
	Path(points []Point, closed bool, style ShapeStyle)

	// Image places raster bytes in a rectangle. PNG and JPEG only — the PDF
	// backend embeds rasters and has no SVG path translator, so an SVG logo is
	// refused at upload rather than dropped here (ADR-007).
	Image(r Rect, mime string, data []byte)
}

// Point is a position in the drawing space: points, origin top-left, y down.
//
// Top-left and y-down because that is how a page is laid out and how SVG
// works. PDF's native space is y-up, and converting once inside that backend is
// far cheaper than making every chart think in two coordinate systems.
type Point struct{ X, Y float64 }

// Rect is a rectangle. W and H are extents, not a second corner.
type Rect struct{ X, Y, W, H float64 }

// Color is opaque RGB. There is no alpha, deliberately: transparency in PDF
// needs an ExtGState resource per distinct value, and a report that needs a
// translucent fill can use a lighter colour instead.
type Color struct{ R, G, B uint8 }

// Hex renders the colour as #rrggbb.
func (c Color) Hex() string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

// Weight is the font weight. Two, because ADR-007 embeds one family at regular
// and bold; a third would be a third font file in the binary.
type Weight int

const (
	Regular Weight = iota
	Bold
)

// Anchor is horizontal alignment of a text run against its x coordinate.
type Anchor int

const (
	Start Anchor = iota
	Middle
	End
)

// Run is text that has been through shaping, or that has been declared not to
// need it.
//
// Today Text is the whole of it and the backend measures. When a shaper arrives,
// Advances carries per-cluster widths and the backend positions from them; a
// nil Advances means "measure this yourself", which is what every caller means
// today and what none of them will have to change to say.
type Run struct {
	Text     string
	Advances []float64
}

// TextStyle is everything the text primitive needs to know.
type TextStyle struct {
	SizePt float64
	Weight Weight
	Fill   Color
	Anchor Anchor
}

// StrokeStyle is a line's appearance. No dash array: see the note on Backend.
type StrokeStyle struct {
	Color Color
	Width float64
}

// ShapeStyle fills, strokes, or both. A zero Fill with a zero Stroke width draws
// nothing, which is a caller's bug rather than this package's.
type ShapeStyle struct {
	Fill        *Color
	Stroke      *Color
	StrokeWidth float64

	// Radius rounds a rectangle's corners. Ignored by Path.
	Radius float64
}

// Fill is a ShapeStyle that only fills, which is most of them.
func Fill(c Color) ShapeStyle { return ShapeStyle{Fill: &c} }

// Stroke is a ShapeStyle that only strokes.
func Stroke(c Color, width float64) ShapeStyle {
	return ShapeStyle{Stroke: &c, StrokeWidth: width}
}
