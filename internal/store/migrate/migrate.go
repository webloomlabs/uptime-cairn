// Package migrate runs the versioned SQL migrations.
//
// Our own runner, no third-party dependency (data model §11.2): golang-migrate,
// goose, and atlas each carry a CLI, a migration-table format, and opinions
// about dialects, none of which is free in a project that publishes an SBOM and
// vendors everything.
//
// Forward-only, numbered, immutable once released. There are no down
// migrations: a rollback path that is never exercised is a rollback path that
// does not work, and the documented recovery stance is restore-from-backup,
// which is tested.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// filename is the naming convention: NNNN_snake_case.sql. A file that does not
// match is an error rather than a skip — a migration silently not applied is the
// worst outcome available here.
var filename = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// noTransaction marks the few migrations that cannot run inside a transaction.
// Postgres has transactional DDL and SQLite largely does too, but Timescale
// operations such as create_hypertable have their own constraints, and SQLite
// will not change auto_vacuum inside one. A file opts out with this on a line of
// its own, and says why in the comment beside it (data model §8).
const noTransaction = "-- +cairn no-transaction"

// Migration is one numbered file.
type Migration struct {
	Version int
	Name    string
	SQL     string

	// Checksum is sha256 over SQL, hex-encoded. A mismatch against an applied
	// migration means a released file was edited, which is fatal on startup
	// rather than a warning — the alternative is two installs claiming the same
	// schema version with different schemas.
	Checksum string

	Transactional bool
}

// Load reads and parses the migrations in dir, sorted by version.
func Load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations in %s: %w", dir, err)
	}

	var migrations []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := filename.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %s does not match NNNN_snake_case.sql", e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}

		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)

		migrations = append(migrations, Migration{
			Version:       version,
			Name:          m[2],
			SQL:           string(body),
			Checksum:      hex.EncodeToString(sum[:]),
			Transactional: !strings.Contains(string(body), noTransaction),
		})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })

	for i, m := range migrations {
		if i > 0 && m.Version == migrations[i-1].Version {
			return nil, fmt.Errorf("duplicate migration version %04d", m.Version)
		}
	}
	return migrations, nil
}

// Apply brings db up to head and returns the migrations it applied. Safe to call
// on every start, which is how it is called (PHASE-1-PLAN.md §4.2).
//
// The order is the point:
//
//  1. Create schema_migrations if absent.
//  2. Verify every already-applied migration's checksum against the embedded
//     file, and abort on mismatch.
//  3. Apply the rest in version order, each inside a transaction that also
//     inserts its schema_migrations row, so a failure leaves no half-applied
//     version.
//
// The transaction opens with BEGIN IMMEDIATE, which takes SQLite's write lock up
// front. That is the advisory lock: two processes starting at once, one waits
// rather than both migrating. Postgres will want pg_advisory_lock in its place,
// which is why this is a package rather than a function on the store.
func Apply(ctx context.Context, db *sql.DB, fsys fs.FS, dir string) ([]Migration, error) {
	migrations, err := Load(fsys, dir)
	if err != nil {
		return nil, err
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			checksum   TEXT    NOT NULL,
			applied_at INTEGER NOT NULL
		) STRICT`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedChecksums(ctx, db)
	if err != nil {
		return nil, err
	}

	var ran []Migration
	for _, m := range migrations {
		if have, ok := applied[m.Version]; ok {
			if have != m.Checksum {
				return nil, fmt.Errorf(
					"migration %04d_%s was applied with checksum %s but the file now hashes to %s: "+
						"a released migration was edited, which two installs will disagree about — "+
						"restore the file and write a new migration instead",
					m.Version, m.Name, have, m.Checksum)
			}
			continue
		}
		if err := apply(ctx, db, m); err != nil {
			return ran, err
		}
		ran = append(ran, m)
	}
	return ran, nil
}

func appliedChecksums(ctx context.Context, db *sql.DB) (map[int]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int]string)
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[version] = checksum
	}
	return out, rows.Err()
}

// apply runs one migration. The record of it is written inside the same
// transaction as its DDL wherever the backend allows, so there is no window in
// which the schema has changed and the ledger has not.
func apply(ctx context.Context, db *sql.DB, m Migration) error {
	record := func(exec func(string, ...any) (sql.Result, error)) error {
		_, err := exec(
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, unixepoch() * 1000)`,
			m.Version, m.Name, m.Checksum)
		return err
	}

	if !m.Transactional {
		if _, err := db.ExecContext(ctx, m.SQL); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
		}
		if err := record(func(q string, args ...any) (sql.Result, error) {
			return db.ExecContext(ctx, q, args...)
		}); err != nil {
			return fmt.Errorf("record migration %04d_%s: %w", m.Version, m.Name, err)
		}
		return nil
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
	}
	defer func() { _ = conn.Close() }()

	// BEGIN IMMEDIATE rather than sql.Tx: database/sql opens a deferred
	// transaction, which takes the write lock only at the first write and so
	// leaves a window where two migrating processes both think they are first.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("migration %04d_%s: begin: %w", m.Version, m.Name, err)
	}
	rollback := func() { _, _ = conn.ExecContext(ctx, `ROLLBACK`) }

	if _, err := conn.ExecContext(ctx, m.SQL); err != nil {
		rollback()
		return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if err := record(func(q string, args ...any) (sql.Result, error) {
		return conn.ExecContext(ctx, q, args...)
	}); err != nil {
		rollback()
		return fmt.Errorf("record migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		rollback()
		return fmt.Errorf("migration %04d_%s: commit: %w", m.Version, m.Name, err)
	}
	return nil
}
