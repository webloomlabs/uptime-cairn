package render

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// Brand is the resolved brand profile, denormalised so a stored document renders
// standalone after the profile has been edited or deleted.
//
// Text fields are plain text and are treated as such by every backend.
type Brand struct {
	CompanyName string

	// Six-digit hex including the leading '#', as pasted from a brand guide.
	// Empty means "use the renderer's own", which is what an unbranded install
	// gets and is a deliberate look rather than a missing one.
	PrimaryColor string
	AccentColor  string

	FooterText string
	CoverText  string

	Logo     []byte
	LogoMIME string

	HidePoweredBy bool
}

// Denormalised is the subset the stored document carries, in the shape the
// frozen spec's `meta.brand` fixes.
//
// Narrower than this struct on purpose. The logo bytes are not in it because the
// spec's field is a `logo_url` and there is no operation that serves one; the
// accent colour and the cover text are not in it because the spec's object has
// no place for them. Both are applied at render time, which is where they are
// used, and the JSON artifact is a data document rather than a second copy of
// the page.
//
// An entirely empty brand denormalises to nil rather than to an object of empty
// strings: "this instance has no branding" is a different answer from "it has
// branding that says nothing", and the spec types the field as nullable so that
// both can be given.
func (b Brand) Denormalised() *report.Brand {
	if b.CompanyName == "" && b.PrimaryColor == "" && b.FooterText == "" && !b.HidePoweredBy {
		return nil
	}
	return &report.Brand{
		CompanyName:   b.CompanyName,
		PrimaryColor:  b.PrimaryColor,
		FooterText:    b.FooterText,
		HidePoweredBy: b.HidePoweredBy,
	}
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
	return ComposeSections(doc, brand, nil)
}

// ComposeSections is Compose with a template's chosen content blocks.
//
// A nil or empty selection composes the defaults for the document, which is what
// Compose passes and what every template that has never named a section gets. See
// sections.go for what selection and ordering mean, and for the one structural
// rule: the per-monitor group is emitted once, where its first member was named.
//
// **The cover, the methodology note and the footer are not sections and cannot be
// deselected.** That is deliberate rather than an omission. The methodology note
// carries the denominator and the maintenance policy, and §4.3 makes both an
// obligation of the report face — a figure whose policy can be switched off is a
// figure that cannot be checked by the person it is handed to, which is the exact
// failure a report exists to prevent. The frozen enum has no name for any of the
// three, so nothing is being refused that the API offers.
func ComposeSections(doc report.Document, brand Brand, sections []string) []Element {
	var out []Element

	layout := resolveLayout(sections, reportShape{
		hasSummary:    doc.Summary != nil,
		hasComparison: doc.Comparison != nil && len(doc.Comparison.Series) > 0,
		hasIncidents:  len(doc.Incidents) > 0,
	})

	// The zone the window was cut in, resolved once. It is what the hourly
	// charts are labelled in: an hour axis reading 00:00–23:00 in UTC on a report
	// an Australian client was sent describes a day that is not the day on the
	// cover, and the label is the only place a reader could notice.
	loc := locationOf(doc.Meta.Timezone)

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

	for _, block := range layout.order {
		switch block {
		case model.SectionSummary:
			if doc.Summary != nil {
				out = append(out, Heading{Text: "Summary", Level: 1})
				out = append(out, KeyValues{Items: uptimeFigures(doc.Summary.Uptime)})
				out = append(out, KeyValues{Items: latencyFigures(doc.Summary.ResponseTime)})
			}
		case monitorBlock:
			for _, s := range doc.Monitors {
				out = append(out, monitorSection(s, layout, loc)...)
			}
		case model.SectionComparison:
			if doc.Comparison != nil && len(doc.Comparison.Series) > 0 {
				out = append(out, comparisonSection(*doc.Comparison)...)
			}
		case model.SectionIncidentLog:
			if len(doc.Incidents) > 0 {
				out = append(out, incidentSection(doc)...)
			}
		case model.SectionMaintenanceLog, model.SectionCertificateExpiry:
			// **Named by the frozen enum and not composed by anything.**
			//
			// Selecting one is accepted at the API — it is a valid section — and
			// contributes no block, which is the honest behaviour while the
			// document model has no element for either. A maintenance log needs a
			// windows query the report store does not have; a certificate expiry
			// table is the expiry-calendar report type, which is its own piece of
			// work with its own entry on the Phase 2 checklist.
			//
			// Silently absent rather than refused, because refusing at render
			// time would fail a queued run over a choice the API accepted, and
			// refusing at the API would narrow a frozen enum.
		}
	}

	out = append(out, Footer{Text: brand.FooterText, HidePoweredBy: brand.HidePoweredBy})
	return out
}

