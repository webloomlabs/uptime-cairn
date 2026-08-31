package render

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// Brand is the resolved brand profile, denormalised so a stored document renders
// standalone after the profile has been edited or deleted.
//
// Text fields are plain text and are treated as such by every backend.
type Brand struct {
	CompanyName   string
	FooterText    string
	CoverText     string
	Logo          []byte
	LogoMIME      string
	HidePoweredBy bool
}

// Compose turns a computed report into the bounded element list.
//
// This is the one place the report's *face* is decided, and it is deliberately
// separate from both the computation and the backends: the numbers are already
// settled when Compose runs, and what it chooses is which of them a reader sees
// and what has to be said next to them.
//
// Two obligations from §4.3 are discharged here rather than left to a template,
// because a report that omits them is a report that misleads:
//
//   - **The denominator is on the face.** Every uptime figure is accompanied by
//     what it was computed over, and by how much of the window observed nothing.
//   - **The maintenance policy is on the face.** The same window yields three
//     different lawful percentages, and a figure without its policy cannot be
//     checked by the person it is handed to.
func Compose(doc report.Document, brand Brand) []Element {
	var out []Element

	period := formatPeriod(doc.Meta.PeriodStart, doc.Meta.PeriodEnd)
	out = append(out, Cover{
		Title:      titleOr(doc.Meta.TemplateName, "Uptime report"),
		ClientName: brand.CompanyName,
		Period:     period,
		Generated:  doc.Meta.GeneratedAt.Format("2 January 2006"),
		Logo:       brand.Logo,
		LogoMIME:   brand.LogoMIME,
	})

	if brand.CoverText != "" {
		out = append(out, Paragraph{Text: brand.CoverText})
	}

	// The methodology note, before any figure. It is short because nobody reads
	// a long one, and it is first because a denominator explained after the
	// number has already been misread.
	out = append(out, Paragraph{Muted: true, Text: methodology(doc)})

	if doc.Summary != nil {
		out = append(out, Heading{Text: "Summary", Level: 1})
		out = append(out, KeyValues{Items: uptimeFigures(doc.Summary.Uptime)})
		out = append(out, KeyValues{Items: latencyFigures(doc.Summary.ResponseTime)})
	}

	for _, s := range doc.Monitors {
		out = append(out, Heading{Text: s.Name, Level: 1})

		if len(s.DailyUptime) > 0 {
			out = append(out, Chart{
				Kind:    ChartUptimeStrip,
				Title:   "Daily availability",
				Caption: "Green: no downtime observed. Red: downtime observed. Grey: nothing observed — a gap, not an outage.",
				Days:    s.DailyUptime,
			})
		}
		out = append(out, KeyValues{Items: uptimeFigures(s.Uptime)})

		if s.SLA != nil {
			out = append(out, Heading{Text: "Service level", Level: 2})
			out = append(out, KeyValues{Items: slaFigures(*s.SLA)})
			if len(s.Breaches) > 0 {
				out = append(out, breachTable(s.Breaches))
			}
		}

		out = append(out, Heading{Text: "Response time", Level: 2})
		if len(s.ResponseTime.Daily) > 0 {
			out = append(out, Chart{
				Kind:    ChartLatencyLine,
				Title:   "Daily average",
				Caption: "The line breaks where nothing was measured rather than joining across it.",
				Latency: s.ResponseTime.Daily,
			})
		}
		out = append(out, KeyValues{Items: latencyFigures(s.ResponseTime)})
	}

	out = append(out, Footer{Text: brand.FooterText, HidePoweredBy: brand.HidePoweredBy})
	return out
}

// methodology is the sentence a disputed figure gets checked against.
func methodology(doc report.Document) string {
	var b strings.Builder
	b.WriteString("Uptime is the share of observed checks that succeeded. ")
	b.WriteString("Checks the probe could not make are excluded from the figure and reported separately: a gap in observation is not an outage. ")

	switch policyOf(doc) {
	case report.MaintenanceCountAsUp:
		b.WriteString("Declared maintenance counts towards uptime. ")
	case report.MaintenanceCountAsDown:
		b.WriteString("Declared maintenance counts against uptime. ")
	default:
		b.WriteString("Declared maintenance is excluded. ")
	}

	fmt.Fprintf(&b, "Figures are read at %s resolution", resolutionWords(doc.Meta.Resolution.Tier))
	if doc.Meta.Resolution.Downgraded {
		b.WriteString(", which is coarser than requested because retention no longer holds finer data for this period")
	}
	b.WriteString(".")

	if from := doc.Meta.Resolution.CoveredFrom; from != nil {
		fmt.Fprintf(&b, " Data is available only from %s; the period before that is not covered.", from.Format("2 January 2006"))
	}
	return b.String()
}

