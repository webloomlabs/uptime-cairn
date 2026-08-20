// Command harness is Uptime Cairn's load-test gate.
//
// It exists before the product it measures, deliberately: "5,000 monitors on one
// install and the UI stays fast" is the project's central claim, and a claim that
// is only measured once the code is written is a claim discovered to be false too
// late to act on.
//
// Phase 0 has no server, so the SQLite target measures the schema directly —
// which is what the data model's open hypotheses actually need. Phase 1 adds the
// HTTP target and the scenarios stay as they are.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	var (
		targetName   = flag.String("target", "sqlite", "target to measure: sqlite | http")
		baseURL      = flag.String("base-url", "http://localhost:3000", "base URL for the http target when not spawning one")
		cairnBinary  = flag.String("cairn", "", "path to a cairn binary for the http target to start and stop itself")
		engineDir    = flag.String("engine-dir", "", "data directory for the spawned engine (default: a temp dir per scale)")
		partition    = flag.Bool("partition", true, "on the http target, fail every monitored endpoint at once and measure the response")
		verbose      = flag.Bool("v", false, "report progress while seeding")
		dbPath       = flag.String("db", "", "SQLite file to build (default: a temp file)")
		migrations   = flag.String("migrations", "../migrations/sqlite", "directory of .sql migrations")
		scalesFlag   = flag.String("scales", "500,5000", "comma-separated monitor counts, smallest first")
		iterations   = flag.Int("iterations", 200, "measured iterations per scenario")
		pageSize     = flag.Int("page-size", 25, "page size for list scenarios")
		writeSeconds = flag.Int("write-seconds", 5, "duration of the sustained heartbeat write test")
		rollupHours  = flag.Int("rollup-hours", 2, "hours of 1m rollups to seed per monitor")
		minWriteRate = flag.Float64("min-write-rate", 250, "required sustained heartbeats/sec at the largest scale")
		seed         = flag.Int64("seed", 1, "workload seed; fixed so runs are comparable")
		jsonOut      = flag.String("json", "", "also write the report as JSON to this path")
	)
	flag.Parse()

	scales, err := parseScales(*scalesFlag)
	if err != nil {
		fatal(err)
	}

	ctx := context.Background()
	scenarios := Scenarios(*pageSize)
	results := make([]ScaleResult, 0, len(scales))

	for _, scale := range scales {
		res, err := runScale(ctx, runConfig{
			targetName:   *targetName,
			baseURL:      *baseURL,
			cairnBinary:  *cairnBinary,
			engineDir:    *engineDir,
			partition:    *partition,
			verbose:      *verbose,
			dbPath:       *dbPath,
			migrations:   *migrations,
			scale:        scale,
			iterations:   *iterations,
			writeSeconds: *writeSeconds,
			rollupHours:  *rollupHours,
			seed:         *seed,
			scenarios:    scenarios,
		})
		if err != nil {
			fatal(err)
		}
		results = append(results, res)
	}

	findings := Evaluate(scenarios, results, *minWriteRate)
	report(scenarios, results, findings)

	if *jsonOut != "" {
		if err := writeJSON(*jsonOut, scenarios, results, findings); err != nil {
			fatal(err)
		}
	}

	for _, f := range findings {
		if f.Failed {
			fmt.Fprintln(os.Stderr, "\nLOAD TEST GATE: FAILED")
			os.Exit(1)
		}
	}
	fmt.Println("\nLOAD TEST GATE: PASSED")
}

type runConfig struct {
	targetName   string
	baseURL      string
	cairnBinary  string
	engineDir    string
	partition    bool
	verbose      bool
	dbPath       string
	migrations   string
	scale        int
	iterations   int
	writeSeconds int
	rollupHours  int
	seed         int64
	scenarios    []Scenario
}

