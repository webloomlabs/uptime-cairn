package report

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/store"
)

var day0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func day(n int, sum float64, count int) store.HistoryBucket {
	return store.HistoryBucket{
		Start:             day0.AddDate(0, 0, n),
		Up:                count,
		ResponseTimeSum:   sum,
		ResponseTimeCount: count,
	}
}

// The window average is a sum over a count, never the mean of the daily
// averages. The two diverge whenever days carry different check volumes, which
// is always — a partial first day, an outage that shed checks, a monitor added
// mid-month.
func TestWindowAverageIsNotTheMeanOfDailyAverages(t *testing.T) {
	t.Parallel()

	// Day one: 1,000 checks averaging 100 ms. Day two: 10 checks averaging 1,000 ms.
	// The mean of the daily averages is 550; the true average is 110,000/1,010 = 108.9.
	daily := []store.HistoryBucket{day(0, 100000, 1000), day(1, 10000, 10)}
	total := Sum(daily)

	l := ComputeLatency(total, daily, nil)

	if l.AverageMs == nil {
		t.Fatal("average is nil")
	}
	if !closeTo(*l.AverageMs, 110000.0/1010.0) {
		t.Errorf("average = %v, want %v — and never 550, the mean of the daily means", *l.AverageMs, 110000.0/1010.0)
	}
	if l.SampleCount != 1010 {
		t.Errorf("sample count = %d, want 1010", l.SampleCount)
	}
}

// A day with no successful checks is a gap, not a fast day. It stays on the
// series with a null average so the chart breaks rather than dipping to zero,
// and it is not eligible to be the best day.
func TestUnobservedDayIsAGapAndCannotBeTheBestDay(t *testing.T) {
	t.Parallel()

	daily := []store.HistoryBucket{
		day(0, 20000, 100),                           // 200 ms
		{Start: day0.AddDate(0, 0, 1), Unknown: 100}, // the probe could not look
		day(2, 30000, 100),                           // 300 ms
	}

	l := ComputeLatency(Sum(daily), daily, nil)

	if len(l.Daily) != 3 {
		t.Fatalf("series has %d points, want 3 — the gap stays on the chart", len(l.Daily))
	}
	if l.Daily[1].AverageMs != nil {
		t.Errorf("gap day has average %v, want nil", *l.Daily[1].AverageMs)
	}
	if l.BestDay == nil || !l.BestDay.Date.Equal(day0) {
		t.Errorf("best day = %v, want 1 March at 200ms; a day with no checks is not the fastest day", l.BestDay)
	}
	if l.WorstDay == nil || !l.WorstDay.Date.Equal(day0.AddDate(0, 0, 2)) {
		t.Errorf("worst day = %v, want 3 March at 300ms", l.WorstDay)
	}
}

// Days over target come with their dates, because "four days exceeded 500 ms"
// invites the question the report should already have answered.
func TestDaysOverTargetCarryTheirDates(t *testing.T) {
	t.Parallel()

	target := 500
	daily := []store.HistoryBucket{
		day(0, 40000, 100), // 400 ms — under
		day(1, 60000, 100), // 600 ms — over
		day(2, 50000, 100), // exactly 500 ms — not over
		day(3, 90000, 100), // 900 ms — over
	}

	l := ComputeLatency(Sum(daily), daily, &target)

	if l.DaysOverTarget == nil || *l.DaysOverTarget != 2 {
		t.Fatalf("days over target = %v, want 2 (exactly at target is not over)", l.DaysOverTarget)
	}
	if len(l.DatesOverTarget) != 2 ||
		!l.DatesOverTarget[0].Equal(day0.AddDate(0, 0, 1)) ||
		!l.DatesOverTarget[1].Equal(day0.AddDate(0, 0, 3)) {
		t.Errorf("dates = %v, want 2 and 4 March", l.DatesOverTarget)
	}
}

// No target is not zero days over target. One is an absence of a rule; the other
// is a compliment, and printing the second where the first is true is a claim
// nobody made.
func TestNoTargetMeansNullNotZero(t *testing.T) {
	t.Parallel()

	daily := []store.HistoryBucket{day(0, 900000, 100)} // 9,000 ms, dreadful
	l := ComputeLatency(Sum(daily), daily, nil)

	if l.DaysOverTarget != nil {
		t.Errorf("days over target = %d, want nil when no target is set", *l.DaysOverTarget)
	}
	if len(l.DatesOverTarget) != 0 {
		t.Errorf("dates = %v, want none", l.DatesOverTarget)
	}
}

