package main

import (
	"strings"
	"testing"
	"time"
)

// The harness has one job and one way to fail at it: report a number that is
// wrong. Everything tested here is a place where a silent mistake would produce
// a passing gate over a measurement that means nothing — which is worse than no
// gate, because it lets an exit criterion be ticked.

func TestParseMetricsIgnoresLabelsAndComments(t *testing.T) {
	t.Parallel()

	body := `# HELP cairn_heartbeats_written_total Heartbeats durably written.
# TYPE cairn_heartbeats_written_total counter
cairn_heartbeats_written_total 45626

# TYPE cairn_results_ingested_total counter
cairn_results_ingested_total 45700
cairn_results_rejected_total 3
cairn_alerts_published_total 9682
cairn_alerts_dropped_total 0
cairn_probe_shed_results_total{probe_id="00000000-0000-7000-8000-000000000002",probe="embedded"} 12
cairn_probe_skipped_checks_total{probe_id="00000000-0000-7000-8000-000000000002",probe="embedded"} 7
cairn_probe_buffered_results{probe_id="00000000-0000-7000-8000-000000000002",probe="embedded"} 20
cairn_webhook_events_dropped_total 4
cairn_monitor_status{monitor_id="x",monitor="Checkout",type="http"} 0
`

	got := parseMetrics(body)

	for _, tc := range []struct {
		name string
		got  uint64
		want uint64
	}{
		{"heartbeats", got.HeartbeatsWritten, 45626},
		{"ingested", got.ResultsIngested, 45700},
		{"rejected", got.ResultsRejected, 3},
		{"alerts published", got.AlertsPublished, 9682},
		{"alerts dropped", got.AlertsDropped, 0},
		{"probe shed", got.ProbeShedResults, 12},
		{"probe skipped", got.ProbeSkippedChecks, 7},
		{"probe buffered", got.ProbeBufferedItems, 20},
		{"webhooks dropped", got.WebhookEventsDropped, 4},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// A metric that vanishes from /metrics must not silently read as zero and then
// be reported as a delta of zero — "nothing happened" and "the series is gone"
// are different, and only one of them is a passing gate.
func TestParseMetricsLeavesMissingSeriesAtZero(t *testing.T) {
	t.Parallel()

	got := parseMetrics("cairn_heartbeats_written_total 10\n")
	if got.HeartbeatsWritten != 10 {
		t.Fatalf("heartbeats = %d, want 10", got.HeartbeatsWritten)
	}
	if got.AlertsPublished != 0 {
		t.Fatalf("a series that was not in the body read as %d", got.AlertsPublished)
	}
}

func TestHexUUIDRoundTrips(t *testing.T) {
	t.Parallel()

	const dashed = "01a019d0-e770-7030-803e-e77d4898213a"

	raw, err := parseUUID(dashed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(raw) != 16 {
		t.Fatalf("parsed %d bytes, want 16", len(raw))
	}
	if got := hexUUID(raw); got != dashed {
		t.Fatalf("round trip = %q, want %q", got, dashed)
	}

	if _, err := parseUUID("not-a-uuid"); err == nil {
		t.Error("a malformed identifier parsed without complaint")
	}
}

// The two targets measure different things, and the gate has to apply the
// assertion that fits. Getting this backwards passes an engine that is minutes
// behind schedule, because it is writing as fast as it can.
func TestWriteAssertionFollowsTheMeasurement(t *testing.T) {
	t.Parallel()

	driven := ScaleResult{Scale: 5000, Writes: WriteResult{Rate: 40000, Method: "driven"}}
	if findings := evaluateWrites(driven, 250); len(findings) != 0 {
		t.Errorf("a driven rate above the floor produced %v", findings)
	}
	slow := ScaleResult{Scale: 5000, Writes: WriteResult{Rate: 100, Method: "driven"}}
	if findings := evaluateWrites(slow, 250); len(findings) != 1 || !findings[0].Failed {
		t.Errorf("a driven rate below the floor produced %v", findings)
	}

	// An observed rate is judged against what the schedule implies, not against
	// an absolute floor: 25/sec is correct for 500 monitors and would fail the
	// driven test.
	observed := ScaleResult{Scale: 500, Writes: WriteResult{Rate: 24.6, Expected: 25, Method: "observed"}}
	if findings := evaluateWrites(observed, 250); len(findings) != 0 {
		t.Errorf("an engine keeping up with its schedule produced %v", findings)
	}

	behind := ScaleResult{Scale: 5000, Writes: WriteResult{Rate: 120, Expected: 250, Method: "observed"}}
	findings := evaluateWrites(behind, 250)
	if len(findings) != 1 || !findings[0].Failed {
		t.Fatalf("an engine at half its schedule produced %v", findings)
	}

	// Above the schedule is a backlog draining, not headroom. Reported, never
	// failed — catching up is the right response to having been behind.
	ahead := ScaleResult{Scale: 5000, Writes: WriteResult{Rate: 499, Expected: 250, Method: "observed"}}
	findings = evaluateWrites(ahead, 250)
	if len(findings) != 1 {
		t.Fatalf("a draining backlog produced %v", findings)
	}
	if findings[0].Failed {
		t.Error("draining a backlog was reported as a failure")
	}
}

func TestPartitionAssertions(t *testing.T) {
	t.Parallel()

	clean := ScaleResult{Scale: 5000, Partition: &PartitionResult{
		Total: 5000, BaselineDown: 159, DownCount: 5000, RecoveredTo: 4841,
		TimeToDetect: 21 * time.Second, TimeToRecover: 21 * time.Second,
		AlertsPublished: 9682,
	}}
	if findings := evaluatePartition(clean); len(findings) != 0 {
		t.Fatalf("a clean partition produced %v", findings)
	}

	// The failure that matters most: monitors still reporting healthy after
	// every one of their targets has failed.
	missed := ScaleResult{Scale: 5000, Partition: &PartitionResult{
		Total: 5000, DownCount: 4900, RecoveredTo: 5000, TimeToDetect: 60 * time.Second,
	}}
	findings := evaluatePartition(missed)
	if len(findings) == 0 || !findings[0].Failed {
		t.Fatalf("100 monitors missing the outage produced %v", findings)
	}

	// A queue sized by argument that drops under the exact burst it was sized
	// for means the argument was wrong.
	dropped := ScaleResult{Scale: 5000, Partition: &PartitionResult{
		Total: 5000, DownCount: 5000, RecoveredTo: 5000,
		TimeToDetect: 21 * time.Second, AlertsPublished: 9682, AlertsDropped: 1200,
	}}
	var found bool
	for _, f := range evaluatePartition(dropped) {
		if f.Scenario == "partition: alert queue" && f.Failed {
			found = true
		}
	}
	if !found {
		t.Error("a shed alert burst was not reported as a failure")
	}
}

// RangeBounded must not fire on a sample too small to read. It did, on one
// bucket against two, which is run-to-run variation rather than growth.
func TestRangeBoundedNeedsASampleWorthReading(t *testing.T) {
	t.Parallel()

	scenarios := []Scenario{{Name: "history", RangeBounded: true}}
	tiny := []ScaleResult{
		{Scale: 500, Stats: map[string]*Stat{"history": {Rows: 1, Durations: []time.Duration{time.Millisecond}}}},
		{Scale: 5000, Stats: map[string]*Stat{"history": {Rows: 2, Durations: []time.Duration{time.Millisecond}}}},
	}
	if findings := Evaluate(scenarios, tiny, 0); len(findings) != 0 {
		t.Errorf("one bucket against two produced %v", findings)
	}

	real := []ScaleResult{
		{Scale: 500, Stats: map[string]*Stat{"history": {Rows: 120, Durations: []time.Duration{time.Millisecond}}}},
		{Scale: 5000, Stats: map[string]*Stat{"history": {Rows: 1200, Durations: []time.Duration{time.Millisecond}}}},
	}
	findings := Evaluate(scenarios, real, 0)
	if len(findings) != 1 || !findings[0].Failed {
		t.Errorf("a tenfold growth in a real sample produced %v", findings)
	}
}

func TestViewportBoundedIsExact(t *testing.T) {
	t.Parallel()

	scenarios := []Scenario{{Name: "list", ViewportBounded: true}}
	results := []ScaleResult{
		{Scale: 500, Stats: map[string]*Stat{"list": {Rows: 25, Durations: []time.Duration{time.Millisecond}}}},
		{Scale: 5000, Stats: map[string]*Stat{"list": {Rows: 26, Durations: []time.Duration{time.Millisecond}}}},
	}
	// One extra row is a payload that grew with the install, which is the thing
	// ADR-004 forbids. No tolerance, because a page limit is exact.
	if findings := Evaluate(scenarios, results, 0); len(findings) != 1 {
		t.Fatalf("25 rows against 26 produced %v", findings)
	}
}

func TestPercentilesAreOrdered(t *testing.T) {
	t.Parallel()

	stat := &Stat{}
	for i := 100; i > 0; i-- {
		stat.Durations = append(stat.Durations, time.Duration(i)*time.Millisecond)
	}
	if stat.P50() > stat.P95() {
		t.Fatalf("p50 %s exceeds p95 %s", stat.P50(), stat.P95())
	}
	if stat.P95() != 95*time.Millisecond {
		t.Errorf("p95 = %s, want 95ms", stat.P95())
	}
}

// A growth ratio between two runs that returned different numbers of rows is
// not a growth ratio. This is the check that turned a passing gate red on a
// history scenario whose sample was one bucket against two — the same build
// produced 257µs and 1.618ms minutes apart, purely on where the clock fell.
func TestGrowthIsNotJudgedOnIncomparableSamples(t *testing.T) {
	t.Parallel()

	scenarios := []Scenario{{Name: "history", RangeBounded: true, MaxGrowth: 3.0}}

	coinFlip := []ScaleResult{
		{Scale: 500, Stats: map[string]*Stat{"history": {Rows: 1, Durations: []time.Duration{285 * time.Microsecond}}}},
		{Scale: 5000, Stats: map[string]*Stat{"history": {Rows: 2, Durations: []time.Duration{1489 * time.Microsecond}}}},
	}
	findings := Evaluate(scenarios, coinFlip, 0)
	if len(findings) != 1 {
		t.Fatalf("one bucket against two produced %v", findings)
	}
	if findings[0].Failed {
		t.Error("a sample too small to read was reported as a failure")
	}
	if !strings.Contains(findings[0].Detail, "not compared") {
		t.Errorf("detail = %q, want it to say the comparison was not made", findings[0].Detail)
	}

	// Same row count at both scales: the comparison is valid and the cap applies.
	real := []ScaleResult{
		{Scale: 500, Stats: map[string]*Stat{"history": {Rows: 2, Durations: []time.Duration{285 * time.Microsecond}}}},
		{Scale: 5000, Stats: map[string]*Stat{"history": {Rows: 2, Durations: []time.Duration{1489 * time.Microsecond}}}},
	}
	findings = Evaluate(scenarios, real, 0)
	if len(findings) != 1 || !findings[0].Failed {
		t.Fatalf("a 5x growth over an equal sample produced %v", findings)
	}

	// And a large sample at both ends, where one row either way cannot be the
	// whole signal.
	big := []ScaleResult{
		{Scale: 500, Stats: map[string]*Stat{"history": {Rows: 60, Durations: []time.Duration{285 * time.Microsecond}}}},
		{Scale: 5000, Stats: map[string]*Stat{"history": {Rows: 61, Durations: []time.Duration{400 * time.Microsecond}}}},
	}
	for _, f := range Evaluate(scenarios, big, 0) {
		if strings.Contains(f.Detail, "not compared") {
			t.Errorf("a sixty-bucket sample was dismissed as too small: %v", f)
		}
	}
}
