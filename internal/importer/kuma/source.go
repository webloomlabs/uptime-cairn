package kuma

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Reading somebody else's schema, defensively.
//
// Uptime Kuma's `monitor` table has grown a column almost every release: 1.18
// has no `timeout`, 1.19 adds `parent`, 1.21 adds `packet_size`, 2.x adds more
// again and renames a couple. A SELECT naming a column the file does not have
// fails the whole import with a SQL error, and the person on the other end of
// that error is migrating away from a tool they have used for two years and has
// no idea what a `packet_size` is.
//
// So every read here asks the file what it has first, via PRAGMA table_info,
// and selects the intersection. A column that is missing reads as its zero
// value, which is the same thing Kuma's own defaults would have produced. This
// is slower than a fixed SELECT by one pragma per table and it is the
// difference between "works on the versions we tested" and "works".

// source is one opened kuma.db.
type source struct {
	db       *sql.DB
	filename string

	// version is what the file says about itself, when it says anything. Kuma
	// 2.x carries it in a settings row; 1.x does not, so this is often empty
	// and the import proceeds anyway — the schema is what is read, not the
	// version string.
	version string

	// tables maps table name to the set of columns it has.
	tables map[string]map[string]bool
}

// openSource opens a Kuma database read-only.
//
// Read-only is not a precaution, it is the contract: this is the user's live
// monitoring database, quite possibly copied out from under a running Kuma, and
// an importer that could write to it is an importer nobody should run twice.
// `mode=ro` makes a mistake fail immediately rather than surface as a corrupted
// file the user notices next week.
func openSource(ctx context.Context, path, filename string) (*source, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filename, err)
	}
	// One connection. There is no concurrency to gain here — the import is a
	// sequential read — and a pool against a read-only file is pure surface.
	db.SetMaxOpenConns(1)

	s := &source{db: db, filename: filename, tables: map[string]map[string]bool{}}

	if err := s.loadSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if !s.has("monitor", "id") {
		_ = db.Close()
		return nil, fmt.Errorf(
			"%s does not look like an Uptime Kuma database: it has no `monitor` table. "+
				"The file wanted is kuma.db, usually at /app/data/kuma.db inside the container", filename)
	}
	s.version = s.readVersion(ctx)
	return s, nil
}

func (s *source) Close() error { return s.db.Close() }

// loadSchema reads every table's columns once.
func (s *source) loadSchema(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return fmt.Errorf("read schema of %s: %w", s.filename, err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, name := range names {
		columns, err := s.columnsOf(ctx, name)
		if err != nil {
			return err
		}
		s.tables[name] = columns
	}
	return nil
}

func (s *source) columnsOf(ctx context.Context, table string) (map[string]bool, error) {
	// The table name cannot be a bind parameter in a pragma, and it comes from
	// sqlite_master rather than from a user, so the quoting below is the whole
	// of the injection story: a name is wrapped in double quotes with its own
	// quotes doubled, which is SQLite's identifier rule.
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdent(table)+`)`)
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name, kind string
			notNull    int
			dflt       any
			pk         int
		)
		if err := rows.Scan(&cid, &name, &kind, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// has reports whether a table exists and, when columns are named, whether it
// has all of them.
func (s *source) has(table string, columns ...string) bool {
	got, ok := s.tables[table]
	if !ok {
		return false
	}
	for _, c := range columns {
		if !got[c] {
			return false
		}
	}
	return true
}

// selectable returns the subset of wanted that this file actually has, in the
// order given, so a scan can line up with it.
func (s *source) selectable(table string, wanted []string) []string {
	got := s.tables[table]
	out := make([]string, 0, len(wanted))
	for _, c := range wanted {
		if got[c] {
			out = append(out, c)
		}
	}
	return out
}

// scanRow reads one row into a map keyed by column name.
//
// A map rather than a struct because the column set is decided at runtime. The
// cost is a type switch at every use, and the benefit is that adding support
// for a Kuma version that renamed a field is a line in a mapping table rather
// than a second struct.
func scanRow(rows *sql.Rows, columns []string) (map[string]any, error) {
	holders := make([]any, len(columns))
	values := make([]any, len(columns))
	for i := range holders {
		holders[i] = &values[i]
	}
	if err := rows.Scan(holders...); err != nil {
		return nil, err
	}

	out := make(map[string]any, len(columns))
	for i, name := range columns {
		out[name] = values[i]
	}
	return out, nil
}

// query runs a SELECT over whichever of wanted the file has, and returns the
// rows as maps.
func (s *source) query(ctx context.Context, table string, wanted []string, where string, args ...any) ([]map[string]any, error) {
	columns := s.selectable(table, wanted)
	if len(columns) == 0 {
		return nil, nil
	}

	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = quoteIdent(c)
	}

	q := `SELECT ` + strings.Join(quoted, ", ") + ` FROM ` + quoteIdent(table)
	if where != "" {
		q += " " + where
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read %s from %s: %w", table, s.filename, err)
	}
	defer func() { _ = rows.Close() }()

	var out []map[string]any
	for rows.Next() {
		row, err := scanRow(rows, columns)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// count is the analysis pass: how many of each entity the file holds, before
// anything is imported. It is what the dry run and the guided flow show, and it
// is deliberately cheap — a COUNT(*) per table, no row reads.
func (s *source) count(ctx context.Context, table, where string) int {
	if !s.has(table) {
		return 0
	}
	q := `SELECT COUNT(*) FROM ` + quoteIdent(table)
	if where != "" {
		q += " " + where
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0
	}
	return n
}

// detected is the per-file entity census the import job reports.
func (s *source) detected(ctx context.Context) map[string]int {
	out := map[string]int{}
	for entity, table := range map[string]string{
		"monitor":      "monitor",
		"tag":          "tag",
		"notification": "notification",
		"status_page":  "status_page",
		"maintenance":  "maintenance",
		"incident":     "incident",
		"heartbeat":    "heartbeat",
	} {
		if n := s.count(ctx, table, ""); n > 0 {
			out[entity] = n
		}
	}
	// Kuma models a group as a monitor of type 'group', so the group count is
	// not a table. Reported separately because a user with forty monitors in six
	// groups reads "46 monitors" as wrong.
	if s.has("monitor", "type") {
		if n := s.count(ctx, "monitor", "WHERE type = 'group'"); n > 0 {
			out["group"] = n
			out["monitor"] -= n
		}
	}
	return out
}

// readVersion reads Kuma's own version string, when the file carries one.
func (s *source) readVersion(ctx context.Context) string {
	if !s.has("setting", "key", "value") {
		return ""
	}
	var value sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM setting WHERE key IN ('version', 'databaseVersion') LIMIT 1`).Scan(&value)
	if err != nil {
		return ""
	}
	return strings.Trim(value.String, `"`)
}

// quoteIdent wraps a SQLite identifier. Names come from sqlite_master and from
// this file's own literals; the doubling is what makes that true rather than
// merely likely.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sortedKeys keeps report output stable. An import report that reorders itself
// between two runs of the same file is one nobody can diff.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
