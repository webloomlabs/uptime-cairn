package sqlite

import (
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The list embeds must seek, not scan.
//
// This is the assertion the 5,000-monitor gate makes end to end, made here in
// milliseconds instead of six minutes. A predicate that names monitor_id without
// org_id cannot use the leading column of the only index on this table, and
// SQLite quietly falls back to reading all of it — so a page of twenty-five rows
// costs the size of the whole heartbeats table and the dashboard's cost stops
// being bounded by the viewport, which is the ADR-004 claim the product is sold
// on.
//
// Deliberately on a database with no ANALYZE statistics, because that is the
// state every cairn database is in: nothing here ever runs ANALYZE. Given
// statistics SQLite will skip-scan the index and the bad query looks fine, which
// is exactly how this survived review — measured on a harness that had run
// ANALYZE, reported fast, and was not fast anywhere it actually ran.
func TestEmbedsSeekRatherThanScan(t *testing.T) {
	t.Parallel()

	s := open(t)
	if stats := hasStatistics(t, s); stats {
		t.Fatal("this database has ANALYZE statistics, which would let a scanning query pass")
	}

	// Three arms, and the parameters have to be supplied even to EXPLAIN: the
	// plan for a seek depends on the predicate, not on the values.
	const arms = 3
	args := make([]any, 0, arms*2)
	for range arms {
		id := model.NewID()
		args = append(args, model.SentinelOrgID[:], id[:])
	}

	for _, limit := range []int{1, 30} {
		rows, err := s.db.QueryContext(t.Context(),
			"EXPLAIN QUERY PLAN "+boundedSeekPerMonitor(arms, limit), args...)
		if err != nil {
			t.Fatalf("explain (limit %d): %v", limit, err)
		}

		var plan []string
		for rows.Next() {
			var id, parent, aux int
			var detail string
			if err := rows.Scan(&id, &parent, &aux, &detail); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("plan rows: %v", err)
		}

		joined := strings.Join(plan, "\n")
		if strings.Contains(joined, "SCAN heartbeats") {
			t.Errorf("limit %d: the embed scans the heartbeats table rather than seeking it:\n%s", limit, joined)
		}
		if !strings.Contains(joined, "org_id=? AND monitor_id=?") {
			t.Errorf("limit %d: the seek does not use both leading index columns:\n%s", limit, joined)
		}
	}
}

func hasStatistics(t *testing.T, s *Store) bool {
	t.Helper()

	var n int
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'sqlite_stat1'`).Scan(&n); err != nil {
		t.Fatalf("check statistics: %v", err)
	}
	return n > 0
}

// LastHeartbeats answers with the newest beat per monitor and nothing about the
// monitors that were not asked for.
func TestLastHeartbeatsIsTheNewestPerMonitor(t *testing.T) {
	t.Parallel()

	s := open(t)
	var monitors []model.Monitor
	for _, name := range []string{"a", "b", "unasked"} {
		m := testMonitor(name)
		if err := s.CreateMonitor(t.Context(), m); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		monitors = append(monitors, m)
	}

	start := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	for _, m := range monitors {
		writeBeats(t, s, m, start, time.Minute, []model.Heartbeat{
			{Status: model.StatusUp}, {Status: model.StatusUp}, {Status: model.StatusDown},
		})
	}

	got, err := s.LastHeartbeats(t.Context(), []model.ID{monitors[0].ID, monitors[1].ID})
	if err != nil {
		t.Fatalf("last heartbeats: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("monitors = %d, want 2 — only the ones asked for", len(got))
	}
	for _, m := range monitors[:2] {
		beat, ok := got[m.ID]
		if !ok {
			t.Fatalf("%s missing", m.Name)
		}
		// The third beat, two minutes in, and the only one that is down.
		if want := start.Add(2 * time.Minute); !beat.Time.Equal(want) {
			t.Errorf("%s: beat at %s, want the newest (%s)", m.Name, beat.Time, want)
		}
		if beat.Status != model.StatusDown {
			t.Errorf("%s: status = %v, want down", m.Name, beat.Status)
		}
	}
}
