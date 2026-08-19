package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
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
	MonitorFilter    = store.MonitorFilter
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
func (s *Store) ListMonitors(ctx context.Context, after *Cursor, limit int, filter MonitorFilter) ([]MonitorWithState, bool, error) {
	query := `
		SELECT ` + monitorColumns + `
		FROM monitors m JOIN monitor_state s ON s.monitor_id = m.id
		WHERE m.org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if len(filter.GroupIDs) > 0 {
		// A group filter reaches its children too, for the same reason a group's
		// monitor count does: filtering to a parent and getting nothing back
		// while the child underneath holds every monitor is a filter nobody
		// trusts twice.
		list := placeholders(len(filter.GroupIDs))
		query += ` AND (m.group_id IN (` + list + `)
		            OR m.group_id IN (SELECT c.id FROM groups c WHERE c.parent_group_id IN (` + list + `)))`
		for range 2 {
			for _, id := range filter.GroupIDs {
				args = append(args, id[:])
			}
		}
	}
	if len(filter.TagIDs) > 0 {
		// OR within the parameter, per the spec: tag_id=a&tag_id=b matches
		// monitors carrying either.
		query += ` AND EXISTS (SELECT 1 FROM monitor_tags mt
		            WHERE mt.monitor_id = m.id AND mt.tag_id IN (` + placeholders(len(filter.TagIDs)) + `))`
		for _, id := range filter.TagIDs {
			args = append(args, id[:])
		}
	}
	query, args = narrow(query, args, filter)

	if after != nil {
		// Keyset, not OFFSET: (updated_at, id) strictly before the cursor. OFFSET
		// re-scans everything it skips, which is exactly the behaviour that makes
		// page 200 of 5,000 monitors slow.
		query += ` AND (m.updated_at, m.id) < (?, ?)`
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

// narrow applies the three scalar filters and the search, shared by the listing
// and by the membership signal so the two can never disagree about what a filter
// means — which they would the first time one of them gained a clause.
func narrow(query string, args []any, filter MonitorFilter) (string, []any) {
	if len(filter.Statuses) > 0 {
		query += ` AND s.status IN (` + placeholders(len(filter.Statuses)) + `)`
		for _, v := range filter.Statuses {
			args = append(args, v)
		}
	}
	if len(filter.Types) > 0 {
		query += ` AND m.type IN (` + placeholders(len(filter.Types)) + `)`
		for _, v := range filter.Types {
			args = append(args, v)
		}
	}
	if filter.Enabled != nil {
		query += ` AND m.enabled = ?`
		args = append(args, boolToInt(*filter.Enabled))
	}
	if filter.Search != "" {
		// Name or target. LIKE with an escaped pattern rather than FTS: at 5,000
		// monitors this is a scan of a small table, and an FTS index is a second
		// thing to keep in step with every write for a query nobody runs in a
		// loop.
		query += ` AND (m.name LIKE ? ESCAPE '\' OR m.target LIKE ? ESCAPE '\')`
		pattern := "%" + escapeLike(filter.Search) + "%"
		args = append(args, pattern, pattern)
	}
	return query, args
}