func policyOf(doc report.Document) string {
	if doc.Summary != nil {
		return doc.Summary.Uptime.MaintenanceHandling
	}
	if len(doc.Monitors) > 0 {
		return doc.Monitors[0].Uptime.MaintenanceHandling
	}
	return report.MaintenanceExclude
}

func resolutionWords(tier string) string {
	switch tier {
	case "1m":
		return "one-minute"
	case "5m":
		return "five-minute"
	case "1h":
		return "hourly"
	case "1d":
		return "daily"
	}
	return tier
}

func uptimeFigures(u report.Uptime) []KeyValue {
	items := []KeyValue{{
		Key:   "Uptime",
		Value: percentOrDash(u.Ratio),
		Note:  fmt.Sprintf("of %s observed checks", thousands(u.ObservedChecks)),
	}}

	if u.Ratio == nil {
		// The one case where the note has to do the whole job: no figure, and a
		// reader owed an explanation rather than a blank.
		items[0].Note = "nothing was observed in this period"
	}

	items = append(items,
		KeyValue{Key: "Successful checks", Value: thousands(u.UpChecks)},
		KeyValue{Key: "Failed checks", Value: thousands(u.DownChecks)},
	)
	if u.MaintenanceChecks > 0 {
		items = append(items, KeyValue{
			Key:   "During maintenance",
			Value: thousands(u.MaintenanceChecks),
			Note:  maintenanceWords(u.MaintenanceHandling),
		})
	}
	if u.UnknownChecks+u.SkippedChecks > 0 {
		items = append(items, KeyValue{
			Key:   "Not observed",
			Value: thousands(u.UnknownChecks + u.SkippedChecks),
			Note:  shareNote(u.UnobservedShare),
		})
	}
	return items
}

func maintenanceWords(handling string) string {
	switch handling {
	case report.MaintenanceCountAsUp:
		return "counted towards uptime"
	case report.MaintenanceCountAsDown:
		return "counted against uptime"
	default:
		return "excluded from the figure above"
	}
}

func shareNote(share *float64) string {
	if share == nil {
		return "excluded from the figure above"
	}
	return fmt.Sprintf("%s of the period; excluded from the figure above", percentOrDash(share))
}

func slaFigures(s report.SLA) []KeyValue {
	items := []KeyValue{
		{
			Key:   "Target",
			Value: trimZeros(strconv.FormatFloat(s.TargetPercent, 'f', 3, 64)) + "%",
			Note:  targetSourceWords(s.TargetSource),
		},
		{Key: "Achieved", Value: percentValueOrDash(s.ActualPercent), Note: metWords(s.Met)},
		{Key: "Error budget", Value: duration(s.ErrorBudgetSeconds), Note: "for the period"},
		{Key: "Budget used", Value: duration(s.ErrorBudgetConsumedSeconds)},
	}

	if s.ErrorBudgetRemainingSeconds < 0 {
		items = append(items, KeyValue{
			Key:   "Over budget by",
			Value: duration(-s.ErrorBudgetRemainingSeconds),
		})
	} else {
		items = append(items, KeyValue{
			Key:   "Budget remaining",
			Value: duration(s.ErrorBudgetRemainingSeconds),
		})
	}

	if s.BurnRate != nil {
		items = append(items, KeyValue{
			Key:   "Burn rate",
			Value: trimZeros(strconv.FormatFloat(*s.BurnRate, 'f', 2, 64)) + "×",
			Note:  "1× spends the budget exactly over the period",
		})
	}
	return items
}

func targetSourceWords(source string) string {
	switch source {
	case report.TargetFromTemplate:
		return "set on this report"
	case report.TargetFromGroup:
		return "inherited from the monitor's group"
	default:
		return "set on the monitor"
	}
}

func metWords(met *bool) string {
	switch {
	case met == nil:
		return "no figure: nothing was observed"
	case *met:
		return "target met"
	default:
		return "target missed"
	}
}

