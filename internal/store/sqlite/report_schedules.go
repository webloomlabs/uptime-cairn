package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Report schedules and their delivery targets.
//
// Targets are replaced wholesale on every write rather than diffed, the same
// choice status page sections make and for the same reason: they are an ordered
// list a human edits in one form, and a diff would have to reconcile position,
// membership and identity at once for a handful of rows. Replacing means the
// request is the state.
//
// The one thing that survives a replace is the **delivery log**: report_deliveries
// references a target with ON DELETE SET NULL, so "we sent it to them in March"
// outlives somebody removing the recipient in April.

const reportScheduleColumns = `
	id, org_id, report_template_id, name, enabled, frequency, cron, timezone,
	send_at, last_run_at, next_run_at, created_at, updated_at`

const scheduleDeliveryColumns = `
	id, org_id, report_schedule_id, type, config, secrets,
	notification_channel_id, formats, created_at, updated_at`

// CreateReportSchedule writes a schedule and its targets in one transaction.
func (s *Store) CreateReportSchedule(ctx context.Context, sched model.ReportSchedule, targets []model.ReportScheduleDelivery) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO report_schedules (`+reportScheduleColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, scheduleArgs(sched)...); err != nil {
		return fmt.Errorf("insert report schedule: %w", err)
	}
	if err := writeScheduleDeliveries(ctx, tx, sched.ID, sched.OrgID, targets); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateReportSchedule rewrites a schedule and replaces its targets.
func (s *Store) UpdateReportSchedule(ctx context.Context, sched model.ReportSchedule, targets []model.ReportScheduleDelivery) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE report_schedules
		SET report_template_id = ?, name = ?, enabled = ?, frequency = ?, cron = ?,
		    timezone = ?, send_at = ?, next_run_at = ?, updated_at = ?
		WHERE id = ? AND org_id = ? AND deleted_at IS NULL`,
		sched.ReportTemplateID[:], nullString(sched.Name), boolToInt(sched.Enabled),
		sched.Frequency, nullString(sched.Cron), sched.Timezone, sched.SendAt,
		nullMillis(sched.NextRunAt), millis(sched.UpdatedAt), sched.ID[:], sched.OrgID[:])
	if err != nil {
		return fmt.Errorf("update report schedule: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	if err := writeScheduleDeliveries(ctx, tx, sched.ID, sched.OrgID, targets); err != nil {
		return err
	}
	return tx.Commit()
}

// writeScheduleDeliveries replaces the target list.
//
// last_run_at is deliberately not touched by an update: it is a record of what
// happened, and editing a recipient does not un-send March's report.
func writeScheduleDeliveries(ctx context.Context, tx *sql.Tx, scheduleID, orgID model.ID, targets []model.ReportScheduleDelivery) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM report_schedule_deliveries WHERE report_schedule_id = ? AND org_id = ?`,
		scheduleID[:], orgID[:]); err != nil {
		return fmt.Errorf("clear schedule deliveries: %w", err)
	}

	for _, target := range targets {
		config := target.Config
		if len(config) == 0 {
			config = json.RawMessage("{}")
		}
		formats, err := json.Marshal(orEmpty(target.Formats))
		if err != nil {
			return fmt.Errorf("encode delivery formats: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO report_schedule_deliveries (`+scheduleDeliveryColumns+`)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			target.ID[:], orgID[:], scheduleID[:], target.Type, string(config),
			nullBytes(target.SecretsSealed), nullID(target.NotificationChannelID),
			string(formats), millis(target.CreatedAt), millis(target.UpdatedAt)); err != nil {
			return fmt.Errorf("insert schedule delivery: %w", err)
		}
	}
	return nil
}

// GetReportSchedule reads one live schedule.
func (s *Store) GetReportSchedule(ctx context.Context, id model.ID) (model.ReportSchedule, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+reportScheduleColumns+`
		 FROM report_schedules WHERE id = ? AND org_id = ? AND deleted_at IS NULL`,
		id[:], model.SentinelOrgID[:])
	return scanReportSchedule(row)
}

// ListReportSchedules pages the organisation's schedules, newest change first.
func (s *Store) ListReportSchedules(ctx context.Context, after *Cursor, limit int, templateID *model.ID) ([]model.ReportSchedule, bool, error) {
	query := `SELECT ` + reportScheduleColumns + `
		FROM report_schedules WHERE org_id = ? AND deleted_at IS NULL`
	args := []any{model.SentinelOrgID[:]}

	if templateID != nil {
		query += ` AND report_template_id = ?`
		args = append(args, templateID[:])
	}
	if after != nil {
		query += ` AND (updated_at, id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list report schedules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ReportSchedule
	for rows.Next() {
		sched, err := scanReportSchedule(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, sched)
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

// DeleteReportSchedule hides a schedule and stops it firing.
//
// Soft, like a template's, and for the same reason: the runs it produced name it
// and stay readable. Disabled and cleared of its next firing as well as hidden,
// so that a bug in a read path that forgets `deleted_at IS NULL` cannot resurrect
// it into the scheduler's due query.
func (s *Store) DeleteReportSchedule(ctx context.Context, id model.ID, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE report_schedules
		SET deleted_at = ?, enabled = 0, next_run_at = NULL, updated_at = ?
		WHERE id = ? AND org_id = ? AND deleted_at IS NULL`,
		millis(at), millis(at), id[:], model.SentinelOrgID[:])
	if err != nil {
		return fmt.Errorf("delete report schedule: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DueReportSchedules is the scheduler's only query.
//
// It rides the partial index migration 0008 created for exactly this — enabled,
// not deleted, with a firing time — so a tick costs a seek rather than a scan of
// every schedule an install has ever had.
func (s *Store) DueReportSchedules(ctx context.Context, now time.Time, limit int) ([]model.ReportSchedule, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT `+reportScheduleColumns+`
		FROM report_schedules
		WHERE enabled = 1 AND deleted_at IS NULL AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at LIMIT ?`, millis(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list due report schedules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ReportSchedule
	for rows.Next() {
		sched, err := scanReportSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sched)
	}
	return out, rows.Err()
}

// ClaimReportSchedule advances a schedule and queues the run it owes, in one
// transaction.
//
// **One transaction, and that is the whole point.** Done separately, a crash
// between them goes one of two bad ways: advance-then-create loses a report
// silently, and create-then-advance sends the client the same report twice on
// the next tick. Together they are atomic — either the schedule moved and the run
// exists, or neither happened and the next tick tries again.
//
// The update is conditional on next_run_at still holding the value the caller
// read, which is what makes the scheduler safe without a lock: two ticks that
// both see the same due schedule both try to claim it, one updates a row and one
// gets ErrConflict, and only the winner queues anything.
func (s *Store) ClaimReportSchedule(ctx context.Context, id model.ID, expected time.Time, lastRun time.Time, next *time.Time, run model.ReportRun) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE report_schedules
		SET last_run_at = ?, next_run_at = ?, updated_at = ?
		WHERE id = ? AND org_id = ? AND next_run_at = ?`,
		millis(lastRun), nullMillis(next), millis(lastRun),
		id[:], model.SentinelOrgID[:], millis(expected))
	if err != nil {
		return fmt.Errorf("claim report schedule: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrConflict
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO report_runs (`+reportRunColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID[:], run.OrgID[:], run.ReportTemplateID[:], nullID(run.ReportScheduleID),
		run.State, millis(run.PeriodStart), millis(run.PeriodEnd), run.Timezone,
		boolToInt(run.Late), nullString(run.Error), nullMillis(run.StartedAt),
		nullMillis(run.FinishedAt), millis(run.CreatedAt)); err != nil {
		return fmt.Errorf("queue scheduled run: %w", err)
	}
	return tx.Commit()
}

