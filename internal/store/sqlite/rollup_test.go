package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/rollup"
)

// A rollup bug is the quietest kind there is: nothing errors, the numbers are
// just wrong, and they are wrong in history that raw heartbeats no longer exist
// to contradict. So these tests assert the arithmetic directly.

// bucketRow is what a tier row looks like when read back.
type bucketRow struct {
	start                      time.Time
	up, down, unknown, skipped int
	rtSum                      float64
	rtCount                    int
	rtMin, rtMax, p95          float64
}

func readTier(t *testing.T, s *Store, tier string, id model.ID) []bucketRow {
	t.Helper()

	rows, err := s.db.QueryContext(t.Context(), fmt.Sprintf(`
		SELECT bucket_start, up_count, down_count, unknown_count, skipped_count,
		       COALESCE(response_time_sum, 0), COALESCE(response_time_count, 0),
		       COALESCE(response_time_min, 0), COALESCE(response_time_max, 0),
		       COALESCE(response_time_p95, 0)
		FROM heartbeat_%s WHERE monitor_id = ? ORDER BY bucket_start`, tier), id[:])
	if err != nil {
		t.Fatalf("read %s: %v", tier, err)
	}
	defer func() { _ = rows.Close() }()

	var out []bucketRow
	for rows.Next() {
		var b bucketRow
		var ms int64
		if err := rows.Scan(&ms, &b.up, &b.down, &b.unknown, &b.skipped,
			&b.rtSum, &b.rtCount, &b.rtMin, &b.rtMax, &b.p95); err != nil {
			t.Fatalf("scan %s: %v", tier, err)
		}
		b.start = time.UnixMilli(ms).UTC()
		out = append(out, b)
	}
	return out
}

// writeBeats inserts heartbeats at a fixed cadence starting at from.
func writeBeats(t *testing.T, s *Store, m model.Monitor, from time.Time, every time.Duration, beats []model.Heartbeat) {
	t.Helper()

	at := from
	for i := range beats {
		beats[i].Time = at
		beats[i].MonitorID = m.ID
		beats[i].OrgID = m.OrgID
		beats[i].ProbeID = model.EmbeddedProbeID
		at = at.Add(every)
	}
	if _, err := s.WriteBatch(t.Context(), beats); err != nil {
		t.Fatalf("write heartbeats: %v", err)
	}
}

func ms(v float64) *time.Duration {
	d := time.Duration(v * float64(time.Millisecond))
	return &d
}

func TestRollUpRawCounts(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("rollup")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// One minute, twelve checks at five seconds: nine up, one down, one unknown,
	// one skipped. unknown and skipped are counted and must stay out of the
	// uptime denominator — that is the whole of ADR-005 decision 16 in one row.
	start := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	beats := make([]model.Heartbeat, 12)
	for i := range beats {
		beats[i] = model.Heartbeat{Status: model.StatusUp, ResponseTime: ms(float64(10 * (i + 1)))}
	}
	beats[9] = model.Heartbeat{Status: model.StatusDown}
	beats[10] = model.Heartbeat{Status: model.StatusUnknown}
	beats[11] = model.Heartbeat{Status: model.StatusSkipped}
	writeBeats(t, s, m, start, 5*time.Second, beats)

	if _, err := s.RollUpRaw(t.Context(), start, start.Add(time.Minute)); err != nil {
		t.Fatalf("roll up: %v", err)
	}

	got := readTier(t, s, "1m", m.ID)
	if len(got) != 1 {
		t.Fatalf("produced %d buckets, want 1", len(got))
	}
	b := got[0]

	if !b.start.Equal(start) {
		t.Errorf("bucket_start = %s, want %s", b.start, start)
	}
	if b.up != 9 || b.down != 1 || b.unknown != 1 || b.skipped != 1 {
		t.Errorf("counts = up %d down %d unknown %d skipped %d, want 9/1/1/1", b.up, b.down, b.unknown, b.skipped)
	}

	// Response times: nine measurements of 10..90ms. Sum and count, never a
	// stored average — an average cannot be re-weighted into a coarser tier.
	if b.rtCount != 9 {
		t.Errorf("response_time_count = %d, want 9", b.rtCount)
	}
	if want := 10.0 + 20 + 30 + 40 + 50 + 60 + 70 + 80 + 90; b.rtSum != want {
		t.Errorf("response_time_sum = %v, want %v", b.rtSum, want)
	}
	if b.rtMin != 10 || b.rtMax != 90 {
		t.Errorf("min/max = %v/%v, want 10/90", b.rtMin, b.rtMax)
	}
	// Nearest rank over nine samples: ceil(0.95 * 9) = 9, so the largest.
	if b.p95 != 90 {
		t.Errorf("p95 = %v, want 90", b.p95)
	}
}

