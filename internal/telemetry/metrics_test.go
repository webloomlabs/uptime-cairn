package telemetry

import (
	"sync"
	"testing"
	"time"
)

func TestProbeHealthKeepsTheLatestPerProbe(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	RecordProbeHealth(ProbeHealth{ProbeID: "b", ShedResultsTotal: 1})
	RecordProbeHealth(ProbeHealth{ProbeID: "a", ShedResultsTotal: 5})
	RecordProbeHealth(ProbeHealth{ProbeID: "b", ShedResultsTotal: 9})

	got := Probes()
	if len(got) != 2 {
		t.Fatalf("reported %d probes, want 2", len(got))
	}
	// Stable order, so a scrape does not reshuffle its own output between calls
	// and make a diff of two scrapes unreadable.
	if got[0].ProbeID != "a" || got[1].ProbeID != "b" {
		t.Fatalf("order = %s, %s; want a, b", got[0].ProbeID, got[1].ProbeID)
	}
	// Last value wins: these are the probe's current state, not a series.
	if got[1].ShedResultsTotal != 9 {
		t.Errorf("shed = %d, want the most recent report's 9", got[1].ShedResultsTotal)
	}
}

func TestUptimeIsZeroBeforeStartIsMarked(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	if d := Uptime(time.Now()); d != 0 {
		t.Fatalf("uptime = %s before MarkStart, want 0", d)
	}

	start := time.Now().Add(-90 * time.Second)
	MarkStart(start)
	if d := Uptime(start.Add(90 * time.Second)); d.Round(time.Second) != 90*time.Second {
		t.Fatalf("uptime = %s, want 90s", d)
	}
}

// The counters are read by /metrics while the ingest path writes them, and the
// probe reports arrive on a third goroutine. Under -race this is the test that
// says so.
func TestConcurrentUse(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				Engine.HeartbeatsWritten.Add(1)
				Engine.ResultsIngested.Add(1)
				RecordProbeHealth(ProbeHealth{ProbeID: string(rune('a' + i))})
				_ = Probes()
			}
		}()
	}
	wg.Wait()

	if got := Engine.HeartbeatsWritten.Load(); got != 800 {
		t.Fatalf("heartbeats = %d, want 800", got)
	}
	if got := len(Probes()); got != 8 {
		t.Fatalf("probes = %d, want 8", got)
	}
}