func latencyFigures(l report.Latency) []KeyValue {
	items := []KeyValue{{
		Key:   "Average response",
		Value: millis(l.AverageMs),
		Note:  fmt.Sprintf("over %s successful checks", thousands(l.SampleCount)),
	}}

	if l.BestDay != nil {
		items = append(items, KeyValue{
			Key:   "Best day",
			Value: millis(l.BestDay.AverageMs),
			Note:  l.BestDay.Date.Format("2 January"),
		})
	}
	if l.WorstDay != nil {
		items = append(items, KeyValue{
			Key:   "Worst day",
			Value: millis(l.WorstDay.AverageMs),
			Note:  l.WorstDay.Date.Format("2 January"),
		})
	}
	if l.DaysOverTarget != nil && l.TargetMs != nil {
		items = append(items, KeyValue{
			Key:   "Days over target",
			Value: strconv.Itoa(*l.DaysOverTarget),
			Note:  fmt.Sprintf("target %dms", *l.TargetMs),
		})
	}

	// The percentile, and its window, and why it is missing when it is. Never a
	// bare blank: an absent figure with no reason reads as a defect.
	if l.P95 != nil {
		items = append(items, p95Figure(*l.P95))
	}
	return items
}

func p95Figure(p report.P95) KeyValue {
	if p.Available {
		note := "nearest rank"
		if p.WindowStart != nil && p.WindowEnd != nil {
			note = fmt.Sprintf("nearest rank, %s — the last seven days of the period, not the whole of it",
				formatPeriod(*p.WindowStart, *p.WindowEnd))
		}
		return KeyValue{Key: "95th percentile", Value: millis(p.ValueMs), Note: note}
	}

	reason := "not available"
	switch p.Reason {
	case report.ReasonInsufficientRaw:
		reason = "raw history is kept for less than seven days, so a shorter figure would be reported under a seven-day heading"
	case report.ReasonNoSuccessfulChecks:
		reason = "no successful checks in the last seven days"
	case report.ReasonScopeTooLarge:
		reason = "not computed for a report of this size"
	}
	return KeyValue{Key: "95th percentile", Value: "—", Note: reason}
}

func breachTable(breaches []report.Breach) Table {
	t := Table{Columns: []Column{
		{Title: "From"},
		{Title: "To"},
		{Title: "Downtime", Numeric: true},
	}}
	for _, b := range breaches {
		t.Rows = append(t.Rows, []string{
			b.StartedAt.Format("2 Jan 2006"),
			b.EndedAt.Format("2 Jan 2006"),
			duration(b.DurationSeconds),
		})
	}
	return t
}

func formatPeriod(from, to time.Time) string {
	// The end is exclusive everywhere in this product, and a reader does not
	// think that way: a month ending at midnight on 1 April is "to 31 March".
	last := to.Add(-time.Second)
	if from.Year() == last.Year() {
		return from.Format("2 Jan") + " – " + last.Format("2 Jan 2006")
	}
	return from.Format("2 Jan 2006") + " – " + last.Format("2 Jan 2006")
}

func titleOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func percentOrDash(ratio *float64) string {
	if ratio == nil {
		return "—"
	}
	return trimZeros(strconv.FormatFloat(*ratio*100, 'f', 3, 64)) + "%"
}

func percentValueOrDash(pct *float64) string {
	if pct == nil {
		return "—"
	}
	return trimZeros(strconv.FormatFloat(*pct, 'f', 3, 64)) + "%"
}

func millis(v *float64) string {
	if v == nil {
		return "—"
	}
	return trimZeros(strconv.FormatFloat(*v, 'f', 1, 64)) + "ms"
}

func trimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

// duration renders seconds the way somebody reads a budget: hours and minutes,
// not 5184 seconds.
func duration(seconds int) string {
	if seconds <= 0 {
		return "0m"
	}
	d := time.Duration(seconds) * time.Second

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	var parts []string
	if days > 0 {
		parts = append(parts, strconv.Itoa(days)+"d")
	}
	if hours > 0 {
		parts = append(parts, strconv.Itoa(hours)+"h")
	}
	if mins > 0 || len(parts) == 0 {
		parts = append(parts, strconv.Itoa(mins)+"m")
	}
	return strings.Join(parts, " ")
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
