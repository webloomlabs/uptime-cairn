package report

import (
	"math"
	"time"
)

// TargetFromTemplate is the third source a target can have, and the one that
// wins: a template's own sla_target overrides whatever the monitors in scope
// carry. The store cannot see it, so the caller applies it.
const TargetFromTemplate = "template"

// SLA is the computed SLO block for one monitor, mirroring ReportSlaBlock in the
// API contract. It is present only where a target was resolved; there is no
// default target and a report with none simply has no SLA block.
type SLA struct {
	TargetPercent float64
	TargetSource  string

	// ActualPercent is nil when nothing was observed, and everything derived
	// from it is nil with it. A window the probe could not see is not a breach.
	ActualPercent *float64
	Met           *bool

	// The budget in seconds, and what became of it. Remaining goes negative when
	// the budget is overspent, which is the number somebody actually wants: "you
	// are 41 minutes past" reads better than a floor at zero.
	ErrorBudgetSeconds          int
	ErrorBudgetConsumedSeconds  int
	ErrorBudgetRemainingSeconds int

	ErrorBudgetConsumedRatio *float64
	BurnRate                 *float64
}

// ComputeSLA turns an uptime figure and a target into an error budget.
//
// Every figure here comes from up and down counts, which are additive at every
// tier, so the budget is exact rather than estimated. **No figure in this
// function derives from a percentile** (ADR-006), which is the direct answer to
// the instruction data model §11.5 left for this phase.
//
// # What a second is
//
// Uptime is a ratio of observed checks; a budget is quoted in seconds. The
// conversion is a choice, and it is made in one place — DowntimeSeconds, which
// sums the observed down proportion of each **day** — with the result passed in
// here rather than re-derived.
//
// Two consequences worth stating, because both were wrong in an earlier cut of
// this function and the second reached a rendered report.
//
// A day nobody observed contributes nothing. Projecting the window-level down
// proportion onto the window's whole length instead would attribute the observed
// failure rate to days the probe was off — inventing downtime on a day nothing
// is known about, which is exactly what the denominator rules refuse one layer
// up. The budget denominator is still the whole window, because a contractual
// 99.9% of a month is 43m12s however much of it was observed; UnobservedShare is
// what tells the reader how much to trust the numerator.
//
// And the breach log and the budget agree by construction, because they are the
// same sum. A report reading "budget used 3h 36m" above a table of breaches
// totalling 2h 24m is an internal contradiction an auditor finds immediately.
//
// The alternative, multiplying down checks by the monitor's interval, was not
// taken: an interval changed mid-window, or checks never made, produce a
// confident figure with no relationship to the time the service was unavailable.
func ComputeSLA(u Uptime, target Target, window time.Duration, downtimeSeconds int) SLA {
	s := SLA{
		TargetPercent: target.Percent,
		TargetSource:  target.Source,
	}

	windowSeconds := window.Seconds()
	if windowSeconds < 0 {
		windowSeconds = 0
	}

	// A target of 100 has an error budget of zero seconds, which makes the
	// consumed ratio and the burn rate divisions by zero and every report a
	// breach report. The API refuses it and the schema's exclusiveMaximum
	// refuses it again; if one ever arrives anyway, the budget figures are
	// omitted rather than returned as infinity.
	budget := windowSeconds * (1 - target.Percent/100)
	if budget < 0 {
		budget = 0
	}
	s.ErrorBudgetSeconds = int(math.Round(budget))

	if u.Ratio == nil {
		// Nothing observed. The actual figure is unknown, so nothing derived
		// from it is reported — and in particular the budget is *not* recorded
		// as fully consumed. A probe that could not look has not spent anything.
		s.ErrorBudgetRemainingSeconds = s.ErrorBudgetSeconds
		return s
	}

	actual := *u.Ratio * 100
	s.ActualPercent = &actual

	// Compared as percentages rather than through the seconds, so that the
	// verdict matches the two numbers printed above it. The epsilon is float
	// noise only: 99.9 computed as 8991/9000 is not exactly 99.9, and a monitor
	// that hit its target exactly must not be reported as having missed it.
	met := actual >= target.Percent-1e-9
	s.Met = &met

	consumed := float64(downtimeSeconds)
	s.ErrorBudgetConsumedSeconds = downtimeSeconds
	s.ErrorBudgetRemainingSeconds = s.ErrorBudgetSeconds - s.ErrorBudgetConsumedSeconds

	if budget > 0 {
		ratio := consumed / budget
		s.ErrorBudgetConsumedRatio = &ratio

		// Over a complete window the burn rate and the consumed ratio coincide
		// by construction: burning at exactly 1 for the whole period spends
		// exactly the budget. They separate only for a window still in progress,
		// where the rate is measured against the elapsed part — and this phase
		// reports completed periods, so the distinction has no caller yet. Both
		// fields exist in the contract, so both are filled rather than one left
		// null for a reader to puzzle over.
		burn := ratio
		s.BurnRate = &burn
	}
	return s
}
