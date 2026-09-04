package report

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// SchemaVersion is the shape of Document, and it travels inside the JSON
// artifact because that artifact is a published format. A BI tool parsing a
// stored report years from now still needs to know which shape it is holding,
// and the decision (Q4) was a field rather than an envelope.
//
// It is incremented only on a change that could break a consumer.
const SchemaVersion = 1

// Spec is what to compute: a report definition reduced to the questions this
// package can answer.
//
// Deliberately not the stored template. Persistence carries names, brand
// profiles, formats, recipients and schedules, none of which changes a number,
// and a computation that took the stored row would drag every one of those into
// the tests that check the arithmetic. The API maps one to the other.
type Spec struct {
	TemplateID   model.ID
	TemplateName string
	Type         string

	Scope Scope

	Period      string
	PeriodStyle string
	Timezone    string

	// Explicit boundaries, for PeriodCustom and for a re-run over exactly the
	// window an earlier run used — which is what makes a regenerated report
	// comparable to the one it replaces.
	From, To time.Time

	MaintenanceHandling string

	// SLATarget overrides whatever the monitors carry. Null is "use their own",
	// which is not the same as "no target".
	SLATarget            *float64
	ResponseTimeTargetMs *int

	// Tier requested, or "auto". Retention decides what actually answers and the
	// document says which.
	Tier string

	// Comparison is what a comparative template asks for. The zero value's empty
	// Mode means "no comparison", which every other report type has.
	Comparison ComparisonSpec
}

// Meta is ReportDocument.meta: everything needed to read the figures correctly.
type Meta struct {
	SchemaVersion int
	GeneratedAt   time.Time

	ReportRunID      model.ID
	ReportTemplateID model.ID
	TemplateName     string

	PeriodStart, PeriodEnd time.Time
	Timezone               string

	Resolution Resolution

	// Brand is the profile as it stood when this run executed, copied onto the
	// document rather than referenced from it.
	//
	// Denormalised deliberately. An artifact is a record of what a client was
	// sent, and a profile is an editable row: an agency that rebrands in June
	// would otherwise change what every January report *claims* it said, which
	// is the one thing an artifact exists to prevent. The copy is what makes the
	// stored JSON re-renderable years later, after the profile has been edited
	// or the client has left and the row is gone.
	//
	// Nil where the instance has no branding at all, which is the solo user's
	// case and is not a failure — the report simply has no client name on it.
	Brand *Brand
}

// Brand is the resolved branding, in the shape `meta.brand` fixes in the frozen
// spec and no wider.
//
// The accent colour is deliberately not here. The profile carries one, the spec's
// `meta.brand` does not, and inventing a field would be an API change on a
// document a BI tool binds to (AGENTS.md rule 4). It is applied at render time
// from the profile instead, which is where it is used.
//
// LogoURL is likewise absent rather than null-and-hopeful: there is no operation
// in the spec that serves a brand logo's bytes, so a URL here would name an
// endpoint that answers 405. The rendered HTML and PDF embed the logo directly,
// which is what actually makes those two artifacts standalone; the JSON is a
// data document and carries the words, not the picture.
type Brand struct {
	CompanyName   string
	PrimaryColor  string
	FooterText    string
	HidePoweredBy bool
}

// ScopeSummary is what the document covers, resolved at run time.
type ScopeSummary struct {
	MonitorCount int
	GroupIDs     []model.ID
	TagIDs       []model.ID
}

// DayUptime is one day's uptime for one monitor, at the grain the tiers store.
//
// Carried for the CSV, which promises a row per bucket per monitor and would
// otherwise emit days with a latency and no uptime beside it. The JSON artifact
// does not include it: ReportMonitorSection in the contract has no field for a
// daily uptime array, and CSV column layout is not part of the API.
type DayUptime struct {
	Date   time.Time
	Uptime Uptime
}