func runScale(ctx context.Context, cfg runConfig) (ScaleResult, error) {
	res := ScaleResult{Scale: cfg.scale, Stats: map[string]*Stat{}}

	var target Target
	var sqliteTarget *SQLiteTarget
	switch cfg.targetName {
	case "sqlite":
		path := cfg.dbPath
		if path == "" {
			path = filepath.Join(os.TempDir(), fmt.Sprintf("cairn-loadtest-%d.db", cfg.scale))
		}
		sqliteTarget = NewSQLiteTarget(path, cfg.migrations)
		target = sqliteTarget
	case "http":
		dir := cfg.engineDir
		if dir != "" {
			dir = filepath.Join(dir, fmt.Sprintf("scale-%d", cfg.scale))
		}
		target = &HTTPTarget{
			BaseURL: cfg.baseURL,
			Binary:  cfg.cairnBinary,
			DataDir: dir,
			Verbose: cfg.verbose,
		}
	default:
		return res, fmt.Errorf("unknown target %q: expected sqlite or http", cfg.targetName)
	}
	defer func() { _ = target.Close() }()

	fmt.Printf("\n=== %s, %d monitors ===\n", target.Name(), cfg.scale)

	workload := GenerateWorkload(cfg.scale, cfg.seed)

	start := time.Now()
	if err := target.Setup(ctx, workload, cfg.rollupHours); err != nil {
		return res, fmt.Errorf("setup at %d monitors: %w", cfg.scale, err)
	}
	seeding := time.Since(start)
	res.SeedRate = float64(cfg.scale) / seeding.Seconds()
	fmt.Printf("seeded in %s (%.0f monitors/sec)\n", seeding.Round(time.Millisecond), res.SeedRate)

	writes, err := target.MeasureWrites(ctx, workload, cfg.writeSeconds)
	if err != nil {
		return res, fmt.Errorf("write test at %d monitors: %w", cfg.scale, err)
	}
	res.Writes = writes

	if cfg.partition {
		disruptor, ok := target.(Disruptor)
		switch {
		case !ok:
			// Said rather than skipped. A phase that vanishes silently is a
			// phase somebody assumes ran.
			fmt.Println("partition phase skipped: this target has no engine to react to a failing host")
		default:
			p, err := measurePartition(ctx, target, disruptor, workload)
			if err != nil {
				return res, fmt.Errorf("partition at %d monitors: %w", cfg.scale, err)
			}
			res.Partition = &p
		}
	}

	r := rand.New(rand.NewSource(cfg.seed))
	for _, sc := range cfg.scenarios {
		stat := &Stat{Name: sc.Name, Durations: make([]time.Duration, 0, cfg.iterations)}

		// One untimed pass so the first iteration's page-cache miss does not
		// land in the sample.
		if _, err := sc.Run(ctx, target, workload, r); err != nil {
			return res, fmt.Errorf("scenario %q: %w", sc.Name, err)
		}
		for i := 0; i < cfg.iterations; i++ {
			t0 := time.Now()
			rows, err := sc.Run(ctx, target, workload, r)
			elapsed := time.Since(t0)
			if err != nil {
				return res, fmt.Errorf("scenario %q: %w", sc.Name, err)
			}
			stat.Durations = append(stat.Durations, elapsed)
			stat.Rows = rows
		}
		res.Stats[sc.Name] = stat
	}

	if sqliteTarget != nil {
		res.DBBytes = sqliteTarget.DBSizeBytes()
	}
	return res, nil
}

