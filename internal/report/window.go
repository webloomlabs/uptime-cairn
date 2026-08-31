package report

import (
	"fmt"
	"time"
)

// The periods a report can cover.
const (
	PeriodDay     = "day"
	PeriodWeek    = "week"
	PeriodMonth   = "month"
	PeriodQuarter = "quarter"
	PeriodYear    = "year"
	PeriodCustom  = "custom"
)

// How the period is cut.
const (
	// StyleCalendar takes the last *complete* period — what a client invoiced
	// monthly expects, and what makes a report re-runnable: the same definition
	// run twice in March covers February both times.
	StyleCalendar = "calendar"

	// StyleRolling counts back from the run time. "The last 30 days" moves every
	// time it runs, which is right for an operational review and wrong for an
	// invoice attachment.
	StyleRolling = "rolling"
)

// Window is the period a report covers, and the zone its boundaries were cut in.
//
// The zone is carried rather than assumed because it cannot be recovered from
// the timestamps afterwards: 2026-03-01T00:00+11:00 and 2026-03-01T00:00Z are
// different instants, and which one a monthly report used is the difference
// between including and excluding a working day. The API contract has
// ReportRun.timezone for exactly this.
type Window struct {
	From, To time.Time
	Timezone string
}

// Duration is the wall-clock length of the window, which is what the error
// budget is a proportion of.
//
// Taken as the difference between the two instants rather than as a count of
// days, so a month containing a daylight-saving transition is 23 or 25 hours
// long on that day and the budget follows. A budget computed from 30 × 24 hours
// would be an hour wrong twice a year, in a number people check.
func (w Window) Duration() time.Duration { return w.To.Sub(w.From) }

// ResolveWindow cuts the reporting period in a stated zone.
//
// The rule the plan is emphatic about: **a monthly report that starts at
// midnight UTC for an Australian agency is wrong by a working day and will be
// reported as a bug.** So every boundary here is constructed in loc, never in
// UTC and never in the process's local zone, and the returned instants are the
// real moments those local midnights happened.
//
// now is passed in rather than read from the clock so that a run is reproducible
// and so that these rules are testable without waiting for April.
func ResolveWindow(period, style string, loc *time.Location, now time.Time) (Window, error) {
	if loc == nil {
		return Window{}, fmt.Errorf("no timezone: a report window has to be cut in a stated zone")
	}
	if style == "" {
		style = StyleCalendar
	}
	local := now.In(loc)

	if style == StyleRolling {
		to := local
		var from time.Time
		switch period {
		case PeriodDay:
			from = to.AddDate(0, 0, -1)
		case PeriodWeek:
			from = to.AddDate(0, 0, -7)
		case PeriodMonth:
			from = to.AddDate(0, -1, 0)
		case PeriodQuarter:
			from = to.AddDate(0, -3, 0)
		case PeriodYear:
			from = to.AddDate(-1, 0, 0)
		default:
			return Window{}, fmt.Errorf("period %q cannot be rolling; use calendar or supply explicit dates", period)
		}
		return Window{From: from, To: to, Timezone: loc.String()}, nil
	}

	if style != StyleCalendar {
		return Window{}, fmt.Errorf("period style %q is not one the spec defines: want calendar or rolling", style)
	}

	midnight := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	y, m, d := local.Date()

	switch period {
	case PeriodDay:
		to := midnight(y, m, d)
		return Window{From: to.AddDate(0, 0, -1), To: to, Timezone: loc.String()}, nil

	case PeriodWeek:
		// Weeks start on Monday, matching ISO-8601 and every European and
		// Australian invoice. Sunday is weekday 0 in Go and 7 in ISO.
		offset := (int(local.Weekday()) + 6) % 7
		thisWeek := midnight(y, m, d).AddDate(0, 0, -offset)
		return Window{From: thisWeek.AddDate(0, 0, -7), To: thisWeek, Timezone: loc.String()}, nil

	case PeriodMonth:
		thisMonth := midnight(y, m, 1)
		return Window{From: thisMonth.AddDate(0, -1, 0), To: thisMonth, Timezone: loc.String()}, nil

	case PeriodQuarter:
		// Calendar quarters: Jan–Mar, Apr–Jun, Jul–Sep, Oct–Dec. Fiscal years
		// vary by country and are not guessed at; a client on an Australian or
		// UK fiscal year uses a custom window until somebody asks for the option.
		firstMonth := time.Month((int(m)-1)/3*3 + 1)
		thisQuarter := midnight(y, firstMonth, 1)
		return Window{From: thisQuarter.AddDate(0, -3, 0), To: thisQuarter, Timezone: loc.String()}, nil

	case PeriodYear:
		thisYear := midnight(y, time.January, 1)
		return Window{From: thisYear.AddDate(-1, 0, 0), To: thisYear, Timezone: loc.String()}, nil

	case PeriodCustom:
		return Window{}, fmt.Errorf("period %q needs explicit start and end dates", period)
	}
	return Window{}, fmt.Errorf("period %q is not one the spec defines: want day, week, month, quarter, year, or custom", period)
}

