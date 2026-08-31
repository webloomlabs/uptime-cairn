package report

import (
	"math"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/store"
)

func ratio(t *testing.T, u Uptime) float64 {
	t.Helper()
	if u.Ratio == nil {
		t.Fatal("ratio is nil, want a figure")
	}
	return *u.Ratio
}

func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// The rule the whole phase rests on, and the one a wrong answer sends to an
// auditor: unknown and skipped are gaps in observation, not observations of
// failure. internal/status/doc.go names rendering a probe failure as customer
// downtime "a lie that Phase 2's SLA reports would inherit". This is the test
// that refuses the inheritance.
func TestUnknownAndSkippedLeaveTheDenominator(t *testing.T) {
	t.Parallel()

	// 90 up, 10 down, and 100 checks the probe could not make. Counting those as
	// downtime gives 45%. Counting them as up gives 95%. The truth is 90% over
	// half the window, and the report has to say both halves.
	b := store.HistoryBucket{Up: 90, Down: 10, Unknown: 60, Skipped: 40}
	u := ComputeUptime(b, MaintenanceExclude)

	if got := ratio(t, u); !closeTo(got, 0.90) {
		t.Errorf("ratio = %v, want 0.90 — not 0.45 (gaps as downtime) and not 0.95 (gaps as uptime)", got)
	}
	if u.ObservedChecks != 100 {
		t.Errorf("observed = %d, want 100", u.ObservedChecks)
	}
	if u.UnknownChecks != 60 || u.SkippedChecks != 40 {
		t.Errorf("unknown/skipped = %d/%d, want 60/40 — they must survive to the report", u.UnknownChecks, u.SkippedChecks)
	}
	if u.UnobservedShare == nil || !closeTo(*u.UnobservedShare, 0.5) {
		t.Fatalf("unobserved share = %v, want 0.5; an SLA over 50%% observation must say so", u.UnobservedShare)
	}
}

// Three policies, one bucket, three different lawful answers. The figure is
// meaningless without the policy beside it, which is why the policy is on the
// result and not only on the template.
func TestMaintenancePolicyChangesTheFigureAndIsCarriedWithIt(t *testing.T) {
	t.Parallel()

	b := store.HistoryBucket{Up: 80, Down: 20, Maintenance: 100}

	for _, tc := range []struct {
		handling string
		want     float64
		observed int
	}{
		{MaintenanceExclude, 0.80, 100},     // planned work is not an outage
		{MaintenanceCountAsUp, 0.90, 200},   // the window still consumes the month
		{MaintenanceCountAsDown, 0.40, 200}, // an SLO that refuses to let it be free
	} {
		u := ComputeUptime(b, tc.handling)
		if got := ratio(t, u); !closeTo(got, tc.want) {
			t.Errorf("%s: ratio = %v, want %v", tc.handling, got, tc.want)
		}
		if u.ObservedChecks != tc.observed {
			t.Errorf("%s: observed = %d, want %d", tc.handling, u.ObservedChecks, tc.observed)
		}
		if u.MaintenanceHandling != tc.handling {
			t.Errorf("%s: handling not carried on the figure (got %q)", tc.handling, u.MaintenanceHandling)
		}
		if u.MaintenanceChecks != 100 {
			t.Errorf("%s: maintenance_checks = %d, want 100 under every policy", tc.handling, u.MaintenanceChecks)
		}
	}
}

// Excluding maintenance from the denominator must not also flatter the quality
// of the observation. The share is taken over everything scheduled, so a month
// that was half maintenance and half unobserved does not report itself as
// perfectly observed.
func TestExcludingMaintenanceDoesNotImproveTheObservedShare(t *testing.T) {
	t.Parallel()

	b := store.HistoryBucket{Up: 50, Maintenance: 25, Unknown: 25}
	u := ComputeUptime(b, MaintenanceExclude)

	if u.UnobservedShare == nil || !closeTo(*u.UnobservedShare, 0.25) {
		t.Fatalf("unobserved share = %v, want 0.25 (25 of 100 scheduled), not 0.333 (25 of 75 counted)", u.UnobservedShare)
	}
}