// monitorSection draws one monitor's blocks, in the order the template named
// them.
//
// The heading is emitted only when there is something under it. A selection of
// document-level sections alone should not produce a page of monitor names with
// nothing beneath each — which is what an unconditional heading would give, and
// which reads as a rendering fault rather than as a choice somebody made.
func monitorSection(s report.MonitorSection, layout layout, loc *time.Location) []Element {
	var body []Element

	for _, block := range layout.within {
		switch block {
		case model.SectionUptimeChart:
			if chart, ok := uptimeChart(s, loc); ok {
				body = append(body, chart)
			}
		case model.SectionUptimeTable:
			body = append(body, KeyValues{Items: uptimeFigures(s.Uptime)})
		case model.SectionSLABreakdown, model.SectionErrorBudget:
			// One block for two names. The spec separates them and ADR-006 does
			// not: the error budget is computed from the same up and down counts
			// as the target-versus-actual line and is rendered beside it, so
			// splitting the block would put a budget on one page and the target
			// it is a budget against on another. Selecting either gives the
			// whole service-level block; selecting both gives it once, which the
			// guard below is for.
			if s.SLA != nil && !slaAlreadyDrawn(body) {
				body = append(body, Heading{Text: "Service level", Level: 2})
				body = append(body, KeyValues{Items: slaFigures(*s.SLA)})
				if len(s.Breaches) > 0 {
					body = append(body, breachTable(s.Breaches))
				}
			}
		case model.SectionResponseTime:
			body = append(body, Heading{Text: "Response time", Level: 2})
			if chart, ok := latencyChart(s, loc); ok {
				body = append(body, chart)
			}
			body = append(body, KeyValues{Items: latencyFigures(s.ResponseTime)})
		}
	}

	if len(body) == 0 {
		return nil
	}
	return append([]Element{Heading{Text: s.Name, Level: 1}}, body...)
}

// hourAxisFormat labels an hourly axis. Hours and minutes without a date,
// because every bucket on the chart falls on the day already named on the cover
// and repeating it twice under a 24-cell strip is noise.
const hourAxisFormat = "15:04"

// uptimeChart draws availability at the finest grain the document carries.
//
// **A report covering one day gets one daily cell**, which is a picture of the
// uptime percentage printed directly beneath it and tells a reader nothing they
// did not already have. Where the document carries an hourly series — which is
// exactly where the window was too short for the daily one to be a chart — that
// is what gets drawn, and the caption says which grain it is looking at rather
// than leaving the reader to count cells.
func uptimeChart(s report.MonitorSection, loc *time.Location) (Chart, bool) {
	const legend = "Green: no downtime observed. Red: downtime observed. Grey: nothing observed — a gap, not an outage."

	if len(s.Hourly) > 0 {
		points := make([]ChartPoint, 0, len(s.Hourly))
		for _, h := range s.Hourly {
			points = append(points, ChartPoint{At: h.Start.In(loc), Value: h.Uptime.Ratio})
		}
		return Chart{
			Kind:       ChartUptimeStrip,
			Title:      "Hourly availability",
			Caption:    "One cell per hour, " + zoneWords(loc) + ". " + legend,
			Points:     points,
			AxisFormat: hourAxisFormat,
		}, true
	}

	if len(s.DailyUptime) == 0 {
		return Chart{}, false
	}
	points := make([]ChartPoint, 0, len(s.DailyUptime))
	for _, d := range s.DailyUptime {
		points = append(points, ChartPoint{At: d.Date, Value: d.Uptime.Ratio})
	}
	return Chart{
		Kind:    ChartUptimeStrip,
		Title:   "Daily availability",
		Caption: legend,
		Points:  points,
	}, true
}

// latencyChart is the same choice for the response-time series, and it is made
// the same way for the same reason: a line of one point is a dot.
func latencyChart(s report.MonitorSection, loc *time.Location) (Chart, bool) {
	const gapNote = "The line breaks where nothing was measured rather than joining across it."

	if len(s.Hourly) > 0 {
		points := make([]ChartPoint, 0, len(s.Hourly))
		for _, h := range s.Hourly {
			points = append(points, ChartPoint{At: h.Start.In(loc), Value: h.AverageMs})
		}
		return Chart{
			Kind:       ChartLatencyLine,
			Title:      "Hourly average",
			Caption:    "One point per hour, " + zoneWords(loc) + ". " + gapNote,
			Points:     points,
			AxisFormat: hourAxisFormat,
		}, true
	}

	if len(s.ResponseTime.Daily) == 0 {
		return Chart{}, false
	}
	points := make([]ChartPoint, 0, len(s.ResponseTime.Daily))
	for _, d := range s.ResponseTime.Daily {
		points = append(points, ChartPoint{At: d.Date, Value: d.AverageMs})
	}
	return Chart{
		Kind:    ChartLatencyLine,
		Title:   "Daily average",
		Caption: gapNote,
		Points:  points,
	}, true
}