// HourBucket is one hour of a short window, carrying both figures the two
// per-monitor charts draw.
//
// Present only where the window is short enough that the daily series is a
// bucket or two — a daily report, or a custom window of a day or so. A strip of
// one cell and a line of one point are a picture of a number already printed
// beside them, and this is what makes those two exhibits say something.
//
// **Not in the JSON artifact, and it cannot be.** ReportResponseTimeBlock.daily
// is one point per day typed `format: date`; twenty-four hours of one day would
// be twenty-four points carrying the same date, which is not a finer reading of
// that field but a different field. The hourly series is a drawing on the
// rendered page, and the published document keeps the grain its contract fixes.
type HourBucket struct {
	Start  time.Time
	Uptime Uptime

	// AverageMs is nil for an hour with no successful check, which draws as a
	// gap rather than as zero — exactly as a day does.
	AverageMs   *float64
	SampleCount int
}

// HourlySeriesMaxWindow is the longest window that gets an hourly series.
//
// Two days rather than one. The complaint the hourly series answers is a chart
// of one or two buckets, and a two-day report has that complaint just as a
// one-day report does; beyond it the daily series is a chart in its own right
// and forty-nine hourly cells would be a denser drawing of the same shape.
//
// It is also what keeps this off the runs the load gate measures: forty-eight
// rows per monitor is less than the daily series of a two-month window, and a
// monthly report never reaches this branch at all.
const HourlySeriesMaxWindow = 48 * time.Hour

// MonitorSection is one monitor's figures.
type MonitorSection struct {
	MonitorID model.ID
	Name      string
	Type      string
	GroupID   *model.ID

	Uptime       Uptime
	DailyUptime  []DayUptime
	Hourly       []HourBucket
	SLA          *SLA
	ResponseTime Latency
	Breaches     []Breach
}

// Estate is the summary block across every monitor in scope.
//
// It carries uptime and latency and deliberately no SLA: targets differ per
// monitor, and an estate-wide "target vs actual" would either pick one monitor's
// number for everybody or invent an average of targets. Neither is a fact.
type Estate struct {
	Uptime       Uptime
	ResponseTime Latency
}

// Document is the computed report. Every renderer consumes this — HTML, PDF, CSV
// and JSON are siblings over one model (ADR-007) — and it is the JSON artifact
// verbatim.
type Document struct {
	Meta     Meta
	Scope    ScopeSummary
	Summary  *Estate
	Monitors []MonitorSection

	// Incidents are the ones overlapping the window, with their MTT* intervals
	// computed. Present on every type that asks for an incident log, and the
	// substance of a post_mortem.
	Incidents []Incident

	// MTT is the aggregate across those incidents. Zero-valued where there are
	// none, and each mean is nil where no incident supplied that figure — never
	// zero, because averaging an unknown as zero drags the mean towards zero in
	// proportion to how much is unknown.
	MTT MTTSummary

	// Comparison is present for a comparative report and nil for every other
	// type, which is the spec's own shape.
	Comparison *Comparison
}

