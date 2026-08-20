// Package sqlite implements the store interfaces against embedded SQLite.
//
// The solo-mode default and the one that has to run on a Pi: WAL mode, zero
// external services, no Redis (ADR-002).
//
// auto_vacuum is here too, and it is worth saying why rather than in migration
// 0001 where the data model puts it. auto_vacuum is a per-database setting that
// only takes effect on a database with no tables yet, and a PRAGMA issued inside
// a transaction is silently ignored — which is exactly what happens when the
// migration runner wraps 0001 in one. The line in 0001 therefore documents the
// intent and does nothing; this DSN is what actually sets it, because Open runs
// before the first migration and outside any transaction.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"time"

	_ "modernc.org/sqlite" // pure Go: cgo would forfeit the static cross-compiled binary

	"github.com/webloomlabs/uptime-cairn/internal/store"
	"github.com/webloomlabs/uptime-cairn/internal/store/migrate"
	"github.com/webloomlabs/uptime-cairn/internal/telemetry"
	"github.com/webloomlabs/uptime-cairn/migrations"
)

// ErrNotFound is the package-level alias of store.ErrNotFound, returned instead
// of sql.ErrNoRows so callers never have to import database/sql to handle a
// missing row — which would leak the backend past the interface ADR-002 put here.
var ErrNotFound = store.ErrNotFound

// ErrConflict is the same arrangement for the other error every backend has to
// report identically: a well-formed write the current state will not have.
var ErrConflict = store.ErrConflict

// Store is the SQLite implementation of the interfaces in internal/store.
//
// Two pools, and which one a statement uses is a correctness decision rather
// than a tuning one:
//
//   - db is the writer. Exactly one connection, always. SQLite takes one write
//     lock per database and a pool of writers produces lock contention rather
//     than throughput. Everything that writes, and every read that a write in
//     the same function depends on, goes here.
//   - ro is the reader pool, opened read-only. WAL lets readers run against a
//     committed snapshot while a write is in flight, so a long scan no longer
//     stalls the writer behind it.
//
// The invariant that makes this safe is that the writer stays at one connection.
// Every check-then-act in this package does its check inside a transaction on
// db — CreateTag against a taken slug, CreateSubscriber against a repeat
// address — and those stay exact for the same reason they were exact before:
// there is one write connection, so there is one such transaction at a time.
// Moving a check to ro would break that, which is why the reader pool is only
// ever used by functions that read and return.
type Store struct {
	db *sql.DB
	ro *sql.DB
}

// Options tunes the pools. The zero value is the default, which is what solo
// mode uses.
type Options struct {
	// Readers is the size of the read pool. Zero picks a default from the
	// machine.
	Readers int
}

// maxReaders caps the default. Past a handful of concurrent scans the bottleneck
// stops being connection availability and becomes the disk, and every connection
// carries its own page cache — about 2MB — which on the 1GB Pi this has to run
// on is a real number rather than a rounding error.
const maxReaders = 8

// readerIdleTimeout is how long an unused reader connection is kept before it is
// closed and its page cache handed back. An instance checking four monitors
// overnight should not still be holding eight caches it stopped using at 6pm.
const readerIdleTimeout = 5 * time.Minute

// Open opens the database at path with the default pool sizes.
func Open(ctx context.Context, path string) (*Store, error) {
	return OpenWithOptions(ctx, path, Options{})
}