// The gate is a gate. Short raw retention omits the figure with its reason
// rather than computing a shorter percentile under a seven-day heading, which is
// the unlabelled approximation ADR-006 exists to remove.
func TestP95IsOmittedWithItsReasonWhenRawIsShort(t *testing.T) {
	t.Parallel()

	value := 940.0
	p := TrailingP95(false, &value, day0, day0.AddDate(0, 0, 7))

	if p.Available {
		t.Error("p95 reported as available with insufficient raw retention")
	}
	if p.ValueMs != nil {
		t.Errorf("value = %v, want nil — a short figure under a seven-day heading is the failure mode", *p.ValueMs)
	}
	if p.Reason != ReasonInsufficientRaw {
		t.Errorf("reason = %q, want %q", p.Reason, ReasonInsufficientRaw)
	}
}

// Covered but empty is a different absence and says so: a percentile of no
// successful checks is not zero.
func TestP95DistinguishesNoDataFromNoRetention(t *testing.T) {
	t.Parallel()

	p := TrailingP95(true, nil, day0, day0.AddDate(0, 0, 7))

	if p.Available || p.Reason != ReasonNoSuccessfulChecks {
		t.Errorf("got available=%v reason=%q, want unavailable with %q", p.Available, p.Reason, ReasonNoSuccessfulChecks)
	}
}

// When it is available it carries its own window and its method, because it is
// not the report window and because a percentile without its method is worse
// than none.
func TestAvailableP95CarriesItsWindowAndMethod(t *testing.T) {
	t.Parallel()

	value := 940.0
	from, to := day0, day0.AddDate(0, 0, 7)
	p := TrailingP95(true, &value, from, to)

	if !p.Available || p.ValueMs == nil || *p.ValueMs != 940 {
		t.Fatalf("p95 = %+v, want 940 available", p)
	}
	if p.Method != MethodNearestRank {
		t.Errorf("method = %q, want %q", p.Method, MethodNearestRank)
	}
	if p.WindowStart == nil || !p.WindowStart.Equal(from) || p.WindowEnd == nil || !p.WindowEnd.Equal(to) {
		t.Error("p95 does not carry its own seven-day window; beside a 30-day average it would read as a contradiction")
	}
	if p.Reason != "" {
		t.Errorf("reason = %q, want empty when available", p.Reason)
	}
}

// Nothing measured at all: no average, no extremes, and emphatically not zero.
func TestNoSuccessfulChecksLeavesEveryLatencyFigureNull(t *testing.T) {
	t.Parallel()

	daily := []store.HistoryBucket{{Start: day0, Down: 100}, {Start: day0.AddDate(0, 0, 1), Unknown: 100}}
	l := ComputeLatency(Sum(daily), daily, nil)

	if l.AverageMs != nil {
		t.Errorf("average = %v, want nil", *l.AverageMs)
	}
	if l.BestDay != nil || l.WorstDay != nil {
		t.Error("extremes present with no successful check behind them")
	}
	if l.SampleCount != 0 {
		t.Errorf("sample count = %d, want 0", l.SampleCount)
	}
}

// The block ADR-006 fixed has five figures and a window minimum and maximum are
// not among them. Sum already refuses to carry them across monitors; this pins
// the same rule at the block, so that adding one later is a deliberate act
// against a failing test rather than a convenience nobody noticed.
func TestLatencyBlockExposesNoWindowExtremes(t *testing.T) {
	t.Parallel()

	low, high := 3.0, 9000.0
	total := store.HistoryBucket{
		ResponseTimeSum: 20000, ResponseTimeCount: 100,
		ResponseTimeMin: &low, ResponseTimeMax: &high,
	}
	l := ComputeLatency(total, []store.HistoryBucket{day(0, 20000, 100)}, nil)

	if l.AverageMs == nil || !closeTo(*l.AverageMs, 200) {
		t.Fatalf("average = %v, want 200", l.AverageMs)
	}
	// The window extremes on the input must not have found a way onto the block:
	// day-level extremes are the replacement, and they come from the series.
	if l.BestDay == nil || *l.BestDay.AverageMs != 200 {
		t.Errorf("best day = %v, want the day average of 200 rather than the window minimum of 3", l.BestDay)
	}
	if l.WorstDay == nil || *l.WorstDay.AverageMs != 200 {
		t.Errorf("worst day = %v, want the day average of 200 rather than the window maximum of 9000", l.WorstDay)
	}
}
