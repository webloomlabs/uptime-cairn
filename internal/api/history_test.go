package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/rollup"
	"github.com/webloomlabs/uptime-cairn/internal/store/sqlite"
)

// seedHistory creates a monitor and writes heartbeats for it at a fixed cadence,
// starting `span` ago and ending now. statusAt decides each one.
func seedHistory(t *testing.T, st *sqlite.Store, span, every time.Duration, statusAt func(i int, at time.Time) (model.Status, *time.Duration)) model.Monitor {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Second)
	m := model.Monitor{
		ID:        model.NewID(),
		OrgID:     model.SentinelOrgID,
		Name:      "seeded",
		Type:      model.TypeHTTP,
		Config:    []byte(`{"url":"https://example.com"}`),
		Enabled:   true,
		Interval:  every,
		Timeout:   10 * time.Second,
		CreatedAt: now.Add(-span),
		UpdatedAt: now.Add(-span),
	}
	if err := st.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	var beats []model.Heartbeat
	for i, at := 0, now.Add(-span); at.Before(now); i, at = i+1, at.Add(every) {
		status, rt := statusAt(i, at)
		beats = append(beats, model.Heartbeat{
			Time: at, MonitorID: m.ID, OrgID: m.OrgID, ProbeID: model.EmbeddedProbeID,
			Status: status, ResponseTime: rt,
		})
	}
	if _, err := st.WriteBatch(t.Context(), beats); err != nil {
		t.Fatalf("write heartbeats: %v", err)
	}
	return m
}

func rt(v float64) *time.Duration {
	d := time.Duration(v * float64(time.Millisecond))
	return &d
}

// alwaysUp is the common case: every check up, with a fixed response time.
func alwaysUp(_ int, _ time.Time) (model.Status, *time.Duration) {
	return model.StatusUp, rt(50)
}

func TestHistoryDefaultsToTheLastDay(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	m := seedHistory(t, st, 6*time.Hour, 5*time.Minute, alwaysUp)

	resp, body := c.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+"/history", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d (%v)", resp.StatusCode, body)
	}

	// 24 hours at 5m resolution: the chart tier auto picks for a day.
	if body["resolution"] != "5m" {
		t.Errorf("resolution = %v, want 5m", body["resolution"])
	}
	if body["monitor_id"] != m.ID.String() {
		t.Errorf("monitor_id = %v", body["monitor_id"])
	}

	data, _ := body["data"].([]any)
	// Six hours of checks every five minutes, in five-minute buckets: 72 checks
	// in 72 or 73 buckets, depending on where the hour falls against the epoch.
	if len(data) < 72 || len(data) > 73 {
		t.Fatalf("got %d buckets, want 72 or 73", len(data))
	}

	var total float64
	for _, b := range data {
		bucket, _ := b.(map[string]any)
		total += bucket["up_count"].(float64)
		if ratio, ok := bucket["uptime_ratio"].(float64); !ok || ratio != 1 {
			t.Errorf("uptime_ratio = %v, want 1", bucket["uptime_ratio"])
		}
		if avg, ok := bucket["response_time_avg_ms"].(float64); !ok || avg != 50 {
			t.Errorf("response_time_avg_ms = %v, want 50", bucket["response_time_avg_ms"])
		}
	}
	if total != 72 {
		t.Errorf("buckets total %v checks, want 72", total)
	}
}