// Membership answers "has this filtered view changed?" without returning the
// view. One index-only pass over monitors joined to their state.
//
// The version is the sum of state_version across the matching set, which moves
// whenever any member's state is written — and, because a monitor entering or
// leaving carries its own version with it, whenever membership changes too. It
// is deliberately opaque: a client tests it for inequality and nothing else, so
// summing is as good as any hash and costs no extra read.
//
// updated_at is folded in as well, so that a configuration edit which never
// touches monitor_state still moves the signal. Without it, renaming a monitor
// would leave every open list view showing the old name until something failed.
func (s *Store) Membership(ctx context.Context, filter MonitorFilter) (store.Membership, error) {
	query := `
		SELECT COUNT(*), COALESCE(SUM(s.state_version), 0), COALESCE(MAX(m.updated_at), 0)
		FROM monitors m JOIN monitor_state s ON s.monitor_id = m.id
		WHERE m.org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if len(filter.GroupIDs) > 0 {
		list := placeholders(len(filter.GroupIDs))
		query += ` AND (m.group_id IN (` + list + `)
		            OR m.group_id IN (SELECT c.id FROM groups c WHERE c.parent_group_id IN (` + list + `)))`
		for range 2 {
			for _, id := range filter.GroupIDs {
				args = append(args, id[:])
			}
		}
	}
	if len(filter.TagIDs) > 0 {
		query += ` AND EXISTS (SELECT 1 FROM monitor_tags mt
		            WHERE mt.monitor_id = m.id AND mt.tag_id IN (` + placeholders(len(filter.TagIDs)) + `))`
		for _, id := range filter.TagIDs {
			args = append(args, id[:])
		}
	}
	query, args = narrow(query, args, filter)

	var out store.Membership
	var versions, updated int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&out.Count, &versions, &updated); err != nil {
		return store.Membership{}, fmt.Errorf("monitor membership: %w", err)
	}
	out.Version = versions + updated
	return out, nil
}

// UpdateMonitor rewrites the mutable half of a monitor. type is not among the
// columns: changing what a monitor checks would make its history a record of two
// different things, so the spec makes it immutable and this statement is where
// that is true rather than merely stated.
func (s *Store) UpdateMonitor(ctx context.Context, m model.Monitor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE monitors
		SET name = ?, description = ?, config = ?, config_secrets = ?, target = ?,
		    enabled = ?, interval_seconds = ?, timeout_seconds = ?, retries = ?,
		    retry_interval_seconds = ?, resend_after = ?, upside_down = ?,
		    notify_on_recovery = ?, group_id = ?, parent_monitor_id = ?, updated_at = ?
		WHERE id = ?`,
		m.Name, nullString(m.Description), string(m.Config), nullBytes(m.ConfigSecrets),
		nullString(m.Target), boolToInt(m.Enabled), int64(m.Interval.Seconds()),
		int64(m.Timeout.Seconds()), m.Retries, nullSeconds(m.RetryInterval),
		m.ResendAfter, boolToInt(m.UpsideDown), boolToInt(m.NotifyOnRecovery),
		nullID(m.GroupID), nullID(m.ParentMonitorID), millis(m.UpdatedAt), m.ID[:])
	if err != nil {
		return fmt.Errorf("update monitor: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}

	// The membership signal moves on a configuration edit too, not only on a
	// check result. Without this a rename would leave every open list view
	// showing the old name until the monitor next failed — and two edits inside
	// one millisecond would be indistinguishable, which is precisely when a
	// change signal has to work.
	if _, err := tx.ExecContext(ctx,
		`UPDATE monitor_state SET state_version = state_version + 1 WHERE monitor_id = ?`, m.ID[:]); err != nil {
		return fmt.Errorf("bump state version: %w", err)
	}
	return tx.Commit()
}

// SetMonitorEnabled pauses or resumes, in one transaction with the state change
// that has to accompany it.
//
// Pausing writes status 'paused' and clears next_check_at, so the scheduler
// stops picking the monitor up. Resuming writes 'pending' rather than the status
// the monitor had before: it has not been checked since, and reporting a stale
// verdict as current is how a monitor that broke while paused stays green.
//
// A monitor under maintenance keeps that status, for the same reason SaveState
// defends it — the sweep owns the value, and a resume must not drag a monitor
// out of a window the operator declared.
func (s *Store) SetMonitorEnabled(ctx context.Context, id model.ID, enabled bool, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`UPDATE monitors SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), millis(at), id[:])
	if err != nil {
		return fmt.Errorf("set monitor enabled: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}

	status := model.MonitorStatusPending
	if !enabled {
		status = model.MonitorStatusPaused
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE monitor_state
		SET status = CASE WHEN suppressed_by = 'maintenance' THEN 'maintenance' ELSE ? END,
		    next_check_at = NULL,
		    consecutive_failures = 0,
		    last_status_change_at = ?,
		    state_version = state_version + 1
		WHERE monitor_id = ?`, status, millis(at), id[:]); err != nil {
		return fmt.Errorf("set monitor state: %w", err)
	}
	return tx.Commit()
}

