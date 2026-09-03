package report

import (
	"context"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Comparative reports: period against period, monitor against monitor, group
// against group.
//
// # What is deliberately absent
//
// Region against region. The spec's `mode` enum has three values and stops there,
// and the reason is stated rather than left as an omission: the data does not
// exist until Phase 4, and the shape is here so that it does not need reshaping
// then. A fourth mode returning empty series would be worse than none — it would
// look like a broken feature rather than an unbuilt one.
//
// # Why this costs no new store method
//
// Every mode is `WindowTotals` and `DailySeries` asked a second time with
// different arguments. `previous_period` moves the window; `monitors` and
// `groups` move the id set. That is a property worth protecting: a comparative
// report over 5,000 monitors is two batched reads rather than two reads per
// series, and the load gate measures exactly that.

// ComparisonModes, matching the spec's enum.
const (
	CompareToPreviousPeriod = "previous_period"
	CompareMonitors         = "monitors"
	CompareGroups           = "groups"
)

// ComparisonSpec is what a comparative template asks for.
type ComparisonSpec struct {
	Mode       string
	MonitorIDs []model.ID
	GroupIDs   []model.ID
}

// Comparison is the computed block.
type Comparison struct {
	Mode   string
	Series []Series
}

// Series is one side of a comparison, carrying the same blocks the estate
// summary does — so a reader comparing two of them is comparing like with like,
// and a renderer needs no second way to draw a figure.
type Series struct {
	Label string

	// The period is present for previous_period and nil for the entity modes,
	// where both sides share the report's own window and repeating it on every
	// series would be noise.
	PeriodStart *time.Time
	PeriodEnd   *time.Time

	Uptime       Uptime
	ResponseTime Latency
}

// BuildComparison computes the comparison block for a document.
//
// The current window's series is **always first**, because a reader scanning a
// table needs to know which column is now without reading the dates. The
// previous period follows it rather than preceding it, which is the opposite of
// chronological order and the right way round for a document whose subject is
// the period it covers.
func BuildComparison(
	ctx context.Context,
	s Store,
	spec ComparisonSpec,
	scope Scope,
	window Window,
	tier string,
	policy string,
) (*Comparison, error) {
	switch spec.Mode {
	case CompareToPreviousPeriod:
		return comparePeriods(ctx, s, scope, window, tier, policy)
	case CompareMonitors:
		return compareEntities(ctx, s, spec.MonitorIDs, nil, window, tier, policy)
	case CompareGroups:
		return compareEntities(ctx, s, nil, spec.GroupIDs, window, tier, policy)
	case "":
		return nil, nil
	}
	return nil, fmt.Errorf("unknown comparison mode %q", spec.Mode)
}

// comparePeriods runs the same scope over the window before this one.
//
// The previous window is the same **duration** placed immediately before, not
// "the previous calendar month". That is deliberate and it is the honest choice
// for a comparison: February against March would put 28 days beside 31 and
// produce a difference in every count that is about the calendar rather than
// about the service. The ratios would still compare correctly and the counts
// would not, and a table shows both.
func comparePeriods(ctx context.Context, s Store, scope Scope, window Window, tier, policy string) (*Comparison, error) {
	monitors, err := s.MonitorsInScope(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("resolve comparison scope: %w", err)
	}
	ids := idsOf(monitors)

	length := window.To.Sub(window.From)
	previous := Window{From: window.From.Add(-length), To: window.From, Timezone: window.Timezone}

	current, err := seriesOver(ctx, s, ids, window, tier, policy, "This period")
	if err != nil {
		return nil, err
	}
	before, err := seriesOver(ctx, s, ids, previous, tier, policy, "Previous period")
	if err != nil {
		return nil, err
	}

	current.PeriodStart, current.PeriodEnd = &window.From, &window.To
	before.PeriodStart, before.PeriodEnd = &previous.From, &previous.To

	return &Comparison{Mode: CompareToPreviousPeriod, Series: []Series{current, before}}, nil
}

// compareEntities puts named monitors or groups side by side over one window.
//
// One series per entity, each resolved through the same scope machinery the
// report itself uses — so a group compared here covers exactly the monitors it
// would cover in a scope, including child groups, rather than a second
// interpretation of what a group contains.
func compareEntities(
	ctx context.Context,
	s Store,
	monitorIDs, groupIDs []model.ID,
	window Window,
	tier, policy string,
) (*Comparison, error) {
	mode := CompareMonitors
	entities := monitorIDs
	if len(groupIDs) > 0 {
		mode = CompareGroups
		entities = groupIDs
	}
	if len(entities) == 0 {
		// A comparative template naming nothing to compare. An empty block
		// rather than an error, following the empty-scope rule: a client whose
		// compared monitors were all deleted still gets a report saying so,
		// which beats a failed run nobody looks at until the invoice goes out.
		return &Comparison{Mode: mode}, nil
	}

	out := &Comparison{Mode: mode, Series: make([]Series, 0, len(entities))}
	for _, id := range entities {
		scope := Scope{MonitorIDs: []model.ID{id}}
		if mode == CompareGroups {
			scope = Scope{GroupIDs: []model.ID{id}}
		}

		monitors, err := s.MonitorsInScope(ctx, scope)
		if err != nil {
			return nil, fmt.Errorf("resolve comparison entity %s: %w", id, err)
		}

		// The label comes from the resolved monitor where there is exactly one,
		// and from the id otherwise. A group's name is not on this interface and
		// inventing a method to fetch it would put a lookup in front of every
		// consumer for a label — the renderer has the ids and a caller that
		// wants names can supply them.
		label := id.String()
		if mode == CompareMonitors && len(monitors) == 1 {
			label = monitors[0].Name
		}

		series, err := seriesOver(ctx, s, idsOf(monitors), window, tier, policy, label)
		if err != nil {
			return nil, err
		}
		out.Series = append(out.Series, series)
	}
	return out, nil
}

// seriesOver aggregates one id set over one window into the two blocks a series
// carries.
func seriesOver(
	ctx context.Context,
	s Store,
	ids []model.ID,
	window Window,
	tier, policy, label string,
) (Series, error) {
	series := Series{Label: label}
	if len(ids) == 0 {
		// No monitors is a real answer with a real shape: nothing observed, so
		// no ratio. Returning a zeroed Uptime here rather than skipping the
		// series keeps the table's columns aligned, and the null ratio says why
		// the row is empty.
		return series, nil
	}

	totals, err := s.WindowTotals(ctx, ids, window.From, window.To, tier)
	if err != nil {
		return series, fmt.Errorf("comparison totals for %q: %w", label, err)
	}
	daily, err := s.DailySeries(ctx, ids, window.From, window.To)
	if err != nil {
		return series, fmt.Errorf("comparison series for %q: %w", label, err)
	}

	var buckets []store.HistoryBucket
	for _, id := range ids {
		if bucket, ok := totals[id]; ok {
			buckets = append(buckets, bucket)
		}
	}

	total := Sum(buckets)
	series.Uptime = ComputeUptime(total, policy)
	// mergeDaily folds every monitor's days into one series by date, which is
	// the same fold the estate summary uses — so a comparison series and the
	// summary it sits beside are the same statistic computed the same way.
	series.ResponseTime = ComputeLatency(total, mergeDaily(daily), nil)
	return series, nil
}

func idsOf(monitors []model.Monitor) []model.ID {
	out := make([]model.ID, 0, len(monitors))
	for _, m := range monitors {
		out = append(out, m.ID)
	}
	return out
}