// measurePartition breaks everything at once and watches the engine notice.
//
// This is the scenario the whole harness was missing. The read scenarios measure
// a system at rest; the write phase measures it in steady state. Neither touches
// the case the delivery queues were actually sized for — several thousand
// monitors transitioning inside one scheduler tick — and until now that size has
// been an argument in a comment rather than a measurement.
//
// Three numbers come out. How long the fleet takes to be marked down, which is
// the product promise. How much of the alert burst survived, which is the queue
// depth argument settled. And whether the probe shed anything, because shedding
// under a burst is correct behaviour that must still be visible: a probe quietly
// dropping results looks exactly like a target that recovered.
func measurePartition(ctx context.Context, target Target, d Disruptor, w *Workload) (PartitionResult, error) {
	var out PartitionResult
	total := int64(len(w.Monitors))

	before, err := d.Counters(ctx)
	if err != nil {
		return out, err
	}
	deliveredBefore := d.Deliveries()

	// The baseline matters. A realistic workload is not entirely healthy — a few
	// percent are down before anything is broken — and recovery means returning
	// to that, not to zero. Waiting for zero would hang forever and then be
	// reported as a failure to recover, which would be the harness's bug
	// attributed to the engine.
	baseline, err := settledBaseline(ctx, target)
	if err != nil {
		return out, err
	}
	out.BaselineDown = baseline

	fmt.Printf("partitioning: every monitored endpoint starts failing at once (%d were already down)\n",
		out.BaselineDown)
	if err := d.Partition(ctx, false); err != nil {
		return out, err
	}

	// Polled through the membership endpoint, which is exactly what it is for:
	// a cheap count for a filter, asked repeatedly. Using the monitor listing
	// instead would page through 5,000 rows to count them.
	down, elapsed, err := waitForDown(ctx, target, total, partitionDeadline(w))
	if err != nil {
		return out, err
	}
	out.DownCount = down
	out.TimeToDetect = elapsed
	out.Total = total

	fmt.Printf("recovering\n")
	if err := d.Partition(ctx, true); err != nil {
		return out, err
	}
	stillDown, recoverElapsed, err := waitForRecovery(ctx, target, out.BaselineDown, partitionDeadline(w))
	if err != nil {
		return out, err
	}
	out.RecoveredTo = total - stillDown
	out.TimeToRecover = recoverElapsed

	// A moment for the delivery queues to drain before the counters are read.
	// Alerting is fire-and-forget by design, so a count taken the instant the
	// last monitor recovers is a count of a queue still in flight.
	select {
	case <-ctx.Done():
		return out, ctx.Err()
	case <-time.After(3 * time.Second):
	}

	after, err := d.Counters(ctx)
	if err != nil {
		return out, err
	}
	out.AlertsPublished = after.AlertsPublished - before.AlertsPublished
	out.AlertsDropped = after.AlertsDropped - before.AlertsDropped
	out.WebhooksDropped = after.WebhookEventsDropped - before.WebhookEventsDropped
	out.WebhooksDelivered = d.Deliveries() - deliveredBefore
	out.ProbeShed = after.ProbeShedResults - before.ProbeShedResults
	out.ProbeSkipped = after.ProbeSkippedChecks - before.ProbeSkippedChecks
	out.Rejected = after.ResultsRejected - before.ResultsRejected

	fmt.Printf("detected %d/%d down in %s, recovered %d in %s\n",
		out.DownCount, out.Total, out.TimeToDetect.Round(time.Millisecond),
		out.RecoveredTo, out.TimeToRecover.Round(time.Millisecond))
	fmt.Printf("alert burst: %d published, %d dropped; webhooks %d delivered, %d dropped\n",
		out.AlertsPublished, out.AlertsDropped, out.WebhooksDelivered, out.WebhooksDropped)
	return out, nil
}

// partitionDeadline is how long the engine is given to notice. Three intervals
// plus a margin: one for the check in flight when the partition landed, one for
// the next scheduled round, and one for the ingest and state writes behind it.
func partitionDeadline(w *Workload) time.Duration {
	return time.Duration(monitorInterval)*time.Second*3 + 30*time.Second
}