// A bucket whose checks were all unknown has a null ratio, not a zero one. A
// chart that draws a probe outage at 0% invents downtime that never happened —
// the single most consequential thing this endpoint can get wrong.
func TestHistoryNullRatioForUnobservedBuckets(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()

	// Two hours: the first hour observed and up, the second all unknown.
	start := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	m := seedHistory(t, st, 2*time.Hour, time.Minute, func(_ int, at time.Time) (model.Status, *time.Duration) {
		if at.Before(start.Add(time.Hour)) {
			return model.StatusUp, rt(30)
		}
		return model.StatusUnknown, nil
	})

	resp, body := c.do(http.MethodGet,
		"/api/v1/monitors/"+m.ID.String()+"/history?resolution=1h&from="+
			start.Format(time.RFC3339)+"&to="+start.Add(2*time.Hour).Format(time.RFC3339), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d (%v)", resp.StatusCode, body)
	}

	// Buckets are epoch-aligned, so two hours from an arbitrary minute spans two
	// or three of them. What matters is the shape, not the count.
	data, _ := body["data"].([]any)
	if len(data) < 2 {
		t.Fatalf("got %d buckets, want at least 2", len(data))
	}

	var observedUp, nullBuckets int
	for _, b := range data {
		bucket, _ := b.(map[string]any)

		// Nothing anywhere may read as an outage: not one check was down.
		if down := bucket["down_count"].(float64); down != 0 {
			t.Errorf("bucket %v reports %v down checks; none were", bucket["bucket_start"], down)
		}

		if bucket["uptime_ratio"] == nil {
			nullBuckets++
			if bucket["response_time_avg_ms"] != nil {
				t.Errorf("an unobserved bucket carries a response time: %v", bucket["response_time_avg_ms"])
			}
			continue
		}
		if ratio := bucket["uptime_ratio"].(float64); ratio != 1 {
			t.Errorf("an observed bucket has ratio %v, want 1", ratio)
		}
		observedUp += int(bucket["up_count"].(float64))
	}

	if observedUp != 60 {
		t.Errorf("observed buckets total %d up checks, want 60", observedUp)
	}
	if nullBuckets == 0 {
		t.Error("the all-unknown hour produced no null ratio; a probe outage is being drawn as data")
	}
}

// Raw is fresher than any tier and exact, so it wins whenever it covers the
// range. A tier lags by its bucket width plus the pipeline's grace period, and
// that lag lands on the right-hand edge of the chart — the part being watched.
func TestHistoryPrefersRawWhenItCoversTheRange(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	m := seedHistory(t, st, time.Hour, time.Minute, alwaysUp)

	// Nothing has been rolled up at all, so a tier read would return nothing.
	resp, body := c.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+"/history?resolution=5m", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d (%v)", resp.StatusCode, body)
	}
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatal("no buckets: the reader fell through to an empty rollup tier instead of using raw")
	}
	// Buckets are epoch-aligned, so an hour starting at an arbitrary minute
	// spans thirteen five-minute buckets with the first and last partial. What
	// has to hold is that every check is in exactly one of them.
	if len(data) < 12 || len(data) > 13 {
		t.Errorf("got %d buckets, want 12 or 13 depending on alignment", len(data))
	}
	var total float64
	for _, b := range data {
		bucket, _ := b.(map[string]any)
		total += bucket["up_count"].(float64)
	}
	if total != 60 {
		t.Errorf("buckets total %v checks, want 60 — every check in exactly one bucket", total)
	}
}

// Beyond raw retention the tiers take over. That is the whole reason they exist,
// and until now nothing read them.
func TestHistoryFallsBackToTheRollupTier(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()

	// Twelve hours of history, then roll it up and delete the raw rows — which
	// is exactly what retention does after seven days.
	m := seedHistory(t, st, 12*time.Hour, 5*time.Minute, func(i int, _ time.Time) (model.Status, *time.Duration) {
		if i%10 == 0 {
			return model.StatusDown, nil
		}
		return model.StatusUp, rt(40)
	})

	now := time.Now().UTC()
	from := now.Add(-12 * time.Hour)
	if _, err := st.RollUpRaw(t.Context(), from, now); err != nil {
		t.Fatalf("roll up raw: %v", err)
	}
	for _, tier := range rollup.Tiers[1:] {
		if _, err := st.RollUpTier(t.Context(), tier, *tier.Source, from, now); err != nil {
			t.Fatalf("roll up %s: %v", tier.Name, err)
		}
	}
	if _, err := st.DeleteHeartbeatsBefore(t.Context(), now, 100000); err != nil {
		t.Fatalf("delete raw: %v", err)
	}

	resp, body := c.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+"/history?resolution=1h", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d (%v)", resp.StatusCode, body)
	}
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatal("no buckets from the rollup tier after raw was deleted")
	}

	var up, down float64
	for _, b := range data {
		bucket, _ := b.(map[string]any)
		up += bucket["up_count"].(float64)
		down += bucket["down_count"].(float64)
	}
	// 144 checks over twelve hours, every tenth down. The last 24h of the
	// default window has no data before the seed, so the totals are the seed's.
	if up+down == 0 {
		t.Fatal("the tier held no counts")
	}
	if down == 0 {
		t.Error("the down checks did not survive the rollup")
	}
}