// A bucket with no checks has no row, because absence means "no data" and a
// status page that renders no data as downtime is lying. A bucket whose checks
// were all unknown or skipped does have a row, with up + down = 0 — it carries
// the reason the observation is missing, which an absent bucket cannot.
func TestRollUpRawEmptyVersusUnobserved(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("gaps")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	start := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	// Minute 0: observed. Minute 1: nothing at all. Minute 2: all unknown.
	writeBeats(t, s, m, start, 20*time.Second, []model.Heartbeat{
		{Status: model.StatusUp}, {Status: model.StatusUp}, {Status: model.StatusUp},
	})
	writeBeats(t, s, m, start.Add(2*time.Minute), 20*time.Second, []model.Heartbeat{
		{Status: model.StatusUnknown}, {Status: model.StatusSkipped}, {Status: model.StatusUnknown},
	})

	if _, err := s.RollUpRaw(t.Context(), start, start.Add(3*time.Minute)); err != nil {
		t.Fatalf("roll up: %v", err)
	}

	got := readTier(t, s, "1m", m.ID)
	if len(got) != 2 {
		t.Fatalf("produced %d buckets, want 2 — the empty minute must have no row", len(got))
	}
	if !got[1].start.Equal(start.Add(2 * time.Minute)) {
		t.Errorf("second bucket at %s, want the all-unknown minute", got[1].start)
	}
	if got[1].up+got[1].down != 0 {
		t.Errorf("all-unknown bucket has up+down = %d, want 0", got[1].up+got[1].down)
	}
	if got[1].unknown != 2 || got[1].skipped != 1 {
		t.Errorf("all-unknown bucket = unknown %d skipped %d, want 2/1", got[1].unknown, got[1].skipped)
	}
}

// Each tier is computed from the tier below, not from raw. That only produces
// the right answer because the columns are a sum and a count rather than an
// average, so this asserts the totals survive all three hops.
func TestTiersAggregateFromTheTierBelow(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("chain")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Two hours of checks every 30 seconds, alternating a 100ms and a 200ms
	// response, with every 10th check down.
	start := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	const total = 240
	beats := make([]model.Heartbeat, total)
	wantUp, wantDown := 0, 0
	var wantSum float64
	for i := range beats {
		if i%10 == 9 {
			beats[i] = model.Heartbeat{Status: model.StatusDown}
			wantDown++
			continue
		}
		rt := 100.0
		if i%2 == 1 {
			rt = 200
		}
		beats[i] = model.Heartbeat{Status: model.StatusUp, ResponseTime: ms(rt)}
		wantUp++
		wantSum += rt
	}
	writeBeats(t, s, m, start, 30*time.Second, beats)

	end := start.Add(2 * time.Hour)
	if _, err := s.RollUpRaw(t.Context(), start, end); err != nil {
		t.Fatalf("roll up raw: %v", err)
	}
	for _, tier := range rollup.Tiers[1:] {
		if _, err := s.RollUpTier(t.Context(), tier, *tier.Source, start, end); err != nil {
			t.Fatalf("roll up %s: %v", tier.Name, err)
		}
	}

	// Every tier must total the same thing. If they do not, one of them is
	// double-counting or dropping a bucket, and neither shows up as an error.
	for _, tier := range []string{"1m", "5m", "1h", "1d"} {
		var up, down, count int
		var sum, min, max float64
		if err := s.db.QueryRowContext(t.Context(), fmt.Sprintf(`
			SELECT SUM(up_count), SUM(down_count), SUM(response_time_count),
			       SUM(response_time_sum), MIN(response_time_min), MAX(response_time_max)
			FROM heartbeat_%s WHERE monitor_id = ?`, tier), m.ID[:]).
			Scan(&up, &down, &count, &sum, &min, &max); err != nil {
			t.Fatalf("total %s: %v", tier, err)
		}

		if up != wantUp || down != wantDown {
			t.Errorf("%s: up %d down %d, want %d/%d", tier, up, down, wantUp, wantDown)
		}
		if count != wantUp {
			t.Errorf("%s: response_time_count %d, want %d", tier, count, wantUp)
		}
		if sum != wantSum {
			t.Errorf("%s: response_time_sum %v, want %v", tier, sum, wantSum)
		}
		if min != 100 || max != 200 {
			t.Errorf("%s: min/max %v/%v, want 100/200", tier, min, max)
		}
	}

	// And the bucket counts have to match the tier widths: two hours is 120
	// one-minute buckets, 24 five-minute, 2 hourly, 1 daily.
	for tier, want := range map[string]int{"1m": 120, "5m": 24, "1h": 2, "1d": 1} {
		var n int
		if err := s.db.QueryRowContext(t.Context(),
			fmt.Sprintf(`SELECT count(*) FROM heartbeat_%s WHERE monitor_id = ?`, tier), m.ID[:]).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tier, err)
		}
		if n != want {
			t.Errorf("%s has %d buckets, want %d", tier, n, want)
		}
	}
}

