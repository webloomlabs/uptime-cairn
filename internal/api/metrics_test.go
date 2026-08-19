package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/telemetry"
)

// scrape fetches /metrics as a Prometheus client would.
func scrape(t *testing.T, base string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/metrics", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want the text exposition format", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(body)
}

// The load-test harness reads these series to decide whether the engine kept up.
// A rename or a removal breaks the gate silently — it would measure a delta of
// zero and pass — so the names are asserted here rather than left to the gate to
// discover on somebody else's branch.
func TestMetricsExposeTheSeriesTheGateReads(t *testing.T) {
	// Not parallel: the telemetry registry is process-wide, which is what lets
	// the probe and the control plane both publish into it without importing
	// each other.
	telemetry.Reset()
	t.Cleanup(telemetry.Reset)

	server := testServer(t)
	c := newClient(t, server)
	c.setup()
	createHTTPMonitor(t, c, "Checkout")

	telemetry.Engine.HeartbeatsWritten.Add(7)
	telemetry.Engine.ResultsIngested.Add(9)
	telemetry.RecordProbeHealth(telemetry.ProbeHealth{
		ProbeID: "00000000-0000-7000-8000-000000000002", Name: "embedded",
		Assigned: 1, BufferedResults: 3, ShedResultsTotal: 2, ChecksStartedTotal: 11,
	})

	body := scrape(t, server.URL)

	for _, name := range []string{
		"cairn_build_info",
		"cairn_monitors",
		"cairn_monitor_status",
		"cairn_heartbeats_written_total",
		"cairn_results_ingested_total",
		"cairn_results_rejected_total",
		"cairn_alerts_published_total",
		"cairn_alerts_dropped_total",
		"cairn_process_uptime_seconds",
		"cairn_probe_assigned_monitors",
		"cairn_probe_buffered_results",
		"cairn_probe_shed_results_total",
		"cairn_probe_skipped_checks_total",
		"cairn_probe_checks_started_total",
		"cairn_probe_clock_offset_seconds",
	} {
		if !strings.Contains(body, "\n"+name+" ") && !strings.Contains(body, "\n"+name+"{") {
			t.Errorf("series %q is missing from /metrics", name)
		}
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Errorf("series %q has no HELP line", name)
		}
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("series %q has no TYPE line", name)
		}
	}

	if !strings.Contains(body, "cairn_heartbeats_written_total 7") {
		t.Error("the heartbeat counter did not carry its value")
	}
	// Rows written and results offered are separate on purpose: they differ
	// exactly when a probe redelivers, and one counter for both would make
	// "the probe is resending" and "the system is doing twice the work"
	// indistinguishable.
	if !strings.Contains(body, "cairn_results_ingested_total 9") {
		t.Error("the ingested counter did not carry its value")
	}
}

// A probe that has never reported must not appear as a probe reporting zeros:
// "no probe has checked in" and "the probe is idle" are different alerts.
func TestProbeSeriesAreAbsentUntilAProbeReports(t *testing.T) {
	telemetry.Reset()
	t.Cleanup(telemetry.Reset)

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	body := scrape(t, server.URL)
	if strings.Contains(body, "cairn_probe_") {
		t.Fatal("probe series appeared before any probe reported health")
	}
}

func TestMetricsNeedsAScopeFromOffLoopback(t *testing.T) {
	telemetry.Reset()
	t.Cleanup(telemetry.Reset)

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	// httptest listens on loopback, which is the unauthenticated case: the
	// overwhelmingly common deployment is a Prometheus on the same host, and a
	// metrics endpoint that needs a credential is one somebody turns off.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback scrape = %d, want 200", resp.StatusCode)
	}

	// The off-host path cannot be exercised through httptest without spoofing
	// the peer address, so what is asserted here is the classifier the decision
	// rests on.
	for _, ip := range []string{"127.0.0.1", "::1", "127.0.0.53"} {
		if !isLoopback(ip) {
			t.Errorf("isLoopback(%q) = false, want true", ip)
		}
	}
	for _, ip := range []string{"10.0.0.4", "192.168.1.9", "", "1.2.3.4"} {
		if isLoopback(ip) {
			t.Errorf("isLoopback(%q) = true, want false", ip)
		}
	}
}