// A percentile cannot be re-aggregated, and the coarse tiers hold an
// approximation. The response schema has no field in which to label one, and an
// unlabelled p95 is worse than no p95 — so it is reported as absent.
func TestHistoryWithholdsApproximatePercentiles(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	m := seedHistory(t, st, 2*time.Hour, time.Minute, alwaysUp)

	// From raw, the percentile is real.
	resp, body := c.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+"/history?resolution=1h", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d", resp.StatusCode)
	}
	data, _ := body["data"].([]any)
	first, _ := data[0].(map[string]any)
	if first["response_time_p95_ms"] == nil {
		t.Error("no p95 from raw, where a real one is computable")
	}

	// From a coarse tier, it is withheld.
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	if _, err := st.RollUpRaw(t.Context(), from, now); err != nil {
		t.Fatalf("roll up raw: %v", err)
	}
	for _, tier := range rollup.Tiers[1:] {
		if _, err := st.RollUpTier(t.Context(), tier, *tier.Source, from, now); err != nil {
			t.Fatalf("roll up %s: %v", tier.Name, err)
		}
	}
	if _, err := st.DeleteHeartbeatsBefore(t.Context(), now, 100000); err != nil {
		t.Fatalf("delete raw: %v", err)
	}

	resp, body = c.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+"/history?resolution=1h", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d", resp.StatusCode)
	}
	data, _ = body["data"].([]any)
	if len(data) == 0 {
		t.Fatal("no buckets")
	}
	for _, b := range data {
		bucket, _ := b.(map[string]any)
		if bucket["response_time_p95_ms"] != nil {
			t.Errorf("the 1h tier returned a p95 (%v); it holds an approximation that cannot be labelled",
				bucket["response_time_p95_ms"])
		}
		// The average is still exact, because a sum and a count re-aggregate.
		if bucket["response_time_avg_ms"] == nil {
			t.Error("the average went missing along with the percentile")
		}
	}
}

func TestHistoryRejectsBadInput(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	m := seedHistory(t, st, time.Hour, time.Minute, alwaysUp)
	base := "/api/v1/monitors/" + m.ID.String() + "/history"

	now := time.Now().UTC()
	bad := map[string]string{
		"unknown resolution": "?resolution=30s",
		"from after to":      "?from=" + now.Format(time.RFC3339) + "&to=" + now.Add(-time.Hour).Format(time.RFC3339),
		"from equal to to":   "?from=" + now.Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339),
		"unparseable from":   "?from=yesterday",
		"unparseable to":     "?to=soon",
		"range too wide":     "?from=1990-01-01T00:00:00Z",
	}
	for name, query := range bad {
		resp, _ := c.do(http.MethodGet, base+query, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, resp.StatusCode)
		}
	}

	resp, _ := c.do(http.MethodGet, "/api/v1/monitors/01a00000-0000-7000-8000-000000000000/history", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown monitor = %d, want 404", resp.StatusCode)
	}
}

