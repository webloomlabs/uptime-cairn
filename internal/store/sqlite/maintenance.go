package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Maintenance windows, their targets, and the suppression flag they set.
//
// Targets are resolved by query at evaluation time and never snapshotted into a
// list of monitor ids. That is the whole reason tag targeting exists: a window
// covering "everything tagged production" has to keep covering monitors created
// after it, and a snapshot taken at creation cannot.

const windowColumns = `
	w.id, w.org_id, w.title, w.description, w.strategy, w.timezone,
	w.starts_at, w.ends_at, w.duration_minutes, w.recurrence,
	w.suppress_notifications, w.show_on_status_pages, w.cancelled_at,
	w.next_occurrence_at, w.created_at, w.updated_at`

// CreateMaintenanceWindow writes the window and its targets in one transaction.
// A window with no targets covers nothing, so the two are written together or
// not at all.
func (s *Store) CreateMaintenanceWindow(ctx context.Context, w model.MaintenanceWindow, statusPageIDs []model.ID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	recurrence, err := w.EncodeRecurrence()
	if err != nil {
		return fmt.Errorf("encode recurrence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO maintenance_windows (
			id, org_id, title, description, strategy, timezone, starts_at, ends_at,
			duration_minutes, recurrence, suppress_notifications, show_on_status_pages,
			cancelled_at, next_occurrence_at, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.ID[:], w.OrgID[:], w.Title, nullString(w.Description), w.Strategy, w.Timezone,
		millis(w.StartsAt), nullMillis(w.EndsAt), nullMinutes(w.Duration), recurrence,
		boolToInt(w.SuppressNotifications), boolToInt(w.ShowOnStatusPages),
		nullMillis(w.CancelledAt), nullMillis(w.NextOccurrenceAt),
		millis(w.CreatedAt), millis(w.UpdatedAt),
	); err != nil {
		return fmt.Errorf("insert maintenance window: %w", err)
	}

	if err := writeTargets(ctx, tx, w); err != nil {
		return err
	}
	if err := writeStatusPages(ctx, tx, w.ID, w.OrgID, statusPageIDs); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateMaintenanceWindow replaces the window and its whole target set.
//
// Replace rather than merge, for the same reason a monitor's channel assignment
// is replaced: a request carrying two monitor ids means those two, and a partial
// application would leave a window silently covering something the user just
// removed from it.
func (s *Store) UpdateMaintenanceWindow(ctx context.Context, w model.MaintenanceWindow, statusPageIDs []model.ID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	recurrence, err := w.EncodeRecurrence()
	if err != nil {
		return fmt.Errorf("encode recurrence: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE maintenance_windows
		SET title = ?, description = ?, strategy = ?, timezone = ?, starts_at = ?,
		    ends_at = ?, duration_minutes = ?, recurrence = ?, suppress_notifications = ?,
		    show_on_status_pages = ?, cancelled_at = ?, next_occurrence_at = ?, updated_at = ?
		WHERE id = ?`,
		w.Title, nullString(w.Description), w.Strategy, w.Timezone, millis(w.StartsAt),
		nullMillis(w.EndsAt), nullMinutes(w.Duration), recurrence,
		boolToInt(w.SuppressNotifications), boolToInt(w.ShowOnStatusPages),
		nullMillis(w.CancelledAt), nullMillis(w.NextOccurrenceAt), millis(w.UpdatedAt), w.ID[:])
	if err != nil {
		return fmt.Errorf("update maintenance window: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM maintenance_targets WHERE window_id = ?`, w.ID[:]); err != nil {
		return fmt.Errorf("clear maintenance targets: %w", err)
	}
	if err := writeTargets(ctx, tx, w); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM maintenance_status_pages WHERE window_id = ?`, w.ID[:]); err != nil {
		return fmt.Errorf("clear maintenance status pages: %w", err)
	}
	if err := writeStatusPages(ctx, tx, w.ID, w.OrgID, statusPageIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func writeStatusPages(ctx context.Context, tx *sql.Tx, windowID, orgID model.ID, ids []model.ID) error {
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO maintenance_status_pages (window_id, status_page_id, org_id)
			VALUES (?,?,?)`, windowID[:], id[:], orgID[:]); err != nil {
			return fmt.Errorf("attach maintenance window to status page: %w", err)
		}
	}
	return nil
}

// StatusPageIDsForWindows answers for a whole page of windows in one query, so a
// list does not cost one round trip per row.
func (s *Store) StatusPageIDsForWindows(ctx context.Context, windowIDs []model.ID) (map[model.ID][]model.ID, error) {
	out := make(map[model.ID][]model.ID, len(windowIDs))
	if len(windowIDs) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(windowIDs))
	for _, id := range windowIDs {
		args = append(args, id[:])
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT window_id, status_page_id FROM maintenance_status_pages
		WHERE window_id IN (`+placeholders(len(windowIDs))+`)
		ORDER BY window_id, status_page_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("load maintenance status pages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var windowRaw, pageRaw []byte
		if err := rows.Scan(&windowRaw, &pageRaw); err != nil {
			return nil, err
		}
		var windowID, pageID model.ID
		copy(windowID[:], windowRaw)
		copy(pageID[:], pageRaw)
		out[windowID] = append(out[windowID], pageID)
	}
	return out, rows.Err()
}

func writeTargets(ctx context.Context, tx *sql.Tx, w model.MaintenanceWindow) error {
	insert := func(targetType string, ids []model.ID) error {
		for _, id := range ids {
			rowID := model.NewID()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO maintenance_targets (id, window_id, org_id, target_type, target_id)
				VALUES (?,?,?,?,?)`,
				rowID[:], w.ID[:], w.OrgID[:], targetType, id[:]); err != nil {
				return fmt.Errorf("insert maintenance target: %w", err)
			}
		}
		return nil
	}
	if err := insert(model.TargetMonitor, w.Targets.MonitorIDs); err != nil {
		return err
	}
	if err := insert(model.TargetGroup, w.Targets.GroupIDs); err != nil {
		return err
	}
	return insert(model.TargetTag, w.Targets.TagIDs)
}

// GetMaintenanceWindow returns one window with its targets.
func (s *Store) GetMaintenanceWindow(ctx context.Context, id model.ID) (model.MaintenanceWindow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+windowColumns+` FROM maintenance_windows w WHERE w.id = ?`, id[:])

	w, err := scanWindow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.MaintenanceWindow{}, ErrNotFound
	}
	if err != nil {
		return model.MaintenanceWindow{}, err
	}
	if err := s.loadTargets(ctx, []*model.MaintenanceWindow{&w}); err != nil {
		return model.MaintenanceWindow{}, err
	}
	return w, nil
}

// ListMaintenanceWindows returns one page, newest-updated first.
//
// state is filtered by the caller rather than in SQL, because state is derived
// from the recurrence rule and the clock — deriving it in SQL would mean a cron
// evaluator in SQL.
func (s *Store) ListMaintenanceWindows(ctx context.Context, after *Cursor, limit int, search string, monitorID *model.ID) ([]model.MaintenanceWindow, bool, error) {
	query := `SELECT ` + windowColumns + ` FROM maintenance_windows w WHERE w.org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if after != nil {
		query += ` AND (w.updated_at, w.id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	if search != "" {
		query += ` AND w.title LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(search)+"%")
	}
	if monitorID != nil {
		// Whether directly or through the monitor's group or tags, which is what
		// "windows affecting this monitor" has to mean for tag targeting to be
		// worth anything.
		// The id is bound three times rather than referenced as ?1: this query is
		// assembled from optional clauses, so numbered and positional parameters
		// would have to agree about a count that changes with the filters.
		query += ` AND EXISTS (
			SELECT 1 FROM maintenance_targets t
			WHERE t.window_id = w.id AND (
				(t.target_type = 'monitor' AND t.target_id = ?)
				OR (t.target_type = 'group' AND t.target_id = (SELECT group_id FROM monitors WHERE id = ?))
				OR (t.target_type = 'tag' AND t.target_id IN (
					SELECT tag_id FROM monitor_tags WHERE monitor_id = ?))))`
		args = append(args, monitorID[:], monitorID[:], monitorID[:])
	}
	query += ` ORDER BY w.updated_at DESC, w.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list maintenance windows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.MaintenanceWindow
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	pointers := make([]*model.MaintenanceWindow, len(out))
	for i := range out {
		pointers[i] = &out[i]
	}
	if err := s.loadTargets(ctx, pointers); err != nil {
		return nil, false, err
	}
	return out, hasMore, nil
}

// DueMaintenanceWindows returns the windows whose schedule needs re-evaluating:
// everything that has never been evaluated, plus everything whose next
// occurrence has arrived.
//
// This is what next_occurrence_at is materialised for. Without it the sweep
// would evaluate every recurrence rule — including every cron expression — on
// every tick, forever, for windows scheduled months out.
//
// An active window always qualifies, because its occurrence started in the past;
// that is what lets the caller compute the whole active set from this subset.
func (s *Store) DueMaintenanceWindows(ctx context.Context, now time.Time) ([]model.MaintenanceWindow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+windowColumns+`
		FROM maintenance_windows w
		WHERE w.org_id = ? AND w.cancelled_at IS NULL
		  AND (w.next_occurrence_at IS NULL OR w.next_occurrence_at <= ?)
		ORDER BY w.id`, model.SentinelOrgID[:], millis(now))
	if err != nil {
		return nil, fmt.Errorf("list due maintenance windows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.MaintenanceWindow
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pointers := make([]*model.MaintenanceWindow, len(out))
	for i := range out {
		pointers[i] = &out[i]
	}
	if err := s.loadTargets(ctx, pointers); err != nil {
		return nil, err
	}
	return out, nil
}

// SetNextOccurrence materialises the evaluated schedule. updated_at is
// deliberately untouched: this is the system recording what it computed, not the
// user changing anything, and moving updated_at would reorder the list under a
// paginating reader.
func (s *Store) SetNextOccurrence(ctx context.Context, id model.ID, next *time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE maintenance_windows SET next_occurrence_at = ? WHERE id = ?`,
		nullMillis(next), id[:])
	if err != nil {
		return fmt.Errorf("set next occurrence: %w", err)
	}
	return nil
}

// DeleteMaintenanceWindow removes the schedule. Its targets cascade; heartbeats
// already annotated as under maintenance keep that annotation, so past uptime
// figures do not silently change because a window was deleted.
func (s *Store) DeleteMaintenanceWindow(ctx context.Context, id model.ID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM maintenance_windows WHERE id = ?`, id[:])
	if err != nil {
		return fmt.Errorf("delete maintenance window: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// MonitorsUnderWindows resolves the target sets of the given windows into the
// monitors they cover.
func (s *Store) MonitorsUnderWindows(ctx context.Context, windowIDs []model.ID) ([]model.ID, error) {
	if len(windowIDs) == 0 {
		return nil, nil
	}

	args := []any{model.SentinelOrgID[:]}
	for _, id := range windowIDs {
		args = append(args, id[:])
	}
	list := placeholders(len(windowIDs))

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id FROM monitors m
		WHERE m.org_id = ? AND m.enabled = 1 AND (
			m.id IN (SELECT target_id FROM maintenance_targets
			         WHERE target_type = 'monitor' AND window_id IN (`+list+`))
			OR (m.group_id IS NOT NULL AND m.group_id IN (
			         SELECT target_id FROM maintenance_targets
			         WHERE target_type = 'group' AND window_id IN (`+list+`)))
			OR EXISTS (SELECT 1 FROM monitor_tags mt
			           JOIN maintenance_targets t ON t.target_id = mt.tag_id
			           WHERE mt.monitor_id = m.id AND t.target_type = 'tag'
			             AND t.window_id IN (`+list+`)))
		ORDER BY m.id`,
		append(append(append([]any{}, args...), args[1:]...), args[1:]...)...)
	if err != nil {
		return nil, fmt.Errorf("resolve maintenance targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ID
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var id model.ID
		copy(id[:], raw)
		out = append(out, id)
	}
	return out, rows.Err()
}

// ApplyMaintenance moves monitors in and out of maintenance and reports how many
// of each it moved.
//
// It only ever writes the maintenance reason. A monitor suppressed because its
// dependency parent is down is left alone, because that flag belongs to ingest —
// two writers on one column, each owning one value, is what keeps the sweep from
// racing the result path.
func (s *Store) ApplyMaintenance(ctx context.Context, under []model.ID) (entered, exited int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if len(under) > 0 {
		args := make([]any, 0, len(under))
		for _, id := range under {
			args = append(args, id[:])
		}
		// A paused monitor is not being checked, so calling it "under
		// maintenance" would replace one true statement with a less true one.
		result, err := tx.ExecContext(ctx, `
			UPDATE monitor_state
			SET status = 'maintenance', suppressed_by = 'maintenance',
			    state_version = state_version + 1
			WHERE monitor_id IN (`+placeholders(len(under))+`)
			  AND status != 'paused'
			  AND (suppressed_by IS NULL OR suppressed_by != 'maintenance')`, args...)
		if err != nil {
			return 0, 0, fmt.Errorf("enter maintenance: %w", err)
		}
		entered, _ = result.RowsAffected()

		// Leaving maintenance restores pending, not the status the monitor had
		// going in: the last real observation predates the window, and
		// presenting it as current would be the dashboard lying about how fresh
		// it is. The next check settles it within one interval.
		result, err = tx.ExecContext(ctx, `
			UPDATE monitor_state
			SET status = 'pending', suppressed_by = NULL, state_version = state_version + 1
			WHERE suppressed_by = 'maintenance'
			  AND monitor_id NOT IN (`+placeholders(len(under))+`)`, args...)
		if err != nil {
			return 0, 0, fmt.Errorf("leave maintenance: %w", err)
		}
		exited, _ = result.RowsAffected()
	} else {
		result, err := tx.ExecContext(ctx, `
			UPDATE monitor_state
			SET status = 'pending', suppressed_by = NULL, state_version = state_version + 1
			WHERE suppressed_by = 'maintenance'`)
		if err != nil {
			return 0, 0, fmt.Errorf("leave maintenance: %w", err)
		}
		exited, _ = result.RowsAffected()
	}

	return entered, exited, tx.Commit()
}

// MissingIDs reports which of the given ids are absent from a table, so a write
// can report every bad reference at once rather than failing on a foreign key
// with a message nobody can map back to a field.
//
// table is not user input: the callers pass a constant from this package.
func (s *Store) MissingIDs(ctx context.Context, table string, orgID model.ID, ids []model.ID) ([]model.ID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if !allowedIDTables[table] {
		return nil, fmt.Errorf("no such table %q", table)
	}

	args := []any{orgID[:]}
	for _, id := range ids {
		args = append(args, id[:])
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM `+table+` WHERE org_id = ? AND id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("check %s ids: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	found := make(map[model.ID]bool, len(ids))
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var id model.ID
		copy(id[:], raw)
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var missing []model.ID
	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// allowedIDTables is the allow-list for the interpolated table name above.
var allowedIDTables = map[string]bool{
	"monitors": true, "groups": true, "tags": true, "status_pages": true,
}

func (s *Store) loadTargets(ctx context.Context, windows []*model.MaintenanceWindow) error {
	if len(windows) == 0 {
		return nil
	}

	byID := make(map[model.ID]*model.MaintenanceWindow, len(windows))
	args := make([]any, 0, len(windows))
	for _, w := range windows {
		byID[w.ID] = w
		args = append(args, w.ID[:])
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT window_id, target_type, target_id FROM maintenance_targets
		WHERE window_id IN (`+placeholders(len(windows))+`)
		ORDER BY window_id, target_type, target_id`, args...)
	if err != nil {
		return fmt.Errorf("load maintenance targets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var windowRaw, targetRaw []byte
		var targetType string
		if err := rows.Scan(&windowRaw, &targetType, &targetRaw); err != nil {
			return err
		}
		var windowID, targetID model.ID
		copy(windowID[:], windowRaw)
		copy(targetID[:], targetRaw)

		w, ok := byID[windowID]
		if !ok {
			continue
		}
		switch targetType {
		case model.TargetMonitor:
			w.Targets.MonitorIDs = append(w.Targets.MonitorIDs, targetID)
		case model.TargetGroup:
			w.Targets.GroupIDs = append(w.Targets.GroupIDs, targetID)
		case model.TargetTag:
			w.Targets.TagIDs = append(w.Targets.TagIDs, targetID)
		}
	}
	return rows.Err()
}

func scanWindow(row scanner) (model.MaintenanceWindow, error) {
	var (
		w                                          model.MaintenanceWindow
		id, orgID                                  []byte
		description                                sql.NullString
		endsAt, durationMinutes, cancelledAt, next sql.NullInt64
		recurrence                                 string
		startsAt, createdAt, updatedAt             int64
		suppress, showOnPages                      int64
	)

	if err := row.Scan(&id, &orgID, &w.Title, &description, &w.Strategy, &w.Timezone,
		&startsAt, &endsAt, &durationMinutes, &recurrence, &suppress, &showOnPages,
		&cancelledAt, &next, &createdAt, &updatedAt); err != nil {
		return model.MaintenanceWindow{}, err
	}

	copy(w.ID[:], id)
	copy(w.OrgID[:], orgID)
	w.Description = description.String
	w.StartsAt = fromMillis(startsAt)
	w.EndsAt = nullableTime(endsAt)
	if durationMinutes.Valid {
		w.Duration = time.Duration(durationMinutes.Int64) * time.Minute
	}
	w.SuppressNotifications = suppress == 1
	w.ShowOnStatusPages = showOnPages == 1
	w.CancelledAt = nullableTime(cancelledAt)
	w.NextOccurrenceAt = nullableTime(next)
	w.CreatedAt = fromMillis(createdAt)
	w.UpdatedAt = fromMillis(updatedAt)

	if strings.TrimSpace(recurrence) != "" {
		if err := json.Unmarshal([]byte(recurrence), &w.Recurrence); err != nil {
			return model.MaintenanceWindow{}, fmt.Errorf("decode recurrence: %w", err)
		}
	}
	return w, nil
}

func nullMinutes(d time.Duration) any {
	if d <= 0 {
		return nil
	}
	return int64(d.Minutes())
}

// WindowsForStatusPage returns the upcoming windows a page displays.
//
// Only windows that have not been cancelled and that opted in via
// show_on_status_pages: "we are taking the database down at 02:00" is sometimes
// exactly what a customer needs to see and sometimes an internal detail, and the
// operator decides which per window rather than per page.
func (s *Store) WindowsForStatusPage(ctx context.Context, pageID model.ID) ([]model.MaintenanceWindow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+windowColumns+`
		FROM maintenance_windows w
		WHERE w.id IN (SELECT window_id FROM maintenance_status_pages WHERE status_page_id = ?)
		  AND w.cancelled_at IS NULL AND w.show_on_status_pages = 1
		ORDER BY COALESCE(w.next_occurrence_at, w.starts_at)`, pageID[:])
	if err != nil {
		return nil, fmt.Errorf("status page windows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var windows []model.MaintenanceWindow
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			return nil, err
		}
		windows = append(windows, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pointers := make([]*model.MaintenanceWindow, len(windows))
	for i := range windows {
		pointers[i] = &windows[i]
	}
	if err := s.loadTargets(ctx, pointers); err != nil {
		return nil, err
	}
	return windows, nil
}
