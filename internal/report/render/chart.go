package render

import "math"

// The report palette.
//
// Deliberately not read from the brand profile: an accent colour is a client's
// logo colour and a status colour is a claim about whether their service was up.
// A client whose brand is red would otherwise get a chart where healthy days are
// drawn in the colour every reader takes for failure.
var (
	inkColor   = Color{0x1a, 0x1f, 0x2b}
	mutedColor = Color{0x6b, 0x72, 0x84}
	gridColor  = Color{0xe2, 0xe5, 0xea}
	upColor    = Color{0x1a, 0x8f, 0x5a}
	downColor  = Color{0xc0, 0x39, 0x2b}
	gapColor   = Color{0xc7, 0xcc, 0xd4}
	lineColor  = Color{0x3b, 0x5b, 0xdb}
)

// UptimeStrip draws one cell per bucket, the shape a reader already knows from
// the status page.
//
// The bucket is a day on most reports and an hour on a report covering one, and
// nothing here knows which: a strip is a row of cells whose colours are decided
// by a ratio, and the grain belongs to the caption beneath it.
//
// A bucket with no observation is drawn in **grey, not red**. That is the single
// most common way a status page lies, it is the reason HistoryBucket keeps
// unknown and skipped apart from down, and a report that got it wrong would put
// the lie in front of the client rather than on a page they might not visit.
//
// Colour is never the only carrier: every cell gets a title, and the caller
// draws a legend beneath. Roughly one man in twelve cannot reliably separate
// this green from this red, and the strip is the most repeated element in the
// document.
func UptimeStrip(b Backend, area Rect, points []ChartPoint) {
	if len(points) == 0 {
		return
	}

	const gap = 2.0
	width := (area.W - gap*float64(len(points)-1)) / float64(len(points))
	if width <= 0 {
		// More buckets than pixels. Drawing sub-pixel slivers would produce a
		// smear that reads as data; drawing nothing is honest and the caller
		// falls back to the daily table.
		return
	}

	for i, d := range points {
		x := area.X + float64(i)*(width+gap)

		fill := gapColor
		switch {
		case d.Value == nil:
			// Nothing observed. Grey.
		case *d.Value >= 1:
			fill = upColor
		case *d.Value > 0:
			// Partial. The down colour, because a bucket with any downtime in it
			// is time the client noticed, and a third colour for "mostly up"
			// would be a judgement the report has no basis for.
			fill = downColor
		default:
			fill = downColor
		}
		b.Rect(Rect{X: x, Y: area.Y, W: width, H: area.H}, ShapeStyle{Fill: &fill, Radius: 1})
	}
}

// LatencyLine draws the average response series.
//
// **The line breaks at a bucket with no measurement rather than interpolating
// across it**, which is the rule the Phase 1 monitor detail chart already
// follows and the reason this is a set of paths rather than one. Joining across
// a gap draws a straight, confident line through a period nobody observed —
// which is a claim, and a wrong one.
//
// Returns the axis bounds it used, so the caller can label them: a chart whose
// y-axis is unlabelled is decoration.
func LatencyLine(b Backend, area Rect, points []ChartPoint) (low, high float64, ok bool) {
	low, high, ok = latencyBounds(points)
	if !ok {
		return 0, 0, false
	}

	// Baseline and top rule, so the plot has a frame to be read against.
	b.Line(area.X, area.Y+area.H, area.X+area.W, area.Y+area.H, StrokeStyle{Color: gridColor, Width: 1})
	b.Line(area.X, area.Y, area.X+area.W, area.Y, StrokeStyle{Color: gridColor, Width: 1})

	step := 0.0
	if len(points) > 1 {
		step = area.W / float64(len(points)-1)
	}
	span := high - low
	if span == 0 {
		// A flat series. Centring it beats dividing by zero, and beats drawing a
		// line along the floor that reads as a collapse to nothing.
		span = 1
	}

	var run []Point
	flush := func() {
		if len(run) > 1 {
			b.Path(run, false, Stroke(lineColor, 1.5))
		} else if len(run) == 1 {
			// A single observed bucket between two gaps still gets a mark. It is
			// the one case where a point carries information a path cannot.
			b.Rect(Rect{X: run[0].X - 1.5, Y: run[0].Y - 1.5, W: 3, H: 3}, Fill(lineColor))
		}
		run = nil
	}

	for i, d := range points {
		if d.Value == nil {
			flush()
			continue
		}
		x := area.X + float64(i)*step
		y := area.Y + area.H - (*d.Value-low)/span*area.H
		run = append(run, Point{X: x, Y: y})
	}
	flush()

	return low, high, true
}

// latencyBounds picks the y-axis range over observed buckets only.
//
// Padded by a tenth and floored at zero: a series that oscillates between 180
// and 184 ms drawn edge to edge looks like a service in crisis, and the reader
// cannot tell without reading the axis — which is exactly the misreading a
// report handed to a client must not invite.
func latencyBounds(points []ChartPoint) (low, high float64, ok bool) {
	low, high = math.Inf(1), math.Inf(-1)
	for _, d := range points {
		if d.Value == nil {
			continue
		}
		low = math.Min(low, *d.Value)
		high = math.Max(high, *d.Value)
		ok = true
	}
	if !ok {
		return 0, 0, false
	}

	// Padded against the *magnitude*, not only against the range, and that is
	// the part that matters. Ten per cent of the range leaves a series that
	// wobbles between 180 and 184 ms still filling four fifths of the height —
	// which is the misreading this padding exists to prevent, not a milder
	// version of it. Ten per cent of the upper value puts a 4 ms wobble in
	// roughly a tenth of the plot, which is what it deserves.
	pad := math.Max((high-low)*0.1, high*0.1)
	if pad == 0 {
		pad = 1
	}
	low = math.Max(0, low-pad)
	return low, high + pad, true
}
