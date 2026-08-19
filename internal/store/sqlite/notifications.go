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
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Notification channels, their monitor assignments, and the delivery log.
//
// The delivery log is the reason this file is larger than a CRUD file needs to
// be. It is append-only, written on every attempt including the ones that
// failed, and it is what turns "the alert never arrived" from an argument into
// a query.

// ChannelWithCount and ChannelFilter are the shared store types, aliased so this
// package's signatures read naturally.
type (
	ChannelWithCount = store.ChannelWithCount
	ChannelFilter    = store.ChannelFilter
)

const channelColumns = `
	c.id, c.org_id, c.name, c.type, c.config, c.secrets, c.enabled, c.is_default,
	c.events, c.last_used_at, c.last_error, c.created_at, c.updated_at`

// CreateChannel inserts a channel and, when it is a default, attaches it to
// nothing retroactively. Defaults apply to monitors created afterwards, which is
// what "default" means everywhere else in the product — a channel marked default
// today must not silently start alerting on five thousand existing monitors.
func (s *Store) CreateChannel(ctx context.Context, c model.NotificationChannel) error {
	events, err := json.Marshal(c.Events)
	if err != nil {
		return fmt.Errorf("encode channel events: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_channels (
			id, org_id, name, type, config, secrets, enabled, is_default,
			events, last_used_at, last_error, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID[:], c.OrgID[:], c.Name, c.Type, string(c.Config), nullBytes(c.Secrets),
		boolToInt(c.Enabled), boolToInt(c.IsDefault), string(events),
		nullMillis(c.LastUsedAt), nullString(c.LastError),
		millis(c.CreatedAt), millis(c.UpdatedAt),
	); err != nil {
		return fmt.Errorf("insert notification channel: %w", err)
	}
	return nil
}

// UpdateChannel replaces the mutable columns. type is not among them: it selects
// how config is interpreted, and changing it would reinterpret stored bytes
// against a different schema.
func (s *Store) UpdateChannel(ctx context.Context, c model.NotificationChannel) error {
	events, err := json.Marshal(c.Events)
	if err != nil {
		return fmt.Errorf("encode channel events: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE notification_channels
		SET name = ?, config = ?, secrets = ?, enabled = ?, is_default = ?,
		    events = ?, updated_at = ?
		WHERE id = ?`,
		c.Name, string(c.Config), nullBytes(c.Secrets), boolToInt(c.Enabled),
		boolToInt(c.IsDefault), string(events), millis(c.UpdatedAt), c.ID[:])
	if err != nil {
		return fmt.Errorf("update notification channel: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetChannel returns one channel with its monitor count.
func (s *Store) GetChannel(ctx context.Context, id model.ID) (ChannelWithCount, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+channelColumns+`, (
			SELECT COUNT(*) FROM monitor_notification_channels mc WHERE mc.channel_id = c.id
		)
		FROM notification_channels c WHERE c.id = ?`, id[:])

	out, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ChannelWithCount{}, ErrNotFound
	}
	return out, err
}

// ListChannels returns one page, newest-updated first, with the same keyset
// pagination every other collection uses.
func (s *Store) ListChannels(ctx context.Context, after *Cursor, limit int, filter ChannelFilter) ([]ChannelWithCount, bool, error) {
	query := `
		SELECT ` + channelColumns + `, (
			SELECT COUNT(*) FROM monitor_notification_channels mc WHERE mc.channel_id = c.id
		)
		FROM notification_channels c
		WHERE c.org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if after != nil {
		query += ` AND (c.updated_at, c.id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	if filter.Search != "" {
		// LIKE with a leading wildcard cannot use an index, and deliberately so:
		// this table holds tens of rows, not millions, and an FTS index for it
		// would be machinery nobody can justify.
		query += ` AND c.name LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(filter.Search)+"%")
	}
	if len(filter.Types) > 0 {
		query += ` AND c.type IN (` + placeholders(len(filter.Types)) + `)`
		for _, t := range filter.Types {
			args = append(args, t)
		}
	}
	if filter.Enabled != nil {
		query += ` AND c.enabled = ?`
		args = append(args, boolToInt(*filter.Enabled))
	}

	query += ` ORDER BY c.updated_at DESC, c.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list notification channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ChannelWithCount
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, c)
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

// DeleteChannel removes a channel. The assignment rows go with it through the
// schema's ON DELETE CASCADE, and the delivery log keeps its rows with a null
// channel_id — the history of what was sent survives the destination, which is
// what an after-the-fact question about an incident needs.
func (s *Store) DeleteChannel(ctx context.Context, id model.ID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM notification_channels WHERE id = ?`, id[:])
	if err != nil {
		return fmt.Errorf("delete notification channel: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkChannelResult records the outcome of the most recent delivery attempt.
//
// last_error is cleared on success rather than left to age, so a channel that
// broke and recovered stops looking broken. Leaving it would train operators to
// ignore the field, which is the same as not having it.
func (s *Store) MarkChannelResult(ctx context.Context, id model.ID, at time.Time, deliveryError string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE notification_channels SET last_used_at = ?, last_error = ? WHERE id = ?`,
		millis(at), nullString(deliveryError), id[:])
	if err != nil {
		return fmt.Errorf("mark notification channel result: %w", err)
	}
	return nil
}

// ChannelsForMonitor returns the enabled channels attached to a monitor.
//
// Ordered by id so a fan-out is deterministic: two runs of the same event reach
// the same channels in the same order, which makes a delivery log comparable
// against itself.
func (s *Store) ChannelsForMonitor(ctx context.Context, monitorID model.ID) ([]model.NotificationChannel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+channelColumns+`, 0
		FROM notification_channels c
		JOIN monitor_notification_channels mc ON mc.channel_id = c.id
		WHERE mc.monitor_id = ? AND c.enabled = 1
		ORDER BY c.id`, monitorID[:])
	if err != nil {
		return nil, fmt.Errorf("load channels for monitor %s: %w", monitorID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.NotificationChannel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c.Channel)
	}
	return out, rows.Err()
}

// ChannelIDsForMonitor is the assignment list a monitor read returns.
func (s *Store) ChannelIDsForMonitor(ctx context.Context, monitorID model.ID) ([]model.ID, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT channel_id FROM monitor_notification_channels WHERE monitor_id = ? ORDER BY channel_id`,
		monitorID[:])
	if err != nil {
		return nil, fmt.Errorf("load channel assignments for %s: %w", monitorID, err)
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

// ChannelIDsForMonitors answers the same question for a page of monitors in one
// query. A monitor list is the hottest read in the product, and one round trip
// per row would make the assignment field cost more than everything else on it.
func (s *Store) ChannelIDsForMonitors(ctx context.Context, monitorIDs []model.ID) (map[model.ID][]model.ID, error) {
	out := make(map[model.ID][]model.ID, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(monitorIDs))
	for _, id := range monitorIDs {
		args = append(args, id[:])
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT monitor_id, channel_id FROM monitor_notification_channels
		WHERE monitor_id IN (`+placeholders(len(monitorIDs))+`)
		ORDER BY monitor_id, channel_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("load channel assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var monitorRaw, channelRaw []byte
		if err := rows.Scan(&monitorRaw, &channelRaw); err != nil {
			return nil, err
		}
		var monitorID, channelID model.ID
		copy(monitorID[:], monitorRaw)
		copy(channelID[:], channelRaw)
		out[monitorID] = append(out[monitorID], channelID)
	}
	return out, rows.Err()
}

// SetMonitorChannels replaces a monitor's assignment set.
//
// Replace rather than merge, in one transaction: a PATCH that sends two channel
// ids means "these two", and a partial application would leave a monitor
// alerting somewhere the user just removed.
func (s *Store) SetMonitorChannels(ctx context.Context, monitorID, orgID model.ID, channelIDs []model.ID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM monitor_notification_channels WHERE monitor_id = ?`, monitorID[:]); err != nil {
		return fmt.Errorf("clear channel assignments: %w", err)
	}
	for _, id := range channelIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO monitor_notification_channels (monitor_id, channel_id, org_id)
			VALUES (?,?,?)`, monitorID[:], id[:], orgID[:]); err != nil {
			return fmt.Errorf("assign channel %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// DefaultChannelIDs are the channels attached to a newly created monitor.
func (s *Store) DefaultChannelIDs(ctx context.Context, orgID model.ID) ([]model.ID, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM notification_channels WHERE org_id = ? AND is_default = 1 ORDER BY id`, orgID[:])
	if err != nil {
		return nil, fmt.Errorf("load default channels: %w", err)
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

// MissingChannels returns the ids that do not exist, so a monitor write can
// report every bad reference at once rather than failing on a foreign key with
// a message nobody can map back to a field.
func (s *Store) MissingChannels(ctx context.Context, orgID model.ID, ids []model.ID) ([]model.ID, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	args := []any{orgID[:]}
	for _, id := range ids {
		args = append(args, id[:])
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM notification_channels WHERE org_id = ? AND id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("check channel ids: %w", err)
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

// RecordDelivery appends one attempt to the log.
func (s *Store) RecordDelivery(ctx context.Context, d model.NotificationDelivery) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO notification_deliveries (
			id, org_id, monitor_id, channel_id, event_type, incident_id,
			outcome, error, duration_ms, attempt, rendered_payload, created_at
		) VALUES (?,?,?,?,?,NULL,?,?,?,?,?,?)`,
		d.ID[:], d.OrgID[:], nullID(d.MonitorID), nullID(d.ChannelID), d.EventType,
		d.Outcome, nullString(d.Error), nullFloat(d.DurationMs), d.Attempt,
		nullString(d.RenderedPayload), millis(d.CreatedAt))
	if err != nil {
		return fmt.Errorf("record notification delivery: %w", err)
	}
	return nil
}

func scanChannel(row scanner) (ChannelWithCount, error) {
	var (
		out                  ChannelWithCount
		id, orgID            []byte
		config               string
		secrets              []byte
		events               string
		enabled, isDefault   int64
		lastUsedAt           sql.NullInt64
		lastError            sql.NullString
		createdAt, updatedAt int64
	)

	if err := row.Scan(&id, &orgID, &out.Channel.Name, &out.Channel.Type, &config, &secrets,
		&enabled, &isDefault, &events, &lastUsedAt, &lastError,
		&createdAt, &updatedAt, &out.MonitorCount); err != nil {
		return ChannelWithCount{}, err
	}

	copy(out.Channel.ID[:], id)
	copy(out.Channel.OrgID[:], orgID)
	out.Channel.Config = json.RawMessage(config)
	out.Channel.Secrets = append([]byte(nil), secrets...)
	out.Channel.Enabled = enabled == 1
	out.Channel.IsDefault = isDefault == 1
	out.Channel.LastUsedAt = nullableTime(lastUsedAt)
	out.Channel.LastError = lastError.String
	out.Channel.CreatedAt = fromMillis(createdAt)
	out.Channel.UpdatedAt = fromMillis(updatedAt)

	if err := json.Unmarshal([]byte(events), &out.Channel.Events); err != nil {
		return ChannelWithCount{}, fmt.Errorf("decode channel events: %w", err)
	}
	return out, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// escapeLike neutralises the wildcards inside a user's search term, so searching
// for a name containing % matches that name rather than everything.
func escapeLike(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(s)
}