// DeliveriesForSchedule lists a schedule's configured targets.
func (s *Store) DeliveriesForSchedule(ctx context.Context, scheduleID model.ID) ([]model.ReportScheduleDelivery, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+scheduleDeliveryColumns+`
		 FROM report_schedule_deliveries
		 WHERE report_schedule_id = ? AND org_id = ? ORDER BY created_at, id`,
		scheduleID[:], model.SentinelOrgID[:])
	if err != nil {
		return nil, fmt.Errorf("list schedule deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ReportScheduleDelivery
	for rows.Next() {
		var (
			target               model.ReportScheduleDelivery
			id, orgID, schedID   []byte
			channelID            []byte
			config, formats      string
			secrets              []byte
			createdAt, updatedAt int64
		)
		if err := rows.Scan(&id, &orgID, &schedID, &target.Type, &config, &secrets,
			&channelID, &formats, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan schedule delivery: %w", err)
		}
		copy(target.ID[:], id)
		copy(target.OrgID[:], orgID)
		copy(target.ReportScheduleID[:], schedID)
		target.Config = json.RawMessage(config)
		target.SecretsSealed = secrets
		target.NotificationChannelID = idFromBytes(channelID)
		if err := json.Unmarshal([]byte(formats), &target.Formats); err != nil {
			return nil, fmt.Errorf("decode delivery formats: %w", err)
		}
		target.CreatedAt = fromMillis(createdAt)
		target.UpdatedAt = fromMillis(updatedAt)
		out = append(out, target)
	}
	return out, rows.Err()
}

func scheduleArgs(s model.ReportSchedule) []any {
	return []any{
		s.ID[:], s.OrgID[:], s.ReportTemplateID[:], nullString(s.Name),
		boolToInt(s.Enabled), s.Frequency, nullString(s.Cron), s.Timezone, s.SendAt,
		nullMillis(s.LastRunAt), nullMillis(s.NextRunAt),
		millis(s.CreatedAt), millis(s.UpdatedAt),
	}
}

func scanReportSchedule(row scanner) (model.ReportSchedule, error) {
	var (
		out                   model.ReportSchedule
		id, orgID, templateID []byte
		name, cronText        sql.NullString
		lastRun, nextRun      sql.NullInt64
		enabled               int64
		createdAt, updatedAt  int64
	)

	if err := row.Scan(&id, &orgID, &templateID, &name, &enabled, &out.Frequency,
		&cronText, &out.Timezone, &out.SendAt, &lastRun, &nextRun,
		&createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ReportSchedule{}, ErrNotFound
		}
		return model.ReportSchedule{}, fmt.Errorf("scan report schedule: %w", err)
	}

	copy(out.ID[:], id)
	copy(out.OrgID[:], orgID)
	copy(out.ReportTemplateID[:], templateID)
	out.Name = name.String
	out.Enabled = enabled == 1
	out.Cron = cronText.String
	out.LastRunAt = nullableTime(lastRun)
	out.NextRunAt = nullableTime(nextRun)
	out.CreatedAt = fromMillis(createdAt)
	out.UpdatedAt = fromMillis(updatedAt)
	return out, nil
}