// OpenWithOptions opens the database at path and applies the connection pragmas.
func OpenWithOptions(ctx context.Context, path string, opts Options) (*Store, error) {
	// auto_vacuum(2) is INCREMENTAL. It MUST be set before the first table is
	// created: changing it afterwards needs a full VACUUM that rewrites the
	// whole file and wants free space equal to its size, which on a Pi with a
	// 32GB card is the difference between working and not (data model §9.2).
	// Without it, retention deletes rows and the file never shrinks.
	// secure_delete(fast) zeroes freed content inside pages SQLite is rewriting
	// anyway, and skips the ones that would need an extra write. Full
	// secure_delete zeroes everything, which on a table deleting twenty million
	// heartbeats a day is a cost nobody asked for; FAST is close to free and
	// covers what actually matters — an overwritten or deleted credential should
	// not stay legible in the slack space of its own page.
	//
	// It bounds the problem rather than solving it, and the difference is worth
	// stating: it applies from this connection onward, so bytes already sitting
	// in a database written by an earlier version stay there until the pages are
	// reused. A credential that was ever stored in plaintext has to be rotated,
	// not scrubbed — it was in every backup too (data model §12.7).
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"+
			"&_pragma=busy_timeout(5000)&_pragma=synchronous(1)"+
			"&_pragma=auto_vacuum(2)&_pragma=secure_delete(fast)",
		path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	// The writer's one connection is never allowed to go idle out of the pool.
	// It is what holds the database in WAL mode and keeps the -shm file mapped,
	// and the read-only pool below cannot create either: a mode=ro connection
	// that arrives to find no shared-memory index has no way to build one and
	// fails with "attempt to write a readonly database". Keeping the writer
	// pinned removes that ordering hazard entirely.
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// mode=ro rather than a promise. The routing in this package is reviewed by
	// hand, and a read that slips onto the wrong pool would otherwise show up as
	// a rare lock error under load rather than as an obvious failure the first
	// time it ran. The operating system refusing the write makes the mistake
	// loud and immediate.
	//
	// journal_mode, auto_vacuum and secure_delete are deliberately absent: all
	// three write to the database file, and issuing them here would fail. They
	// are properties of the file, already set by the writer above, and readers
	// inherit them.
	roDSN := fmt.Sprintf(
		"file:%s?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)

	ro, err := sql.Open("sqlite", roDSN)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite readers %s: %w", path, err)
	}
	readers := opts.Readers
	if readers <= 0 {
		readers = defaultReaders()
	}
	ro.SetMaxOpenConns(readers)
	ro.SetMaxIdleConns(readers)
	ro.SetConnMaxIdleTime(readerIdleTimeout)

	if err := ro.PingContext(ctx); err != nil {
		_ = ro.Close()
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite readers %s: %w", path, err)
	}
	return &Store{db: db, ro: ro}, nil
}

// defaultReaders sizes the read pool from the machine, clamped at both ends.
// Two is the floor because one reader would reintroduce the queue this pool
// exists to remove, just on the other side.
func defaultReaders() int {
	n := runtime.GOMAXPROCS(0)
	if n < 2 {
		return 2
	}
	if n > maxReaders {
		return maxReaders
	}
	return n
}

// Migrate brings the database to head. Called on every start, per
// PHASE-1-PLAN.md §4.2, and fatal on a checksum mismatch.
//
// On the writer, and it has to be: DDL is a write, and a migration that ran on a
// read-only connection would fail at the first CREATE TABLE.
func (s *Store) Migrate(ctx context.Context) ([]migrate.Migration, error) {
	return migrate.Apply(ctx, s.db, migrations.SQLite, "sqlite")
}

// Pools reports what each pool is doing, for /metrics.
//
// The writer's wait count is the number that answers the question the load-test
// harness kept raising: whether a slow path is slow because the work is slow or
// because it is queued behind somebody else's write. Before the reader pool
// existed there was nothing to ask — every statement queued on the same
// connection, so the answer was always "both" and the counter would have been
// unreadable.
func (s *Store) Pools() []telemetry.Pool {
	return []telemetry.Pool{
		pool("writer", s.db.Stats()),
		pool("reader", s.ro.Stats()),
	}
}

func pool(name string, st sql.DBStats) telemetry.Pool {
	return telemetry.Pool{
		Name:        name,
		Max:         st.MaxOpenConnections,
		Open:        st.OpenConnections,
		InUse:       st.InUse,
		Idle:        st.Idle,
		WaitCount:   st.WaitCount,
		WaitSeconds: st.WaitDuration.Seconds(),
	}
}

// Close checkpoints and closes the database. WAL wants a clean close.
//
// Readers first. A read-only connection cannot checkpoint, and the writer's
// close is what truncates the WAL — doing it in the other order would leave
// readers holding a snapshot the writer is trying to fold away.
func (s *Store) Close() error {
	roErr := s.ro.Close()
	if err := s.db.Close(); err != nil {
		return err
	}
	return roErr
}

// millis and micros convert to the schema's integer time columns. Time is stored
// as epoch milliseconds everywhere except heartbeats, which are microseconds
// (data model §1) — the one exception, and the reason these are two functions
// rather than one.
func millis(t time.Time) int64 { return t.UnixMilli() }
func micros(t time.Time) int64 { return t.UnixMicro() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
func fromMicros(us int64) time.Time { return time.UnixMicro(us).UTC() }

func nullableTime(ms sql.NullInt64) *time.Time {
	if !ms.Valid {
		return nil
	}
	t := fromMillis(ms.Int64)
	return &t
}

func nullableFloat(f sql.NullFloat64) *float64 {
	if !f.Valid {
		return nil
	}
	return &f.Float64
}
