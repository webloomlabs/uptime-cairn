package sqlite

import (
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The reader pool exists to stop one kind of stall, and these are the two
// directions it goes. Both would hang on a store with a single connection, so
// each carries its own deadline: a regression here is a deadlock, and a test
// that hangs tells nobody which one broke.

const poolDeadline = 5 * time.Second

// A read must not queue behind an open write transaction.
//
// This is the shape of the failure the load-test harness found. Publishing an
// assignment set scans every assignable monitor, and while that scan held the
// one connection every write needed, monitor creation fell from 1,144/sec at
// 500 monitors to 38/sec at 5,000 — not because creating a monitor got harder
// but because it was queued behind a reader.
func TestReadsProceedWhileAWriteTransactionIsOpen(t *testing.T) {
	t.Parallel()

	s := open(t)
	if err := s.CreateMonitor(t.Context(), testMonitor("Example")); err != nil {
		t.Fatalf("create: %v", err)
	}

	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Written to, not just begun: a deferred transaction takes no lock until it
	// touches something, and a test that held nothing would pass on a store that
	// had never been fixed.
	if _, err := tx.ExecContext(t.Context(),
		`UPDATE monitors SET name = 'held' WHERE 1 = 1`); err != nil {
		t.Fatalf("write inside the transaction: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := s.ListMonitors(t.Context(), nil, 10, MonitorFilter{})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read during an open write transaction: %v", err)
		}
	case <-time.After(poolDeadline):
		t.Fatal("a read blocked behind an open write transaction")
	}
}

// And the other direction: a long scan must not hold up a write.
func TestWritesProceedWhileAReadIsInFlight(t *testing.T) {
	t.Parallel()

	s := open(t)

	// A read transaction, held open. Begun and then actually used, for the same
	// reason as above — SQLite acquires the read lock at the first statement.
	rtx, err := s.ro.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	defer func() { _ = rtx.Rollback() }()

	var n int
	if err := rtx.QueryRowContext(t.Context(), `SELECT count(*) FROM monitors`).Scan(&n); err != nil {
		t.Fatalf("read inside the transaction: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.CreateMonitor(t.Context(), testMonitor("Written")) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write during an open read: %v", err)
		}
	case <-time.After(poolDeadline):
		t.Fatal("a write blocked behind an open read")
	}
}

// mode=ro is enforcement, not documentation. Routing in this package is decided
// by hand, and without this the first symptom of a read landing on the wrong
// pool would be a lock error under load rather than a failure the first time it
// ran.
func TestTheReaderPoolRefusesWrites(t *testing.T) {
	t.Parallel()

	s := open(t)

	// A statement the writer would accept without complaint — it matches no
	// rows and changes nothing — so the only thing that can refuse it is the
	// connection being read-only. An INSERT would have been refused by a
	// constraint too, and would have passed this test on a pool that was never
	// read-only at all.
	const write = `UPDATE monitors SET name = name`
	if _, err := s.db.ExecContext(t.Context(), write); err != nil {
		t.Fatalf("the writer refused the control statement: %v", err)
	}

	err := func() error {
		_, err := s.ro.ExecContext(t.Context(), write)
		return err
	}()
	if err == nil {
		t.Fatal("the read-only pool accepted a write")
	}
	if !strings.Contains(err.Error(), "readonly") {
		t.Errorf("the read-only pool failed with %v, want a readonly error", err)
	}
}

// A separate connection must not mean a stale one. Every handler in the product
// writes and then reads back what it wrote — create a monitor, return it — and a
// reader on a snapshot from before the commit would serve the caller a 404 for
// the row they just created.
func TestReadersSeeACommitImmediately(t *testing.T) {
	t.Parallel()

	s := open(t)
	m := testMonitor("Fresh")
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetMonitor(t.Context(), m.ID)
	if err != nil {
		t.Fatalf("read back the row just written: %v", err)
	}
	if got.Monitor.Name != "Fresh" {
		t.Errorf("name = %q, want the value just committed", got.Monitor.Name)
	}

	if err := s.DeleteMonitor(t.Context(), m.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetMonitor(t.Context(), m.ID); err != store.ErrNotFound {
		t.Errorf("get after delete = %v, want store.ErrNotFound", err)
	}
}

// The writer stays at one connection whatever the reader pool is set to. Every
// check-then-act in this package — a taken tag slug, a repeat subscriber — is
// exact because of it, and widening the writer would break those quietly.
func TestTheWriterIsAlwaysOneConnection(t *testing.T) {
	t.Parallel()

	for _, readers := range []int{0, 1, 16} {
		s, err := OpenWithOptions(t.Context(),
			filepath.Join(t.TempDir(), "cairn.db"), Options{Readers: readers})
		if err != nil {
			t.Fatalf("open with %d readers: %v", readers, err)
		}
		if got := s.db.Stats().MaxOpenConnections; got != 1 {
			t.Errorf("readers=%d: writer pool = %d connections, want 1", readers, got)
		}
		_ = s.Close()
	}
}

func TestDefaultReadersIsClampedAtBothEnds(t *testing.T) {
	t.Parallel()

	if got := defaultReaders(); got < 2 || got > maxReaders {
		t.Errorf("defaultReaders() = %d, want between 2 and %d on a %d-CPU machine",
			got, maxReaders, runtime.GOMAXPROCS(0))
	}
}

func TestPoolsReportsBothSides(t *testing.T) {
	t.Parallel()

	s, err := OpenWithOptions(t.Context(),
		filepath.Join(t.TempDir(), "cairn.db"), Options{Readers: 3})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	pools := s.Pools()
	if len(pools) != 2 {
		t.Fatalf("reported %d pools, want 2", len(pools))
	}
	byName := map[string]int{}
	for _, p := range pools {
		byName[p.Name] = p.Max
	}
	if byName["writer"] != 1 {
		t.Errorf("writer max = %d, want 1", byName["writer"])
	}
	if byName["reader"] != 3 {
		t.Errorf("reader max = %d, want the 3 asked for", byName["reader"])
	}
}
