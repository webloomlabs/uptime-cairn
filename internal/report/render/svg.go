package render

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"strconv"
	"strings"
)

// SVG is the first of ADR-007's two backends: the one the HTML report draws
// with. The PDF content-stream backend is the second, and the point of the
// primitive set is that neither knows the other exists.
type SVG struct {
	buf    bytes.Buffer
	width  float64
	height float64
}

// NewSVG starts a drawing surface of the given size in points.
func NewSVG(width, height float64) *SVG {
	return &SVG{width: width, height: height}
}

// Document returns the finished SVG element.
//
// Self-contained and inline-able: no external stylesheet, no script, no
// reference to a font file. An artifact is a record that has to render years
// from now, from a file somebody saved, with no network — which rules out the
// ordinary web answers before anything else does.
func (s *SVG) Document() string {
	var out strings.Builder
	fmt.Fprintf(&out,
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %s %s" width="%s" height="%s" role="img">`,
		num(s.width), num(s.height), num(s.width), num(s.height))
	out.WriteString(s.buf.String())
	out.WriteString(`</svg>`)
	return out.String()
}

func (s *SVG) Text(x, y float64, run Run, style TextStyle) {
	weight := "400"
	if style.Weight == Bold {
		weight = "700"
	}
	anchor := map[Anchor]string{Start: "start", Middle: "middle", End: "end"}[style.Anchor]

	fmt.Fprintf(&s.buf,
		`<text x="%s" y="%s" font-size="%s" font-weight="%s" fill="%s" text-anchor="%s">%s</text>`,
		num(x), num(y), num(style.SizePt), weight, style.Fill.Hex(), anchor,
		// Escaped, always. A monitor is named by a user, and a client called
		// `Smith & Co <Ltd>` must produce a report rather than a parse error —
		// which is the kind of defect that only shows up on somebody else's
		// data, after the artifact has been mailed.
		html.EscapeString(run.Text))
}

func (s *SVG) Rect(r Rect, style ShapeStyle) {
	s.buf.WriteString(`<rect x="` + num(r.X) + `" y="` + num(r.Y) +
		`" width="` + num(r.W) + `" height="` + num(r.H) + `"`)
	if style.Radius > 0 {
		s.buf.WriteString(` rx="` + num(style.Radius) + `"`)
	}
	s.writePaint(style)
	s.buf.WriteString(`/>`)
}

func (s *SVG) Line(x1, y1, x2, y2 float64, style StrokeStyle) {
	fmt.Fprintf(&s.buf, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="%s"/>`,
		num(x1), num(y1), num(x2), num(y2), style.Color.Hex(), num(style.Width))
}

func (s *SVG) Path(points []Point, closed bool, style ShapeStyle) {
	if len(points) < 2 {
		// One point is not a line. A monitor with a single day of history is a
		// real case, and drawing nothing is the honest answer to it.
		return
	}

	var d strings.Builder
	for i, p := range points {
		if i == 0 {
			d.WriteString("M")
		} else {
			d.WriteString("L")
		}
		d.WriteString(num(p.X) + " " + num(p.Y))
		if i < len(points)-1 {
			d.WriteString(" ")
		}
	}
	if closed {
		d.WriteString("Z")
	}

	s.buf.WriteString(`<path d="` + d.String() + `"`)
	s.writePaint(style)
	s.buf.WriteString(`/>`)
}

func (s *SVG) Image(r Rect, mime string, data []byte) {
	fmt.Fprintf(&s.buf,
		`<image x="%s" y="%s" width="%s" height="%s" href="data:%s;base64,%s" preserveAspectRatio="xMidYMid meet"/>`,
		num(r.X), num(r.Y), num(r.W), num(r.H), mime, base64.StdEncoding.EncodeToString(data))
}

func (s *SVG) writePaint(style ShapeStyle) {
	if style.Fill != nil {
		s.buf.WriteString(` fill="` + style.Fill.Hex() + `"`)
	} else {
		// Explicit rather than inherited: SVG fills with black by default, so an
		// outline-only shape without this comes out solid.
		s.buf.WriteString(` fill="none"`)
	}
	if style.Stroke != nil {
		s.buf.WriteString(` stroke="` + style.Stroke.Hex() + `" stroke-width="` + num(style.StrokeWidth) + `"`)
	}
}

// num formats a coordinate.
//
// Fixed to two decimals and with trailing zeros trimmed, for one reason that
// matters more than tidiness: ADR-007 item 6 requires the same model rendered
// twice to be byte-identical, and Go's shortest-representation float formatting
// is stable but verbose enough that a one-ulp difference in an intermediate
// would show up as a different string. Rounding first makes the output
// insensitive to that.
func num(v float64) string {
	if v == 0 {
		// Avoids "-0", which is a real float and an ugly diff.
		return "0"
	}
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
