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

// Outbound webhooks and their delivery log.
//
// The delivery log is append-only and high volume — one row per attempt, and a
// retry is another attempt — which is why it sits with the time-series tables
// for retention purposes even though it is not a hypertable. Nothing here
// updates a delivery row: a failed attempt followed by a successful retry is two
// facts, and collapsing them would lose the one an operator asks about.

const webhookColumns = `
	id, org_id, name, url, events, enabled, headers, secret_encrypted, secret_prefix,
	verify_tls, consecutive_failures, disabled_at, created_at, updated_at`

// CreateWebhook inserts a subscription.
func (s *Store) CreateWebhook(ctx context.Context, h model.Webhook, headers []byte) error {
	events, err := json.Marshal(h.Events)
	if err != nil {
		return fmt.Errorf("marshal webhook events: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO webhooks (`+webhookColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.ID[:], h.OrgID[:], h.Name, h.URL, string(events), boolToInt(h.Enabled),
		nullBytes(headers), nullBytes(h.SecretEncrypted), nullString(h.SecretPrefix),
		boolToInt(h.VerifyTLS), h.ConsecutiveFailures, nullMillis(h.DisabledAt),
		millis(h.CreatedAt), millis(h.UpdatedAt)); err != nil {
		return fmt.Errorf("insert webhook: %w", err)
	}
	return nil
}

// UpdateWebhook rewrites the mutable columns. The signing secret is not among
// them: it is minted once and shown once, and rotating it is a different
// operation from editing a URL.
func (s *Store) UpdateWebhook(ctx context.Context, h model.Webhook, headers []byte) error {
	events, err := json.Marshal(h.Events)
	if err != nil {
		return fmt.Errorf("marshal webhook events: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE webhooks
		SET name = ?, url = ?, events = ?, enabled = ?, headers = ?, verify_tls = ?,
		    consecutive_failures = ?, disabled_at = ?, updated_at = ?
		WHERE id = ?`,
		h.Name, h.URL, string(events), boolToInt(h.Enabled), nullBytes(headers),
		boolToInt(h.VerifyTLS), h.ConsecutiveFailures, nullMillis(h.DisabledAt),
		millis(h.UpdatedAt), h.ID[:])
	if err != nil {
		return fmt.Errorf("update webhook: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetWebhook returns one subscription with its sealed headers.
func (s *Store) GetWebhook(ctx context.Context, id model.ID) (model.Webhook, []byte, error) {
	row := s.ro.QueryRowContext(ctx, `SELECT `+webhookColumns+` FROM webhooks WHERE id = ?`, id[:])

	h, headers, err := scanWebhook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Webhook{}, nil, ErrNotFound
	} else if err != nil {
		return model.Webhook{}, nil, err
	}
	if err := s.loadLastDelivery(ctx, []*model.Webhook{&h}); err != nil {
		return model.Webhook{}, nil, err
	}
	return h, headers, nil
}

// ListWebhooks returns one page, newest-updated first.
func (s *Store) ListWebhooks(ctx context.Context, after *Cursor, limit int) ([]model.Webhook, bool, error) {
	query := `SELECT ` + webhookColumns + ` FROM webhooks WHERE org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if after != nil {
		query += ` AND (updated_at, id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list webhooks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Webhook
	for rows.Next() {
		h, _, err := scanWebhook(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	pointers := make([]*model.Webhook, len(out))
	for i := range out {
		pointers[i] = &out[i]
	}
	if err := s.loadLastDelivery(ctx, pointers); err != nil {
		return nil, false, err
	}
	return out, hasMore, nil
}

// loadLastDelivery attaches the most recent attempt per webhook.
//
// Worth a second query on every list because it is the whole answer to "is this
// still working?", and a webhook that quietly stopped delivering is the one
// failure mode this feature cannot be allowed to have.
func (s *Store) loadLastDelivery(ctx context.Context, hooks []*model.Webhook) error {
	if len(hooks) == 0 {
		return nil
	}

	index := make(map[model.ID]*model.Webhook, len(hooks))
	args := make([]any, 0, len(hooks)*2)
	for _, h := range hooks {
		index[h.ID] = h
		args = append(args, h.ID[:])
	}
	for _, h := range hooks {
		args = append(args, h.ID[:])
	}

	list := placeholders(len(hooks))
	rows, err := s.ro.QueryContext(ctx, `
		SELECT d.webhook_id, d.created_at, d.outcome
		FROM webhook_deliveries d
		JOIN (SELECT webhook_id, MAX(created_at) AS created_at FROM webhook_deliveries
		      WHERE webhook_id IN (`+list+`) GROUP BY webhook_id) latest
		  ON latest.webhook_id = d.webhook_id AND latest.created_at = d.created_at
		WHERE d.webhook_id IN (`+list+`)`, args...)
	if err != nil {
		return fmt.Errorf("last webhook delivery: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id      []byte
			at      int64
			outcome string
		)
		if err := rows.Scan(&id, &at, &outcome); err != nil {
			return err
		}
		var key model.ID
		copy(key[:], id)
		if h, ok := index[key]; ok && h.LastDeliveryAt == nil {
			when := fromMillis(at)
			h.LastDeliveryAt = &when
			h.LastDeliveryOutcome = outcome
		}
	}
	return rows.Err()
}

// DeleteWebhook removes the subscription; its deliveries cascade.
func (s *Store) DeleteWebhook(ctx context.Context, id model.ID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id[:])
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// EnabledWebhooks returns every subscription a dispatch should consider. The
// event filter is applied in memory by the dispatcher rather than in SQL,
// because the events column is a JSON array and the set is small enough that a
// scan of it costs less than the index would.
func (s *Store) EnabledWebhooks(ctx context.Context) ([]model.Webhook, [][]byte, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE org_id = ? AND enabled = 1 AND disabled_at IS NULL`,
		model.SentinelOrgID[:])
	if err != nil {
		return nil, nil, fmt.Errorf("enabled webhooks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out     []model.Webhook
		headers [][]byte
	)
	for rows.Next() {
		h, sealed, err := scanWebhook(rows)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, h)
		headers = append(headers, sealed)
	}
	return out, headers, rows.Err()
}

// RecordWebhookDelivery appends one attempt and moves the failure counter.
//
// The counter and the auto-disable live in the same statement as the log entry,
// so a subscription cannot be recorded as failing without the count that will
// eventually switch it off — and cannot be switched off without the entry
// explaining why.
func (s *Store) RecordWebhookDelivery(ctx context.Context, d model.WebhookDelivery, maxFailures int, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (id, webhook_id, org_id, event_id, event_type, outcome,
		                                attempt, request_body, response_status, response_body,
		                                error, duration_ms, next_retry_at, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID[:], d.WebhookID[:], d.OrgID[:], d.EventID[:], d.EventType, d.Outcome,
		d.Attempt, nullString(d.RequestBody), nullInt(d.ResponseStatus),
		nullString(d.ResponseBody), nullString(d.Error), nullFloat(d.DurationMs),
		nullMillis(d.NextRetryAt), millis(d.CreatedAt)); err != nil {
		return fmt.Errorf("insert webhook delivery: %w", err)
	}

	switch d.Outcome {
	case model.DeliverySucceeded:
		if _, err := tx.ExecContext(ctx,
			`UPDATE webhooks SET consecutive_failures = 0 WHERE id = ?`, d.WebhookID[:]); err != nil {
			return fmt.Errorf("reset webhook failures: %w", err)
		}
	case model.DeliveryFailed:
		if _, err := tx.ExecContext(ctx, `
			UPDATE webhooks
			SET consecutive_failures = consecutive_failures + 1,
			    disabled_at = CASE WHEN consecutive_failures + 1 >= ? THEN ? ELSE disabled_at END
			WHERE id = ?`, maxFailures, millis(at), d.WebhookID[:]); err != nil {
			return fmt.Errorf("count webhook failure: %w", err)
		}
	}
	return tx.Commit()
}

// ListWebhookDeliveries returns one page of attempts, newest first, paginated on
// created_at alone — deliveries are immutable and time-ordered, so time is the
// keyset, the same argument the heartbeat listing makes.
func (s *Store) ListWebhookDeliveries(ctx context.Context, webhookID model.ID, before *time.Time, limit int, outcome string) ([]model.WebhookDelivery, bool, error) {
	query := `
		SELECT id, webhook_id, org_id, event_id, event_type, outcome, attempt,
		       request_body, response_status, response_body, error, duration_ms,
		       next_retry_at, created_at
		FROM webhook_deliveries WHERE webhook_id = ?`
	args := []any{webhookID[:]}

	if before != nil {
		query += ` AND created_at < ?`
		args = append(args, millis(*before))
	}
	if outcome != "" {
		query += ` AND outcome = ?`
		args = append(args, outcome)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list webhook deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.WebhookDelivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, d)
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

// GetWebhookDelivery returns one attempt, which the redeliver endpoint needs so
// it can resend the exact body the receiver was originally sent.
func (s *Store) GetWebhookDelivery(ctx context.Context, id model.ID) (model.WebhookDelivery, error) {
	row := s.ro.QueryRowContext(ctx, `
		SELECT id, webhook_id, org_id, event_id, event_type, outcome, attempt,
		       request_body, response_status, response_body, error, duration_ms,
		       next_retry_at, created_at
		FROM webhook_deliveries WHERE id = ?`, id[:])

	d, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.WebhookDelivery{}, ErrNotFound
	}
	return d, err
}

func scanWebhook(row scanner) (model.Webhook, []byte, error) {
	var (
		h                    model.Webhook
		id, orgID            []byte
		headers, secret      []byte
		events               string
		prefix               sql.NullString
		enabled, verifyTLS   int64
		disabledAt           sql.NullInt64
		createdAt, updatedAt int64
	)
	if err := row.Scan(&id, &orgID, &h.Name, &h.URL, &events, &enabled, &headers,
		&secret, &prefix, &verifyTLS, &h.ConsecutiveFailures, &disabledAt,
		&createdAt, &updatedAt); err != nil {
		return model.Webhook{}, nil, err
	}

	copy(h.ID[:], id)
	copy(h.OrgID[:], orgID)
	if err := json.Unmarshal([]byte(events), &h.Events); err != nil {
		return model.Webhook{}, nil, fmt.Errorf("webhook events: %w", err)
	}
	h.Enabled = enabled == 1
	h.SecretEncrypted = append([]byte(nil), secret...)
	h.SecretPrefix = prefix.String
	h.VerifyTLS = verifyTLS == 1
	h.DisabledAt = nullableTime(disabledAt)
	h.CreatedAt = fromMillis(createdAt)
	h.UpdatedAt = fromMillis(updatedAt)
	return h, append([]byte(nil), headers...), nil
}

func scanDelivery(row scanner) (model.WebhookDelivery, error) {
	var (
		d                             model.WebhookDelivery
		id, webhookID, orgID, eventID []byte
		requestBody, responseBody     sql.NullString
		deliveryError                 sql.NullString
		responseStatus                sql.NullInt64
		durationMs                    sql.NullFloat64
		nextRetry                     sql.NullInt64
		createdAt                     int64
	)
	if err := row.Scan(&id, &webhookID, &orgID, &eventID, &d.EventType, &d.Outcome,
		&d.Attempt, &requestBody, &responseStatus, &responseBody, &deliveryError,
		&durationMs, &nextRetry, &createdAt); err != nil {
		return model.WebhookDelivery{}, err
	}

	copy(d.ID[:], id)
	copy(d.WebhookID[:], webhookID)
	copy(d.OrgID[:], orgID)
	copy(d.EventID[:], eventID)
	d.RequestBody = requestBody.String
	if responseStatus.Valid {
		status := int(responseStatus.Int64)
		d.ResponseStatus = &status
	}
	d.ResponseBody = responseBody.String
	d.Error = deliveryError.String
	d.DurationMs = nullableFloat(durationMs)
	d.NextRetryAt = nullableTime(nextRetry)
	d.CreatedAt = fromMillis(createdAt)
	return d, nil
}

func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}