// The watermark is derived from the data and every write is an upsert over a
// full recount, so re-running a range must change nothing. The whole late-data
// design leans on this: if a second pass drifted, every reprocess window would
// corrupt the tier it was meant to repair.
func TestRollUpIsIdempotent(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("idempotent")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	start := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	writeBeats(t, s, m, start, 10*time.Second, []model.Heartbeat{
		{Status: model.StatusUp, ResponseTime: ms(50)},
		{Status: model.StatusUp, ResponseTime: ms(70)},
		{Status: model.StatusDown},
	})

	end := start.Add(time.Minute)
	for range 3 {
		if _, err := s.RollUpRaw(t.Context(), start, end); err != nil {
			t.Fatalf("roll up: %v", err)
		}
	}

	got := readTier(t, s, "1m", m.ID)
	if len(got) != 1 {
		t.Fatalf("three passes produced %d buckets, want 1", len(got))
	}
	if got[0].up != 2 || got[0].down != 1 || got[0].rtCount != 2 || got[0].rtSum != 120 {
		t.Errorf("counts drifted across passes: %+v", got[0])
	}
}

// Probes buffer and replay after a control-plane outage (ADR-001), so a
// heartbeat can arrive minutes after its bucket was computed. Recomputing a
// trailing window is what folds it in; without that the outage would leave a
// permanently undercounted hole in history.
func TestRollUpFoldsInLateHeartbeats(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("late")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	start := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	writeBeats(t, s, m, start, 20*time.Second, []model.Heartbeat{{Status: model.StatusUp}})

	end := start.Add(time.Minute)
	if _, err := s.RollUpRaw(t.Context(), start, end); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := readTier(t, s, "1m", m.ID); len(got) != 1 || got[0].up != 1 {
		t.Fatalf("first pass: %+v", got)
	}

	// The replayed results land in a bucket already computed.
	writeBeats(t, s, m, start.Add(20*time.Second), 20*time.Second, []model.Heartbeat{
		{Status: model.StatusUp}, {Status: model.StatusDown},
	})
	if _, err := s.RollUpRaw(t.Context(), start, end); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	got := readTier(t, s, "1m", m.ID)
	if len(got) != 1 {
		t.Fatalf("produced %d buckets, want 1", len(got))
	}
	if got[0].up != 2 || got[0].down != 1 {
		t.Errorf("late heartbeats not folded in: up %d down %d, want 2/1", got[0].up, got[0].down)
	}
}

