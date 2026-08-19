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
	"time"

	_ "modernc.org/sqlite" // pure Go: cgo would forfeit the static cross-compiled binary

	"github.com/webloomlabs/uptime-cairn/internal/store"
	"github.com/webloomlabs/uptime-cairn/internal/store/migrate"
	"github.com/webloomlabs/uptime-cairn/migrations"
)

// ErrNotFound is the package-level alias of store.ErrNotFound, returned instead
// of sql.ErrNoRows so callers never have to import database/sql to handle a
// missing row — which would leak the backend past the interface ADR-002 put here.
var ErrNotFound = store.ErrNotFound

// Store is the SQLite implementation of the interfaces in internal/store.
type Store struct {
	db *sql.DB
}

// Open opens the database at path and applies the connection pragmas.
//
// One connection, deliberately. SQLite has a single writer, and a pool of them
// produces lock contention rather than throughput. A reader pool alongside a
// single writer is the Phase 1 refinement; at the scale this slice runs it would
// be optimising something nobody has measured.
func Open(ctx context.Context, path string) (*Store, error) {
	// auto_vacuum(2) is INCREMENTAL. It MUST be set before the first table is
	// created: changing it afterwards needs a full VACUUM that rewrites the
	// whole file and wants free space equal to its size, which on a Pi with a
	// 32GB card is the difference between working and not (data model §9.2).
	// Without it, retention deletes rows and the file never shrinks.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"+
			"&_pragma=busy_timeout(5000)&_pragma=synchronous(1)"+
			"&_pragma=auto_vacuum(2)",
		path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Migrate brings the database to head. Called on every start, per
// PHASE-1-PLAN.md §4.2, and fatal on a checksum mismatch.
func (s *Store) Migrate(ctx context.Context) ([]migrate.Migration, error) {
	return migrate.Apply(ctx, s.db, migrations.SQLite, "sqlite")
}

// Close checkpoints and closes the database. WAL wants a clean close.
func (s *Store) Close() error { return s.db.Close() }

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