// settledBaseline reads the pre-partition down count once it has stopped moving.
//
// One sample is not enough, and the failure it produces is the harness blaming
// the engine for the harness's own timing. The workload keeps a fixed proportion
// of endpoints permanently failing, but a monitor is `pending` until it has
// actually been checked once, and pending is neither up nor down. At 5,000
// monitors the first sweep is still working through the set when the write
// measurement ends — the probe reported 4,923 checks started against 5,000
// monitors — so a baseline taken there reads low by however many of the
// permanently-failing ones had not been reached yet.
//
// What that costs is not a slightly wrong number. The recovery target is
// total-minus-baseline, so a baseline one too low is a target one too high: a
// count that can never be reached, a wait that runs to its deadline, and a gate
// reporting that the engine failed to recover a monitor which was never up.
// This exact failure took a passing gate to FAILED with nothing wrong.
//
// Two agreeing samples a full interval apart. Cheap — it is the membership
// count, which exists to be asked repeatedly — and it converts a coin flip into
// a measurement.
func settledBaseline(ctx context.Context, target Target) (int64, error) {
	const interval = time.Duration(monitorInterval) * time.Second

	deadline := time.Now().Add(3 * interval)
	last := int64(-1)
	for {
		res, err := target.Membership(ctx, ListQuery{Status: "down"})
		if err != nil {
			return 0, err
		}
		if res.Count == last {
			return res.Count, nil
		}
		if time.Now().After(deadline) {
			// Reported rather than errored, and it is worth saying out loud: a
			// baseline that never settles means the workload is still churning,
			// and every partition number below it is measured against a moving
			// floor.
			fmt.Printf("  baseline did not settle: %d down, was %d one interval ago\n", res.Count, last)
			return res.Count, nil
		}
		last = res.Count

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func waitForDown(ctx context.Context, target Target, total int64, within time.Duration) (int64, time.Duration, error) {
	start := time.Now()
	deadline := start.Add(within)

	var last int64
	for time.Now().Before(deadline) {
		res, err := target.Membership(ctx, ListQuery{Status: "down"})
		if err != nil {
			return last, time.Since(start), err
		}
		last = res.Count
		if last >= total {
			return last, time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return last, time.Since(start), ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	// Returned rather than errored: "it marked 4,900 of 5,000 down in 70
	// seconds" is a finding the gate should report, not a crash.
	return last, time.Since(start), nil
}

// waitForRecovery waits for the down count to fall back to the baseline — the
// monitors that were failing before the partition and are meant to keep failing.
func waitForRecovery(ctx context.Context, target Target, baseline int64, within time.Duration) (int64, time.Duration, error) {
	start := time.Now()
	deadline := start.Add(within)

	last := int64(-1)
	for time.Now().Before(deadline) {
		res, err := target.Membership(ctx, ListQuery{Status: "down"})
		if err != nil {
			return last, time.Since(start), err
		}
		last = res.Count
		if last <= baseline {
			return last, time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return last, time.Since(start), ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return last, time.Since(start), nil
}

func report(scenarios []Scenario, results []ScaleResult, findings []Finding) {
	fmt.Println("\n=== Results ===")

	header := fmt.Sprintf("%-38s", "scenario")
	for _, res := range results {
		header += fmt.Sprintf(" %14s", fmt.Sprintf("%d mon p95", res.Scale))
	}
	if len(results) > 1 {
		header += fmt.Sprintf(" %8s", "growth")
	}
	header += "  rows"
	fmt.Println(header)
	fmt.Println(strings.Repeat("-", len(header)+4))

	for _, sc := range scenarios {
		line := fmt.Sprintf("%-38s", sc.Name)
		var first, last time.Duration
		rows := 0
		for i, res := range results {
			st, ok := res.Stats[sc.Name]
			if !ok {
				line += fmt.Sprintf(" %14s", "-")
				continue
			}
			line += fmt.Sprintf(" %14s", st.P95().Round(time.Microsecond))
			if i == 0 {
				first = st.P95()
			}
			last = st.P95()
			rows = st.Rows
		}
		if len(results) > 1 && first > 0 {
			line += fmt.Sprintf(" %7.1fx", float64(last)/float64(first))
		}
		line += fmt.Sprintf("  %d", rows)
		fmt.Println(line)
	}

	fmt.Println()
	for _, res := range results {
		line := fmt.Sprintf("%d monitors: %.1f heartbeats/sec", res.Scale, res.Writes.Rate)
		if res.Writes.Expected > 0 {
			line += fmt.Sprintf(" against %.1f/sec implied by the schedule", res.Writes.Expected)
		}
		if res.Writes.TargetRequests > 0 {
			line += fmt.Sprintf(", %d requests seen by the checked endpoint", res.Writes.TargetRequests)
		}
		if res.Writes.Redelivered > 0 {
			line += fmt.Sprintf(", %d results redelivered", res.Writes.Redelivered)
		}
		if res.DBBytes > 0 {
			line += fmt.Sprintf(", database %.1f MiB", float64(res.DBBytes)/(1024*1024))
		}
		if res.SeedRate > 0 {
			line += fmt.Sprintf("; seeded at %.0f monitors/sec", res.SeedRate)
		}
		// The method travels with the number. Two rates measured different ways
		// printed in the same column without saying so is how a report becomes
		// misleading without anybody writing anything false.
		fmt.Println(line)
		fmt.Printf("  %s\n", res.Writes.Method)
	}

	for _, res := range results {
		p := res.Partition
		if p == nil {
			continue
		}
		fmt.Printf("\n%d monitors, total partition:\n", res.Scale)
		fmt.Printf("  detected   %d/%d down in %s\n", p.DownCount, p.Total, p.TimeToDetect.Round(time.Millisecond))
		fmt.Printf("  recovered  %d/%d up in %s\n", p.RecoveredTo, p.Total, p.TimeToRecover.Round(time.Millisecond))
		fmt.Printf("  alerts     %d published, %d shed\n", p.AlertsPublished, p.AlertsDropped)
		fmt.Printf("  webhooks   %d delivered, %d shed\n", p.WebhooksDelivered, p.WebhooksDropped)
		fmt.Printf("  probe      %d results shed, %d checks skipped\n", p.ProbeShed, p.ProbeSkipped)
	}

	if len(findings) == 0 {
		fmt.Println("\nNo threshold violations.")
		return
	}
	fmt.Println("\n=== Findings ===")
	for _, f := range findings {
		status := "WARN"
		if f.Failed {
			status = "FAIL"
		}
		fmt.Printf("[%s] %s: %s\n", status, f.Scenario, f.Detail)
	}
}

func writeJSON(path string, scenarios []Scenario, results []ScaleResult, findings []Finding) error {
	type scenarioJSON struct {
		Scale int    `json:"scale"`
		Name  string `json:"scenario"`
		P50Us int64  `json:"p50_us"`
		P95Us int64  `json:"p95_us"`
		Rows  int    `json:"rows"`
	}
	out := struct {
		Scenarios []scenarioJSON `json:"scenarios"`
		Findings  []Finding      `json:"findings"`
		WriteRate map[string]any `json:"write_rate"`
		Partition map[string]any `json:"partition,omitempty"`
	}{Findings: findings, WriteRate: map[string]any{}, Partition: map[string]any{}}

	for _, res := range results {
		for _, sc := range scenarios {
			if st, ok := res.Stats[sc.Name]; ok {
				out.Scenarios = append(out.Scenarios, scenarioJSON{
					Scale: res.Scale, Name: sc.Name,
					P50Us: st.P50().Microseconds(),
					P95Us: st.P95().Microseconds(),
					Rows:  st.Rows,
				})
			}
		}
		out.WriteRate[strconv.Itoa(res.Scale)] = map[string]any{
			"rate":     res.Writes.Rate,
			"expected": res.Writes.Expected,
			"method":   res.Writes.Method,
			"shed":     res.Writes.Shed,
		}
		if res.Partition != nil {
			out.Partition[strconv.Itoa(res.Scale)] = res.Partition
		}
	}

	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func parseScales(s string) ([]int, error) {
	var scales []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid scale %q", part)
		}
		scales = append(scales, n)
	}
	if len(scales) == 0 {
		return nil, fmt.Errorf("no scales given")
	}
	return scales, nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "harness: %v\n", err)
	os.Exit(1)
}
