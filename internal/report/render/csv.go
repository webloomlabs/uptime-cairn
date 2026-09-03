package render

import (
	"bytes"
	"encoding/csv"
	"strconv"

	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// Row kinds, in the first column.
//
// One file with a discriminator rather than several files or a header block
// above the data. A CSV with prose or a second table above the header is not a
// CSV — every tool that opens it has to be told where the real header is, and
// the first thing anybody does with this file is drop it into a spreadsheet or a
// COPY statement.
const (
	// RowDaily is one monitor on one day: the grain the tiers store.
	RowDaily = "daily"

	// RowMonitorTotal is one monitor over the whole window, so a reader that
	// wants the headline figure does not have to re-derive it — and cannot
	// re-derive it wrongly by averaging the daily ratios, which is the mistake
	// this whole package is arranged to prevent.
	RowMonitorTotal = "monitor_total"

	// RowEstateTotal is every monitor in scope over the whole window.
	RowEstateTotal = "estate_total"

	// RowComparison is one side of a comparative report, carrying the same
	// columns a total does — so a spreadsheet comparing two of them is
	// subtracting like from like rather than reconciling two shapes.
	RowComparison = "comparison_series"
)

var csvHeader = []string{
	"row_type",
	"monitor_id",
	"monitor_name",
	"date",
	"up_checks",
	"down_checks",
	"maintenance_checks",
	"unknown_checks",
	"skipped_checks",
	"observed_checks",
	"uptime_ratio",
	"unobserved_share",
	"maintenance_handling",
	"response_time_avg_ms",
	"response_time_samples",
}

// CSV renders the document as data rather than as layout.
//
// This and JSON are what a client's own BI tool consumes, and they are cheap to
// get right, so they are got right: one row per monitor per day at the grain the
// tiers actually store, plus a total per monitor and one for the estate,
// distinguished by `row_type`.
//
// **An unknown value is an empty field, never a zero.** A null uptime ratio
// written as 0 becomes a day of total downtime the moment somebody charts the
// column, and that is the single most likely way a figure from this product ends
// up wrong in front of a client — the spreadsheet is where the null gets lost.
//
// The maintenance policy travels on every row, because the ratio beside it is
// meaningless without it and a CSV has no footnotes.
func CSV(doc report.Document) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	if err := w.Write(csvHeader); err != nil {
		return nil, err
	}

	if doc.Summary != nil {
		row := totalRow(RowEstateTotal, "", "", doc.Summary.Uptime, doc.Summary.ResponseTime)
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}

	if doc.Comparison != nil {
		for _, series := range doc.Comparison.Series {
			// The label goes in the monitor_name column and the id stays empty:
			// a series is not a monitor, and putting a monitor id on a row that
			// aggregates several would be a join key that joins to the wrong
			// thing. The name column is free text on this row type by design.
			row := totalRow(RowComparison, "", series.Label, series.Uptime, series.ResponseTime)
			// The period is what distinguishes the two sides of a
			// previous_period comparison, and it is the one thing the total row
			// has no column for — so it goes in `date`, which is otherwise empty
			// on a total. Stated here because a reader will meet it.
			if series.PeriodStart != nil {
				row[3] = series.PeriodStart.Format(dateOnly)
			}
			if err := w.Write(row); err != nil {
				return nil, err
			}
		}
	}

	for _, s := range doc.Monitors {
		id, name := s.MonitorID.String(), s.Name

		if err := w.Write(totalRow(RowMonitorTotal, id, name, s.Uptime, s.ResponseTime)); err != nil {
			return nil, err
		}

		// Uptime and latency for the same day come from two structures, so they
		// are joined by date rather than by position: the latency series has a
		// point for every day in the window and the uptime series only for days
		// with a bucket, and zipping two slices that are not the same length is
		// how a chart ends up attributing Tuesday's latency to Monday.
		latencyByDate := make(map[string]report.DayLatency, len(s.ResponseTime.Daily))
		for _, d := range s.ResponseTime.Daily {
			latencyByDate[d.Date.Format(dateOnly)] = d
		}

		for _, d := range s.DailyUptime {
			date := d.Date.Format(dateOnly)
			l := latencyByDate[date]
			row := []string{
				RowDaily, id, name, date,
				strconv.Itoa(d.Uptime.UpChecks),
				strconv.Itoa(d.Uptime.DownChecks),
				strconv.Itoa(d.Uptime.MaintenanceChecks),
				strconv.Itoa(d.Uptime.UnknownChecks),
				strconv.Itoa(d.Uptime.SkippedChecks),
				strconv.Itoa(d.Uptime.ObservedChecks),
				floatOrEmpty(d.Uptime.Ratio, 6),
				floatOrEmpty(d.Uptime.UnobservedShare, 6),
				d.Uptime.MaintenanceHandling,
				floatOrEmpty(l.AverageMs, 3),
				strconv.Itoa(l.SampleCount),
			}
			if err := w.Write(row); err != nil {
				return nil, err
			}
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func totalRow(kind, id, name string, u report.Uptime, l report.Latency) []string {
	return []string{
		kind, id, name, "",
		strconv.Itoa(u.UpChecks),
		strconv.Itoa(u.DownChecks),
		strconv.Itoa(u.MaintenanceChecks),
		strconv.Itoa(u.UnknownChecks),
		strconv.Itoa(u.SkippedChecks),
		strconv.Itoa(u.ObservedChecks),
		floatOrEmpty(u.Ratio, 6),
		floatOrEmpty(u.UnobservedShare, 6),
		u.MaintenanceHandling,
		floatOrEmpty(l.AverageMs, 3),
		strconv.Itoa(l.SampleCount),
	}
}

// floatOrEmpty is where the null survives contact with a spreadsheet.
//
// 'f' rather than 'g': an exponent in a CSV column is read as text by at least
// one spreadsheet somebody will use, and a uptime ratio rendered as 9.99e-01 is
// a support conversation.
func floatOrEmpty(v *float64, decimals int) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', decimals, 64)
}