// LastHeartbeats returns the most recent heartbeat per monitor, for the list
// view's include=last_heartbeat.
//
// One query with a correlated MAX(time) rather than a query per row: the whole
// point of the include parameter is that the dashboard's list view asks for it
// on every page, and a fan-out of 25 range scans is what the 5,000-monitor gate
// exists to catch.
func (s *Store) LastHeartbeats(ctx context.Context, ids []model.ID) (map[model.ID]model.Heartbeat, error) {
	if len(ids) == 0 {
		return map[model.ID]model.Heartbeat{}, nil
	}

	args := make([]any, 0, len(ids)*2)
	for _, id := range ids {
		args = append(args, id[:])
	}
	for _, id := range ids {
		args = append(args, id[:])
	}

	list := placeholders(len(ids))
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.time, h.monitor_id, h.org_id, h.probe_id, h.status, h.response_time_ms,
		       h.code, h.message, h.attempt, h.important, h.suppressed, h.suppression_reason
		FROM heartbeats h
		JOIN (SELECT monitor_id, MAX(time) AS time FROM heartbeats
		      WHERE monitor_id IN (`+list+`) GROUP BY monitor_id) latest
		  ON latest.monitor_id = h.monitor_id AND latest.time = h.time
		WHERE h.monitor_id IN (`+list+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("last heartbeats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[model.ID]model.Heartbeat, len(ids))
	for rows.Next() {
		beat, err := scanHeartbeat(rows)
		if err != nil {
			return nil, err
		}
		// Several probes may have reported at the same microsecond. Whichever
		// arrives first wins; they describe the same instant, so the choice
		// cannot be wrong in a way anybody can see.
		if _, seen := out[beat.MonitorID]; !seen {
			out[beat.MonitorID] = beat
		}
	}
	return out, rows.Err()
}

// UptimeRatios reads the precomputed uptime cache for a window.
//
// The cache is a performance structure and never a source of truth: a monitor
// missing from it has not been computed yet, which the caller renders as null
// rather than as zero. Zero would be a claim of total downtime, made by a table
// that simply had not run.
func (s *Store) UptimeRatios(ctx context.Context, ids []model.ID, window string) (map[model.ID]float64, error) {
	if len(ids) == 0 {
		return map[model.ID]float64{}, nil
	}

	args := []any{window}
	for _, id := range ids {
		args = append(args, id[:])
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT monitor_id, uptime_ratio FROM monitor_uptime_cache
		WHERE window = ? AND uptime_ratio IS NOT NULL AND monitor_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("uptime cache: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[model.ID]float64, len(ids))
	for rows.Next() {
		var (
			id    []byte
			ratio float64
		)
		if err := rows.Scan(&id, &ratio); err != nil {
			return nil, err
		}
		var key model.ID
		copy(key[:], id)
		out[key] = ratio
	}
	return out, rows.Err()
}

// StatusCounts is the overview's monitor tally, one grouped scan of the state
// index rather than six counting queries.
func (s *Store) StatusCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT status, COUNT(*) FROM monitor_state WHERE org_id = ? GROUP BY status`,
		model.SentinelOrgID[:])
	if err != nil {
		return nil, fmt.Errorf("status counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int, 5)
	for rows.Next() {
		var (
			status string
			count  int
		)
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

// GetCertificate returns the certificate last observed on a monitor.
func (s *Store) GetCertificate(ctx context.Context, id model.ID) (model.Certificate, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT monitor_id, org_id, subject, issuer, serial_number, valid_from, valid_to,
		       fingerprint_sha256, sans, chain_valid, chain_error, observed_at
		FROM monitor_certificates WHERE monitor_id = ?`, id[:])

	var (
		out                           model.Certificate
		monitorID, orgID, fingerprint []byte
		subject, issuer, serial, sans sql.NullString
		chainError                    sql.NullString
		validFrom, chainValid         sql.NullInt64
		validTo, observedAt           int64
	)
	if err := row.Scan(&monitorID, &orgID, &subject, &issuer, &serial, &validFrom,
		&validTo, &fingerprint, &sans, &chainValid, &chainError, &observedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Certificate{}, ErrNotFound
		}
		return model.Certificate{}, err
	}

	copy(out.MonitorID[:], monitorID)
	copy(out.OrgID[:], orgID)
	out.Subject = subject.String
	out.Issuer = issuer.String
	out.SerialNumber = serial.String
	out.ValidFrom = nullableTime(validFrom)
	out.ValidTo = fromMillis(validTo)
	out.FingerprintSHA256 = append([]byte(nil), fingerprint...)
	if sans.Valid && sans.String != "" {
		if err := json.Unmarshal([]byte(sans.String), &out.SANs); err != nil {
			return model.Certificate{}, fmt.Errorf("certificate sans: %w", err)
		}
	}
	if chainValid.Valid {
		valid := chainValid.Int64 == 1
		out.ChainValid = &valid
	}
	out.ChainError = chainError.String
	out.ObservedAt = fromMillis(observedAt)
	return out, nil
}

// ExpiringSoon counts certificates and domain registrations falling due inside
// the horizon, for the overview.
func (s *Store) ExpiringSoon(ctx context.Context, before time.Time) (certificates, domains int, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM monitor_certificates WHERE org_id = ?1 AND valid_to < ?2),
		       (SELECT COUNT(*) FROM monitor_domain_expiry WHERE org_id = ?1 AND expires_at < ?2)`,
		model.SentinelOrgID[:], millis(before))
	if err := row.Scan(&certificates, &domains); err != nil {
		return 0, 0, fmt.Errorf("expiring soon: %w", err)
	}
	return certificates, domains, nil
}
