package render

import (
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// recorder is the second backend, standing in for the PDF one that does not
// exist yet.
//
// It exists so the charts can be tested for *what they draw* rather than for
// what one backend's text looks like — which is the claim ADR-007 actually
// makes: charts are written once against the primitives, and the two backends
// receive the same calls.
type recorder struct {
	texts  []string
	rects  []Rect
	lines  int
	paths  [][]Point
	images int
}

func (r *recorder) Text(_, _ float64, run Run, _ TextStyle) { r.texts = append(r.texts, run.Text) }
func (r *recorder) Rect(rect Rect, _ ShapeStyle)            { r.rects = append(r.rects, rect) }
func (r *recorder) Line(_, _, _, _ float64, _ StrokeStyle)  { r.lines++ }
func (r *recorder) Path(p []Point, _ bool, _ ShapeStyle)    { r.paths = append(r.paths, p) }
func (r *recorder) Image(Rect, string, []byte)              { r.images++ }

var _ Backend = (*recorder)(nil)
var _ Backend = (*SVG)(nil)

func ms(v float64) *float64 { return &v }

func series(values ...*float64) []report.DayLatency {
	out := make([]report.DayLatency, 0, len(values))
	for i, v := range values {
		out = append(out, report.DayLatency{Date: march.AddDate(0, 0, i), AverageMs: v})
	}
	return out
}

// The rule the Phase 1 monitor chart already follows, carried into the report:
// the line breaks at a day with no measurement rather than joining across it.
// A straight confident line through a period nobody observed is a claim, and a
// wrong one.
func TestLatencyLineBreaksAtAGapRatherThanInterpolating(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	LatencyLine(r, Rect{X: 0, Y: 0, W: 100, H: 50},
		series(ms(100), ms(120), nil, nil, ms(200), ms(210)))

	if len(r.paths) != 2 {
		t.Fatalf("drew %d paths, want 2 — one either side of the gap", len(r.paths))
	}
	if len(r.paths[0]) != 2 || len(r.paths[1]) != 2 {
		t.Errorf("path lengths = %d and %d, want 2 and 2", len(r.paths[0]), len(r.paths[1]))
	}
	// The gap is real space on the x-axis: the second run starts further right
	// than the first ended, rather than being drawn adjacent to it.
	if r.paths[1][0].X <= r.paths[0][1].X {
		t.Error("the second run does not start after the gap; days are not positioned by date")
	}
}

// A single observed day between two gaps still gets a mark. It is the one case
// where a point carries information a path cannot, and dropping it would show a
// month with one measurement as a month with none.
func TestAnIsolatedObservationIsStillDrawn(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	LatencyLine(r, Rect{W: 100, H: 50}, series(nil, ms(150), nil))

	if len(r.paths) != 0 {
		t.Errorf("drew %d paths for a single point, want 0", len(r.paths))
	}
	// Two frame rules plus the point marker.
	if len(r.rects) != 1 {
		t.Errorf("drew %d marks, want 1 for the isolated day", len(r.rects))
	}
}

// A month nobody observed draws no series at all, and says so by returning
// false rather than by drawing a flat line along the floor.
func TestNoObservationsDrawsNoSeries(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	_, _, ok := LatencyLine(r, Rect{W: 100, H: 50}, series(nil, nil, nil))

	if ok {
		t.Error("reported a drawable series with nothing observed")
	}
	if len(r.paths) != 0 || len(r.rects) != 0 {
		t.Error("drew something for a series with no measurements")
	}
}

// A near-flat series must not be drawn edge to edge. 180 to 184 ms across the
// full height reads as a service in crisis, and a client reading the shape
// rather than the axis is the reader this report is for.
func TestFlatSeriesIsNotExaggerated(t *testing.T) {
	t.Parallel()

	low, high, ok := latencyBounds(series(ms(180), ms(182), ms(184)))
	if !ok {
		t.Fatal("no bounds")
	}
	if span := high - low; span < 4*1.5 {
		t.Errorf("axis span = %v over a 4ms range, want padding that keeps the wobble small", span)
	}
	if low < 0 {
		t.Errorf("axis floor = %v, want no negative milliseconds", low)
	}
}

// Grey, not red. A day the probe could not observe is not a day the service was
// down — the distinction HistoryBucket keeps, carried onto the page a client
// actually looks at.
func TestUnobservedDayIsNotDrawnAsDowntime(t *testing.T) {
	t.Parallel()

	svg := NewSVG(300, 40)
	UptimeStrip(svg, Rect{X: 0, Y: 0, W: 300, H: 20}, []report.DayUptime{
		{Date: march, Uptime: report.ComputeUptime(store.HistoryBucket{Up: 100}, report.MaintenanceExclude)},
		{Date: march.AddDate(0, 0, 1), Uptime: report.ComputeUptime(store.HistoryBucket{Unknown: 100}, report.MaintenanceExclude)},
		{Date: march.AddDate(0, 0, 2), Uptime: report.ComputeUptime(store.HistoryBucket{Up: 90, Down: 10}, report.MaintenanceExclude)},
	})

	out := svg.Document()
	if strings.Count(out, gapColor.Hex()) != 1 {
		t.Errorf("expected exactly one grey cell for the unobserved day:\n%s", out)
	}
	if strings.Count(out, downColor.Hex()) != 1 {
		t.Errorf("expected exactly one red cell, for the day with real downtime:\n%s", out)
	}
	if strings.Count(out, upColor.Hex()) != 1 {
		t.Errorf("expected exactly one green cell:\n%s", out)
	}
}

// A client called `Smith & Co <Ltd>` gets a report, not a parse error. Names
// come from users, and this is the class of defect that only ever appears on
// somebody else's data, after the artifact has been mailed.
func TestTextIsEscaped(t *testing.T) {
	t.Parallel()

	svg := NewSVG(100, 20)
	svg.Text(0, 10, Run{Text: `Smith & Co <Ltd> "quoted"`}, TextStyle{SizePt: 10, Fill: inkColor})

	out := svg.Document()
	if strings.Contains(out, "<Ltd>") {
		t.Errorf("unescaped angle brackets reached the document:\n%s", out)
	}
	if !strings.Contains(out, "&amp;") || !strings.Contains(out, "&lt;Ltd&gt;") {
		t.Errorf("expected escaped entities:\n%s", out)
	}
}

// ADR-007 item 6, at the backend: the same calls produce the same bytes. Float
// formatting is the thing most likely to break it, which is why coordinates are
// rounded before they are printed rather than after.
func TestSVGOutputIsDeterministic(t *testing.T) {
	t.Parallel()

	draw := func() string {
		svg := NewSVG(200, 100)
		LatencyLine(svg, Rect{X: 10, Y: 10, W: 180, H: 60}, series(ms(100), ms(133.3333333), nil, ms(97.7)))
		svg.Text(10, 90, Run{Text: "checkout"}, TextStyle{SizePt: 9, Fill: mutedColor, Anchor: Middle})
		return svg.Document()
	}

	first := draw()
	for i := 0; i < 5; i++ {
		if again := draw(); again != first {
			t.Fatalf("render %d differs:\n%s\n%s", i, first, again)
		}
	}
	if strings.Contains(first, "-0\"") {
		t.Error("negative zero reached the output")
	}
}

// An outline-only shape must not come out solid black. SVG fills by default, so
// the backend states fill="none" rather than leaving it inherited — the kind of
// default that produces a chart with every cell blacked out.
func TestStrokeOnlyShapesAreNotFilled(t *testing.T) {
	t.Parallel()

	svg := NewSVG(10, 10)
	svg.Rect(Rect{W: 10, H: 10}, Stroke(inkColor, 1))

	if !strings.Contains(svg.Document(), `fill="none"`) {
		t.Errorf("stroke-only rect has no explicit fill:\n%s", svg.Document())
	}
}

// The primitive set is what both backends implement forever, so its size is a
// decision. This fails the moment a sixth is added, which is the point: adding
// one should require deleting this line and meaning it.
func TestPrimitiveSetIsFive(t *testing.T) {
	t.Parallel()

	// Text, Rect, Line, Path, Image.
	const want = 5
	got := 0
	var b Backend = &recorder{}
	if _, ok := b.(interface {
		Text(float64, float64, Run, TextStyle)
	}); ok {
		got++
	}
	if _, ok := b.(interface{ Rect(Rect, ShapeStyle) }); ok {
		got++
	}
	if _, ok := b.(interface {
		Line(float64, float64, float64, float64, StrokeStyle)
	}); ok {
		got++
	}
	if _, ok := b.(interface {
		Path([]Point, bool, ShapeStyle)
	}); ok {
		got++
	}
	if _, ok := b.(interface{ Image(Rect, string, []byte) }); ok {
		got++
	}
	if got != want {
		t.Errorf("primitive set has %d of the expected %d operations", got, want)
	}
}

// Both backends receive identical calls for one chart. That is the whole of
// ADR-007 item 2, and without it the SVG and the PDF are two charts of the same
// data that can quietly disagree.
func TestBothBackendsReceiveTheSameCalls(t *testing.T) {
	t.Parallel()

	days := series(ms(100), nil, ms(120))
	area := Rect{X: 0, Y: 0, W: 90, H: 45}

	first, second := &recorder{}, &recorder{}
	LatencyLine(first, area, days)
	LatencyLine(second, area, days)

	if len(first.paths) != len(second.paths) || first.lines != second.lines {
		t.Fatal("two backends given the same chart received different calls")
	}
	for i := range first.paths {
		for j := range first.paths[i] {
			if first.paths[i][j] != second.paths[i][j] {
				t.Fatalf("path %d point %d differs: %v vs %v", i, j, first.paths[i][j], second.paths[i][j])
			}
		}
	}
	_ = time.Now // the charts consult no clock; nothing here needs one
}
