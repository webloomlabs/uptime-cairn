package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Brand profiles: the white-label identity a report is rendered under.
//
// The logo is a column here rather than a file beside the artifacts, and that is
// a deliberate departure from ADR-008 rather than an oversight. The ADR sends
// artifacts to the filesystem on three specifics — every `VACUUM INTO` backup
// growing in proportion to them, fifty concurrent writes contending with
// heartbeat ingest during the monthly burst, and no incremental blob access for
// a hundred-megabyte CSV. A logo shares none of the three: it is written once
// when somebody sets up a client and is bounded below a megabyte at the API. So
// it stays where the documented backup procedure already reaches it, instead of
// needing a second directory beside the one that release adds.

const brandProfileColumns = `
	id, org_id, name, company_name, primary_color, accent_color, footer_text,
	cover_text, hide_powered_by, logo_content_type, logo_bytes, logo_updated_at,
	is_default, created_at, updated_at`

// CreateBrandProfile writes a profile.
//
// Setting this one as the default clears whichever profile held that flag, in
// the same transaction. The alternative — letting the unique partial index
// refuse the write — would be correct and useless: "there is already a default"
// is not something an operator can act on, because making this one the default
// is precisely what they asked for.
func (s *Store) CreateBrandProfile(ctx context.Context, p model.BrandProfile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := clearDefaultBrand(ctx, tx, p); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO brand_profiles (`+brandProfileColumns+`, logo)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, append(brandArgs(p), nullBytes(p.Logo))...); err != nil {
		return fmt.Errorf("insert brand profile: %w", err)
	}
	return tx.Commit()
}

// UpdateBrandProfile rewrites a profile, leaving the logo alone.
//
// The logo is a separate write path (SetBrandLogo) because it arrives as a file
// upload rather than as a field in a JSON body, and folding it in here would
// mean every profile edit either carried a megabyte or silently cleared one.
func (s *Store) UpdateBrandProfile(ctx context.Context, p model.BrandProfile) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := clearDefaultBrand(ctx, tx, p); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE brand_profiles
		SET name = ?, company_name = ?, primary_color = ?, accent_color = ?,
		    footer_text = ?, cover_text = ?, hide_powered_by = ?, is_default = ?,
		    updated_at = ?
		WHERE id = ? AND org_id = ?`,
		p.Name, nullString(p.CompanyName), nullString(p.PrimaryColor), nullString(p.AccentColor),
		nullString(p.FooterText), nullString(p.CoverText), boolToInt(p.HidePoweredBy),
		boolToInt(p.IsDefault), millis(p.UpdatedAt), p.ID[:], p.OrgID[:])
	if err != nil {
		return fmt.Errorf("update brand profile: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// clearDefaultBrand demotes whichever other profile is currently the default.
// Scoped to the organisation and excluding this row, so re-saving the default
// profile does not demote itself.
func clearDefaultBrand(ctx context.Context, tx *sql.Tx, p model.BrandProfile) error {
	if !p.IsDefault {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE brand_profiles SET is_default = 0 WHERE org_id = ? AND id != ?`,
		p.OrgID[:], p.ID[:])
	if err != nil {
		return fmt.Errorf("clear default brand profile: %w", err)
	}
	return nil
}