// Build computes a report.
//
// Four reads, regardless of how many monitors are in scope: the scope
// resolution, the window totals, the daily series, and the targets. That is the
// property the extended load gate is going to measure, and it is why the store
// methods are batched — a fan-out per monitor here would be fifty concurrent
// runs each issuing thousands of queries during the burst on the first of the
// month.
//
// now is passed rather than read from the clock so that a run is reproducible:
// ADR-007 requires the same model rendered twice to be byte-identical, and a
// generated_at taken from time.Now would break that before the renderer got a
// chance to.
func Build(ctx context.Context, s Store, spec Spec, retention Retention, runID model.ID, now time.Time) (Document, error) {
	window, err := resolveSpecWindow(spec, now)
	if err != nil {
		return Document{}, err
	}

	monitors, err := s.MonitorsInScope(ctx, spec.Scope)
	if err != nil {
		return Document{}, fmt.Errorf("resolve scope: %w", err)
	}

	res := ResolveTier(spec.Tier, window, now, retention)

	doc := Document{
		Meta: Meta{
			SchemaVersion:    SchemaVersion,
			GeneratedAt:      now,
			ReportRunID:      runID,
			ReportTemplateID: spec.TemplateID,
			TemplateName:     spec.TemplateName,
			PeriodStart:      window.From,
			PeriodEnd:        window.To,
			Timezone:         window.Timezone,
			Resolution:       res,
		},
		Scope: ScopeSummary{
			MonitorCount: len(monitors),
			GroupIDs:     spec.Scope.GroupIDs,
			TagIDs:       spec.Scope.TagIDs,
		},
	}
	if len(monitors) == 0 {
		// An empty scope is a document, not an error. A client whose monitors
		// were all deleted still gets a report saying so, which is a great deal
		// better than a failed run nobody looks at until the invoice goes out.
		return doc, nil
	}

	ids := make([]model.ID, len(monitors))
	for i, m := range monitors {
		ids[i] = m.ID
	}

	// The window the figures are read over is the covered window, not the
	// requested one. Where retention has truncated the start, asking for the
	// full range would return the same rows and label them with a period the
	// data does not reach.
	from := window.From
	if res.CoveredFrom != nil && res.CoveredFrom.After(from) {
		from = *res.CoveredFrom
	}

	totals, err := s.WindowTotals(ctx, ids, from, window.To, res.Tier)
	if err != nil {
		return Document{}, fmt.Errorf("window totals: %w", err)
	}
	series, err := s.DailySeries(ctx, ids, from, window.To)
	if err != nil {
		return Document{}, fmt.Errorf("daily series: %w", err)
	}

	// The fifth read, and only for a window the daily series cannot draw. Every
	// other report — including every scheduled monthly one — takes the four
	// above and no more.
	var hourly map[model.ID][]store.HistoryBucket
	if wantsHourlySeries(window, res) {
		hourly, err = s.HourlySeries(ctx, ids, from, window.To)
		if err != nil {
			return Document{}, fmt.Errorf("hourly series: %w", err)
		}
	}
	targets, err := s.SLOTargets(ctx, ids)
	if err != nil {
		return Document{}, fmt.Errorf("slo targets: %w", err)
	}

	length := window.To.Sub(from)
	for _, m := range monitors {
		section := MonitorSection{
			MonitorID: m.ID,
			Name:      m.Name,
			Type:      string(m.Type),
			GroupID:   m.GroupID,
		}

		total := totals[m.ID] // zero value for a monitor with no buckets, which
		daily := series[m.ID] // computes to a null ratio rather than to zero

		section.Uptime = ComputeUptime(total, spec.MaintenanceHandling)
		for _, b := range daily {
			section.DailyUptime = append(section.DailyUptime, DayUptime{
				Date:   b.Start,
				Uptime: ComputeUptime(b, spec.MaintenanceHandling),
			})
		}
		for _, b := range hourly[m.ID] {
			point := HourBucket{
				Start:       b.Start,
				Uptime:      ComputeUptime(b, spec.MaintenanceHandling),
				SampleCount: b.ResponseTimeCount,
			}
			if b.ResponseTimeCount > 0 {
				avg := b.ResponseTimeSum / float64(b.ResponseTimeCount)
				point.AverageMs = &avg
			}
			section.Hourly = append(section.Hourly, point)
		}
		section.ResponseTime = ComputeLatency(total, daily, spec.ResponseTimeTargetMs)
		section.Breaches = ComputeBreaches(daily, spec.MaintenanceHandling)

		// **An uptime report carries no SLO vocabulary at all**, even where the
		// monitors in scope have targets. That is what makes it "the default a
		// solo user gets", and it has a second use worth stating: an agency
		// running an uptime summary for a client does not put the internal
		// target it set on that client's monitors onto the client's document.
		// Choosing the type is the choice; a target set for the agency's own
		// dashboards is not a decision to publish it.
		if target, ok := resolveTarget(spec, targets, m.ID); ok && spec.Type != model.ReportTypeUptime {
			// The same total the breach table above shows, so the two cannot
			// disagree on the face of one document.
			sla := ComputeSLA(section.Uptime, target, length,
				DowntimeSeconds(daily, spec.MaintenanceHandling))
			section.SLA = &sla
		}
		doc.Monitors = append(doc.Monitors, section)
	}

	if err := attachP95(ctx, s, doc.Monitors, retention, window); err != nil {
		return Document{}, err
	}

	summary := Estate{
		Uptime:       ComputeUptime(Sum(collect(totals, ids)), spec.MaintenanceHandling),
		ResponseTime: ComputeLatency(Sum(collect(totals, ids)), mergeDaily(series), spec.ResponseTimeTargetMs),
	}
	// The estate block carries no percentile and its P95 stays nil — an absent
	// object rather than a present one reporting itself unavailable. A quantile
	// merges no better across monitors than it does across time, so there is no
	// reason to give: the figure does not exist rather than being withheld.
	doc.Summary = &summary

	if err := attachIncidents(ctx, s, &doc, spec, window); err != nil {
		return Document{}, err
	}

	if spec.Comparison.Mode != "" {
		comparison, err := BuildComparison(ctx, s, spec.Comparison, spec.Scope, window, res.Tier, spec.MaintenanceHandling)
		if err != nil {
			return Document{}, err
		}
		doc.Comparison = comparison
	}

	return doc, nil
}

