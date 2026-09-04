package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Instance settings: one row per organisation, eight JSON columns.
//
// A column per section rather than one blob, because the sections have different
// audiences and different sensitivities — `smtp` carries an encrypted credential
// and `appearance` carries a colour — and because a partial update writes only
// the sections it was given. One blob would make every PATCH a read-modify-write
// of everything, which is how two concurrent edits lose one of them.

// GetSettings reads the settings row, creating nothing.
//
// A missing row is not an error: an install that has never opened the settings
// page has no row, and every field has a documented default. Returning the zero
// value means the caller's defaults apply without a special case at the call
// site.
func (s *Store) GetSettings(ctx context.Context, orgID model.ID) (model.Settings, error) {
	row := s.ro.QueryRowContext(ctx, `
		SELECT general, appearance, retention, smtp, monitoring, security, telemetry,
		       report_storage, updated_at
		FROM settings WHERE org_id = ?`, orgID[:])

	var (
		general, appearance, retention string
		smtp, monitoring, security     string
		telemetry, reportStorage       string
		updatedAt                      int64
	)
	err := row.Scan(&general, &appearance, &retention, &smtp, &monitoring,
		&security, &telemetry, &reportStorage, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Settings{OrgID: orgID}, nil
	}
	if err != nil {
		return model.Settings{}, fmt.Errorf("get settings: %w", err)
	}

	out := model.Settings{OrgID: orgID, UpdatedAt: fromMillis(updatedAt)}
	for _, section := range []struct {
		raw  string
		into any
	}{
		{general, &out.General},
		{appearance, &out.Appearance},
		{retention, &out.Retention},
		{smtp, &out.SMTP},
		{monitoring, &out.Monitoring},
		{security, &out.Security},
		{telemetry, &out.Telemetry},
		{reportStorage, &out.ReportStorage},
	} {
		if err := json.Unmarshal([]byte(section.raw), section.into); err != nil {
			return model.Settings{}, fmt.Errorf("decode settings section: %w", err)
		}
	}
	return out, nil
}

// SaveSettings writes every section. The caller has already merged the request
// onto the stored value, so this is a full write rather than a partial one — the
// merge belongs where the validation is, not spread across two layers.
func (s *Store) SaveSettings(ctx context.Context, set model.Settings) error {
	sections := make([]any, 0, 8)
	for _, section := range []any{
		set.General, set.Appearance, set.Retention, set.SMTP,
		set.Monitoring, set.Security, set.Telemetry, set.ReportStorage,
	} {
		encoded, err := json.Marshal(section)
		if err != nil {
			return fmt.Errorf("encode settings section: %w", err)
		}
		sections = append(sections, string(encoded))
	}

	args := append([]any{set.OrgID[:]}, sections...)
	args = append(args, millis(set.UpdatedAt))

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (org_id, general, appearance, retention, smtp, monitoring,
		                      security, telemetry, report_storage, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (org_id) DO UPDATE SET
		    general = excluded.general, appearance = excluded.appearance,
		    retention = excluded.retention, smtp = excluded.smtp,
		    monitoring = excluded.monitoring, security = excluded.security,
		    telemetry = excluded.telemetry,
		    report_storage = excluded.report_storage,
		    updated_at = excluded.updated_at`,
		args...); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return nil
}

// ListUsers returns every account. Phase 1 installs have one; the endpoint
// exists so a client written against it does not have to be rewritten when
// Phase 3 adds the second.
func (s *Store) ListUsers(ctx context.Context, orgID model.ID) ([]model.User, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT id, org_id, email, name, role, active, password_hash, totp_secret,
		       totp_enabled_at, timezone, locale, last_login_at, created_at, updated_at
		FROM users WHERE org_id = ? ORDER BY created_at, id`, orgID[:])
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUserProfile writes the fields an account holder may change about
// themselves. Role and active are deliberately absent: those are administration,
// and administration of the only account on a Phase 1 install is how somebody
// locks themselves out.
func (s *Store) UpdateUserProfile(ctx context.Context, u model.User) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE users SET email = ?, name = ?, timezone = ?, locale = ?,
		                 password_hash = ?, updated_at = ?
		WHERE id = ?`,
		u.Email, nullString(u.Name), nullString(u.Timezone), nullString(u.Locale),
		u.PasswordHash, millis(u.UpdatedAt), u.ID[:])
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}
