package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// oneDay is a document over a single day, with the hourly series Build fills in
// for a window that short.
func oneDay(t *testing.T, hours int) report.Document {
	t.Helper()

	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, sydney)

	var buckets []store.HistoryBucket
	for i := 0; i < hours; i++ {
		buckets = append(buckets, store.HistoryBucket{
			Start:             start.Add(time.Duration(i) * time.Hour).UTC(),
			Up:                60,
			ResponseTimeSum:   60 * float64(600+i),
			ResponseTimeCount: 60,
		})
	}
	total := report.Sum(buckets)
	daily := []store.HistoryBucket{{
		Start: start.UTC(), Up: total.Up,
		ResponseTimeSum: total.ResponseTimeSum, ResponseTimeCount: total.ResponseTimeCount,
	}}

	var hourly []report.HourBucket
	for _, b := range buckets {
		avg := b.ResponseTimeSum / float64(b.ResponseTimeCount)
		hourly = append(hourly, report.HourBucket{
			Start:       b.Start,
			Uptime:      report.ComputeUptime(b, report.MaintenanceExclude),
			AverageMs:   &avg,
			SampleCount: b.ResponseTimeCount,
		})
	}

	return report.Document{
		Meta: report.Meta{
			SchemaVersion: report.SchemaVersion,
			GeneratedAt:   time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC),
			TemplateName:  "Acme daily",
			PeriodStart:   start,
			PeriodEnd:     start.AddDate(0, 0, 1),
			Timezone:      "Australia/Sydney",
			Resolution:    report.Resolution{Tier: "1m"},
		},
		Scope: report.ScopeSummary{MonitorCount: 1},
		Monitors: []report.MonitorSection{{
			MonitorID:    fixedMonitorID,
			Name:         "checkout",
			Type:         "http",
			Uptime:       report.ComputeUptime(total, report.MaintenanceExclude),
			DailyUptime:  dailyUptime(daily),
			Hourly:       hourly,
			ResponseTime: report.ComputeLatency(total, daily, nil),
		}},
	}
}

func chartsOf(elements []Element) []Chart {
	var out []Chart
	for _, e := range elements {
		if c, ok := e.(Chart); ok {
			out = append(out, c)
		}
	}
	return out
}

// The complaint this answers, in the shape a reader sees it: a report covering
// one day drew a strip of one cell and a line of one point, both of which are
// pictures of a number printed directly beneath them.
func TestADailyReportDrawsTwentyFourBucketsRatherThanOne(t *testing.T) {
	t.Parallel()

	charts := chartsOf(Compose(oneDay(t, 24), Brand{}))
	if len(charts) != 2 {
		t.Fatalf("charts = %d, want an availability strip and a latency line", len(charts))
	}
	for _, c := range charts {
		if len(c.Points) != 24 {
			t.Errorf("%q drew %d points, want 24", c.Title, len(c.Points))
		}
		if !strings.HasPrefix(c.Title, "Hourly") {
			t.Errorf("title = %q, want it to say the grain is hours", c.Title)
		}
		if c.AxisFormat != hourAxisFormat {
			t.Errorf("%q labels its axis with %q, want %q", c.Title, c.AxisFormat, hourAxisFormat)
		}
	}
}

// An hour axis read in UTC on a report an Australian client was sent describes a
// different day from the one on the cover, and the label is the only place a
// reader could ever notice.
func TestHourLabelsAreInTheZoneTheWindowWasCutIn(t *testing.T) {
	t.Parallel()

	charts := chartsOf(Compose(oneDay(t, 24), Brand{}))
	if len(charts) == 0 {
		t.Fatal("no charts")
	}
	for _, c := range charts {
		first := c.Points[0].At.Format(c.axisFormat())
		last := c.Points[len(c.Points)-1].At.Format(c.axisFormat())
		if first != "00:00" || last != "23:00" {
			t.Errorf("%q runs %s–%s, want 00:00–23:00 in Sydney", c.Title, first, last)
		}
		if !strings.Contains(c.Caption, "Australia/Sydney") {
			t.Errorf("%q does not name the zone its hours are read in: %q", c.Title, c.Caption)
		}
	}
}

