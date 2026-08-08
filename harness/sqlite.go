package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteTarget measures the schema directly, applying the canonical migrations
// from migrations/sqlite rather than a copy of them. If the harness kept its own
// schema it would validate something the product does not use, which is the one
// way this whole exercise could quietly become worthless.
type SQLiteTarget struct {
	db            *sql.DB
	path          string
	migrationsDir string
	orgID         []byte
	probeID       []byte
}

func NewSQLiteTarget(path, migrationsDir string) *SQLiteTarget {
	return &SQLiteTarget{path: path, migrationsDir: migrationsDir}
}

func (t *SQLiteTarget) Name() string { return "sqlite" }

func (t *SQLiteTarget) Setup(ctx context.Context, w *Workload, rollupHours int) error {
	_ = os.Remove(t.path)
	_ = os.Remove(t.path + "-wal")
	_ = os.Remove(t.path + "-shm")

	// Pragmas are part of the schema contract (data model §7), not incidental
	// setup. busy_timeout matters because SQLite has a single writer: waiting is
	// correct, failing is not. synchronous=NORMAL is the §11.7 decision.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"+
			"&_pragma=busy_timeout(5000)&_pragma=synchronous(1)",
		t.path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	// One connection. SQLite is a single writer, and letting database/sql open a
	// pool would produce lock contention that tells us nothing about the schema.
	db.SetMaxOpenConns(1)
	t.db = db

	if err := t.migrate(ctx); err != nil {
		return err
	}
	if err := t.seed(ctx, w, rollupHours); err != nil {
		return err
	}
	// Without this the planner has no statistics and will pick plans that have
	// nothing to do with the ones a real install would use.
	if _, err := db.ExecContext(ctx, "ANALYZE"); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	return nil
}

// migrate applies every .sql file in the migrations directory in name order.
// This is the shape the data model (§8) specifies for the real runner: numbered,
// forward-only, applied in order. The real one adds checksums and advisory
// locking; this one only needs to build a database to measure.
func (t *SQLiteTarget) migrate(ctx context.Context) error {
	entries, err := os.ReadDir(t.migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir %s: %w", t.migrationsDir, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("no .sql migrations found in %s", t.migrationsDir)
	}
	sort.Strings(files)

	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(t.migrationsDir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		for _, stmt := range splitStatements(string(body)) {
			if _, err := t.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w\nstatement: %s", name, err, truncate(stmt, 200))
			}
		}
	}
	return nil
}

