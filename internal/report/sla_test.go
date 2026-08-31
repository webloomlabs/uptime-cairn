package report

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/store"
)

const month = 30 * 24 * time.Hour

// uptimeAt builds a bucket with a given ratio over a round number of checks, so
// the arithmetic in each test reads off the numbers rather than off a fixture.
func uptimeAt(upPercent float64) Uptime {
	up := int(upPercent * 100)
	return ComputeUptime(store.HistoryBucket{Up: up, Down: 10000 - up}, MaintenanceExclude)
}

// The headline arithmetic, checked against figures somebody can verify by hand:
// 99.9% of thirty days is 43m12s of budget, and 99.8% observed spends exactly
// twice that.
func TestErrorBudgetIsExactAndOverspendGoesNegative(t *testing.T) {
	t.Parallel()

	s := ComputeSLA(uptimeAt(99.8), Target{Percent: 99.9, Source: TargetFromMonitor}, month)

	if s.ErrorBudgetSeconds != 2592 {
		t.Errorf("budget = %ds, want 2592 (43m12s: 0.1%% of 30 days)", s.ErrorBudgetSeconds)
	}
	if s.ErrorBudgetConsumedSeconds != 5184 {
		t.Errorf("consumed = %ds, want 5184 (0.2%% of 30 days)", s.ErrorBudgetConsumedSeconds)
	}
	if s.ErrorBudgetRemainingSeconds != -2592 {
		t.Errorf("remaining = %ds, want -2592; a floor at zero hides how far past the budget is", s.ErrorBudgetRemainingSeconds)
	}
	if s.ErrorBudgetConsumedRatio == nil || !closeTo(*s.ErrorBudgetConsumedRatio, 2) {
		t.Errorf("consumed ratio = %v, want 2", s.ErrorBudgetConsumedRatio)
	}
	if s.BurnRate == nil || !closeTo(*s.BurnRate, 2) {
		t.Errorf("burn rate = %v, want 2 — above 1 is overspending", s.BurnRate)
	}
	if s.Met == nil || *s.Met {
		t.Error("met should be false at 99.8 against a 99.9 target")
	}
}

// A monitor that hit its target exactly must not be reported as having missed
// it. 99.9% arrives as 8991/9000, which is not exactly 99.9 in binary floating
// point, and a naive comparison turns a met SLA into a breach on the report a
// client reads.
func TestTargetMetExactlyIsMet(t *testing.T) {
	t.Parallel()

	u := ComputeUptime(store.HistoryBucket{Up: 8991, Down: 9}, MaintenanceExclude)
	s := ComputeSLA(u, Target{Percent: 99.9, Source: TargetFromMonitor}, month)

	if s.Met == nil || !*s.Met {
		t.Fatalf("met = %v at exactly 99.9%%, want true (actual %v)", s.Met, *s.ActualPercent)
	}
}

// A window the probe could not see is not a breach. The budget is reported
// intact and unspent, because a probe that never looked has consumed nothing —
// treating unobserved time as burnt budget is the same lie as treating it as
// downtime, one layer up.
func TestNothingObservedConsumesNothing(t *testing.T) {
	t.Parallel()

	u := ComputeUptime(store.HistoryBucket{Unknown: 5000}, MaintenanceExclude)
	s := ComputeSLA(u, Target{Percent: 99.9, Source: TargetFromGroup}, month)

	if s.ActualPercent != nil {
		t.Errorf("actual = %v, want nil", *s.ActualPercent)
	}
	if s.Met != nil {
		t.Errorf("met = %v, want nil — neither met nor missed", *s.Met)
	}
	if s.ErrorBudgetConsumedSeconds != 0 {
		t.Errorf("consumed = %ds, want 0", s.ErrorBudgetConsumedSeconds)
	}
	if s.ErrorBudgetRemainingSeconds != s.ErrorBudgetSeconds {
		t.Errorf("remaining = %d, want the full budget %d", s.ErrorBudgetRemainingSeconds, s.ErrorBudgetSeconds)
	}
	if s.BurnRate != nil || s.ErrorBudgetConsumedRatio != nil {
		t.Error("burn rate and consumed ratio must be nil when there is no actual figure")
	}
}