// wantsHourlySeries decides whether the hourly exhibit is worth the extra read.
//
// Two conditions, and the second is not a refinement of the first. A window
// short enough to want hourly cells can still have no hourly data behind it: if
// the daily tier is what answered, retention no longer holds anything finer for
// this period, and asking would return an empty map and draw nothing. Skipping
// the query says the same thing without the round trip.
func wantsHourlySeries(w Window, res Resolution) bool {
	d := w.Duration()
	return d > 0 && d <= HourlySeriesMaxWindow && res.Tier != "1d"
}

// IncidentPageSize bounds the incident log.
//
// A window with more incidents than this has a problem a report is not going to
// fix, and a document listing five hundred of them is one nobody reads. The
// listing is the newest page rather than a truncated head, because the recent
// ones are the ones a post-mortem is about.
const IncidentPageSize = 100

// attachIncidents fills the incident log and the MTT* summary.
//
// Only for the types that want one, and that is a cost decision rather than a
// stylistic one: it is a fifth read, and the four-reads-whatever-the-scope
// property the load gate measures is worth keeping for the two types that make
// up almost every scheduled report.
func attachIncidents(ctx context.Context, s Store, doc *Document, spec Spec, window Window) error {
	switch spec.Type {
	case model.ReportTypePostMortem, model.ReportTypeCustom:
	default:
		return nil
	}

	incidents, _, err := s.ListIncidents(ctx, nil, IncidentPageSize, store.IncidentFilter{
		From: &window.From,
		To:   &window.To,
	})
	if err != nil {
		return fmt.Errorf("incident log: %w", err)
	}

	// The alert counts are nil, which reports every incident's alerts_fired as
	// unknown rather than as zero. The delivery log is not on this package's
	// read-side contract and adding a method for one report type would put it in
	// front of every consumer — and **unknown is the honest answer** in the
	// meantime: zero would read as "nobody was told", which is one of the more
	// serious findings a post-mortem can carry and not one a missing query
	// should be able to manufacture.
	doc.Incidents = PostMortem(incidents, nil, nil)
	doc.MTT = Summarise(doc.Incidents)
	return nil
}

// P95MaxMonitors bounds the one figure in the document that cannot be batched.
//
// Everything else Build reads costs four queries however large the scope is. A
// nearest-rank percentile is a rank statistic over raw heartbeats, so it costs
// one query per monitor over roughly ten thousand rows each — at five thousand
// monitors that is fifty million rows ranked to produce a block that sits beside
// figures which took four queries to compute.
//
// Twenty-five is the size of a client report, which is the case that wants this
// figure. An estate-wide report gets the block with scope_too_large on it, which
// says what happened rather than leaving the reader to wonder.
const P95MaxMonitors = 25

