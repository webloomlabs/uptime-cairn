package render

import "github.com/webloomlabs/uptime-cairn/internal/model"

// Which blocks a document emits, and in what order.
//
// # Selection is what makes `custom` a builder rather than a synonym
//
// `sections` was stored and round-tripped long before it did anything, so a
// custom template rendered the whole document regardless of what it selected.
// This is the half that was missing.
//
// # Order is honoured, because the spec says "ordered content blocks"
//
// Treating the list as a set would have been simpler and would have quietly
// narrowed the contract. So the blocks come out in the order they were named —
// with one structural consequence worth stating, because it is the only part of
// this that is not literal.
//
// Five of the ten sections are **per monitor** (`uptime_table`, `uptime_chart`,
// `response_time`, `sla_breakdown`, `error_budget`) and the rest are
// document-level. A monitor's blocks cannot be scattered across the document
// without repeating its heading for each one, which would turn a three-monitor
// report into fifteen headings. So the per-monitor group is emitted **once, at
// the position of the first per-monitor section named**, and within each monitor
// its blocks appear in the order they were named. `[summary, response_time,
// uptime_table]` therefore gives a summary, then each monitor with its response
// time above its uptime table — which is what was asked for — rather than each
// monitor twice.
//
// # An empty selection means the defaults, never an empty report
//
// Both because that is what the spec says and because the alternative is a
// document with a cover, a footer and nothing between them, produced for every
// template written before this existed.

// perMonitor is the subset of sections that render inside a monitor's own block.
var perMonitor = map[string]bool{
	model.SectionUptimeTable:  true,
	model.SectionUptimeChart:  true,
	model.SectionResponseTime: true,
	model.SectionSLABreakdown: true,
	model.SectionErrorBudget:  true,
}

// layout is the resolved answer to "what does this document contain".
type layout struct {
	// order is the document-level sequence, with the sentinel monitorBlock
	// standing in for the per-monitor group.
	order []string

	// within is the per-monitor sequence.
	within []string

	selected map[string]bool
}

// monitorBlock marks where the per-monitor group sits in the document order. Not
// a section name and never valid in a template: it is a position, not a choice.
const monitorBlock = "\x00monitors"

// has reports whether a block was selected.
func (l layout) has(section string) bool { return l.selected[section] }

// resolveLayout turns a template's selection into a layout.
//
// Unknown names are dropped rather than rejected. Validation belongs at the API
// boundary and happens there; by the time a document is being composed the run
// has already been queued, and failing it over a section name would turn a
// stored typo into a report that does not arrive — where dropping it produces a
// report missing one block, which is visible and fixable.
//
// A selection that survives to nothing falls back to the defaults for the same
// reason: a cover and a footer with nothing between them is not a better answer
// than the report somebody was expecting.
func resolveLayout(sections []string, doc reportShape) layout {
	l := layout{selected: map[string]bool{}}

	var kept []string
	for _, s := range sections {
		if !model.ValidSection(s) || l.selected[s] {
			continue
		}
		l.selected[s] = true
		kept = append(kept, s)
	}
	if len(kept) == 0 {
		return defaultLayout(doc)
	}

	placedMonitors := false
	for _, s := range kept {
		if perMonitor[s] {
			l.within = append(l.within, s)
			if !placedMonitors {
				l.order = append(l.order, monitorBlock)
				placedMonitors = true
			}
			continue
		}
		l.order = append(l.order, s)
	}
	return l
}

// reportShape is what the defaults need to know about a document, which is only
// whether the optional blocks have anything in them.
type reportShape struct {
	hasSummary    bool
	hasComparison bool
	hasIncidents  bool
}

// defaultLayout is the document as it composed before selection existed.
//
// **Byte-for-byte the previous behaviour**, which is the property that matters:
// every template that has never named a section — which is every template on
// every install today — must render exactly what it rendered yesterday. The
// tests assert that against a template with an empty selection rather than
// trusting this comment.
func defaultLayout(doc reportShape) layout {
	l := layout{
		selected: map[string]bool{
			model.SectionUptimeChart:  true,
			model.SectionUptimeTable:  true,
			model.SectionSLABreakdown: true,
			model.SectionErrorBudget:  true,
			model.SectionResponseTime: true,
		},
		within: []string{
			model.SectionUptimeChart,
			model.SectionUptimeTable,
			model.SectionSLABreakdown,
			model.SectionResponseTime,
		},
	}
	if doc.hasSummary {
		l.selected[model.SectionSummary] = true
		l.order = append(l.order, model.SectionSummary)
	}
	l.order = append(l.order, monitorBlock)
	if doc.hasComparison {
		l.selected[model.SectionComparison] = true
		l.order = append(l.order, model.SectionComparison)
	}
	if doc.hasIncidents {
		l.selected[model.SectionIncidentLog] = true
		l.order = append(l.order, model.SectionIncidentLog)
	}
	return l
}
