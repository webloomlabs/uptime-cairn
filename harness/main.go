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
		baseURL      = flag.String("base-url", "http://localhost:3000", "base URL for the http target")
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
		target = &HTTPTarget{BaseURL: cfg.baseURL}
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
	fmt.Printf("seeded in %s\n", time.Since(start).Round(time.Millisecond))

	rate, err := measureWriteRate(ctx, target, workload, cfg.writeSeconds)
	if err != nil {
		return res, fmt.Errorf("write test at %d monitors: %w", cfg.scale, err)
	}
	res.WriteRate = rate

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

// measureWriteRate drives sustained batched heartbeat writes. Batching is the
// contract, not a shortcut: the data model requires one transaction per scheduler
// tick because SQLite fsyncs per commit.
func measureWriteRate(ctx context.Context, target Target, w *Workload, seconds int) (float64, error) {
	if seconds <= 0 {
		return 0, nil
	}
	r := rand.New(rand.NewSource(7))
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)

	// One tick writes every monitor's result, which is what the scheduler does.
	written := 0
	at := w.BaseTime
	start := time.Now()
	for time.Now().Before(deadline) {
		batch := w.HeartbeatBatch(at, 0, len(w.Monitors), r)
		if err := target.WriteHeartbeats(ctx, batch); err != nil {
			return 0, err
		}
		written += len(batch)
		at = at.Add(20 * time.Second)
	}
	elapsed := time.Since(start).Seconds()
	rate := float64(written) / elapsed
	fmt.Printf("wrote %d heartbeats in %.1fs = %.0f/sec\n", written, elapsed, rate)
	return rate, nil
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
		fmt.Printf("%d monitors: %.0f heartbeats/sec sustained, database %.1f MiB\n",
			res.Scale, res.WriteRate, float64(res.DBBytes)/(1024*1024))
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
	}{Findings: findings, WriteRate: map[string]any{}}

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
		out.WriteRate[strconv.Itoa(res.Scale)] = res.WriteRate
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
