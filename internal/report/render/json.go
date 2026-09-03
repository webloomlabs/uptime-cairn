// Package render turns a computed report into the formats a client receives.
//
// ADR-007 item 1: renderers are siblings, not a chain. Each function here
// consumes report.Document; none consumes another's output, and a PDF is never
// produced by converting HTML. That is the decision that keeps this phase from
// becoming an HTML-to-PDF project, and it is why these files share a model
// rather than a pipeline.
//
// The wire shapes are hand-written against docs/api/openapi.yaml, following
// internal/api/dto.go and its reasoning: a small hand-written struct is easier
// to read against the spec than a generated one, and the contract tests are what
// prove the two agree.
package render

import (
	"encoding/json"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
)

// dateOnly is the `format: date` the daily series and the over-target dates use.
const dateOnly = "2006-01-02"

type documentJSON struct {
	Meta       metaJSON             `json:"meta"`
	Scope      scopeJSON            `json:"scope"`
	Summary    *estateJSON          `json:"summary"`
	Monitors   []monitorSectionJSON `json:"monitors"`
	Incidents  []incidentJSON       `json:"incidents"`
	Comparison *comparisonJSON      `json:"comparison"`
}

type metaJSON struct {
	SchemaVersion    int            `json:"schema_version"`
	GeneratedAt      time.Time      `json:"generated_at"`
	ReportRunID      string         `json:"report_run_id"`
	ReportTemplateID string         `json:"report_template_id"`
	TemplateName     string         `json:"template_name"`
	PeriodStart      time.Time      `json:"period_start"`
	PeriodEnd        time.Time      `json:"period_end"`
	Timezone         string         `json:"timezone"`
	Resolution       resolutionJSON `json:"resolution"`

	// Null on an unbranded instance rather than an empty object, because the
	// spec types the field as nullable and "there is no branding" and "there is
	// branding with nothing in it" are different answers to a consumer.
	Brand *brandJSON `json:"brand"`
}

// brandJSON is `meta.brand` exactly as the spec fixes it.
//
// logo_url is emitted as null and stays null: the spec defines the field and
// defines no operation that serves the bytes — PUT .../logo exists with no GET
// beside it — so a URL here would name an endpoint answering 405. It is the same
// gap BrandProfile.logo_url has, recorded rather than invented around.
type brandJSON struct {
	CompanyName   *string `json:"company_name"`
	PrimaryColor  *string `json:"primary_color"`
	FooterText    *string `json:"footer_text"`
	LogoURL       *string `json:"logo_url"`
	HidePoweredBy bool    `json:"hide_powered_by"`
}

type resolutionJSON struct {
	Tier          string     `json:"tier"`
	RequestedTier *string    `json:"requested_tier"`
	Downgraded    bool       `json:"downgraded"`
	CoveredFrom   *time.Time `json:"covered_from"`
}

type scopeJSON struct {
	MonitorCount int      `json:"monitor_count"`
	GroupIDs     []string `json:"group_ids"`
	TagIDs       []string `json:"tag_ids"`
}

type estateJSON struct {
	Uptime       uptimeJSON       `json:"uptime"`
	ResponseTime responseTimeJSON `json:"response_time"`
}

type monitorSectionJSON struct {
	MonitorID    string           `json:"monitor_id"`
	Name         string           `json:"name"`
	Type         string           `json:"type"`
	GroupID      *string          `json:"group_id"`
	Uptime       uptimeJSON       `json:"uptime"`
	SLA          *slaJSON         `json:"sla"`
	ResponseTime responseTimeJSON `json:"response_time"`
}

type uptimeJSON struct {
	// A pointer, and the whole reason the denominator rules exist: null means
	// nothing was observed. Zero would claim total downtime.
	UptimeRatio         *float64 `json:"uptime_ratio"`
	MaintenanceHandling string   `json:"maintenance_handling"`
	Denominator         string   `json:"denominator"`
	ObservedChecks      int      `json:"observed_checks"`
	UpChecks            int      `json:"up_checks"`
	DownChecks          int      `json:"down_checks"`
	MaintenanceChecks   int      `json:"maintenance_checks"`
	UnknownChecks       int      `json:"unknown_checks"`
	SkippedChecks       int      `json:"skipped_checks"`
	UnobservedShare     *float64 `json:"unobserved_share"`
}

