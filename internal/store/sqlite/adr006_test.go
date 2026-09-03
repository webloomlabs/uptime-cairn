package sqlite

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The two guard tests [ADR-006](../../../docs/adr/006-report-latency-statistics.md)
// asks for by name.
//
// The ADR's method is *removal rather than policing*: the coarse tiers hold a
// stored `response_time_p95` column, it is a maximum-of-p95 and therefore an
// approximation, and the fix was to stop selecting it rather than to add a
// warning beside it. That is the right fix and it has one weakness — a removal
// is invisible. Nothing in the type system stops somebody putting the column
// back into the `SELECT` list while tidying the two branches of that query into
// one, and the result would be an approximate percentile flowing into a client's
// SLA report through a field the API schema has no way to label.
//
// So the removal is held by a test rather than by discipline. This is the first
// of the two; the second lives in internal/report and holds the trailing-week
// gate.

// seedTierBucket writes one bucket into an arbitrary tier with a stored p95, so
// the query is asked the question the production tables would ask it.
func seedTierBucket(t *testing.T, s *Store, tier string, id model.ID, start time.Time, p95 float64) {
	t.Helper()

	_, err := s.db.ExecContext(t.Context(), `
		INSERT INTO heartbeat_`+tier+` (bucket_start, monitor_id, org_id,
		    up_count, down_count, pending_count, maintenance_count,
		    unknown_count, skipped_count,
		    response_time_sum, response_time_count, response_time_min,
		    response_time_max, response_time_p95)
		VALUES (?, ?, ?, 10, 0, 0, 0, 0, 0, 1000, 10, 50, 400, ?)`,
		millis(start), id[:], model.SentinelOrgID[:], p95)
	if err != nil {
		t.Fatalf("seed %s bucket: %v", tier, err)
	}
}

// A percentile is a rank statistic and does not merge. The 1m tier's value is
// computed from raw heartbeats inside one minute and is real; every coarser
// tier's is the largest of the p95s it rolled up, which is neither a p95 nor a
// maximum of anything a reader would recognise.
//
// **The stored value is present in all four tables and is returned from exactly
// one of them.** The test seeds the same number into every tier so a failure
// cannot be read as "the coarse tiers happen to be empty" — the column is
// populated, and the query still has to decline to select it.
func TestCoarseTiersNeverReturnTheStoredPercentile(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := mustCreate(t, s, testMonitor("api"))

	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	from := start.Add(-time.Hour)
	to := start.Add(48 * time.Hour)

	for _, tier := range []string{"1m", "5m", "1h", "1d"} {
		seedTierBucket(t, s, tier, m.ID, start, 940)
	}

	for _, tier := range []string{"5m", "1h", "1d"} {
		buckets, err := s.HistoryFromTier(t.Context(), m.ID, from, to, tier)
		if err != nil {
			t.Fatalf("%s: %v", tier, err)
		}
		if len(buckets) != 1 {
			t.Fatalf("%s: %d buckets, want 1 — the fixture did not land", tier, len(buckets))
		}
		if p := buckets[0].ResponseTimeP95; p != nil {
			t.Errorf("the %s tier returned a p95 of %.0f ms. It is stored, it is a "+
				"maximum-of-p95 rather than a percentile, and the API schema has no "+
				"field in which to say so — ADR-006 removes it rather than labelling it",
				tier, *p)
		}
	}

	// And the one tier whose value is real still answers, so the rule above is a
	// rule about approximations rather than a blanket refusal that would have
	// been much easier to write and much less useful.
	minute, err := s.HistoryFromTier(t.Context(), m.ID, from, to, "1m")
	if err != nil {
		t.Fatalf("1m: %v", err)
	}
	if len(minute) != 1 || minute[0].ResponseTimeP95 == nil {
		t.Fatal("the 1m tier withheld a percentile computed from raw heartbeats; " +
			"it is the only real one in the product and it is the one that should survive")
	}
	if got := *minute[0].ResponseTimeP95; got != 940 {
		t.Errorf("1m p95 = %.0f, want 940", got)
	}
}
