// Package sqlite implements the store interfaces against embedded SQLite.
//
// The solo-mode default and the one that has to run on a Pi: WAL mode, zero
// external services, no Redis (ADR-002). The pragmas that are per-connection
// rather than per-database — journal_mode, foreign_keys, busy_timeout,
// synchronous — belong in the connection initialiser here, not in a migration;
// auto_vacuum is the exception and is already set in 0001 because it cannot be
// changed later without rewriting the whole file (data model §9.2).
package sqlite

import (
	"context"
	"errors"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// ErrNotImplemented marks the parts of the skeleton Phase 1 Month 1 fills in.
var ErrNotImplemented = errors.New("sqlite store: not implemented")

// Store is the SQLite implementation of the interfaces in internal/store.
//
// It holds no exported fields: the connection, the statement cache, and the
// write serialisation are its own business, and a caller reaching for them is
// the leak ADR-002 exists to prevent.
type Store struct{}

// Open opens the database at path, applies the connection pragmas, and runs
// migrations to head — automatically, on start, per PHASE-1-PLAN.md §4.2. A
// checksum mismatch against an already-applied migration is fatal here and not
// a warning (data model §8).
//
// The driver is modernc.org/sqlite, pure Go and already justified for the load
// harness: cgo would forfeit the static cross-compiled binary the Pi user gets.
// It is not imported yet because nothing here needs it, and an unused dependency
// in go.mod is a dependency the SBOM has to explain.
func Open(ctx context.Context, path string) (*Store, error) {
	return nil, ErrNotImplemented
}

// WriteBatch implements store.HeartbeatStore.
func (s *Store) WriteBatch(ctx context.Context, beats []model.Heartbeat) error {
	return ErrNotImplemented
}

// Close releases the database. SQLite in WAL mode wants a clean close so the
// -wal file is checkpointed; the crash-recovery test in PHASE-1-PLAN.md §4.4
// asserts what happens when it does not get one.
func (s *Store) Close() error {
	return ErrNotImplemented
}
