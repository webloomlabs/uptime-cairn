package render

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

var (
	fixedRunID      = model.ID{1}
	fixedTemplateID = model.ID{2}
	fixedMonitorID  = model.ID{3}
	march           = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
)

// dailyUptime mirrors what Build fills the section with, so the fixture exercises
// the shape the renderers actually receive rather than a convenient subset.
func dailyUptime(daily []store.HistoryBucket) []report.DayUptime {
	out := make([]report.DayUptime, 0, len(daily))
	for _, b := range daily {
		out = append(out, report.DayUptime{
			Date:   b.Start,
			Uptime: report.ComputeUptime(b, report.MaintenanceExclude),
		})
	}
	return out
}

// sample is a document with every awkward case in it: a null ratio, a gap day, a
// breach, an unavailable percentile, and a monitor that observed nothing.
func sample() report.Document {
	daily := []store.HistoryBucket{
		{Start: march, Up: 900, Down: 100, ResponseTimeSum: 100000, ResponseTimeCount: 900},
		{Start: march.AddDate(0, 0, 1), Unknown: 1000},
		{Start: march.AddDate(0, 0, 2), Up: 1000, ResponseTimeSum: 150000, ResponseTimeCount: 1000},
	}
	total := report.Sum(daily)

	uptime := report.ComputeUptime(total, report.MaintenanceExclude)
	target := 500
	latency := report.ComputeLatency(total, daily, &target)
	latency.P95 = &report.P95{Reason: report.ReasonScopeTooLarge}

	sla := report.ComputeSLA(uptime, report.Target{Percent: 99.9, Source: report.TargetFromMonitor}, 72*time.Hour,
		report.DowntimeSeconds(daily, report.MaintenanceExclude))

	return report.Document{
		Meta: report.Meta{
			SchemaVersion:    report.SchemaVersion,
			GeneratedAt:      time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
			ReportRunID:      fixedRunID,
			ReportTemplateID: fixedTemplateID,
			TemplateName:     "Acme monthly",
			PeriodStart:      march,
			PeriodEnd:        march.AddDate(0, 0, 3),
			Timezone:         "Australia/Sydney",
			Resolution:       report.Resolution{Tier: "1d", RequestedTier: "1m", Downgraded: true},
		},
		Scope: report.ScopeSummary{MonitorCount: 1},
		Summary: &report.Estate{
			Uptime:       uptime,
			ResponseTime: report.ComputeLatency(total, daily, &target),
		},
		Monitors: []report.MonitorSection{{
			MonitorID:    fixedMonitorID,
			Name:         "checkout",
			Type:         "http",
			Uptime:       uptime,
			DailyUptime:  dailyUptime(daily),
			SLA:          &sla,
			ResponseTime: latency,
			Breaches:     report.ComputeBreaches(daily, report.MaintenanceExclude),
		}},
	}
}

// ADR-007 item 6: the same model rendered twice is byte-identical. For these two
// formats that is the whole of the requirement, and it is the property that makes
// a re-run after a correction comparable to the artifact it replaces.
func TestRenderersAreDeterministic(t *testing.T) {
	t.Parallel()

	doc := sample()
	for _, tc := range []struct {
		name   string
		render func(report.Document) ([]byte, error)
	}{
		{"json", JSON},
		{"csv", CSV},
	} {
		first, err := tc.render(doc)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for i := 0; i < 5; i++ {
			again, err := tc.render(doc)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !bytes.Equal(first, again) {
				t.Fatalf("%s render %d differs from the first", tc.name, i)
			}
		}
	}
}