// splitStatements strips line comments and splits on semicolons. Adequate here
// because the migrations contain no semicolons inside string literals; the real
// runner should not reuse this.
func splitStatements(body string) []string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	var out []string
	for _, s := range strings.Split(b.String(), ";") {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (t *SQLiteTarget) seed(ctx context.Context, w *Workload, rollupHours int) error {
	t.orgID = w.OrgID
	t.probeID = w.ProbeID
	now := w.BaseTime.UnixMilli()

	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO organisations (id, name, slug, created_at, updated_at) VALUES (?,?,?,?,?)`,
		w.OrgID, "Load Test", "load-test", now, now); err != nil {
		return fmt.Errorf("seed org: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO probes (id, org_id, name, mode, enabled, created_at) VALUES (?,?,?,?,?,?)`,
		w.ProbeID, w.OrgID, "embedded", "embedded", 1, now); err != nil {
		return fmt.Errorf("seed probe: %w", err)
	}
	for i, g := range w.Groups {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO groups (id, org_id, name, created_at, updated_at) VALUES (?,?,?,?,?)`,
			g, w.OrgID, fmt.Sprintf("group-%04d", i), now, now); err != nil {
			return fmt.Errorf("seed group: %w", err)
		}
	}
	for i, tg := range w.Tags {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tags (id, org_id, name, slug, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
			tg, w.OrgID, fmt.Sprintf("tag-%04d", i), fmt.Sprintf("tag-%04d", i), now, now); err != nil {
			return fmt.Errorf("seed tag: %w", err)
		}
	}

	insMonitor, err := tx.PrepareContext(ctx, `
		INSERT INTO monitors (
			id, org_id, name, type, config, target, enabled,
			interval_seconds, timeout_seconds, group_id, created_at, updated_at
		) VALUES (?,?,?,?,?,?,1,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insMonitor.Close()

	insState, err := tx.PrepareContext(ctx, `
		INSERT INTO monitor_state (
			monitor_id, org_id, status, last_check_at, next_check_at,
			consecutive_failures, last_response_time_ms, state_version
		) VALUES (?,?,?,?,?,0,?,?)`)
	if err != nil {
		return err
	}
	defer insState.Close()

	insTag, err := tx.PrepareContext(ctx,
		`INSERT INTO monitor_tags (monitor_id, tag_id, org_id) VALUES (?,?,?)`)
	if err != nil {
		return err
	}
	defer insTag.Close()

	for i, m := range w.Monitors {
		if _, err := insMonitor.ExecContext(ctx,
			m.ID, w.OrgID, m.Name, m.Type, m.Config, m.Target,
			m.Interval, m.Timeout, m.GroupID,
			m.CreatedAt.UnixMilli(), m.UpdatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("seed monitor %d: %w", i, err)
		}
		if _, err := insState.ExecContext(ctx,
			m.ID, w.OrgID, m.Status,
			m.UpdatedAt.UnixMilli(), m.UpdatedAt.Add(time.Minute).UnixMilli(),
			42.0, int64(i)); err != nil {
			return fmt.Errorf("seed state %d: %w", i, err)
		}
		for _, tg := range m.TagIDs {
			if _, err := insTag.ExecContext(ctx, m.ID, tg, w.OrgID); err != nil {
				return fmt.Errorf("seed monitor_tag %d: %w", i, err)
			}
		}
	}

	if err := seedRollups(ctx, tx, w, rollupHours); err != nil {
		return err
	}
	return tx.Commit()
}

// seedRollups fills heartbeat_1m so the history query has something to read.
// A short window across every monitor is deliberate: a long window across a few
// monitors would leave the index unrealistically narrow, and a long window across
// all of them is more rows than a CI runner should spend its time inserting.
func seedRollups(ctx context.Context, tx *sql.Tx, w *Workload, hours int) error {
	if hours <= 0 {
		return nil
	}
	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO heartbeat_1m (
			bucket_start, monitor_id, org_id, up_count, down_count,
			pending_count, maintenance_count, response_time_sum,
			response_time_count, response_time_min, response_time_max, response_time_p95
		) VALUES (?,?,?,?,?,0,0,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ins.Close()

	r := rand.New(rand.NewSource(99))
	buckets := hours * 60
	for _, m := range w.Monitors {
		for b := 0; b < buckets; b++ {
			start := w.BaseTime.Add(time.Duration(b) * time.Minute).UnixMilli()
			up, down := 1, 0
			if r.Intn(200) == 0 {
				up, down = 0, 1
			}
			sum := 20 + r.Float64()*180
			if _, err := ins.ExecContext(ctx,
				start, m.ID, w.OrgID, up, down, sum, 1, sum, sum, sum); err != nil {
				return fmt.Errorf("seed rollup: %w", err)
			}
		}
	}
	return nil
}

func (t *SQLiteTarget) WriteHeartbeats(ctx context.Context, batch []Heartbeat) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// ON CONFLICT DO NOTHING is the product's ingest path, not a safety net here:
	// ADR-005 makes probe delivery at-least-once, so the data model (§5.2) gives
	// heartbeats a unique natural key and ingest absorbs the resend. Measuring a
	// plain INSERT would measure a write path the product does not use.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO heartbeats (
			time, monitor_id, org_id, probe_id, status,
			response_time_ms, message, attempt, important, suppressed
		) VALUES (?,?,?,?,?,?,?,1,?,0)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, h := range batch {
		var msg any
		important := 0
		if h.Important {
			important = 1
			msg = "state change"
		}
		if _, err := stmt.ExecContext(ctx,
			h.Time.UnixMicro(), h.MonitorID, t.orgID, t.probeID,
			h.Status, h.ResponseMS, msg, important); err != nil {
			return fmt.Errorf("write heartbeat: %w", err)
		}
	}
	return tx.Commit()
}

// ListMonitors is the query ADR-004 specifies: keyset pagination on
// (updated_at, id), filters applied server-side, never a full set.
//
// The status filter drives from monitor_state rather than monitors, because
// status lives there (§4.2) and the filtered side is the small one. This is the
// §6.2 hypothesis the harness exists to confirm or kill.
func (t *SQLiteTarget) ListMonitors(ctx context.Context, q ListQuery) (ListResult, error) {
	var (
		sb   strings.Builder
		args []any
	)

	switch {
	case q.Status != "":
		sb.WriteString(`SELECT m.id, m.updated_at FROM monitor_state s
			JOIN monitors m ON m.id = s.monitor_id
			WHERE s.org_id = ? AND s.status = ?`)
		args = append(args, t.orgID, q.Status)
	case q.TagID != nil:
		sb.WriteString(`SELECT m.id, m.updated_at FROM monitor_tags mt
			JOIN monitors m ON m.id = mt.monitor_id
			WHERE mt.org_id = ? AND mt.tag_id = ?`)
		args = append(args, t.orgID, q.TagID)
	case q.GroupID != nil:
		sb.WriteString(`SELECT m.id, m.updated_at FROM monitors m
			WHERE m.org_id = ? AND m.group_id = ?`)
		args = append(args, t.orgID, q.GroupID)
	default:
		sb.WriteString(`SELECT m.id, m.updated_at FROM monitors m WHERE m.org_id = ?`)
		args = append(args, t.orgID)
	}

	if q.Cursor != nil {
		// Row-value comparison, supported by SQLite 3.15+ and by Postgres, so
		// the same query shape works on both backends without a rewrite.
		sb.WriteString(` AND (m.updated_at, m.id) < (?, ?)`)
		args = append(args, q.Cursor.UpdatedAt.UnixMilli(), q.Cursor.ID)
	}
	sb.WriteString(` ORDER BY m.updated_at DESC, m.id DESC LIMIT ?`)
	args = append(args, q.Limit)

	rows, err := t.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return ListResult{}, err
	}
	defer rows.Close()

	var res ListResult
	for rows.Next() {
		var id []byte
		var updated int64
		if err := rows.Scan(&id, &updated); err != nil {
			return ListResult{}, err
		}
		res.Rows++
		res.Next = &Cursor{UpdatedAt: time.UnixMilli(updated), ID: id}
	}
	return res, rows.Err()
}