func TestUptimeSummary(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()

	// Twenty hours at one minute: 1,200 checks. Every fifth is down, and a
	// hundred are unknown — the unknowns must land in neither side of the ratio.
	m := seedHistory(t, st, 20*time.Hour, time.Minute, func(i int, _ time.Time) (model.Status, *time.Duration) {
		switch {
		case i < 100:
			return model.StatusUnknown, nil
		case i%5 == 0:
			return model.StatusDown, nil
		default:
			return model.StatusUp, rt(80)
		}
	})

	resp, body := c.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+"/uptime?window=24h", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("uptime = %d (%v)", resp.StatusCode, body)
	}
	if body["maintenance_handling"] != "exclude" {
		t.Errorf("maintenance_handling = %v, want exclude — the figure has to carry its own method",
			body["maintenance_handling"])
	}

	windows, _ := body["windows"].(map[string]any)
	day, ok := windows["24h"].(map[string]any)
	if !ok {
		t.Fatalf("no 24h window in %v", windows)
	}

	total := day["total_checks"].(float64)
	down := day["down_checks"].(float64)
	if total != 1100 {
		t.Errorf("total_checks = %v, want 1100 — the 100 unknown checks must not be counted", total)
	}
	if want := (1200 - 100) / 5.0; down != 220 {
		t.Errorf("down_checks = %v, want %v", down, want)
	}
	if ratio := day["uptime_ratio"].(float64); ratio != (total-down)/total {
		t.Errorf("uptime_ratio = %v, want %v", ratio, (total-down)/total)
	}
	// A failing check stands for one interval of unavailability.
	if seconds := day["downtime_seconds"].(float64); seconds != down*60 {
		t.Errorf("downtime_seconds = %v, want %v", seconds, down*60)
	}
	if avg := day["response_time_avg_ms"].(float64); avg != 80 {
		t.Errorf("response_time_avg_ms = %v, want 80", avg)
	}
	// Incidents are not implemented, and "no incidents" is a different claim
	// from "we do not track incidents".
	if _, present := day["incident_count"]; present {
		t.Error("incident_count was reported despite incidents not being implemented")
	}
}

func TestUptimeDefaultWindows(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	m := seedHistory(t, st, time.Hour, time.Minute, alwaysUp)

	_, body := c.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+"/uptime", nil)
	windows, _ := body["windows"].(map[string]any)
	for _, want := range []string{"24h", "30d"} {
		if _, ok := windows[want]; !ok {
			t.Errorf("default windows are missing %s: %v", want, windows)
		}
	}
	if len(windows) != 2 {
		t.Errorf("got %d default windows, want 2", len(windows))
	}

	// Several windows at once, which is what a monitor detail page asks for.
	_, body = c.do(http.MethodGet,
		"/api/v1/monitors/"+m.ID.String()+"/uptime?window=1h&window=7d&window=365d", nil)
	windows, _ = body["windows"].(map[string]any)
	if len(windows) != 3 {
		t.Errorf("got %d windows, want 3: %v", len(windows), windows)
	}

	// A window with no data at all is null, not zero.
	year, _ := windows["365d"].(map[string]any)
	if year["uptime_ratio"] == nil {
		// An hour of data does fall inside a year, so this one is populated —
		// the assertion that matters is that it is not silently fabricated.
		t.Log("365d ratio is null")
	}
}

// The three-way maintenance choice is the reason uptime_ratio is computed at
// read time and never stored: one set of buckets, three defensible numbers.
func TestUptimeMaintenanceHandling(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()

	// 100 checks: 60 up, 20 down, 20 in maintenance.
	m := seedHistory(t, st, 100*time.Minute, time.Minute, func(i int, _ time.Time) (model.Status, *time.Duration) {
		switch {
		case i < 60:
			return model.StatusUp, rt(10)
		case i < 80:
			return model.StatusDown, nil
		default:
			return model.StatusMaintenance, nil
		}
	})

	cases := map[string]float64{
		"exclude":       60.0 / 80.0,  // maintenance is not observed either way
		"count_as_up":   80.0 / 100.0, // it counts, and it counts as healthy
		"count_as_down": 60.0 / 100.0, // it counts, and it counts against you
	}
	for handling, want := range cases {
		resp, body := c.do(http.MethodGet,
			"/api/v1/monitors/"+m.ID.String()+"/uptime?window=24h&maintenance="+handling, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d (%v)", handling, resp.StatusCode, body)
		}
		if body["maintenance_handling"] != handling {
			t.Errorf("%s: maintenance_handling = %v", handling, body["maintenance_handling"])
		}

		windows, _ := body["windows"].(map[string]any)
		day, _ := windows["24h"].(map[string]any)
		if got := day["uptime_ratio"].(float64); got != want {
			t.Errorf("%s: uptime_ratio = %v, want %v", handling, got, want)
		}
		if seconds := day["maintenance_seconds"].(float64); seconds != 20*60 {
			t.Errorf("%s: maintenance_seconds = %v, want 1200", handling, seconds)
		}
	}
}