// The keys are the published contract — a BI tool binds to these names and
// outlives several releases. Checked against the spec's property names by hand
// and pinned here so a rename is a deliberate act.
func TestJSONKeysMatchTheContract(t *testing.T) {
	t.Parallel()

	raw, err := JSON(sample())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"meta", "scope", "summary", "monitors", "incidents"} {
		if _, ok := got[key]; !ok {
			t.Errorf("top level is missing %q", key)
		}
	}

	meta := got["meta"].(map[string]any)
	for _, key := range []string{
		"schema_version", "generated_at", "report_run_id", "report_template_id",
		"template_name", "period_start", "period_end", "timezone", "resolution",
	} {
		if _, ok := meta[key]; !ok {
			t.Errorf("meta is missing %q", key)
		}
	}
	if meta["schema_version"].(float64) != float64(report.SchemaVersion) {
		t.Errorf("schema_version = %v, want %d", meta["schema_version"], report.SchemaVersion)
	}

	section := got["monitors"].([]any)[0].(map[string]any)
	uptime := section["uptime"].(map[string]any)
	for _, key := range []string{
		"uptime_ratio", "maintenance_handling", "denominator", "observed_checks",
		"up_checks", "down_checks", "maintenance_checks", "unknown_checks",
		"skipped_checks", "unobserved_share",
	} {
		if _, ok := uptime[key]; !ok {
			t.Errorf("uptime block is missing %q", key)
		}
	}
	// The denominator is stated in the machine-readable output, not only on the
	// rendered face. §4.3 requires the report to say what it counted.
	if uptime["denominator"] != "observed_checks" {
		t.Errorf("denominator = %v, want observed_checks", uptime["denominator"])
	}

	rt := section["response_time"].(map[string]any)
	for _, key := range []string{
		"average_ms", "sample_count", "daily", "best_day", "worst_day",
		"target_ms", "days_over_target", "dates_over_target", "p95",
	} {
		if _, ok := rt[key]; !ok {
			t.Errorf("response_time block is missing %q", key)
		}
	}

	sla := section["sla"].(map[string]any)
	for _, key := range []string{
		"target_percent", "target_source", "actual_percent", "met",
		"error_budget_seconds", "error_budget_consumed_seconds",
		"error_budget_remaining_seconds", "error_budget_consumed_ratio",
		"burn_rate", "breaches",
	} {
		if _, ok := sla[key]; !ok {
			t.Errorf("sla block is missing %q", key)
		}
	}
}

// A null has to survive being serialised. Zero is a claim — of total downtime,
// or of an instantaneous response — and JSON is where a careless renderer makes
// that claim on the product's behalf.
func TestNullsSurviveAsNullsNotZeros(t *testing.T) {
	t.Parallel()

	// A monitor that observed nothing at all.
	doc := sample()
	empty := report.ComputeUptime(store.HistoryBucket{Unknown: 100}, report.MaintenanceExclude)
	doc.Monitors[0].Uptime = empty
	doc.Monitors[0].ResponseTime = report.ComputeLatency(store.HistoryBucket{}, nil, nil)
	doc.Monitors[0].SLA = nil

	raw, err := JSON(doc)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	section := got["monitors"].([]any)[0].(map[string]any)

	if v := section["uptime"].(map[string]any)["uptime_ratio"]; v != nil {
		t.Errorf("uptime_ratio = %v, want null", v)
	}
	if v := section["response_time"].(map[string]any)["average_ms"]; v != nil {
		t.Errorf("average_ms = %v, want null", v)
	}
	if v := section["response_time"].(map[string]any)["days_over_target"]; v != nil {
		t.Errorf("days_over_target = %v, want null with no target set", v)
	}
	if v := section["sla"]; v != nil {
		t.Errorf("sla = %v, want null with no target", v)
	}
	// The p95 object is absent rather than present-and-unavailable when the
	// figure does not apply.
	if v := section["response_time"].(map[string]any)["p95"]; v != nil {
		t.Errorf("p95 = %v, want null", v)
	}
}

// An unavailable percentile always states its reason, which is the sentence the
// spec now carries and the reason scope_too_large exists.
func TestUnavailablePercentileSerialisesItsReason(t *testing.T) {
	t.Parallel()

	raw, err := JSON(sample())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	p95 := got["monitors"].([]any)[0].(map[string]any)["response_time"].(map[string]any)["p95"].(map[string]any)
	if p95["available"] != false {
		t.Errorf("available = %v, want false", p95["available"])
	}
	if p95["unavailable_reason"] != report.ReasonScopeTooLarge {
		t.Errorf("unavailable_reason = %v, want %q", p95["unavailable_reason"], report.ReasonScopeTooLarge)
	}
	if p95["method"] != nil {
		t.Errorf("method = %v, want null on an unavailable figure", p95["method"])
	}
}