type slaJSON struct {
	TargetPercent               float64      `json:"target_percent"`
	TargetSource                string       `json:"target_source"`
	ActualPercent               *float64     `json:"actual_percent"`
	Met                         *bool        `json:"met"`
	ErrorBudgetSeconds          int          `json:"error_budget_seconds"`
	ErrorBudgetConsumedSeconds  int          `json:"error_budget_consumed_seconds"`
	ErrorBudgetRemainingSeconds int          `json:"error_budget_remaining_seconds"`
	ErrorBudgetConsumedRatio    *float64     `json:"error_budget_consumed_ratio"`
	BurnRate                    *float64     `json:"burn_rate"`
	Breaches                    []breachJSON `json:"breaches"`
}

type breachJSON struct {
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at"`
	DurationSeconds int        `json:"duration_seconds"`
	IncidentID      *string    `json:"incident_id"`
}

type responseTimeJSON struct {
	AverageMs       *float64     `json:"average_ms"`
	SampleCount     int          `json:"sample_count"`
	Daily           []dayJSON    `json:"daily"`
	BestDay         *dayPeakJSON `json:"best_day"`
	WorstDay        *dayPeakJSON `json:"worst_day"`
	TargetMs        *int         `json:"target_ms"`
	DaysOverTarget  *int         `json:"days_over_target"`
	DatesOverTarget []string     `json:"dates_over_target"`
	P95             *p95JSON     `json:"p95"`
}

type dayJSON struct {
	Date        string   `json:"date"`
	AverageMs   *float64 `json:"average_ms"`
	SampleCount int      `json:"sample_count"`
}

type dayPeakJSON struct {
	Date      string  `json:"date"`
	AverageMs float64 `json:"average_ms"`
}

type p95JSON struct {
	Available         bool       `json:"available"`
	ValueMs           *float64   `json:"value_ms"`
	WindowStart       *time.Time `json:"window_start"`
	WindowEnd         *time.Time `json:"window_end"`
	Method            *string    `json:"method"`
	UnavailableReason *string    `json:"unavailable_reason"`
}