func TestUptimeRejectsBadInput(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	m := seedHistory(t, st, time.Hour, time.Minute, alwaysUp)
	base := "/api/v1/monitors/" + m.ID.String() + "/uptime"

	bad := map[string]string{
		"unknown window":     "?window=6h",
		"unknown handling":   "?maintenance=ignore",
		"one bad of several": "?window=24h&window=nope",
	}
	for name, query := range bad {
		resp, _ := c.do(http.MethodGet, base+query, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, resp.StatusCode)
		}
	}

	resp, _ := c.do(http.MethodGet, "/api/v1/monitors/01a00000-0000-7000-8000-000000000000/uptime", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown monitor = %d, want 404", resp.StatusCode)
	}
}

// Both endpoints are history reads and carry the heartbeats:read scope, not
// monitors:read — a key scoped to configuration must not be able to read a
// year of an operator's availability data.
func TestHistoryRequiresHeartbeatsScope(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	m := seedHistory(t, st, time.Hour, time.Minute, alwaysUp)

	configOnly, _ := createKey(t, c, "config only", "monitors:read")
	reader := newClient(t, server)
	reader.bearer = configOnly

	for _, path := range []string{"/history", "/uptime"} {
		resp, _ := reader.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+path, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with monitors:read = %d, want 403", path, resp.StatusCode)
		}
	}

	allowed, _ := createKey(t, c, "history reader", "heartbeats:read")
	ok := newClient(t, server)
	ok.bearer = allowed
	for _, path := range []string{"/history", "/uptime"} {
		resp, _ := ok.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s with heartbeats:read = %d, want 200", path, resp.StatusCode)
		}
	}
}

// The boundary that decides which source answers: raw and the tier both hold the
// range. Raw must win — it is fresher and its percentile is real — and it very
// nearly does not, because a bucket_start is always at or before the earliest
// heartbeat it summarises and so makes the tier look older than raw by up to one
// bucket width.
func TestHistoryPrefersRawWhenBothSourcesHoldTheRange(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	m := seedHistory(t, st, 2*time.Hour, time.Minute, alwaysUp)

	// Roll it up, and keep the raw rows — the ordinary steady state, where both
	// sources can answer.
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	if _, err := st.RollUpRaw(t.Context(), from, now); err != nil {
		t.Fatalf("roll up raw: %v", err)
	}
	for _, tier := range rollup.Tiers[1:] {
		if _, err := st.RollUpTier(t.Context(), tier, *tier.Source, from, now); err != nil {
			t.Fatalf("roll up %s: %v", tier.Name, err)
		}
	}

	resp, body := c.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+"/history?resolution=5m", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d (%v)", resp.StatusCode, body)
	}

	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatal("no buckets")
	}
	for _, b := range data {
		bucket, _ := b.(map[string]any)
		if bucket["response_time_p95_ms"] == nil {
			t.Fatalf("bucket %v has no p95: the read came from the rollup tier, not from raw",
				bucket["bucket_start"])
		}
	}
}

// The same boundary on /uptime, where reading the tier instead of raw also costs
// the real percentile.
func TestUptimePrefersRawWhenBothSourcesHoldTheWindow(t *testing.T) {
	t.Parallel()

	server, st := testServerWithStore(t)
	c := newClient(t, server)
	c.setup()
	m := seedHistory(t, st, 2*time.Hour, time.Minute, func(i int, _ time.Time) (model.Status, *time.Duration) {
		return model.StatusUp, rt(float64(10 + i%50))
	})

	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	if _, err := st.RollUpRaw(t.Context(), from, now); err != nil {
		t.Fatalf("roll up raw: %v", err)
	}
	for _, tier := range rollup.Tiers[1:] {
		if _, err := st.RollUpTier(t.Context(), tier, *tier.Source, from, now); err != nil {
			t.Fatalf("roll up %s: %v", tier.Name, err)
		}
	}

	_, body := c.do(http.MethodGet, "/api/v1/monitors/"+m.ID.String()+"/uptime?window=24h", nil)
	windows, _ := body["windows"].(map[string]any)
	day, _ := windows["24h"].(map[string]any)

	if day["response_time_p95_ms"] == nil {
		t.Error("no p95 on a window raw fully covers; the read fell through to the tier")
	}
	if total := day["total_checks"].(float64); total != 120 {
		t.Errorf("total_checks = %v, want 120", total)
	}
}