// A well-formed CSV: one header, every row the same width, a discriminator in
// the first column. Anything else and the first thing that opens it — a
// spreadsheet — gets it wrong.
func TestCSVIsWellFormedAndDiscriminated(t *testing.T) {
	t.Parallel()

	raw, err := CSV(sample())
	if err != nil {
		t.Fatal(err)
	}

	records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatalf("not parseable as CSV: %v", err)
	}
	if len(records) < 2 {
		t.Fatal("no rows")
	}
	if strings.Join(records[0], ",") != strings.Join(csvHeader, ",") {
		t.Errorf("header = %v", records[0])
	}

	kinds := map[string]int{}
	for _, row := range records[1:] {
		if len(row) != len(csvHeader) {
			t.Fatalf("row width %d, want %d: %v", len(row), len(csvHeader), row)
		}
		kinds[row[0]]++
	}
	if kinds[RowEstateTotal] != 1 || kinds[RowMonitorTotal] != 1 {
		t.Errorf("row kinds = %v, want one estate total and one monitor total", kinds)
	}
	if kinds[RowDaily] != 3 {
		t.Errorf("daily rows = %d, want 3", kinds[RowDaily])
	}
}

// The place a figure from this product is most likely to end up wrong in front
// of a client: a null written as 0 becomes a day of total downtime the moment
// somebody charts the column.
func TestCSVWritesNullsAsEmptyNotZero(t *testing.T) {
	t.Parallel()

	doc := sample()
	doc.Summary = nil
	doc.Monitors[0].Uptime = report.ComputeUptime(store.HistoryBucket{Unknown: 10}, report.MaintenanceExclude)

	raw, err := CSV(doc)
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}

	col := map[string]int{}
	for i, name := range records[0] {
		col[name] = i
	}
	total := records[1]
	if total[col["row_type"]] != RowMonitorTotal {
		t.Fatalf("first data row = %s, want %s", total[col["row_type"]], RowMonitorTotal)
	}
	if total[col["uptime_ratio"]] != "" {
		t.Errorf("uptime_ratio = %q, want empty — 0 would chart as total downtime", total[col["uptime_ratio"]])
	}

	// The gap day's latency is empty too, for the same reason: zero would chart
	// as an instantaneous response.
	for _, row := range records[1:] {
		if row[col["row_type"]] == RowDaily && row[col["date"]] == "2026-03-02" {
			if row[col["response_time_avg_ms"]] != "" {
				t.Errorf("gap day average = %q, want empty", row[col["response_time_avg_ms"]])
			}
		}
	}
}

// No exponents. A ratio rendered as 9.99e-01 is read as text by at least one
// spreadsheet somebody will use, and that is a support conversation.
func TestCSVNumbersAreNotInExponentNotation(t *testing.T) {
	t.Parallel()

	raw, err := CSV(sample())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsAny(raw, "eE") {
		for _, line := range strings.Split(string(raw), "\n") {
			for _, field := range strings.Split(line, ",") {
				if strings.ContainsAny(field, "eE") && strings.ContainsAny(field, "0123456789") &&
					!strings.Contains(field, "-") && strings.Count(field, ".") > 0 {
					t.Errorf("field %q looks like exponent notation", field)
				}
			}
		}
	}
}

// The maintenance policy travels on every row, because the ratio beside it is
// meaningless without it and a CSV has no footnotes.
func TestCSVCarriesTheMaintenancePolicyOnEveryRow(t *testing.T) {
	t.Parallel()

	doc := sample()
	doc.Monitors[0].Uptime.MaintenanceHandling = report.MaintenanceCountAsDown

	raw, err := CSV(doc)
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, name := range records[0] {
		col[name] = i
	}
	for _, row := range records[1:] {
		if row[col["row_type"]] == RowEstateTotal {
			continue
		}
		if row[col["maintenance_handling"]] == "" {
			t.Errorf("row %v carries no maintenance policy", row[:4])
		}
	}
}
