// Package migrate runs the versioned SQL migrations.
//
// Our own runner, no third-party dependency (data model §11.2): golang-migrate,
// goose, and atlas each carry a CLI, a migration-table format, and opinions
// about dialects, none of which is free in a project that publishes an SBOM and
// vendors everything. The whole runner is about a hundred lines.
//
// Forward-only, numbered, immutable once released. There are no down
// migrations: a rollback path that is never exercised is a rollback path that
// does not work, and the documented recovery stance is restore-from-backup,
// which is tested.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
)

// ErrNotImplemented marks the parts of the skeleton Phase 1 Month 1 fills in.
var ErrNotImplemented = errors.New("migrate: not implemented")

// Migration is one numbered file: 0007_add_thing.sql.
type Migration struct {
	Version int
	Name    string
	SQL     string

	// Checksum is over SQL. A mismatch against an applied migration means a
	// released file was edited, which is fatal on startup rather than a warning
	// — the alternative is two installs claiming the same schema version with
	// different schemas.
	Checksum string

	// Transactional is false for the few migrations that cannot run inside one.
	// Postgres has transactional DDL and SQLite largely does too, but Timescale
	// operations such as create_hypertable and policy management have their own
	// constraints. Such a file says so in a header comment, with the trade
	// recorded in the file itself (data model §8).
	Transactional bool
}

// Apply brings db up to head using the migrations embedded in fsys under dir
// ("sqlite" or "postgres"), and is safe to call on every start.
//
// The algorithm, specified in data model §8 and repeated here because the order
// is the point:
//
//  1. Create schema_migrations (version, name, checksum, applied_at) if absent.
//  2. Take an advisory lock, so two processes starting at once cannot both
//     migrate. Postgres has pg_advisory_lock; SQLite's single-writer model plus
//     BEGIN IMMEDIATE is the equivalent.
//  3. Read the applied rows and verify each one's checksum against the embedded
//     file. Abort on mismatch.
//  4. Apply the remainder in order, each inside a transaction that also inserts
//     its schema_migrations row, so a failure leaves no half-applied version.
//
// It must be idempotent against a partially-applied state, because that is what
// a crash mid-migration leaves behind.
func Apply(ctx context.Context, db *sql.DB, fsys fs.FS, dir string) error {
	return ErrNotImplemented
}
