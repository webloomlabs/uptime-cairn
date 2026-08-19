package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// MonitorWithState and Cursor are the shared store types; aliased so this
// package's signatures read naturally without redeclaring them.
type (
	MonitorWithState = store.MonitorWithState
	Cursor           = store.Cursor
)

// DecodeCursor is re-exported for callers already holding this package.
var DecodeCursor = store.DecodeCursor

const monitorColumns = `
	m.id, m.org_id, m.name, m.description, m.type, m.config, m.config_secrets, m.target,
	m.push_token_hash, m.enabled, m.interval_seconds, m.timeout_seconds, m.retries,
	m.retry_interval_seconds, m.resend_after, m.upside_down, m.notify_on_recovery,
	m.group_id, m.parent_monitor_id, m.created_at, m.updated_at,
	s.status, s.last_check_at, s.next_check_at, s.last_status_change_at,
	s.consecutive_failures, s.last_response_time_ms, s.last_message, s.state_version`

// CreateMonitor inserts the monitor and its initial state in one transaction.
//
// A monitor without a state row would be invisible to the status filter and to
// the scheduler's due query, so the two are written together or not at all.
func (s *Store) CreateMonitor(ctx context.Context, m model.Monitor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO monitors (
			id, org_id, name, description, type, config, config_secrets, target, push_token_hash,
			enabled, interval_seconds, timeout_seconds, retries, retry_interval_seconds,
			resend_after, upside_down, notify_on_recovery, group_id,
			parent_monitor_id, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID[:], m.OrgID[:], m.Name, nullString(m.Description), m.Type, string(m.Config),
		nullBytes(m.ConfigSecrets),
		nullString(m.Target), nullBytes(m.PushTokenHash), boolToInt(m.Enabled),
		int64(m.Interval.Seconds()), int64(m.Timeout.Seconds()), m.Retries,
		nullSeconds(m.RetryInterval), m.ResendAfter, boolToInt(m.UpsideDown),
		boolToInt(m.NotifyOnRecovery), nullID(m.GroupID), nullID(m.ParentMonitorID),
		millis(m.CreatedAt), millis(m.UpdatedAt),
	); err != nil {
		return fmt.Errorf("insert monitor: %w", err)
	}

	// A monitor that has never reported is pending, not down: it has not earned
	// a verdict either way, and rendering it as down would page someone for a
	// monitor that has not run yet.
	status := model.MonitorStatusPending
	if !m.Enabled {
		status = model.MonitorStatusPaused
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO monitor_state (monitor_id, org_id, status, consecutive_failures, state_version)
		VALUES (?,?,?,0,0)`,
		m.ID[:], m.OrgID[:], status,
	); err != nil {
		return fmt.Errorf("insert monitor_state: %w", err)
	}
	return tx.Commit()
}

// GetMonitor returns one monitor with its state, or ErrNotFound.
func (s *Store) GetMonitor(ctx context.Context, id model.ID) (MonitorWithState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+monitorColumns+`
		FROM monitors m JOIN monitor_state s ON s.monitor_id = m.id
		WHERE m.id = ?`, id[:])

	out, err := scanMonitor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MonitorWithState{}, ErrNotFound
	}
	return out, err
}

// ListMonitors returns one page, newest-updated first, plus the cursor for the
// next page. It asks for limit+1 rows and reports has_more from whether the
// extra one came back — a count would cost a scan of the filtered set on every
// page fetch, which is the cost cursor pagination exists to avoid.
func (s *Store) ListMonitors(ctx context.Context, after *Cursor, limit int) ([]MonitorWithState, bool, error) {
	query := `
		SELECT ` + monitorColumns + `
		FROM monitors m JOIN monitor_state s ON s.monitor_id = m.id`
	args := []any{}
	if after != nil {
		// Keyset, not OFFSET: (updated_at, id) strictly before the cursor. OFFSET
		// re-scans everything it skips, which is exactly the behaviour that makes
		// page 200 of 5,000 monitors slow.
		query += ` WHERE (m.updated_at, m.id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	query += ` ORDER BY m.updated_at DESC, m.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list monitors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MonitorWithState
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// ListAssignable returns every monitor a probe should be running: enabled, and
// of a type that executes on a probe at all. Push monitors are excluded here
// rather than filtered probe-side, because they are evaluated by the control
// plane and are never assigned to anyone (ADR-005 decision 6).
func (s *Store) ListAssignable(ctx context.Context) ([]model.Monitor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+monitorColumns+`
		FROM monitors m JOIN monitor_state s ON s.monitor_id = m.id
		WHERE m.enabled = 1 AND m.type != 'push'
		ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("list assignable: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m.Monitor)
	}
	return out, rows.Err()
}

// AllMonitors returns every monitor, enabled or not, configuration only.
//
// Exists for the credential re-sealing pass, which has to reach the disabled and
// the paused ones too: a monitor nobody is checking today still has its password
// sitting in the database.
func (s *Store) AllMonitors(ctx context.Context) ([]model.Monitor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+monitorColumns+`
		FROM monitors m JOIN monitor_state s ON s.monitor_id = m.id
		ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("list all monitors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m.Monitor)
	}
	return out, rows.Err()
}

// SetMonitorConfig replaces both halves of a monitor's configuration.
//
// updated_at is deliberately not touched. The config version the probe compares
// is derived from it, and re-sealing changes where the credentials are stored
// rather than what the monitor checks — bumping it would make every probe in the
// fleet reload every monitor for a change none of them can see.
func (s *Store) SetMonitorConfig(ctx context.Context, id model.ID, config []byte, sealed []byte) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE monitors SET config = ?, config_secrets = ? WHERE id = ?`,
		string(config), nullBytes(sealed), id[:])
	if err != nil {
		return fmt.Errorf("set monitor config: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPushMonitors returns every enabled push monitor with its state. It is the
// inverse of ListAssignable: these are the monitors no probe will ever run, so
// the control plane's own sweep is the only thing that will ever move them.
func (s *Store) ListPushMonitors(ctx context.Context) ([]MonitorWithState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+monitorColumns+`
		FROM monitors m JOIN monitor_state s ON s.monitor_id = m.id
		WHERE m.enabled = 1 AND m.type = 'push'
		ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("list push monitors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []MonitorWithState
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MonitorByPushToken resolves a push token hash to its monitor.
//
// Looked up by hash through the unique index, so the cost is one index probe
// whatever the install size — this endpoint is unauthenticated and anyone can
// call it with anything, which makes a linear scan a denial-of-service tool.
func (s *Store) MonitorByPushToken(ctx context.Context, hash []byte) (model.Monitor, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+monitorColumns+`
		FROM monitors m JOIN monitor_state s ON s.monitor_id = m.id
		WHERE m.push_token_hash = ? AND m.type = 'push'`, hash)

	out, err := scanMonitor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Monitor{}, ErrNotFound
	}
	return out.Monitor, err
}

// DeleteMonitor removes the monitor; state and heartbeats follow by cascade.
func (s *Store) DeleteMonitor(ctx context.Context, id model.ID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The org is read before the row goes, because the purge queue carries it
	// and a tenancy key recovered after the fact is a tenancy key guessed.
	var orgID []byte
	err = tx.QueryRowContext(ctx, `SELECT org_id FROM monitors WHERE id = ?`, id[:]).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("delete monitor: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM monitors WHERE id = ?`, id[:]); err != nil {
		return fmt.Errorf("delete monitor: %w", err)
	}

	// Configuration goes synchronously so the API's 204 is honest; history is
	// enqueued for the background purge, because a cascade over a week of
	// heartbeats and a year of buckets cannot run inside a request (§9.3).
	var org model.ID
	copy(org[:], orgID)
	if err := s.EnqueuePurge(ctx, tx, "monitor", id, org, time.Now().UTC()); err != nil {
		return fmt.Errorf("enqueue purge for monitor %s: %w", id, err)
	}

	return tx.Commit()
}

// GetState reads the current state, which ingest needs before it can decide
// whether a result is a transition.
func (s *Store) GetState(ctx context.Context, id model.ID) (model.MonitorState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT monitor_id, org_id, status, last_check_at, next_check_at,
		       last_status_change_at, consecutive_failures, last_response_time_ms,
		       last_message, suppressed_by, state_version
		FROM monitor_state WHERE monitor_id = ?`, id[:])

	var (
		st                               model.MonitorState
		monitorID, orgID                 []byte
		lastCheck, nextCheck, lastChange sql.NullInt64
		responseTime                     sql.NullFloat64
		message, suppressedBy            sql.NullString
	)
	if err := row.Scan(&monitorID, &orgID, &st.Status, &lastCheck, &nextCheck,
		&lastChange, &st.ConsecutiveFailures, &responseTime, &message,
		&suppressedBy, &st.StateVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.MonitorState{}, ErrNotFound
		}
		return model.MonitorState{}, err
	}
	copy(st.MonitorID[:], monitorID)
	copy(st.OrgID[:], orgID)
	st.LastCheckAt = nullableTime(lastCheck)
	st.NextCheckAt = nullableTime(nextCheck)
	st.LastStatusChangeAt = nullableTime(lastChange)
	st.LastResponseTimeMs = nullableFloat(responseTime)
	st.LastMessage = message.String
	st.SuppressedBy = suppressedBy.String
	return st, nil
}

// SaveState writes the state back and bumps state_version, which is ADR-004's
// membership signal: a browser polls the version to learn its filtered view
// moved, instead of being sent the view.
func (s *Store) SaveState(ctx context.Context, st model.MonitorState) error {
	// The two CASE expressions are what keep the maintenance sweep and the result
	// path from racing on one column. The sweep owns the value 'maintenance';
	// ingest owns 'dependency' and null. A sweep that lands between this caller
	// reading the state and writing it back therefore wins, which is the right
	// way round: a monitor the operator has declared under maintenance must not
	// be dragged back out by a check that was already in flight.
	_, err := s.db.ExecContext(ctx, `
		UPDATE monitor_state
		SET status = CASE WHEN suppressed_by = 'maintenance' THEN 'maintenance' ELSE ? END,
		    last_check_at = ?, next_check_at = ?, last_status_change_at = ?,
		    consecutive_failures = ?, last_response_time_ms = ?, last_message = ?,
		    suppressed_by = CASE WHEN suppressed_by = 'maintenance' THEN suppressed_by ELSE ? END,
		    state_version = state_version + 1
		WHERE monitor_id = ?`,
		st.Status, nullMillis(st.LastCheckAt), nullMillis(st.NextCheckAt),
		nullMillis(st.LastStatusChangeAt), st.ConsecutiveFailures,
		nullFloat(st.LastResponseTimeMs), nullString(st.LastMessage),
		nullString(st.SuppressedBy), st.MonitorID[:])
	if err != nil {
		return fmt.Errorf("save monitor_state: %w", err)
	}
	return nil
}

// scanner is what QueryRow and Rows have in common, so one scan function serves
// both the single-row and the list path.
type scanner interface {
	Scan(dest ...any) error
}

func scanMonitor(row scanner) (MonitorWithState, error) {
	var (
		out                                 MonitorWithState
		id, orgID, config, configSecrets    []byte
		groupID, parentID, pushTokenHash    []byte
		description, target, message        sql.NullString
		retryInterval                       sql.NullInt64
		lastCheck, nextCheck, lastChange    sql.NullInt64
		responseTime                        sql.NullFloat64
		intervalSeconds, timeoutSeconds     int64
		enabled, upsideDown, notifyRecovery int64
		createdAt, updatedAt                int64
	)

	if err := row.Scan(
		&id, &orgID, &out.Monitor.Name, &description, &out.Monitor.Type, &config, &configSecrets, &target,
		&pushTokenHash, &enabled, &intervalSeconds, &timeoutSeconds, &out.Monitor.Retries,
		&retryInterval, &out.Monitor.ResendAfter, &upsideDown, &notifyRecovery,
		&groupID, &parentID, &createdAt, &updatedAt,
		&out.State.Status, &lastCheck, &nextCheck, &lastChange,
		&out.State.ConsecutiveFailures, &responseTime, &message, &out.State.StateVersion,
	); err != nil {
		return MonitorWithState{}, err
	}

	copy(out.Monitor.ID[:], id)
	copy(out.Monitor.OrgID[:], orgID)
	out.Monitor.Description = description.String
	out.Monitor.Config = append([]byte(nil), config...)
	out.Monitor.ConfigSecrets = append([]byte(nil), configSecrets...)
	out.Monitor.Target = target.String
	out.Monitor.PushTokenHash = append([]byte(nil), pushTokenHash...)
	out.Monitor.Enabled = enabled == 1
	out.Monitor.Interval = time.Duration(intervalSeconds) * time.Second
	out.Monitor.Timeout = time.Duration(timeoutSeconds) * time.Second
	if retryInterval.Valid {
		out.Monitor.RetryInterval = time.Duration(retryInterval.Int64) * time.Second
	}
	out.Monitor.UpsideDown = upsideDown == 1
	out.Monitor.NotifyOnRecovery = notifyRecovery == 1
	out.Monitor.GroupID = idFromBytes(groupID)
	out.Monitor.ParentMonitorID = idFromBytes(parentID)
	out.Monitor.CreatedAt = fromMillis(createdAt)
	out.Monitor.UpdatedAt = fromMillis(updatedAt)

	out.State.MonitorID = out.Monitor.ID
	out.State.OrgID = out.Monitor.OrgID
	out.State.LastCheckAt = nullableTime(lastCheck)
	out.State.NextCheckAt = nullableTime(nextCheck)
	out.State.LastStatusChangeAt = nullableTime(lastChange)
	out.State.LastResponseTimeMs = nullableFloat(responseTime)
	out.State.LastMessage = message.String

	return out, nil
}

func idFromBytes(b []byte) *model.ID {
	if len(b) == 0 {
		return nil
	}
	var id model.ID
	copy(id[:], b)
	return &id
}

func nullID(id *model.ID) any {
	if id == nil {
		return nil
	}
	return id[:]
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullSeconds(d time.Duration) any {
	if d <= 0 {
		return nil
	}
	return int64(d.Seconds())
}

func nullMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return millis(*t)
}

func nullFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// LoadMonitor returns the configuration alone, which is what ingest needs: the
// retry threshold and the interval, without the state it is about to overwrite.
func (s *Store) LoadMonitor(ctx context.Context, id model.ID) (model.Monitor, error) {
	m, err := s.GetMonitor(ctx, id)
	if err != nil {
		return model.Monitor{}, err
	}
	return m.Monitor, nil
}