// A 100% target has a zero-second budget, which makes the ratio and the burn
// rate divisions by zero. The API refuses the value twice over; if one arrives
// anyway the figures are omitted rather than returned as infinity, which would
// render as "+Inf" on a client's PDF.
func TestImpossibleTargetOmitsTheRatioRatherThanReturningInfinity(t *testing.T) {
	t.Parallel()

	s := ComputeSLA(uptimeAt(99.0), Target{Percent: 100, Source: TargetFromTemplate}, month)

	if s.ErrorBudgetSeconds != 0 {
		t.Errorf("budget = %d, want 0", s.ErrorBudgetSeconds)
	}
	if s.ErrorBudgetConsumedRatio != nil {
		t.Errorf("consumed ratio = %v, want nil rather than an infinity", *s.ErrorBudgetConsumedRatio)
	}
	if s.BurnRate != nil {
		t.Errorf("burn rate = %v, want nil rather than an infinity", *s.BurnRate)
	}
	// The consumed seconds are still real and still worth printing.
	if s.ErrorBudgetConsumedSeconds != 25920 {
		t.Errorf("consumed = %d, want 25920 (1%% of 30 days)", s.ErrorBudgetConsumedSeconds)
	}
}

// The source travels with the target, because "whose number is 99.9?" is a
// question a client actually asks, and a monitor inheriting its group's target
// is otherwise invisible on the report face.
func TestTargetSourceIsCarriedOntoTheBlock(t *testing.T) {
	t.Parallel()

	for _, source := range []string{TargetFromTemplate, TargetFromMonitor, TargetFromGroup} {
		s := ComputeSLA(uptimeAt(99.95), Target{Percent: 99.9, Source: source}, month)
		if s.TargetSource != source {
			t.Errorf("source = %q, want %q", s.TargetSource, source)
		}
	}
}

// Meeting the target leaves budget on the table, and the remaining figure is
// what a client is being told they still have.
func TestMetTargetLeavesBudgetRemaining(t *testing.T) {
	t.Parallel()

	s := ComputeSLA(uptimeAt(99.95), Target{Percent: 99.9, Source: TargetFromMonitor}, month)

	if s.Met == nil || !*s.Met {
		t.Fatal("met = false at 99.95 against 99.9")
	}
	if s.ErrorBudgetConsumedSeconds != 1296 {
		t.Errorf("consumed = %d, want 1296 (0.05%% of 30 days)", s.ErrorBudgetConsumedSeconds)
	}
	if s.ErrorBudgetRemainingSeconds != 1296 {
		t.Errorf("remaining = %d, want 1296", s.ErrorBudgetRemainingSeconds)
	}
	if s.BurnRate == nil || !closeTo(*s.BurnRate, 0.5) {
		t.Errorf("burn rate = %v, want 0.5 — under 1 is within budget", s.BurnRate)
	}
}

// The projection is stated rather than implied, so it is worth pinning: the
// consumed figure is the observed down proportion applied to the window, which
// is independent of how many checks produced it. Ten thousand checks and a
// hundred checks at the same ratio spend the same budget.
func TestConsumedDependsOnTheRatioNotTheCheckCount(t *testing.T) {
	t.Parallel()

	target := Target{Percent: 99, Source: TargetFromMonitor}
	many := ComputeSLA(ComputeUptime(store.HistoryBucket{Up: 9950, Down: 50}, MaintenanceExclude), target, month)
	few := ComputeSLA(ComputeUptime(store.HistoryBucket{Up: 199, Down: 1}, MaintenanceExclude), target, month)

	if many.ErrorBudgetConsumedSeconds != few.ErrorBudgetConsumedSeconds {
		t.Errorf("consumed differs by sample size: %d vs %d", many.ErrorBudgetConsumedSeconds, few.ErrorBudgetConsumedSeconds)
	}
}

// The maintenance policy reaches the budget through the ratio, so the same
// window under a different policy is a different SLA verdict. This is the
// composition the report has to state on its face: the figure, the policy, and
// the denominator are one claim, not three.
func TestMaintenancePolicyReachesTheBudget(t *testing.T) {
	t.Parallel()

	b := store.HistoryBucket{Up: 9900, Down: 100, Maintenance: 5000}
	target := Target{Percent: 99, Source: TargetFromMonitor}

	excluded := ComputeSLA(ComputeUptime(b, MaintenanceExclude), target, month)
	asDown := ComputeSLA(ComputeUptime(b, MaintenanceCountAsDown), target, month)

	if excluded.Met == nil || !*excluded.Met {
		t.Error("excluding maintenance: 99% observed against a 99% target should be met")
	}
	if asDown.Met == nil || *asDown.Met {
		t.Error("counting maintenance as down: the same window must miss the target")
	}
	if asDown.ErrorBudgetConsumedSeconds <= excluded.ErrorBudgetConsumedSeconds {
		t.Errorf("counting maintenance as down consumed %d, not more than %d",
			asDown.ErrorBudgetConsumedSeconds, excluded.ErrorBudgetConsumedSeconds)
	}
}
