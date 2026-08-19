package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

func open(t *testing.T) *Store {
	t.Helper()

	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "cairn.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func testMonitor(name string) model.Monitor {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return model.Monitor{
		ID:               model.NewID(),
		OrgID:            model.SentinelOrgID,
		Name:             name,
		Type:             model.TypeHTTP,
		Config:           []byte(`{"url":"https://example.com"}`),
		Enabled:          true,
		Interval:         60 * time.Second,
		Timeout:          30 * time.Second,
		NotifyOnRecovery: true,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

// Migrations run on every start, so running them twice must be a no-op rather
// than an error or a second application.
func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()

	s := open(t)
	again, err := s.Migrate(t.Context())
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second migrate applied %d migrations, want 0", len(again))
	}
}

// The seed rows migration 0001 creates are what solo mode runs as. Without them
// every insert fails a foreign key at startup.
func TestSeedRowsExist(t *testing.T) {
	t.Parallel()

	s := open(t)

	var orgs, probes int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM organisations WHERE id = ?`, model.SentinelOrgID[:]).Scan(&orgs); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM probes WHERE id = ? AND mode = 'embedded'`, model.EmbeddedProbeID[:]).Scan(&probes); err != nil {
		t.Fatalf("count probes: %v", err)
	}
	if orgs != 1 {
		t.Errorf("sentinel organisations = %d, want 1", orgs)
	}
	if probes != 1 {
		t.Errorf("embedded probe rows = %d, want 1", probes)
	}
}

func TestMonitorRoundTrip(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("Example")

	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetMonitor(t.Context(), m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Monitor.Name != "Example" || got.Monitor.Type != model.TypeHTTP {
		t.Errorf("got %+v, want the monitor back unchanged", got.Monitor)
	}
	if got.Monitor.Interval != 60*time.Second {
		t.Errorf("interval = %s, want 60s", got.Monitor.Interval)
	}
	// A monitor that has never reported is pending, not down: it has not earned
	// a verdict either way.
	if got.State.Status != model.MonitorStatusPending {
		t.Errorf("status = %s, want pending", got.State.Status)
	}

	if _, err := s.GetMonitor(t.Context(), model.NewID()); err != store.ErrNotFound {
		t.Errorf("get missing = %v, want store.ErrNotFound", err)
	}
}

// The keyset walk must return every row exactly once. An off-by-one here is a
// monitor that never appears in the list — silent, and only at page boundaries.
func TestListMonitorsPaginates(t *testing.T) {
	t.Parallel()

	s := open(t)
	const total = 7
	for i := range total {
		m := testMonitor("monitor")
		// Distinct updated_at values, since that is the cursor's leading key.
		m.UpdatedAt = m.UpdatedAt.Add(time.Duration(i) * time.Second)
		if err := s.CreateMonitor(t.Context(), m); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	seen := map[string]bool{}
	var cursor *store.Cursor
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
		batch, hasMore, err := s.ListMonitors(t.Context(), cursor, 3, MonitorFilter{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, m := range batch {
			if seen[m.Monitor.ID.String()] {
				t.Errorf("monitor %s returned twice", m.Monitor.ID)
			}
			seen[m.Monitor.ID.String()] = true
		}
		if !hasMore {
			break
		}
		last := batch[len(batch)-1]
		cursor = &store.Cursor{UpdatedAt: last.Monitor.UpdatedAt, ID: last.Monitor.ID}
	}

	if len(seen) != total {
		t.Errorf("walked %d monitors, want %d", len(seen), total)
	}
}

// At-least-once delivery means the same batch can arrive twice. The second
// arrival must be a no-op, or every reconnect would double the history.
func TestWriteBatchIsIdempotent(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("Example")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	at := time.Now().UTC()
	beats := []model.Heartbeat{{
		Time:      at,
		MonitorID: m.ID,
		OrgID:     m.OrgID,
		ProbeID:   model.EmbeddedProbeID,
		Status:    model.StatusUp,
		Code:      "200",
		Attempt:   1,
	}}

	// The returned count is the contract the metrics depend on: rows actually
	// inserted, not results offered. Without this assertion the load-test
	// harness reported twice the throughput the database contained, which is a
	// measurement error that looks exactly like good news.
	first, err := s.WriteBatch(t.Context(), beats)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if first != 1 {
		t.Errorf("first write reported %d rows, want 1", first)
	}

	second, err := s.WriteBatch(t.Context(), beats)
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if second != 0 {
		t.Errorf("resending the same batch reported %d rows written, want 0", second)
	}

	got, _, err := s.ListHeartbeats(t.Context(), m.ID, nil, 10, false)
	if err != nil {
		t.Fatalf("list heartbeats: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("stored %d heartbeats after writing the same batch twice, want 1", len(got))
	}
	if got[0].Status != model.StatusUp || got[0].Code != "200" {
		t.Errorf("got %+v, want the heartbeat back unchanged", got[0])
	}
	if got[0].ProbeID != model.EmbeddedProbeID {
		t.Errorf("probe_id = %s, want the embedded probe", got[0].ProbeID)
	}
}

func TestListHeartbeatsFiltersAndOrders(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("Example")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	at := time.Now().UTC()
	var beats []model.Heartbeat
	for i := range 5 {
		beats = append(beats, model.Heartbeat{
			Time:      at.Add(time.Duration(i) * time.Second),
			MonitorID: m.ID,
			OrgID:     m.OrgID,
			ProbeID:   model.EmbeddedProbeID,
			Status:    model.StatusUp,
			Attempt:   1,
			Important: i == 2,
		})
	}
	if _, err := s.WriteBatch(t.Context(), beats); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, _, err := s.ListHeartbeats(t.Context(), m.ID, nil, 10, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d heartbeats, want 5", len(got))
	}
	// Newest first, which is what the history view reads.
	for i := 1; i < len(got); i++ {
		if !got[i].Time.Before(got[i-1].Time) {
			t.Errorf("heartbeat %d is not older than %d", i, i-1)
		}
	}

	events, _, err := s.ListHeartbeats(t.Context(), m.ID, nil, 10, true)
	if err != nil {
		t.Fatalf("list important: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("important_only returned %d, want 1", len(events))
	}
}

// unknown and skipped are gaps in observation, and the null rule is stated on
// the denominator: they must never land in an uptime ratio.
func TestStatusUptimeEligibility(t *testing.T) {
	t.Parallel()

	counts := map[model.Status]bool{
		model.StatusUp:          true,
		model.StatusDown:        true,
		model.StatusPending:     false,
		model.StatusMaintenance: false,
		model.StatusUnknown:     false,
		model.StatusSkipped:     false,
	}
	for status, want := range counts {
		if got := status.CountsTowardUptime(); got != want {
			t.Errorf("%s.CountsTowardUptime() = %v, want %v", status, got, want)
		}
	}
}

// secure_delete is asserted rather than assumed, because it is a connection
// pragma: a DSN typo would leave it off and nothing else would notice.
//
// FAST (2), not ON (1). ON zeroes freed content everywhere, and the hot path
// here deletes millions of heartbeat rows a day under retention; FAST zeroes
// only what is inside a page being rewritten anyway, which is where an
// overwritten credential lands.
func TestSecureDeleteIsFast(t *testing.T) {
	t.Parallel()

	s := open(t)

	var mode int
	if err := s.db.QueryRowContext(t.Context(), `PRAGMA secure_delete`).Scan(&mode); err != nil {
		t.Fatalf("read secure_delete: %v", err)
	}
	if mode != 2 {
		t.Errorf("secure_delete = %d, want 2 (FAST)", mode)
	}
}