// SetBrandLogo replaces the logo bytes.
//
// The content type is stored beside them rather than sniffed on the way out: the
// API has already decided PNG or JPEG and refused everything else with a reason,
// and re-deciding at render time would be a second implementation of a rule that
// exists to be stated once, at upload, to somebody holding an SVG.
func (s *Store) SetBrandLogo(ctx context.Context, id model.ID, logo []byte, contentType string, p model.BrandProfile) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE brand_profiles
		SET logo = ?, logo_content_type = ?, logo_bytes = ?, logo_updated_at = ?, updated_at = ?
		WHERE id = ? AND org_id = ?`,
		nullBytes(logo), nullString(contentType), nullInt64(int64(len(logo))),
		nullMillis(p.LogoUpdatedAt), millis(p.UpdatedAt), id[:], p.OrgID[:])
	if err != nil {
		return fmt.Errorf("set brand logo: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetBrandProfile returns a profile without its logo bytes.
//
// Without, on purpose. Every list and every template read would otherwise carry
// a megabyte per row for a field almost no caller wants; the one that does asks
// for it by name through BrandLogo.
func (s *Store) GetBrandProfile(ctx context.Context, id model.ID) (model.BrandProfile, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+brandProfileColumns+` FROM brand_profiles WHERE id = ? AND org_id = ?`,
		id[:], model.SentinelOrgID[:])
	return scanBrandProfile(row)
}

// DefaultBrandProfile returns the organisation's default, or ErrNotFound when
// none is set.
//
// ErrNotFound rather than a zero value, because "no profile" and "an unnamed
// profile with no colours" are different states and the caller's answer differs:
// the first falls back to settings.appearance, the second renders as saved.
func (s *Store) DefaultBrandProfile(ctx context.Context) (model.BrandProfile, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+brandProfileColumns+` FROM brand_profiles WHERE org_id = ? AND is_default = 1`,
		model.SentinelOrgID[:])
	return scanBrandProfile(row)
}

// BrandLogo returns the bytes and their content type. Separate from the profile
// read for the reason given on GetBrandProfile.
func (s *Store) BrandLogo(ctx context.Context, id model.ID) ([]byte, string, error) {
	var (
		logo        []byte
		contentType sql.NullString
	)
	err := s.ro.QueryRowContext(ctx,
		`SELECT logo, logo_content_type FROM brand_profiles WHERE id = ? AND org_id = ?`,
		id[:], model.SentinelOrgID[:]).Scan(&logo, &contentType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("read brand logo: %w", err)
	}
	return logo, contentType.String, nil
}

// ListBrandProfiles pages the organisation's profiles, newest change first.
func (s *Store) ListBrandProfiles(ctx context.Context, after *Cursor, limit int) ([]model.BrandProfile, bool, error) {
	query := `SELECT ` + brandProfileColumns + ` FROM brand_profiles WHERE org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if after != nil {
		query += ` AND (updated_at, id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list brand profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.BrandProfile
	for rows.Next() {
		p, err := scanBrandProfile(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, p)
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

// DeleteBrandProfile removes a profile, and **refuses while a live template
// still names it**.
//
// The foreign key would allow it — `ON DELETE SET NULL`, so the template would
// fall back to the default and still render. An earlier cut of this function
// took that route on the grounds that an undeletable-looking profile is worse
// than a silent fallback. The frozen spec says otherwise on the operation
// itself, and its reasoning is the better one: *a report that silently loses its
// client's branding on the first of the month is worse than a refused delete.*
// The fallback is invisible until an agency's client receives an unbranded
// document; the refusal happens while somebody is looking at the screen.
//
// Soft-deleted templates do not count. They render nothing, so a profile held
// hostage by a definition the operator already deleted would be undeletable for
// a reason they cannot see anywhere.
func (s *Store) DeleteBrandProfile(ctx context.Context, id model.ID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var referencing int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM report_templates
		WHERE brand_profile_id = ? AND org_id = ? AND deleted_at IS NULL`,
		id[:], model.SentinelOrgID[:]).Scan(&referencing); err != nil {
		return fmt.Errorf("check brand profile references: %w", err)
	}
	if referencing > 0 {
		return ErrConflict
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM brand_profiles WHERE id = ? AND org_id = ?`, id[:], model.SentinelOrgID[:])
	if err != nil {
		return fmt.Errorf("delete brand profile: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// TemplatesUsingBrandProfile counts the live templates naming a profile, so the
// refusal above can say how many rather than only that there were some.
func (s *Store) TemplatesUsingBrandProfile(ctx context.Context, id model.ID) (int, error) {
	var count int
	err := s.ro.QueryRowContext(ctx, `
		SELECT count(*) FROM report_templates
		WHERE brand_profile_id = ? AND org_id = ? AND deleted_at IS NULL`,
		id[:], model.SentinelOrgID[:]).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count templates using brand profile: %w", err)
	}
	return count, nil
}

func brandArgs(p model.BrandProfile) []any {
	return []any{
		p.ID[:], p.OrgID[:], p.Name, nullString(p.CompanyName),
		nullString(p.PrimaryColor), nullString(p.AccentColor),
		nullString(p.FooterText), nullString(p.CoverText), boolToInt(p.HidePoweredBy),
		nullString(p.LogoContentType), nullInt64(p.LogoBytes), nullMillis(p.LogoUpdatedAt),
		boolToInt(p.IsDefault), millis(p.CreatedAt), millis(p.UpdatedAt),
	}
}

func scanBrandProfile(row scanner) (model.BrandProfile, error) {
	var (
		p                                       model.BrandProfile
		id, orgID                               []byte
		company, primary, accent, footer, cover sql.NullString
		logoType                                sql.NullString
		logoBytes, logoUpdated                  sql.NullInt64
		hidePoweredBy, isDefault                int64
		createdAt, updatedAt                    int64
	)

	if err := row.Scan(&id, &orgID, &p.Name, &company, &primary, &accent, &footer,
		&cover, &hidePoweredBy, &logoType, &logoBytes, &logoUpdated,
		&isDefault, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.BrandProfile{}, ErrNotFound
		}
		return model.BrandProfile{}, fmt.Errorf("scan brand profile: %w", err)
	}

	copy(p.ID[:], id)
	copy(p.OrgID[:], orgID)
	p.CompanyName = company.String
	p.PrimaryColor = primary.String
	p.AccentColor = accent.String
	p.FooterText = footer.String
	p.CoverText = cover.String
	p.HidePoweredBy = hidePoweredBy == 1
	p.LogoContentType = logoType.String
	p.LogoBytes = logoBytes.Int64
	p.LogoUpdatedAt = nullableTime(logoUpdated)
	p.IsDefault = isDefault == 1
	p.CreatedAt = fromMillis(createdAt)
	p.UpdatedAt = fromMillis(updatedAt)
	return p, nil
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
