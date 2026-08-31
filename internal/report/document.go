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
}

// ScopeSummary is what the document covers, resolved at run time.
type ScopeSummary struct {
	MonitorCount int
	GroupIDs     []model.ID
	TagIDs       []model.ID
}

// MonitorSection is one monitor's figures.
type MonitorSection struct {
	MonitorID model.ID
	Name      string
	Type      string
	GroupID   *model.ID

	Uptime       Uptime
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
	Meta      Meta
	Scope     ScopeSummary
	Summary   *Estate
	Monitors  []MonitorSection
	Incidents []model.Incident
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
		section.ResponseTime = ComputeLatency(total, daily, spec.ResponseTimeTargetMs)
		section.Breaches = ComputeBreaches(daily, spec.MaintenanceHandling)

		if target, ok := resolveTarget(spec, targets, m.ID); ok {
			sla := ComputeSLA(section.Uptime, target, length)
			section.SLA = &sla
		}
		doc.Monitors = append(doc.Monitors, section)
	}

	summary := Estate{
		Uptime:       ComputeUptime(Sum(collect(totals, ids)), spec.MaintenanceHandling),
		ResponseTime: ComputeLatency(Sum(collect(totals, ids)), mergeDaily(series), spec.ResponseTimeTargetMs),
	}
	doc.Summary = &summary

	return doc, nil
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