// A window with nothing observed has no uptime percentage. Zero claims total
// downtime and one claims perfect service; both are inventions, and the null has
// to survive all the way to the report.
func TestNothingObservedIsNullRatherThanZero(t *testing.T) {
	t.Parallel()

	for _, b := range []store.HistoryBucket{
		{},                 // nothing at all
		{Unknown: 500},     // the probe was down all month
		{Skipped: 500},     // shed under overload all month
		{Pending: 10},      // never checked since it was created
		{Maintenance: 100}, // the whole window was declared maintenance
	} {
		if u := ComputeUptime(b, MaintenanceExclude); u.Ratio != nil {
			t.Errorf("bucket %+v produced ratio %v, want nil", b, *u.Ratio)
		}
	}
}

// pending is a third thing again: no verdict yet, as distinct from "the probe
// could not look". It is not in the denominator and is not configurable.
func TestPendingIsNeverCounted(t *testing.T) {
	t.Parallel()

	u := ComputeUptime(store.HistoryBucket{Up: 90, Down: 10, Pending: 900}, MaintenanceExclude)
	if got := ratio(t, u); !closeTo(got, 0.90) {
		t.Errorf("ratio = %v, want 0.90; pending must not dilute the figure", got)
	}
	if u.ObservedChecks != 100 {
		t.Errorf("observed = %d, want 100", u.ObservedChecks)
	}
}

// An empty policy is the default rather than an error or a fourth behaviour: a
// template written before the field existed, or one that simply never set it,
// must produce the documented default and say that is what it did.
func TestEmptyPolicyDefaultsToExcludeAndSaysSo(t *testing.T) {
	t.Parallel()

	u := ComputeUptime(store.HistoryBucket{Up: 80, Down: 20, Maintenance: 100}, "")
	if u.MaintenanceHandling != MaintenanceExclude {
		t.Errorf("handling = %q, want %q", u.MaintenanceHandling, MaintenanceExclude)
	}
	if got := ratio(t, u); !closeTo(got, 0.80) {
		t.Errorf("ratio = %v, want 0.80", got)
	}
}

// The estate figure is a sum of counts, never an average of ratios. Two monitors
// with wildly different check volumes are exactly where the two answers diverge,
// and the wrong one is the one that looks reasonable.
func TestSumIsExactRatherThanAnAverageOfAverages(t *testing.T) {
	t.Parallel()

	// A busy monitor at 89.9% over 9,900 checks and a quiet one at 50% over 10.
	// The mean of the two ratios is 69.95%; the true figure is 8,905/9,910,
	// which is 89.86% — the quiet monitor moves it by four hundredths of a
	// point, not by twenty points.
	busy := store.HistoryBucket{Up: 8900, Down: 1000, ResponseTimeSum: 89000, ResponseTimeCount: 9900}
	quiet := store.HistoryBucket{Up: 5, Down: 5, ResponseTimeSum: 1000, ResponseTimeCount: 10}

	total := Sum([]store.HistoryBucket{busy, quiet})
	u := ComputeUptime(total, MaintenanceExclude)

	if got := ratio(t, u); !closeTo(got, 8905.0/9910.0) {
		t.Errorf("ratio = %v, want %v — and never 0.70, the mean of the two ratios", got, 8905.0/9910.0)
	}
	if total.ResponseTimeCount != 9910 || total.ResponseTimeSum != 90000 {
		t.Errorf("response time = sum %v count %d, want 90000/9910", total.ResponseTimeSum, total.ResponseTimeCount)
	}
}

// Summing across monitors must not manufacture a latency extreme or a
// percentile. The fastest check in an estate is not a fact about the estate.
func TestSumCarriesNoExtremesOrPercentile(t *testing.T) {
	t.Parallel()

	low, high, p95 := 5.0, 900.0, 400.0
	total := Sum([]store.HistoryBucket{
		{Up: 1, ResponseTimeMin: &low, ResponseTimeMax: &high, ResponseTimeP95: &p95},
		{Up: 1},
	})

	if total.ResponseTimeMin != nil || total.ResponseTimeMax != nil {
		t.Error("Sum carried a window extreme across monitors")
	}
	if total.ResponseTimeP95 != nil {
		t.Error("Sum produced a percentile; a quantile merges no better across monitors than across time")
	}
}

// The earliest bucket wins, so an estate total is stamped with the start of the
// window it covers rather than with whichever monitor happened to be summed
// first.
func TestSumTakesTheEarliestStart(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	total := Sum([]store.HistoryBucket{
		{Start: first.AddDate(0, 0, 5)},
		{Start: first},
	})
	if !total.Start.Equal(first) {
		t.Errorf("Start = %s, want %s", total.Start, first)
	}
}
