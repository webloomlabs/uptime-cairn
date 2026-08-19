package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// WriteBatch writes results idempotently, in one transaction.
//
// Batch rather than row-at-a-time because this is the hottest write path in the
// system — 5,000 monitors on the 20-second floor is 250 writes a second — and
// because the probe protocol's result frame is shaped to hand straight to it.
//
// ON CONFLICT DO NOTHING is what makes at-least-once delivery safe: a batch that
// was written but not acknowledged before the connection dropped is resent, and
// the resend must be a no-op rather than a second row. The natural key
// (org_id, monitor_id, time, probe_id) is the idempotency key (data model §11.8).
// It returns how many rows were actually inserted, which is not the same as how
// many results it was given: a resent batch is deduplicated by the natural key
// and inserts nothing. The difference is worth reporting rather than hiding,
// because "the probe is redelivering" and "the system is writing twice as much"
// look identical from a counter that cannot tell them apart.
func (s *Store) WriteBatch(ctx context.Context, beats []model.Heartbeat) (int64, error) {
	if len(beats) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO heartbeats (
			time, monitor_id, org_id, probe_id, status, response_time_ms,
			code, message, attempt, important, suppressed, suppression_reason
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("prepare heartbeat insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	var written int64
	for _, b := range beats {
		var responseTime any
		if b.ResponseTime != nil {
			responseTime = float64(b.ResponseTime.Microseconds()) / 1000.0
		}
		result, err := stmt.ExecContext(ctx,
			micros(b.Time), b.MonitorID[:], b.OrgID[:], b.ProbeID[:], int64(b.Status),
			responseTime, nullString(b.Code), nullString(b.Message), b.Attempt,
			boolToInt(b.Important), boolToInt(b.Suppressed), nullReason(b.SuppressionReason),
		)
		if err != nil {
			return 0, fmt.Errorf("insert heartbeat: %w", err)
		}
		if affected, err := result.RowsAffected(); err == nil {
			written += affected
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// nullReason stores the suppression reason as an integer, or null when there was
// none. Integer rather than text because this table takes 21 million rows a day
// and the API's string form costs several gigabytes a year for no benefit
// (data model §5.2).
func nullReason(reason int) any {
	if reason == model.SuppressionNone {
		return nil
	}
	return int64(reason)
}

// ListHeartbeats returns one page of raw results, newest first.
//
// Paginated on time alone rather than (updated_at, id): heartbeats are immutable
// and time-ordered, so time is the keyset. The index it rides
// (org_id, monitor_id, time DESC, probe_id) is the one the data model chose for
// exactly this query.
func (s *Store) ListHeartbeats(ctx context.Context, monitorID model.ID, before *time.Time, limit int, importantOnly bool) ([]model.Heartbeat, bool, error) {
	query := `
		SELECT time, monitor_id, org_id, probe_id, status, response_time_ms,
		       code, message, attempt, important, suppressed, suppression_reason
		FROM heartbeats
		WHERE monitor_id = ?`
	args := []any{monitorID[:]}

	if before != nil {
		query += ` AND time < ?`
		args = append(args, micros(*before))
	}
	if importantOnly {
		query += ` AND important = 1`
	}
	query += ` ORDER BY time DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list heartbeats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Heartbeat
	for rows.Next() {
		b, err := scanHeartbeat(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, b)
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

// scanHeartbeat reads one row in the column order every heartbeat query above
// selects. Shared so that a column added to the table is a change in two places
// rather than in every query that reads it.
func scanHeartbeat(row scanner) (model.Heartbeat, error) {
	var (
		b                     model.Heartbeat
		timeMicros            int64
		monitor, org, probe   []byte
		status                int64
		responseTime          any
		code, message         any
		important, suppressed int64
		reason                sql.NullInt64
	)
	if err := row.Scan(&timeMicros, &monitor, &org, &probe, &status,
		&responseTime, &code, &message, &b.Attempt, &important, &suppressed,
		&reason); err != nil {
		return model.Heartbeat{}, err
	}

	b.Time = fromMicros(timeMicros)
	copy(b.MonitorID[:], monitor)
	copy(b.OrgID[:], org)
	copy(b.ProbeID[:], probe)
	b.Status = model.Status(status)
	if ms, ok := responseTime.(float64); ok {
		d := time.Duration(ms * float64(time.Millisecond))
		b.ResponseTime = &d
	}
	if s, ok := code.(string); ok {
		b.Code = s
	}
	if s, ok := message.(string); ok {
		b.Message = s
	}
	b.Important = important == 1
	b.Suppressed = suppressed == 1
	b.SuppressionReason = int(reason.Int64)
	return b, nil
}