// attachP95 fills the trailing-seven-day percentile, or says why it did not.
//
// The window is the last seven days **of the reported period**, not of the
// present moment: a March report generated in April describes March, and a
// percentile drawn from April would be a figure about a month the document is
// not about. It is still labelled with its own window, because seven days beside
// a thirty-day average reads as a contradiction otherwise.
func attachP95(ctx context.Context, s Store, sections []MonitorSection, retention Retention, window Window) error {
	from := window.To.AddDate(0, 0, -7)
	if from.Before(window.From) {
		// A window shorter than a week: the percentile covers the window rather
		// than reaching outside it for days the report does not describe.
		from = window.From
	}

	// Two gates that need no query at all, checked first for that reason.
	switch {
	case len(sections) > P95MaxMonitors:
		for i := range sections {
			sections[i].ResponseTime.P95 = &P95{Reason: ReasonScopeTooLarge}
		}
		return nil
	case !retention.RawCoversTrailingWeek():
		for i := range sections {
			sections[i].ResponseTime.P95 = &P95{Reason: ReasonInsufficientRaw}
		}
		return nil
	}

	for i := range sections {
		id := sections[i].MonitorID

		// Compared against the daily tier rather than asked in the absolute:
		// RawCovers answers "does raw reach at least as far back as the tier",
		// which is false exactly when retention has pruned raw rows the tier
		// summarised — the case ADR-006 gates against. A monitor created three
		// days ago passes, correctly: there is no older data to be missing.
		covered, err := s.RawCovers(ctx, id, from, "1d")
		if err != nil {
			return fmt.Errorf("raw coverage for %s: %w", id, err)
		}

		var value *float64
		if covered {
			b, err := s.UptimeFromRaw(ctx, id, from, window.To)
			if err != nil {
				return fmt.Errorf("trailing percentile for %s: %w", id, err)
			}
			value = b.ResponseTimeP95
		}
		sections[i].ResponseTime.P95 = TrailingP95(covered, value, from, window.To)
	}
	return nil
}

// resolveSpecWindow prefers explicit boundaries over a named period, so that a
// re-run covers exactly what the original run covered. Regenerating "last month"
// in July and getting June would make a correction incomparable to the thing it
// corrects.
func resolveSpecWindow(spec Spec, now time.Time) (Window, error) {
	loc := time.UTC
	if spec.Timezone != "" {
		var err error
		if loc, err = time.LoadLocation(spec.Timezone); err != nil {
			return Window{}, fmt.Errorf("timezone %q: %w", spec.Timezone, err)
		}
	}

	if !spec.From.IsZero() && !spec.To.IsZero() {
		if !spec.From.Before(spec.To) {
			return Window{}, fmt.Errorf("window starts at or after it ends: %s to %s", spec.From, spec.To)
		}
		return Window{From: spec.From, To: spec.To, Timezone: loc.String()}, nil
	}
	return ResolveWindow(spec.Period, spec.PeriodStyle, loc, now)
}

// resolveTarget applies the precedence the spec fixes: the template's own
// target, then the monitor's, then its group's, then none — with the level that
// answered carried onto the figure.
func resolveTarget(spec Spec, stored map[model.ID]Target, id model.ID) (Target, bool) {
	if spec.SLATarget != nil {
		return Target{Percent: *spec.SLATarget, Source: TargetFromTemplate}, true
	}
	target, ok := stored[id]
	return target, ok
}

func collect(totals map[model.ID]store.HistoryBucket, ids []model.ID) []store.HistoryBucket {
	out := make([]store.HistoryBucket, 0, len(ids))
	for _, id := range ids {
		if b, ok := totals[id]; ok {
			out = append(out, b)
		}
	}
	return out
}

// mergeDaily folds every monitor's series into one series by date, for the
// estate's daily latency exhibit.
//
// Summed rather than averaged, for the reason that governs every figure in this
// package: counts are additive and means are not, so a busy monitor and a quiet
// one contribute in proportion to the checks they actually made.
func mergeDaily(series map[model.ID][]store.HistoryBucket) []store.HistoryBucket {
	byDay := map[int64]store.HistoryBucket{}
	for _, buckets := range series {
		for _, b := range buckets {
			key := b.Start.UnixMilli()
			merged := Sum([]store.HistoryBucket{byDay[key], b})
			merged.Start = b.Start
			byDay[key] = merged
		}
	}

	out := make([]store.HistoryBucket, 0, len(byDay))
	for _, b := range byDay {
		out = append(out, b)
	}
	// Map iteration is randomised, and ADR-007 requires the same model rendered
	// twice to be byte-identical. Sorting here is what makes that true of the
	// estate series.
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}