// Resolution records which tier answered and whether retention forced it,
// mirroring ReportDocument.meta.resolution.
//
// §3.2: retention limits *resolution*, not existence. A request for minute
// detail over last March returns hourly data **labelled as hourly** and is never
// silently upsampled — which is the whole reason this structure travels with the
// document instead of the tier name being an implementation detail.
type Resolution struct {
	Tier          string
	RequestedTier string
	Downgraded    bool

	// CoveredFrom is the earliest instant the data actually reaches, when
	// retention has truncated the window. The report states the range it
	// covered, not the range it was asked for.
	CoveredFrom *time.Time
}

// tierDays maps a tier to the retention setting that governs it, coarsest last.
// Zero days means indefinite.
type tierRetention struct {
	tier string
	days int
}

// ResolveTier picks the finest tier whose retention still covers the window, and
// says so when that is coarser than what was asked for.
//
// Reports read tiers, never raw, except for the trailing-seven-day percentile —
// so raw is deliberately not a candidate here. A report that scanned a year of
// heartbeats is the failure ADR-004 exists to prevent, one layer up.
func ResolveTier(requested string, w Window, now time.Time, r Retention) Resolution {
	tiers := []tierRetention{
		{"1m", r.Rollup1mDays},
		{"5m", r.Rollup5mDays},
		{"1h", r.Rollup1hDays},
		{"1d", r.Rollup1dDays},
	}

	covers := func(days int) bool {
		if days == 0 {
			return true // indefinite
		}
		return !w.From.Before(now.AddDate(0, 0, -days))
	}

	start := 0
	if requested != "" && requested != "auto" {
		start = -1
		for i, t := range tiers {
			if t.tier == requested {
				start = i
				break
			}
		}
		if start < 0 {
			// An unknown name falls back to the tier that always answers rather
			// than failing the run: the daily tier is kept indefinitely and is
			// the honest floor.
			return Resolution{Tier: "1d", RequestedTier: requested, Downgraded: true}
		}
	}

	for i := start; i < len(tiers); i++ {
		if covers(tiers[i].days) {
			res := Resolution{
				Tier:          tiers[i].tier,
				RequestedTier: requested,
				Downgraded:    i > start && requested != "" && requested != "auto",
			}
			return res
		}
	}

	// Nothing covers it, which means even the daily tier has been pruned. Report
	// the daily tier and say where the data actually starts, rather than
	// returning figures that quietly omit the beginning of the window.
	res := Resolution{Tier: "1d", RequestedTier: requested, Downgraded: requested != "" && requested != "auto"}
	if r.Rollup1dDays > 0 {
		from := now.AddDate(0, 0, -r.Rollup1dDays)
		if from.After(w.From) {
			res.CoveredFrom = &from
		}
	}
	return res
}

// Retention is the subset of the instance's retention policy that decides which
// tier can answer a window. It mirrors rollup.Retention rather than importing
// it, so that the computation package stays free of the pipeline that produces
// the data it reads.
type Retention struct {
	RawDays      int
	Rollup1mDays int
	Rollup5mDays int
	Rollup1hDays int
	Rollup1dDays int
}

// RawCoversTrailingWeek reports whether raw retention reaches back seven days,
// which is the gate on the only percentile in the product.
//
// Separate from the store's RawCovers, which asks the same question of the data;
// this asks it of the policy, and is what lets the document say
// "insufficient_raw_retention" without a query. Both have to pass.
func (r Retention) RawCoversTrailingWeek() bool {
	return r.RawDays == 0 || r.RawDays >= 7
}