// locationOf resolves the zone the window was cut in, falling back to UTC.
//
// A zone name the runtime cannot load is not worth failing a report over — the
// figures are unaffected and only two axis labels move — but it is worth
// resolving here rather than in each caller, so that the two charts of one
// monitor cannot end up labelled in different zones.
func locationOf(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// zoneWords names the zone an hour axis is read in.
//
// The IANA name rather than an abbreviation: "AEST" is ambiguous across
// hemispheres and silently wrong for half the year, and a client checking a
// report against their own logs needs the zone the boundaries were cut in.
func zoneWords(loc *time.Location) string {
	if loc == nil || loc == time.UTC {
		return "in UTC"
	}
	return "in " + loc.String()
}

// slaAlreadyDrawn stops sla_breakdown and error_budget drawing the block twice
// when a template names both.
func slaAlreadyDrawn(body []Element) bool {
	for _, e := range body {
		if h, ok := e.(Heading); ok && h.Text == "Service level" {
			return true
		}
	}
	return false
}

// comparisonSection draws the comparative block as one table.
//
// A table rather than a chart, and that is the decision. Two or three series over
// one window is four numbers a reader compares by eye in a second; a chart of
// three bars is the same four numbers with an axis nobody reads and a legend that
// has to be matched to it. The chart primitives exist and this deliberately does
// not use them.
func comparisonSection(c report.Comparison) []Element {
	heading := "Compared with the previous period"
	switch c.Mode {
	case report.CompareMonitors:
		heading = "Monitors compared"
	case report.CompareGroups:
		heading = "Groups compared"
	}

	columns := []Column{{Title: "Series"}}
	if c.Mode == report.CompareToPreviousPeriod {
		columns = append(columns, Column{Title: "Period"})
	}
	columns = append(columns,
		Column{Title: "Uptime", Numeric: true},
		Column{Title: "Observed", Numeric: true},
		Column{Title: "Failed", Numeric: true},
		Column{Title: "Avg response", Numeric: true},
	)

	table := Table{Columns: columns}
	for _, series := range c.Series {
		row := []string{series.Label}
		if c.Mode == report.CompareToPreviousPeriod {
			period := "—"
			if series.PeriodStart != nil && series.PeriodEnd != nil {
				period = formatPeriod(*series.PeriodStart, *series.PeriodEnd)
			}
			row = append(row, period)
		}
		row = append(row,
			percentOrDash(series.Uptime.Ratio),
			thousands(series.Uptime.ObservedChecks),
			thousands(series.Uptime.DownChecks),
			millis(series.ResponseTime.AverageMs),
		)
		table.Rows = append(table.Rows, row)
	}

	return []Element{
		Heading{Text: heading, Level: 1},
		// The caption earns its line: a comparison of unequal windows is the
		// commonest way one of these gets misread, and the counts are the half
		// that goes wrong while the ratios stay comparable.
		Paragraph{Muted: true, Text: comparisonNote(c)},
		table,
	}
}

func comparisonNote(c report.Comparison) string {
	if c.Mode != report.CompareToPreviousPeriod {
		return "Both series cover the same window, so the counts are directly comparable."
	}
	return "The previous period is the same length placed immediately before this one, " +
		"rather than the previous calendar period — otherwise February beside March would " +
		"put 28 days against 31 and every count would differ for reasons that are about the " +
		"calendar rather than about the service."
}

// incidentSection draws the post-mortem: the aggregate first, then the log.
//
// The aggregate leads because it is the sentence somebody quotes, and the log
// follows because it is the evidence for it. Each mean carries **how many
// incidents supplied it**, which is not decoration: "22 minutes, from one
// incident of nine" is a very different claim from "22 minutes", and a reader who
// cannot tell them apart will quote the wrong one.
func incidentSection(doc report.Document) []Element {
	out := []Element{Heading{Text: "Incidents", Level: 1}}

	summary := doc.MTT
	items := []KeyValue{{
		Key:   "Incidents",
		Value: thousands(summary.Incidents),
	}}
	for _, figure := range []struct {
		key   string
		value *int
		known int
	}{
		{"Mean time to detect", summary.MeanTimeToDetect, summary.DetectKnownCount},
		{"Mean time to acknowledge", summary.MeanTimeToAcknowledge, summary.AcknowledgeKnownCount},
		{"Mean time to resolve", summary.MeanTimeToResolve, summary.ResolveKnownCount},
	} {
		items = append(items, KeyValue{
			Key:   figure.key,
			Value: durationOrUnknown(figure.value),
			Note:  knownFrom(figure.known, summary.Incidents),
		})
	}
	out = append(out, KeyValues{Items: items})

	table := Table{Columns: []Column{
		{Title: "Incident"},
		{Title: "Started"},
		{Title: "State"},
		{Title: "To detect", Numeric: true},
		{Title: "To acknowledge", Numeric: true},
		{Title: "To resolve", Numeric: true},
	}}
	for _, in := range doc.Incidents {
		table.Rows = append(table.Rows, []string{
			in.Title,
			in.StartedAt.Format("2 Jan 15:04"),
			in.State,
			durationOrUnknown(in.MTTDSeconds),
			durationOrUnknown(in.MTTASeconds),
			durationOrUnknown(in.MTTRSeconds),
		})
	}
	return append(out, table)
}

// durationOrUnknown prints an interval, or says it is unknown.
//
// **"unknown", not a dash and not zero.** A dash reads as "none" and zero reads
// as "instant", and the commonest of the three figures here is a time-to-detect
// that genuinely is not known — `auto_opened` is never set before Phase 3, so an
// incident recorded by hand has no detection time at all. A post-mortem that
// printed 0 s there would claim the outage was noticed the moment it began.
func durationOrUnknown(seconds *int) string {
	if seconds == nil {
		return "unknown"
	}
	return duration(*seconds)
}

// knownFrom says how many incidents supplied a mean, and stays silent when every
// one of them did — a note saying "from 9 of 9" is noise on every row.
func knownFrom(known, total int) string {
	switch {
	case total == 0:
		return ""
	case known == 0:
		return "not recorded on any incident in this period"
	case known == total:
		return ""
	}
	return fmt.Sprintf("from %d of %d incidents", known, total)
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

	// The response-time SLI, but only where a target makes it one. A report with
	// no response-time target has no such indicator to describe, and a sentence
	// about a rule nobody set is noise on a page that is short on purpose.
	if ms, ok := responseTarget(doc); ok {
		fmt.Fprintf(&b, " A day is counted over target when its average response exceeded %d ms.", ms)
		b.WriteString(" A check that exceeded the configured threshold is recorded as down and is not separable, in stored history, from one that did not answer at all — so this figure reports whether the response-time target was met, and no figure here describes how the service felt to use.")
	}
	return b.String()
}

// responseTarget reports the response-time target the document was measured
// against, if it has one.
//
// The target is per-section rather than per-document because a scope can mix
// monitors, but every section of one report shares the template's value in
// practice: the first one found is the one the sentence names, and where they
// differ the per-monitor "target 250ms" note beside each figure is the
// authoritative statement. The alternative — a sentence per monitor in the
// methodology block — would put the least-read text on the page in the place
// most likely to be long.
func responseTarget(doc report.Document) (int, bool) {
	if doc.Summary != nil && doc.Summary.ResponseTime.TargetMs != nil {
		return *doc.Summary.ResponseTime.TargetMs, true
	}
	for _, s := range doc.Monitors {
		if s.ResponseTime.TargetMs != nil {
			return *s.ResponseTime.TargetMs, true
		}
	}
	return 0, false
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

	// Best and worst, and only where they are two different days. A window with
	// one observed day in it — every daily report — has a best day, a worst day
	// and a window average that are the same number printed three times under
	// three headings, which invites a reader to look for a difference that
	// cannot be there.
	if l.BestDay != nil && l.WorstDay != nil && !l.BestDay.Date.Equal(l.WorstDay.Date) {
		items = append(items,
			KeyValue{
				Key:   "Best day",
				Value: millis(l.BestDay.AverageMs),
				Note:  l.BestDay.Date.Format("2 January"),
			},
			KeyValue{
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

	// The percentile and its window, and only when there is one. An unavailable
	// figure is left out of the rendered document altogether rather than printed
	// as a dash with an explanation of itself: a client reading a PDF has no use
	// for the retention setting behind a figure they were never shown. The reason
	// survives where a consumer can act on it — `unavailable_reason` in the JSON
	// export (json.go) — so nothing is lost, it is only not narrated on the page.
	if l.P95 != nil && l.P95.Available {
		items = append(items, p95Figure(*l.P95))
	}
	return items
}

// p95Figure renders an available percentile. Callers check Available first: an
// unavailable one has no tile at all, so there is no unavailable branch here to
// keep in step with the reasons in report/latency.go.
func p95Figure(p report.P95) KeyValue {
	note := "nearest rank"
	if p.WindowStart != nil && p.WindowEnd != nil {
		note = fmt.Sprintf("nearest rank, %s — the last seven days of the period, not the whole of it",
			formatPeriod(*p.WindowStart, *p.WindowEnd))
	}
	return KeyValue{Key: "95th percentile", Value: millis(p.ValueMs), Note: note}
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