// Membership is §6.5 option 3: max(state_version) and count(*) over the filter,
// one index-only scan, detecting both content and membership changes.
func (t *SQLiteTarget) Membership(ctx context.Context, q ListQuery) (MembershipResult, error) {
	var (
		query string
		args  []any
	)
	switch {
	case q.TagID != nil:
		query = `SELECT COALESCE(MAX(s.state_version),0), COUNT(*)
			FROM monitor_tags mt JOIN monitor_state s ON s.monitor_id = mt.monitor_id
			WHERE mt.org_id = ? AND mt.tag_id = ?`
		args = []any{t.orgID, q.TagID}
	case q.Status != "":
		query = `SELECT COALESCE(MAX(state_version),0), COUNT(*)
			FROM monitor_state WHERE org_id = ? AND status = ?`
		args = []any{t.orgID, q.Status}
	default:
		query = `SELECT COALESCE(MAX(state_version),0), COUNT(*)
			FROM monitor_state WHERE org_id = ?`
		args = []any{t.orgID}
	}

	var res MembershipResult
	err := t.db.QueryRowContext(ctx, query, args...).Scan(&res.Version, &res.Count)
	return res, err
}

func (t *SQLiteTarget) History(ctx context.Context, monitorID []byte, from, to time.Time) (int, error) {
	rows, err := t.db.QueryContext(ctx, `
		SELECT bucket_start, up_count, down_count, response_time_sum, response_time_count
		FROM heartbeat_1m
		WHERE org_id = ? AND monitor_id = ? AND bucket_start >= ? AND bucket_start < ?
		ORDER BY bucket_start`,
		t.orgID, monitorID, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var start int64
		var up, down int
		var sum, count sql.NullFloat64
		if err := rows.Scan(&start, &up, &down, &sum, &count); err != nil {
			return 0, err
		}
		n++
	}
	return n, rows.Err()
}

func (t *SQLiteTarget) Close() error {
	if t.db == nil {
		return nil
	}
	return t.db.Close()
}

// DBSizeBytes reports the on-disk size, so the report can show what a given
// monitor count actually costs. Retention defaults in the data model are
// placeholders until this produces real numbers.
func (t *SQLiteTarget) DBSizeBytes() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(t.path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}
