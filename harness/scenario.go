package main

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// Stat collects the timings for one scenario at one scale.
type Stat struct {
	Name      string
	Durations []time.Duration
	Rows      int // rows the scenario returned, from its last iteration
}

func (s *Stat) percentile(q float64) time.Duration {
	if len(s.Durations) == 0 {
		return 0
	}
	d := append([]time.Duration(nil), s.Durations...)
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	i := int(float64(len(d))*q+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(d) {
		i = len(d) - 1
	}
	return d[i]
}

func (s *Stat) P50() time.Duration { return s.percentile(0.50) }
func (s *Stat) P95() time.Duration { return s.percentile(0.95) }

// Scenario is one measured operation.
//
// Thresholds are expressed as growth factors first and absolute ceilings second,
// deliberately. Shared CI runners are noisy enough that an absolute millisecond
// gate is either so tight it flakes or so loose it asserts nothing. What ADR-004
// actually claims is a *scaling* property — cost bounded by viewport, not by
// total monitor count — and a ratio across two scales tests that claim directly
// while cancelling out most of the runner's noise.
type Scenario struct {
	Name string
	Run  func(ctx context.Context, t Target, w *Workload, r *rand.Rand) (int, error)

	// ViewportBounded asserts the row count is identical at every scale. This is
	// ADR-004's second invariant stated literally: client payload size must be
	// bounded by the page, never by how many monitors exist.
	ViewportBounded bool

	// MaxGrowth caps p95(largest scale) / p95(smallest scale). Zero means report
	// the figure but do not fail on it.
	MaxGrowth float64

	// MaxAbs is a backstop ceiling on p95 at the largest scale. Generous on
	// purpose: it catches an order-of-magnitude regression, not a slow runner.
	MaxAbs time.Duration
}

// Scenarios are the data model's §13 hypotheses, made executable.
func Scenarios(pageSize int) []Scenario {
	return []Scenario{
		{
			// The dashboard's default view.
			Name:            "list: first page",
			ViewportBounded: true,
			MaxGrowth:       3.0,
			MaxAbs:          150 * time.Millisecond,
			Run: func(ctx context.Context, t Target, w *Workload, r *rand.Rand) (int, error) {
				res, err := t.ListMonitors(ctx, ListQuery{Limit: pageSize})
				return res.Rows, err
			},
		},
		{
			// The reason for keyset pagination. An OFFSET query degrades with
			// depth because the engine walks and discards every skipped row; a
			// cursor seeks straight to its position. If this scenario grows with
			// scale, the cursor is not being used and the index is wrong.
			Name:            "list: deep page (cursor at 50%)",
			ViewportBounded: true,
			MaxGrowth:       3.0,
			MaxAbs:          150 * time.Millisecond,
			Run: func(ctx context.Context, t Target, w *Workload, r *rand.Rand) (int, error) {
				mid := w.Monitors[len(w.Monitors)/2]
				res, err := t.ListMonitors(ctx, ListQuery{
					Limit:  pageSize,
					Cursor: &Cursor{UpdatedAt: mid.UpdatedAt, ID: mid.ID},
				})
				return res.Rows, err
			},
		},
		{
			// The §6.2 hypothesis. Status lives in monitor_state, the cursor
			// lives on monitors, so this is a join — and the planner has to drive
			// from the small side. If it does not, this is where the data model's
			// monitor_state split fails and the fallback is to denormalise status
			// back onto monitors and accept the write amplification.
			Name:            "list: filter status=down (join)",
			ViewportBounded: false, // fewer than a page may match at small scale
			MaxGrowth:       4.0,
			MaxAbs:          250 * time.Millisecond,
			Run: func(ctx context.Context, t Target, w *Workload, r *rand.Rand) (int, error) {
				res, err := t.ListMonitors(ctx, ListQuery{Limit: pageSize, Status: "down"})
				return res.Rows, err
			},
		},
		{
			Name:            "list: filter by tag",
			ViewportBounded: true,
			MaxGrowth:       3.0,
			MaxAbs:          200 * time.Millisecond,
			Run: func(ctx context.Context, t Target, w *Workload, r *rand.Rand) (int, error) {
				tag := w.Tags[r.Intn(len(w.Tags))]
				res, err := t.ListMonitors(ctx, ListQuery{Limit: pageSize, TagID: tag})
				return res.Rows, err
			},
		},
		{
			// ADR-004's reconciliation signal, polled every ~5s per active
			// filtered view. Its cost scales with connected clients, which the
			// 5,000-monitor gate does not otherwise exercise.
			//
			// No growth bound: COUNT(*) over an index is inherently O(n), so this
			// is expected to grow with scale. The figure is reported because it
			// is the number that decides whether §6.5 option 3 survives contact
			// with many concurrent viewers — if it is already milliseconds at
			// 5,000 monitors, a hundred open dashboards is a real cost.
			Name:      "membership: unfiltered",
			MaxGrowth: 0,
			MaxAbs:    100 * time.Millisecond,
			Run: func(ctx context.Context, t Target, w *Workload, r *rand.Rand) (int, error) {
				res, err := t.Membership(ctx, ListQuery{})
				return int(res.Count), err
			},
		},
		{
			Name:      "membership: status=down",
			MaxGrowth: 0,
			MaxAbs:    100 * time.Millisecond,
			Run: func(ctx context.Context, t Target, w *Workload, r *rand.Rand) (int, error) {
				res, err := t.Membership(ctx, ListQuery{Status: "down"})
				return int(res.Count), err
			},
		},
		{
			// Monitor detail: a rolled-up history read for one monitor. Bounded
			// by the range requested, so it must not care how many monitors exist.
			Name:            "history: 1m rollups, one monitor",
			ViewportBounded: true,
			MaxGrowth:       3.0,
			MaxAbs:          200 * time.Millisecond,
			Run: func(ctx context.Context, t Target, w *Workload, r *rand.Rand) (int, error) {
				m := w.Monitors[r.Intn(len(w.Monitors))]
				return t.History(ctx, m.ID, w.BaseTime, w.BaseTime.Add(2*time.Hour))
			},
		},
	}
}

// ScaleResult is everything measured at one monitor count.
type ScaleResult struct {
	Scale     int
	Stats     map[string]*Stat
	WriteRate float64 // heartbeats per second, sustained
	DBBytes   int64
}

// Finding is one assertion outcome.
type Finding struct {
	Scenario string
	Detail   string
	Failed   bool
}

// Evaluate compares the smallest and largest scales and produces the gate's
// verdict. Comparing two scales is the point: a single-scale run can only tell
// you what a machine did today, whereas the ratio tells you whether the design
// holds as the install grows, which is the actual claim being made.
func Evaluate(scenarios []Scenario, results []ScaleResult, minWriteRate float64) []Finding {
	var findings []Finding
	if len(results) == 0 {
		return findings
	}
	small, large := results[0], results[len(results)-1]

	for _, sc := range scenarios {
		ls, ok := large.Stats[sc.Name]
		if !ok {
			continue
		}
		if sc.MaxAbs > 0 && ls.P95() > sc.MaxAbs {
			findings = append(findings, Finding{
				Scenario: sc.Name,
				Failed:   true,
				Detail: fmt.Sprintf("p95 %s exceeds ceiling %s at %d monitors",
					ls.P95().Round(time.Microsecond), sc.MaxAbs, large.Scale),
			})
		}
		if len(results) < 2 {
			continue
		}
		ss, ok := small.Stats[sc.Name]
		if !ok {
			continue
		}
		if sc.ViewportBounded && ss.Rows != ls.Rows {
			findings = append(findings, Finding{
				Scenario: sc.Name,
				Failed:   true,
				Detail: fmt.Sprintf(
					"returned %d rows at %d monitors but %d at %d — payload size must be bounded by the page, not by total monitor count (ADR-004)",
					ss.Rows, small.Scale, ls.Rows, large.Scale),
			})
		}
		if sc.MaxGrowth > 0 && ss.P95() > 0 {
			growth := float64(ls.P95()) / float64(ss.P95())
			if growth > sc.MaxGrowth {
				findings = append(findings, Finding{
					Scenario: sc.Name,
					Failed:   true,
					Detail: fmt.Sprintf(
						"p95 grew %.1fx (%s -> %s) for a %.0fx increase in monitors; cap is %.1fx",
						growth, ss.P95().Round(time.Microsecond), ls.P95().Round(time.Microsecond),
						float64(large.Scale)/float64(small.Scale), sc.MaxGrowth),
				})
			}
		}
	}

	if minWriteRate > 0 && large.WriteRate < minWriteRate {
		findings = append(findings, Finding{
			Scenario: "heartbeat write rate",
			Failed:   true,
			Detail: fmt.Sprintf(
				"sustained %.0f heartbeats/sec, below the %.0f/sec that %d monitors on a 20-second floor require",
				large.WriteRate, minWriteRate, large.Scale),
		})
	}
	return findings
}