func TestRetentionDeletesPastTheHorizon(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("retention")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	old := now.Add(-30 * 24 * time.Hour)

	writeBeats(t, s, m, old, time.Minute, []model.Heartbeat{
		{Status: model.StatusUp}, {Status: model.StatusUp}, {Status: model.StatusUp},
	})
	writeBeats(t, s, m, now.Add(-time.Hour), time.Minute, []model.Heartbeat{
		{Status: model.StatusUp}, {Status: model.StatusUp},
	})

	cutoff := now.AddDate(0, 0, -7)
	deleted, err := s.DeleteHeartbeatsBefore(t.Context(), cutoff, 1000)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted %d heartbeats, want 3", deleted)
	}

	var remaining int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM heartbeats`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 2 {
		t.Errorf("%d heartbeats remain, want 2", remaining)
	}

	// Batches are bounded so the single SQLite writer is never held for long.
	writeBeats(t, s, m, old.Add(time.Hour), time.Minute, []model.Heartbeat{
		{Status: model.StatusUp}, {Status: model.StatusUp}, {Status: model.StatusUp}, {Status: model.StatusUp},
	})
	n, err := s.DeleteHeartbeatsBefore(t.Context(), cutoff, 2)
	if err != nil {
		t.Fatalf("bounded delete: %v", err)
	}
	if n != 2 {
		t.Errorf("bounded delete removed %d rows, want at most the limit of 2", n)
	}
}

// The §9.2 trap, asserted rather than assumed: deleting rows from SQLite does
// not shrink the file on its own. Without auto_vacuum=INCREMENTAL, retention
// runs, reports success, and the Pi still fills its card.
func TestRetentionActuallyReclaimsDisk(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "reclaim.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	mode, err := s.AutoVacuumMode(t.Context())
	if err != nil {
		t.Fatalf("auto_vacuum: %v", err)
	}
	if mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (INCREMENTAL) — without it retention never returns disk", mode)
	}

	m := testMonitor("bulk")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Enough rows that the file grows well past its empty size, with a message
	// on each so the pages are worth reclaiming.
	const rows = 20000
	at := time.Now().UTC().Add(-60 * 24 * time.Hour)
	batch := make([]model.Heartbeat, 0, rows)
	for i := range rows {
		batch = append(batch, model.Heartbeat{
			Time: at.Add(time.Duration(i) * time.Second), MonitorID: m.ID, OrgID: m.OrgID,
			ProbeID: model.EmbeddedProbeID, Status: model.StatusUp,
			Message: "a message long enough that twenty thousand of them are worth reclaiming",
		})
	}
	if _, err := s.WriteBatch(t.Context(), batch); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Checkpoint so the pages are in the main file rather than the WAL.
	if _, err := s.db.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	before := fileSize(t, path)

	deleted, err := s.DeleteHeartbeatsBefore(t.Context(), time.Now().UTC().AddDate(0, 0, -7), rows)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != rows {
		t.Fatalf("deleted %d rows, want %d", deleted, rows)
	}
	if _, err := s.db.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	afterDelete := fileSize(t, path)
	if err := s.IncrementalVacuum(t.Context(), 100000); err != nil {
		t.Fatalf("incremental vacuum: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	afterVacuum := fileSize(t, path)

	if afterVacuum >= afterDelete {
		t.Errorf("file did not shrink: %d bytes before the vacuum, %d after (%d before the delete)",
			afterDelete, afterVacuum, before)
	}
	t.Logf("%d bytes with data, %d after deleting, %d after the vacuum", before, afterDelete, afterVacuum)
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return info.Size()
}

// Deleting a monitor returns immediately and leaves its history to the purge
// worker. Nothing cascades — the time series has no foreign key to monitors on
// purpose — so if the queue did not work the rows would simply stay forever.
func TestDeleteMonitorEnqueuesItsHistory(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("doomed")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	beats := make([]model.Heartbeat, 60)
	for i := range beats {
		beats[i] = model.Heartbeat{Status: model.StatusUp, ResponseTime: ms(20)}
	}
	writeBeats(t, s, m, start, time.Minute, beats)
	if _, err := s.RollUpRaw(t.Context(), start, start.Add(time.Hour)); err != nil {
		t.Fatalf("roll up: %v", err)
	}

	if err := s.DeleteMonitor(t.Context(), m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The configuration is gone synchronously, so the API's 204 is honest.
	if _, err := s.GetMonitor(t.Context(), m.ID); err == nil {
		t.Error("the monitor row survived its own deletion")
	}
	// The history is not, yet — and that is the point.
	if n := countRows(t, s, "heartbeats"); n != 60 {
		t.Errorf("%d heartbeats after delete, want 60 still queued for purge", n)
	}
	if n := countRows(t, s, "pending_purges"); n != 1 {
		t.Fatalf("%d purge entries, want 1", n)
	}

	// Drain it.
	for range 20 {
		_, done, err := s.PurgeNext(t.Context(), 25)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if done {
			break
		}
	}

	if n := countRows(t, s, "heartbeats"); n != 0 {
		t.Errorf("%d heartbeats survived the purge", n)
	}
	if n := countRows(t, s, "heartbeat_1m"); n != 0 {
		t.Errorf("%d rollup buckets survived the purge — rollup tables have no foreign key either", n)
	}
	if n := countRows(t, s, "pending_purges"); n != 0 {
		t.Errorf("%d purge entries remain after draining", n)
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()

	var n int
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestUptimeCache(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("uptime")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Hour)
	// Six hours of checks every minute: one hour entirely down, one entirely
	// unknown, four up. Unknown must land in neither side of the ratio.
	for hour := range 6 {
		at := now.Add(time.Duration(hour-6) * time.Hour)
		beats := make([]model.Heartbeat, 60)
		for i := range beats {
			switch hour {
			case 1:
				beats[i] = model.Heartbeat{Status: model.StatusDown}
			case 2:
				beats[i] = model.Heartbeat{Status: model.StatusUnknown}
			default:
				beats[i] = model.Heartbeat{Status: model.StatusUp, ResponseTime: ms(15)}
			}
		}
		writeBeats(t, s, m, at, time.Minute, beats)
	}

	from := now.Add(-6 * time.Hour)
	if _, err := s.RollUpRaw(t.Context(), from, now); err != nil {
		t.Fatalf("roll up raw: %v", err)
	}
	for _, tier := range rollup.Tiers[1:] {
		if _, err := s.RollUpTier(t.Context(), tier, *tier.Source, from, now); err != nil {
			t.Fatalf("roll up %s: %v", tier.Name, err)
		}
	}
	if _, err := s.RefreshUptimeCache(t.Context(), now); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	ratio, total, down := readCache(t, s, m.ID, "24h")
	if total != 300 {
		t.Errorf("total_checks = %d, want 300 — the unknown hour must not be counted", total)
	}
	if down != 60 {
		t.Errorf("down_checks = %d, want 60", down)
	}
	if want := 240.0 / 300.0; ratio == nil || *ratio != want {
		t.Errorf("uptime_ratio = %v, want %v", ratio, want)
	}
}

// A monitor with nothing but unknown checks has no uptime figure at all. Null,
// not zero: a probe that could not look is not an outage, and rendering it as
// one would put a fabricated incident on a status page.
func TestUptimeCacheIsNullWithoutObservations(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("unobserved")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Hour)
	beats := make([]model.Heartbeat, 60)
	for i := range beats {
		beats[i] = model.Heartbeat{Status: model.StatusUnknown}
	}
	writeBeats(t, s, m, now.Add(-time.Hour), time.Minute, beats)

	from := now.Add(-time.Hour)
	if _, err := s.RollUpRaw(t.Context(), from, now); err != nil {
		t.Fatalf("roll up raw: %v", err)
	}
	for _, tier := range rollup.Tiers[1:] {
		if _, err := s.RollUpTier(t.Context(), tier, *tier.Source, from, now); err != nil {
			t.Fatalf("roll up %s: %v", tier.Name, err)
		}
	}
	if _, err := s.RefreshUptimeCache(t.Context(), now); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	ratio, total, _ := readCache(t, s, m.ID, "24h")
	if ratio != nil {
		t.Errorf("uptime_ratio = %v, want null", *ratio)
	}
	if total != 0 {
		t.Errorf("total_checks = %d, want 0", total)
	}

	// A monitor that has never been checked at all gets a row too, also null —
	// so the list view has something to read rather than a missing key.
	fresh := testMonitor("brand new")
	if err := s.CreateMonitor(t.Context(), fresh); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.RefreshUptimeCache(t.Context(), now); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if ratio, _, _ := readCache(t, s, fresh.ID, "24h"); ratio != nil {
		t.Errorf("a never-checked monitor has uptime_ratio %v, want null", *ratio)
	}
}

func readCache(t *testing.T, s *Store, id model.ID, window string) (*float64, int, int) {
	t.Helper()

	var (
		ratio       *float64
		total, down int
	)
	err := s.db.QueryRowContext(t.Context(), `
		SELECT uptime_ratio, total_checks, down_checks
		FROM monitor_uptime_cache WHERE monitor_id = ? AND window = ?`, id[:], window).
		Scan(&ratio, &total, &down)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	return ratio, total, down
}

// The uptime cache reads a tier holding up to a year of buckets, once per
// monitor. Driving it from monitors means a primary-key seek per monitor rather
// than a scan of the whole tier — which is the difference between the list view
// working at 5,000 monitors and failing the load gate.
func TestUptimeCacheUsesAnIndexedSeek(t *testing.T) {
	t.Parallel()

	s := open(t)
	plan := explain(t, s, `
		SELECT m.id, SUM(h.up_count) FROM monitors m
		LEFT JOIN heartbeat_1h h ON h.monitor_id = m.id AND h.bucket_start >= ?
		GROUP BY m.id`)

	if !containsAny(plan, "SEARCH h", "USING PRIMARY KEY", "USING INDEX") {
		t.Errorf("the uptime query scans rather than seeks:\n%s", plan)
	}
	if containsAny(plan, "SCAN h") {
		t.Errorf("the uptime query scans heartbeat_1h:\n%s", plan)
	}
}

func explain(t *testing.T, s *Store, query string) string {
	t.Helper()

	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, 0)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := ""
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		out += detail + "\n"
	}
	return out
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if len(n) > 0 && len(haystack) >= len(n) && contains(haystack, n) {
			return true
		}
	}
	return false
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
