package migrate

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func migrationFS(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for name, body := range files {
		out["sqlite/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return out
}

func TestApplyRunsInOrderAndOnlyOnce(t *testing.T) {
	t.Parallel()

	fsys := migrationFS(map[string]string{
		"0001_first.sql":  `CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;`,
		"0002_second.sql": `CREATE TABLE b (id INTEGER PRIMARY KEY REFERENCES a(id)) STRICT;`,
	})
	db := testDB(t)

	applied, err := Apply(t.Context(), db, fsys, "sqlite")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 2 || applied[0].Version != 1 || applied[1].Version != 2 {
		t.Fatalf("applied %+v, want 0001 then 0002", applied)
	}

	again, err := Apply(t.Context(), db, fsys, "sqlite")
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second apply ran %d migrations, want 0", len(again))
	}
}

// A released migration that was edited means two installs claim the same schema
// version with different schemas. Fatal, not a warning — that is the whole point
// of storing the checksum.
func TestApplyRejectsEditedMigration(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	original := migrationFS(map[string]string{"0001_first.sql": `CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;`})
	if _, err := Apply(t.Context(), db, original, "sqlite"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	edited := migrationFS(map[string]string{"0001_first.sql": `CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT) STRICT;`})
	_, err := Apply(t.Context(), db, edited, "sqlite")
	if err == nil {
		t.Fatal("apply accepted an edited migration, want an error")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("error = %v, want it to mention the checksum", err)
	}
}

// A failing migration must leave nothing behind, or the retry on next start
// meets half its own work.
func TestApplyRollsBackOnFailure(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	fsys := migrationFS(map[string]string{
		"0001_broken.sql": "CREATE TABLE good (id INTEGER PRIMARY KEY) STRICT;\nCREATE TABLE bad (id INTEGER PRIMARY KEY) STRIKT;",
	})

	if _, err := Apply(t.Context(), db, fsys, "sqlite"); err == nil {
		t.Fatal("apply accepted invalid SQL, want an error")
	}

	var tables int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'good'`).Scan(&tables); err != nil {
		t.Fatalf("inspect schema: %v", err)
	}
	if tables != 0 {
		t.Error("the first statement of a failed migration was left behind")
	}

	var recorded int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if recorded != 0 {
		t.Errorf("recorded %d migrations, want 0", recorded)
	}
}

func TestLoadRejectsBadFilenames(t *testing.T) {
	t.Parallel()

	_, err := Load(migrationFS(map[string]string{"initial.sql": "SELECT 1;"}), "sqlite")
	if err == nil {
		t.Fatal("Load accepted a file that is not NNNN_snake_case.sql")
	}
}

// A downgrade is the case the runner used to miss entirely: it iterates the
// migrations the binary carries, finds each already applied with a matching
// checksum, and never looks at the versions beyond them. That start is clean,
// silent, and running against a schema it does not understand.
func TestApplyRefusesADatabaseMigratedByANewerBinary(t *testing.T) {
	t.Parallel()

	newer := migrationFS(map[string]string{
		"0001_first.sql":  `CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;`,
		"0002_second.sql": `CREATE TABLE b (id INTEGER PRIMARY KEY) STRICT;`,
	})
	older := migrationFS(map[string]string{
		"0001_first.sql": `CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;`,
	})

	db := testDB(t)
	if _, err := Apply(t.Context(), db, newer, "sqlite"); err != nil {
		t.Fatalf("apply newer: %v", err)
	}

	_, err := Apply(t.Context(), db, older, "sqlite")
	if err == nil {
		t.Fatal("older binary started against a newer schema, want refusal")
	}
	if !errors.Is(err, ErrSchemaAhead) {
		t.Fatalf("error %v, want ErrSchemaAhead", err)
	}
	// The message has to name the version, because "schema too new" without a
	// number leaves the operator guessing which backup to reach for.
	if !strings.Contains(err.Error(), "0002") {
		t.Errorf("error %q does not name the offending migration", err)
	}
}

// A binary at exactly head is the normal case and must not trip the guard.
func TestApplyAcceptsADatabaseAtExactlyHead(t *testing.T) {
	t.Parallel()

	fsys := migrationFS(map[string]string{
		"0001_first.sql": `CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;`,
	})
	db := testDB(t)
	if _, err := Apply(t.Context(), db, fsys, "sqlite"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, err := Apply(t.Context(), db, fsys, "sqlite"); err != nil {
		t.Fatalf("second apply: %v", err)
	}
}