// incidentJSON is ReportIncident, and the three null-able intervals are the
// whole point of the shape.
//
// **mttd_seconds is frequently null and is reported as unknown rather than
// inferred.** `auto_opened` is never set before Phase 3, so an incident recorded
// by hand has no detection time, and a post-mortem that treated `started_at` as
// the detection would report a time-to-detect of zero on an outage nobody noticed
// for forty minutes — a confident wrong number in a document written for the
// people who were affected.
//
// alerts_fired distinguishes null from zero for the same class of reason. Zero
// means the delivery log covers this incident and holds nothing, which reads as
// *nobody was told* and is one of the more serious findings a post-mortem can
// carry; null means the rows have been swept. A retention policy must not be able
// to manufacture that finding.
type incidentJSON struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	State          string     `json:"state"`
	Impact         string     `json:"impact"`
	StartedAt      time.Time  `json:"started_at"`
	DetectedAt     *time.Time `json:"detected_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	AutoOpened     bool       `json:"auto_opened"`
	MonitorIDs     []string   `json:"monitor_ids"`
	MTTDSeconds    *int       `json:"mttd_seconds"`
	MTTASeconds    *int       `json:"mtta_seconds"`
	MTTRSeconds    *int       `json:"mttr_seconds"`
	AlertsFired    *int       `json:"alerts_fired"`
}

// comparisonJSON is the comparative block, present for that type and null for
// every other — the spec's own shape.
type comparisonJSON struct {
	Mode   string             `json:"mode"`
	Series []comparisonSeries `json:"series"`
}

type comparisonSeries struct {
	Label        string           `json:"label"`
	PeriodStart  *time.Time       `json:"period_start"`
	PeriodEnd    *time.Time       `json:"period_end"`
	Uptime       uptimeJSON       `json:"uptime"`
	ResponseTime responseTimeJSON `json:"response_time"`
}

// JSON renders the document as the published JSON artifact.
//
// The artifact **is** ReportDocument, verbatim and with no envelope (spec
// decision Q4). `meta.schema_version` inside it is what a BI tool reads years
// from now to know which shape it is holding — a field rather than a wrapper,
// because a wrapper costs every consumer an unwrapping step forever.
//
// Indented, because this is a file somebody downloads and opens rather than a
// response on a hot path, and a diff between two months of one client's reports
// is a thing an agency will actually do.
//
// Deterministic: struct field order is fixed, every slice is ordered upstream,
// and nothing here consults the clock. ADR-007 item 6 requires the same model
// rendered twice to be byte-identical, and for this format that is the whole of
// the requirement.
func JSON(doc report.Document) ([]byte, error) {
	out := documentJSON{
		Meta: metaJSON{
			SchemaVersion:    doc.Meta.SchemaVersion,
			GeneratedAt:      doc.Meta.GeneratedAt,
			ReportRunID:      doc.Meta.ReportRunID.String(),
			ReportTemplateID: doc.Meta.ReportTemplateID.String(),
			TemplateName:     doc.Meta.TemplateName,
			PeriodStart:      doc.Meta.PeriodStart,
			PeriodEnd:        doc.Meta.PeriodEnd,
			Timezone:         doc.Meta.Timezone,
			Resolution: resolutionJSON{
				Tier:          doc.Meta.Resolution.Tier,
				RequestedTier: emptyToNil(doc.Meta.Resolution.RequestedTier),
				Downgraded:    doc.Meta.Resolution.Downgraded,
				CoveredFrom:   doc.Meta.Resolution.CoveredFrom,
			},
			Brand: brandFor(doc.Meta.Brand),
		},
		Scope: scopeJSON{
			MonitorCount: doc.Scope.MonitorCount,
			GroupIDs:     ids(doc.Scope.GroupIDs),
			TagIDs:       ids(doc.Scope.TagIDs),
		},
		// Never null: a report with no monitors renders an empty array, because
		// a consumer iterating a list should not have to special-case the empty
		// client.
		Monitors:  make([]monitorSectionJSON, 0, len(doc.Monitors)),
		Incidents: make([]incidentJSON, 0, len(doc.Incidents)),
	}

	if doc.Summary != nil {
		out.Summary = &estateJSON{
			Uptime:       uptimeToJSON(doc.Summary.Uptime),
			ResponseTime: latencyToJSON(doc.Summary.ResponseTime),
		}
	}

	for _, s := range doc.Monitors {
		section := monitorSectionJSON{
			MonitorID:    s.MonitorID.String(),
			Name:         s.Name,
			Type:         s.Type,
			GroupID:      idPtr(s.GroupID),
			Uptime:       uptimeToJSON(s.Uptime),
			ResponseTime: latencyToJSON(s.ResponseTime),
		}
		if s.SLA != nil {
			sla := slaJSON{
				TargetPercent:               s.SLA.TargetPercent,
				TargetSource:                s.SLA.TargetSource,
				ActualPercent:               s.SLA.ActualPercent,
				Met:                         s.SLA.Met,
				ErrorBudgetSeconds:          s.SLA.ErrorBudgetSeconds,
				ErrorBudgetConsumedSeconds:  s.SLA.ErrorBudgetConsumedSeconds,
				ErrorBudgetRemainingSeconds: s.SLA.ErrorBudgetRemainingSeconds,
				ErrorBudgetConsumedRatio:    s.SLA.ErrorBudgetConsumedRatio,
				BurnRate:                    s.SLA.BurnRate,
				Breaches:                    make([]breachJSON, 0, len(s.Breaches)),
			}
			for _, b := range s.Breaches {
				end := b.EndedAt
				sla.Breaches = append(sla.Breaches, breachJSON{
					StartedAt:       b.StartedAt,
					EndedAt:         &end,
					DurationSeconds: b.DurationSeconds,
					IncidentID:      idPtr(b.IncidentID),
				})
			}
			section.SLA = &sla
		}
		out.Monitors = append(out.Monitors, section)
	}

	for _, inc := range doc.Incidents {
		out.Incidents = append(out.Incidents, incidentJSON{
			ID:             inc.ID.String(),
			Title:          inc.Title,
			State:          inc.State,
			Impact:         inc.Impact,
			StartedAt:      inc.StartedAt,
			DetectedAt:     inc.DetectedAt,
			AcknowledgedAt: inc.AcknowledgedAt,
			ResolvedAt:     inc.ResolvedAt,
			AutoOpened:     inc.AutoOpened,
			MonitorIDs:     ids(inc.MonitorIDs),
			MTTDSeconds:    inc.MTTDSeconds,
			MTTASeconds:    inc.MTTASeconds,
			MTTRSeconds:    inc.MTTRSeconds,
			AlertsFired:    inc.AlertsFired,
		})
	}

	if doc.Comparison != nil {
		block := comparisonJSON{
			Mode:   doc.Comparison.Mode,
			Series: make([]comparisonSeries, 0, len(doc.Comparison.Series)),
		}
		for _, series := range doc.Comparison.Series {
			block.Series = append(block.Series, comparisonSeries{
				Label:        series.Label,
				PeriodStart:  series.PeriodStart,
				PeriodEnd:    series.PeriodEnd,
				Uptime:       uptimeToJSON(series.Uptime),
				ResponseTime: latencyToJSON(series.ResponseTime),
			})
		}
		out.Comparison = &block
	}

	return json.MarshalIndent(out, "", "  ")
}

func uptimeToJSON(u report.Uptime) uptimeJSON {
	return uptimeJSON{
		UptimeRatio:         u.Ratio,
		MaintenanceHandling: u.MaintenanceHandling,
		// Observed checks, not wall clock. §4.3 requires the denominator to be
		// stated on the report rather than assumed, and this is where a machine
		// reader finds it.
		Denominator:       "observed_checks",
		ObservedChecks:    u.ObservedChecks,
		UpChecks:          u.UpChecks,
		DownChecks:        u.DownChecks,
		MaintenanceChecks: u.MaintenanceChecks,
		UnknownChecks:     u.UnknownChecks,
		SkippedChecks:     u.SkippedChecks,
		UnobservedShare:   u.UnobservedShare,
	}
}

func latencyToJSON(l report.Latency) responseTimeJSON {
	out := responseTimeJSON{
		AverageMs:       l.AverageMs,
		SampleCount:     l.SampleCount,
		Daily:           make([]dayJSON, 0, len(l.Daily)),
		TargetMs:        l.TargetMs,
		DaysOverTarget:  l.DaysOverTarget,
		DatesOverTarget: make([]string, 0, len(l.DatesOverTarget)),
	}
	for _, d := range l.Daily {
		out.Daily = append(out.Daily, dayJSON{
			Date:        d.Date.Format(dateOnly),
			AverageMs:   d.AverageMs,
			SampleCount: d.SampleCount,
		})
	}
	for _, d := range l.DatesOverTarget {
		out.DatesOverTarget = append(out.DatesOverTarget, d.Format(dateOnly))
	}
	out.BestDay = peak(l.BestDay)
	out.WorstDay = peak(l.WorstDay)

	if l.P95 != nil {
		p := p95JSON{
			Available:   l.P95.Available,
			ValueMs:     l.P95.ValueMs,
			WindowStart: l.P95.WindowStart,
			WindowEnd:   l.P95.WindowEnd,
		}
		p.Method = emptyToNil(l.P95.Method)
		p.UnavailableReason = emptyToNil(l.P95.Reason)
		out.P95 = &p
	}
	return out
}

func peak(d *report.DayLatency) *dayPeakJSON {
	if d == nil || d.AverageMs == nil {
		return nil
	}
	return &dayPeakJSON{Date: d.Date.Format(dateOnly), AverageMs: *d.AverageMs}
}

func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ids(in []model.ID) []string {
	out := make([]string, 0, len(in))
	for _, id := range in {
		out = append(out, id.String())
	}
	return out
}

func idPtr(id *model.ID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// brandFor renders the denormalised profile, or null where there is none.
func brandFor(b *report.Brand) *brandJSON {
	if b == nil {
		return nil
	}
	return &brandJSON{
		CompanyName:   emptyToNil(b.CompanyName),
		PrimaryColor:  emptyToNil(b.PrimaryColor),
		FooterText:    emptyToNil(b.FooterText),
		HidePoweredBy: b.HidePoweredBy,
	}
}
