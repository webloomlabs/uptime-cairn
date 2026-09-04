package render

import "time"

// The document model (ADR-007 item 3): cover block, heading, paragraph,
// key–value block, table, chart, footer.
//
// **Seven elements, and anything not on the list is not a report element in
// Phase 2.** The list may grow by decision; it may not grow by accident. That
// wording is the ADR's and it is the whole value of enumerating them: a renderer
// with an open-ended element set is a renderer where the HTML and the PDF drift,
// because each new element is implemented once and remembered twice.
//
// Composition happens once, into this list, and each backend renders the list.
// A PDF is therefore never a converted HTML page — it is the same seven
// elements, drawn differently.
type Element interface{ isElement() }

// Cover is the first page: who the report is for, what it covers, and when.
type Cover struct {
	Title      string
	ClientName string
	Period     string
	Generated  string

	// Logo is raster bytes, PNG or JPEG. SVG is refused at upload rather than
	// dropped here, because the PDF backend embeds rasters and has no path
	// translator — and a logo that silently vanishes between the HTML and the
	// PDF is worse than one that was never accepted.
	Logo     []byte
	LogoMIME string
}

// Heading introduces a section. Two levels; a third would be a document
// structure nobody reads.
type Heading struct {
	Text  string
	Level int
}

// Paragraph is prose. **Plain text, never markup**: a field that renders in HTML
// and not in PDF is worse than one that renders nowhere (ADR-007), and branded
// text comes from a user who may well try bold.
type Paragraph struct {
	Text string

	// Muted marks the small print — the methodology notes that have to be on the
	// face of the report and should not compete with the figures.
	Muted bool
}

// KeyValues is the figure block: label on the left, value on the right.
type KeyValues struct {
	Items []KeyValue
}

// KeyValue is one figure. Note is the qualifier that makes it honest — the
// denominator beside a percentage, the window beside a percentile.
type KeyValue struct {
	Key   string
	Value string
	Note  string
}

// Table is rows of cells, page-broken by the backend that needs to.
//
// Page breaking lives in the PDF backend rather than here: HTML has no pages,
// and a composition step that inserted page breaks would be composing for one
// backend and apologising to the other.
type Table struct {
	Columns []Column
	Rows    [][]string
}

// Column is a heading and how its cells align. Numeric columns are right
// aligned, because a column of figures that does not line up on the decimal
// point is a column nobody can scan.
type Column struct {
	Title   string
	Numeric bool
}

// Chart is a figure drawn against the primitives, so both backends draw it from
// the same calls rather than from two implementations.
type Chart struct {
	Kind    ChartKind
	Title   string
	Caption string

	// Points is the series, at whatever grain the window called for — a day on a
	// monthly report, an hour on a daily one. The primitives that draw it
	// deliberately do not know which: a strip of thirty days and a strip of
	// twenty-four hours are the same drawing, and the axis labels are what tell a
	// reader which one is in front of them.
	Points []ChartPoint

	// AxisFormat is the layout the end labels are written with. Empty is the
	// date, which is what every series of days uses.
	AxisFormat string
}

// ChartPoint is one bucket of a chart series.
//
// One Value serves both kinds — an uptime ratio for the strip, an average
// response for the line — because the two charts differ in how they draw a
// number rather than in what they are handed. Nil is "nothing observed", and
// both draw it as a gap rather than as zero: the separation the rollup tiers
// keep between unknown and down survives all the way onto the page.
type ChartPoint struct {
	At    time.Time
	Value *float64
}

// axisFormat is the layout for the two end labels. A series of days is the
// default because it is every report but the shortest.
func (c Chart) axisFormat() string {
	if c.AxisFormat == "" {
		return "2 Jan"
	}
	return c.AxisFormat
}

// ChartKind enumerates what can be drawn. Growing this means writing the drawing
// once, against the primitives — which is the point.
type ChartKind int

const (
	ChartUptimeStrip ChartKind = iota
	ChartLatencyLine
)

// Footer closes the document.
type Footer struct {
	Text          string
	HidePoweredBy bool
}

func (Cover) isElement()     {}
func (Heading) isElement()   {}
func (Paragraph) isElement() {}
func (KeyValues) isElement() {}
func (Table) isElement()     {}
func (Chart) isElement()     {}
func (Footer) isElement()    {}