// The zone reaches the drawn page and not only the element list. Both backends
// take the same Chart, so checking the SVG checks the PDF's labels too.
func TestTheHourAxisReachesTheRenderedPage(t *testing.T) {
	t.Parallel()

	out, err := HTML(oneDay(t, 24), Brand{})
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	page := string(out)
	for _, want := range []string{">00:00<", ">23:00<", "Hourly availability", "Hourly average"} {
		if !strings.Contains(page, want) {
			t.Errorf("rendered page does not contain %q", want)
		}
	}
	if strings.Contains(page, "Daily availability") {
		t.Error("a one-day report still drew the daily strip")
	}
}

// A month keeps its daily charts and its bare date labels. The hourly series is
// an exhibit for a window the daily one cannot draw, not a replacement for it.
func TestALongerReportKeepsItsDailyCharts(t *testing.T) {
	t.Parallel()

	charts := chartsOf(Compose(sample(), Brand{}))
	if len(charts) == 0 {
		t.Fatal("no charts")
	}
	for _, c := range charts {
		if c.AxisFormat != "" {
			t.Errorf("%q asked for %q labels, want the default date", c.Title, c.AxisFormat)
		}
		if !strings.HasPrefix(c.Title, "Daily") {
			t.Errorf("title = %q, want it to stay daily", c.Title)
		}
	}
}

// Best and worst are the same day on every daily report, and printing them
// beside a window average that is the same number a third time invites a reader
// to look for a difference that cannot be there.
func TestBestAndWorstAreOmittedWhenTheyAreOneDay(t *testing.T) {
	t.Parallel()

	page, err := HTML(oneDay(t, 24), Brand{})
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	for _, gone := range []string{"Best day", "Worst day"} {
		if strings.Contains(string(page), gone) {
			t.Errorf("%q is on a one-day report, where it repeats the average", gone)
		}
	}

	// Still there where two days really did differ.
	page, err = HTML(sample(), Brand{})
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	for _, want := range []string{"Best day", "Worst day"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("%q is missing from a multi-day report", want)
		}
	}
}

// The hourly series is a drawing, and the published document keeps the grain its
// contract fixes: `daily` is one point per day typed `format: date`, and
// twenty-four points carrying one date is not a finer reading of that field.
func TestTheHourlySeriesDoesNotReachTheJSONArtifact(t *testing.T) {
	t.Parallel()

	out, err := JSON(oneDay(t, 24))
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var doc struct {
		Monitors []struct {
			ResponseTime struct {
				Daily []map[string]any `json:"daily"`
			} `json:"response_time"`
		} `json:"monitors"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Monitors) != 1 {
		t.Fatalf("monitors = %d, want 1", len(doc.Monitors))
	}
	if n := len(doc.Monitors[0].ResponseTime.Daily); n != 1 {
		t.Errorf("daily series has %d points on a one-day report, want 1 — the hourly series leaked into a `format: date` field", n)
	}
	if strings.Contains(string(out), "hourly") {
		t.Error("the hourly series leaked into the published document")
	}
}

// Selection still works: a template that names only the response-time block gets
// the hourly line and no strip.
func TestSectionSelectionStillChoosesTheHourlyCharts(t *testing.T) {
	t.Parallel()

	charts := chartsOf(ComposeSections(oneDay(t, 24), Brand{}, []string{model.SectionResponseTime}))
	if len(charts) != 1 {
		t.Fatalf("charts = %d, want just the latency line", len(charts))
	}
	if charts[0].Kind != ChartLatencyLine || charts[0].AxisFormat != hourAxisFormat {
		t.Errorf("chart = %v %q, want an hourly latency line", charts[0].Kind, charts[0].AxisFormat)
	}
}

// The PDF is not a converted page: it is the same seven elements drawn
// differently, so the hour axis has to arrive there under its own steam. A
// client sent the PDF and shown the page must not be reading two different
// charts.
func TestTheHourAxisReachesThePDFToo(t *testing.T) {
	t.Parallel()

	_, drawn := renderPDF(t, oneDay(t, 24), Brand{})
	page := strings.Join(pageTexts(drawn), "\n")
	for _, want := range []string{"Hourly availability", "Hourly average", "00:00", "23:00"} {
		if !strings.Contains(page, want) {
			t.Errorf("the PDF does not draw %q", want)
		}
	}
	if strings.Contains(page, "Daily availability") {
		t.Error("the PDF still drew the daily strip for a one-day report")
	}
}
