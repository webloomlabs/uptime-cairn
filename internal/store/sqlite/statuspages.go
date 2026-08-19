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

// Status pages, their sections, and their subscribers.
//
// Sections are replaced wholesale on every write rather than diffed. They are an
// ordered list a human drags around in a UI; a diff would have to reconcile
// position, membership, and identity at once, and the whole structure is a
// handful of rows. Replacing means the ordering in the request is the ordering
// in the table, with nothing left over from the previous shape.

const statusPageColumns = `
	id, org_id, slug, title, description, published, custom_domain, visibility,
	password_hash, theme, logo_url, favicon_url, primary_color, footer_text,
	custom_css, timezone, show_uptime_percentage, show_response_time_chart,
	uptime_bar_days, show_powered_by, subscriptions_enabled, google_analytics_id,
	created_at, updated_at`

// StatusPageFilter narrows a status page listing.
type StatusPageFilter = store.StatusPageFilter

// CreateStatusPage writes the page and its sections in one transaction.
//
// A duplicate slug or custom domain comes back as ErrConflict rather than as a
// driver error: the slug is in a URL somebody will bookmark, so "that one is
// taken" is an answer the caller acts on rather than an internal failure.
func (s *Store) CreateStatusPage(ctx context.Context, p model.StatusPage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if taken, err := slugOrDomainTaken(ctx, tx, p); err != nil {
		return err
	} else if taken {
		return ErrConflict
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO status_pages (`+statusPageColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, pageArgs(p)...); err != nil {
		return fmt.Errorf("insert status page: %w", err)
	}
	if err := writeSections(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateStatusPage rewrites the page and replaces its sections.
func (s *Store) UpdateStatusPage(ctx context.Context, p model.StatusPage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if taken, err := slugOrDomainTaken(ctx, tx, p); err != nil {
		return err
	} else if taken {
		return ErrConflict
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE status_pages
		SET slug = ?, title = ?, description = ?, published = ?, custom_domain = ?,
		    visibility = ?, password_hash = ?, theme = ?, logo_url = ?, favicon_url = ?,
		    primary_color = ?, footer_text = ?, custom_css = ?, timezone = ?,
		    show_uptime_percentage = ?, show_response_time_chart = ?, uptime_bar_days = ?,
		    show_powered_by = ?, subscriptions_enabled = ?, google_analytics_id = ?,
		    updated_at = ?
		WHERE id = ?`,
		p.Slug, p.Title, nullString(p.Description), boolToInt(p.Published),
		nullString(p.CustomDomain), p.Visibility, nullString(p.PasswordHash),
		nullString(p.Theme), nullString(p.LogoURL), nullString(p.FaviconURL),
		nullString(p.PrimaryColor), nullString(p.FooterText), nullString(p.CustomCSS),
		p.Timezone, boolToInt(p.ShowUptimePercentage), boolToInt(p.ShowResponseTimeChart),
		p.UptimeBarDays, boolToInt(p.ShowPoweredBy), boolToInt(p.SubscriptionsEnabled),
		nullString(p.GoogleAnalyticsID), millis(p.UpdatedAt), p.ID[:])
	if err != nil {
		return fmt.Errorf("update status page: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	if err := writeSections(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

// slugOrDomainTaken checks both uniqueness constraints inside the transaction
// that is about to depend on them, so the answer cannot go stale between the
// check and the write.
func slugOrDomainTaken(ctx context.Context, tx *sql.Tx, p model.StatusPage) (bool, error) {
	var found int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM status_pages WHERE org_id = ? AND slug = ? AND id != ? LIMIT 1`,
		p.OrgID[:], p.Slug, p.ID[:]).Scan(&found)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check status page slug: %w", err)
	}

	if p.CustomDomain == "" {
		return false, nil
	}
	// Across every organisation, not just this one: a request arrives with
	// nothing but a Host header to route on.
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM status_pages WHERE custom_domain = ? AND id != ? LIMIT 1`,
		p.CustomDomain, p.ID[:]).Scan(&found)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("check status page domain: %w", err)
	}
}

func pageArgs(p model.StatusPage) []any {
	return []any{
		p.ID[:], p.OrgID[:], p.Slug, p.Title, nullString(p.Description),
		boolToInt(p.Published), nullString(p.CustomDomain), p.Visibility,
		nullString(p.PasswordHash), nullString(p.Theme), nullString(p.LogoURL),
		nullString(p.FaviconURL), nullString(p.PrimaryColor), nullString(p.FooterText),
		nullString(p.CustomCSS), p.Timezone, boolToInt(p.ShowUptimePercentage),
		boolToInt(p.ShowResponseTimeChart), p.UptimeBarDays, boolToInt(p.ShowPoweredBy),
		boolToInt(p.SubscriptionsEnabled), nullString(p.GoogleAnalyticsID),
		millis(p.CreatedAt), millis(p.UpdatedAt),
	}
}

func writeSections(ctx context.Context, tx *sql.Tx, p model.StatusPage) error {
	// The monitor rows cascade from the section rows, so one delete clears both.
	if _, err := tx.ExecContext(ctx, `DELETE FROM status_page_sections WHERE status_page_id = ?`, p.ID[:]); err != nil {
		return fmt.Errorf("clear status page sections: %w", err)
	}

	for position, section := range p.Sections {
		sectionID := model.NewID()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO status_page_sections (id, status_page_id, org_id, name, description, position)
			VALUES (?,?,?,?,?,?)`,
			sectionID[:], p.ID[:], p.OrgID[:], section.Name, nullString(section.Description), position,
		); err != nil {
			return fmt.Errorf("insert status page section: %w", err)
		}
		for i, monitorID := range section.MonitorIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO status_page_section_monitors
				    (section_id, monitor_id, status_page_id, org_id, position)
				VALUES (?,?,?,?,?)`,
				sectionID[:], monitorID[:], p.ID[:], p.OrgID[:], i,
			); err != nil {
				return fmt.Errorf("insert status page section monitor: %w", err)
			}
		}
	}
	return nil
}

// GetStatusPage returns one page with its sections.
func (s *Store) GetStatusPage(ctx context.Context, id model.ID) (model.StatusPage, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+statusPageColumns+` FROM status_pages WHERE id = ?`, id[:])
	return s.pageFrom(ctx, row)
}

// StatusPageBySlug is the public read path's entry point.
func (s *Store) StatusPageBySlug(ctx context.Context, slug string) (model.StatusPage, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+statusPageColumns+` FROM status_pages WHERE org_id = ? AND slug = ?`,
		model.SentinelOrgID[:], slug)
	return s.pageFrom(ctx, row)
}

func (s *Store) pageFrom(ctx context.Context, row scanner) (model.StatusPage, error) {
	p, err := scanStatusPage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.StatusPage{}, ErrNotFound
	} else if err != nil {
		return model.StatusPage{}, err
	}
	sections, err := s.sectionsFor(ctx, p.ID)
	if err != nil {
		return model.StatusPage{}, err
	}
	p.Sections = sections
	return p, nil
}

func (s *Store) sectionsFor(ctx context.Context, pageID model.ID) ([]model.StatusPageSection, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, status_page_id, org_id, name, description, position
		FROM status_page_sections WHERE status_page_id = ? ORDER BY position, id`, pageID[:])
	if err != nil {
		return nil, fmt.Errorf("status page sections: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		sections []model.StatusPageSection
		index    = map[model.ID]int{}
	)
	for rows.Next() {
		var (
			section       model.StatusPageSection
			id, page, org []byte
			description   sql.NullString
		)
		if err := rows.Scan(&id, &page, &org, &section.Name, &description, &section.Position); err != nil {
			return nil, err
		}
		copy(section.ID[:], id)
		copy(section.StatusPageID[:], page)
		copy(section.OrgID[:], org)
		section.Description = description.String
		index[section.ID] = len(sections)
		sections = append(sections, section)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sections) == 0 {
		return nil, nil
	}

	monitorRows, err := s.db.QueryContext(ctx, `
		SELECT section_id, monitor_id FROM status_page_section_monitors
		WHERE status_page_id = ? ORDER BY position, monitor_id`, pageID[:])
	if err != nil {
		return nil, fmt.Errorf("status page section monitors: %w", err)
	}
	defer func() { _ = monitorRows.Close() }()

	for monitorRows.Next() {
		var sectionID, monitorID []byte
		if err := monitorRows.Scan(&sectionID, &monitorID); err != nil {
			return nil, err
		}
		var key, value model.ID
		copy(key[:], sectionID)
		copy(value[:], monitorID)
		if i, ok := index[key]; ok {
			sections[i].MonitorIDs = append(sections[i].MonitorIDs, value)
		}
	}
	return sections, monitorRows.Err()
}

// ListStatusPages returns one page of pages.
func (s *Store) ListStatusPages(ctx context.Context, after *Cursor, limit int, filter StatusPageFilter) ([]model.StatusPage, bool, error) {
	query := `SELECT ` + statusPageColumns + ` FROM status_pages WHERE org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if filter.Search != "" {
		query += ` AND (title LIKE ? ESCAPE '\' OR slug LIKE ? ESCAPE '\')`
		pattern := "%" + escapeLike(filter.Search) + "%"
		args = append(args, pattern, pattern)
	}
	if filter.Published != nil {
		query += ` AND published = ?`
		args = append(args, boolToInt(*filter.Published))
	}
	if after != nil {
		query += ` AND (updated_at, id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list status pages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.StatusPage
	for rows.Next() {
		p, err := scanStatusPage(rows)
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
	// Sections per page, on the list too: a page with no sections shows no
	// monitors, and an operator scanning the list for the one they forgot to
	// populate should not have to open each one.
	for i := range out {
		sections, err := s.sectionsFor(ctx, out[i].ID)
		if err != nil {
			return nil, false, err
		}
		out[i].Sections = sections
	}
	return out, hasMore, nil
}

// DeleteStatusPage removes the page; sections, subscribers, and the incident and
// maintenance joins cascade.
func (s *Store) DeleteStatusPage(ctx context.Context, id model.ID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM status_pages WHERE id = ?`, id[:])
	if err != nil {
		return fmt.Errorf("delete status page: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// MonitorsOnStatusPage returns the id, name, description, and current status of
// every monitor listed on a page, in one query.
//
// The public page renders exactly this and nothing else. It is a projection
// rather than a filter over the monitor read path, because a field cannot leak
// through a shape that has no place to put it — and this is the one endpoint
// where a leak reaches strangers.
func (s *Store) MonitorsOnStatusPage(ctx context.Context, pageID model.ID) (map[model.ID]store.PublicMonitor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.name, m.description, st.status, st.last_response_time_ms
		FROM status_page_section_monitors spm
		JOIN monitors m ON m.id = spm.monitor_id
		JOIN monitor_state st ON st.monitor_id = m.id
		WHERE spm.status_page_id = ?`, pageID[:])
	if err != nil {
		return nil, fmt.Errorf("status page monitors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[model.ID]store.PublicMonitor{}
	for rows.Next() {
		var (
			m            store.PublicMonitor
			id           []byte
			description  sql.NullString
			responseTime sql.NullFloat64
		)
		if err := rows.Scan(&id, &m.Name, &description, &m.Status, &responseTime); err != nil {
			return nil, err
		}
		copy(m.ID[:], id)
		m.Description = description.String
		m.ResponseTimeMs = nullableFloat(responseTime)
		out[m.ID] = m
	}
	return out, rows.Err()
}

// CreateSubscriber records a subscription request.
//
// A repeat request for an address already on the page is ErrConflict rather than
// a second row: the uniqueness index is over the hash, so this holds without a
// plaintext index over every subscriber address on the instance (§12.5).
func (s *Store) CreateSubscriber(ctx context.Context, sub model.Subscriber, sealed []byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Checked in the transaction rather than by matching a driver's error text,
	// the same way a tag slug is: an error message is not an interface, and the
	// single writer makes read-then-write exact.
	var taken int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscribers WHERE status_page_id = ? AND target_hash = ?`,
		sub.StatusPageID[:], sub.TargetHash).Scan(&taken); err != nil {
		return fmt.Errorf("check subscriber: %w", err)
	}
	if taken > 0 {
		return ErrConflict
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscribers (id, status_page_id, org_id, channel, target, target_hash,
		                         confirm_token_hash, confirmed_at, unsubscribe_token_hash, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		sub.ID[:], sub.StatusPageID[:], sub.OrgID[:], sub.Channel, sealed, sub.TargetHash,
		nullBytes(sub.ConfirmTokenHash), nullMillis(sub.ConfirmedAt),
		nullBytes(sub.UnsubscribeTokenHash), millis(sub.CreatedAt)); err != nil {
		return fmt.Errorf("insert subscriber: %w", err)
	}
	return tx.Commit()
}

// ListSubscribers returns a page's subscribers, newest first. The sealed target
// comes back as stored; opening it is the caller's, because only the caller
// holds a key.
func (s *Store) ListSubscribers(ctx context.Context, pageID model.ID, limit int) ([]model.Subscriber, [][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, status_page_id, org_id, channel, target, target_hash,
		       confirm_token_hash, confirmed_at, unsubscribe_token_hash, created_at
		FROM subscribers WHERE status_page_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		pageID[:], limit)
	if err != nil {
		return nil, nil, fmt.Errorf("list subscribers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		subscribers []model.Subscriber
		sealed      [][]byte
	)
	for rows.Next() {
		sub, envelope, err := scanSubscriber(rows)
		if err != nil {
			return nil, nil, err
		}
		subscribers = append(subscribers, sub)
		sealed = append(sealed, envelope)
	}
	return subscribers, sealed, rows.Err()
}

// SubscriberByToken resolves a confirmation or unsubscribe token to its row.
//
// Both tokens are looked up by hash through their own index, so a caller
// guessing tokens costs one index probe — this endpoint is unauthenticated, and
// the token in the path is the whole credential.
func (s *Store) SubscriberByToken(ctx context.Context, confirmHash, unsubscribeHash []byte) (model.Subscriber, []byte, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, status_page_id, org_id, channel, target, target_hash,
		       confirm_token_hash, confirmed_at, unsubscribe_token_hash, created_at
		FROM subscribers WHERE confirm_token_hash = ? OR unsubscribe_token_hash = ?`,
		confirmHash, unsubscribeHash)

	sub, sealed, err := scanSubscriber(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Subscriber{}, nil, ErrNotFound
	}
	return sub, sealed, err
}

// ConfirmSubscriber completes double opt-in and burns the confirmation token.
func (s *Store) ConfirmSubscriber(ctx context.Context, id model.ID, at time.Time) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE subscribers SET confirmed_at = ?, confirm_token_hash = NULL WHERE id = ?`,
		millis(at), id[:])
	if err != nil {
		return fmt.Errorf("confirm subscriber: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSubscriber removes a subscription, whether the operator did it or the
// subscriber followed an unsubscribe link.
func (s *Store) DeleteSubscriber(ctx context.Context, id model.ID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM subscribers WHERE id = ?`, id[:])
	if err != nil {
		return fmt.Errorf("delete subscriber: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

func scanSubscriber(row scanner) (model.Subscriber, []byte, error) {
	var (
		sub                          model.Subscriber
		id, pageID, orgID            []byte
		sealed, targetHash           []byte
		confirmHash, unsubscribeHash []byte
		confirmedAt                  sql.NullInt64
		created                      int64
	)
	if err := row.Scan(&id, &pageID, &orgID, &sub.Channel, &sealed, &targetHash,
		&confirmHash, &confirmedAt, &unsubscribeHash, &created); err != nil {
		return model.Subscriber{}, nil, err
	}

	copy(sub.ID[:], id)
	copy(sub.StatusPageID[:], pageID)
	copy(sub.OrgID[:], orgID)
	sub.TargetHash = append([]byte(nil), targetHash...)
	sub.ConfirmTokenHash = append([]byte(nil), confirmHash...)
	sub.UnsubscribeTokenHash = append([]byte(nil), unsubscribeHash...)
	sub.ConfirmedAt = nullableTime(confirmedAt)
	sub.CreatedAt = fromMillis(created)
	return sub, append([]byte(nil), sealed...), nil
}

func scanStatusPage(row scanner) (model.StatusPage, error) {
	var (
		p                                 model.StatusPage
		id, orgID                         []byte
		description, domain, passwordHash sql.NullString
		theme, logo, favicon, colour      sql.NullString
		footer, css, analytics            sql.NullString
		published, uptimePct, chart       int64
		poweredBy, subscriptions          int64
		createdAt, updatedAt              int64
	)
	if err := row.Scan(&id, &orgID, &p.Slug, &p.Title, &description, &published,
		&domain, &p.Visibility, &passwordHash, &theme, &logo, &favicon, &colour,
		&footer, &css, &p.Timezone, &uptimePct, &chart, &p.UptimeBarDays,
		&poweredBy, &subscriptions, &analytics, &createdAt, &updatedAt); err != nil {
		return model.StatusPage{}, err
	}

	copy(p.ID[:], id)
	copy(p.OrgID[:], orgID)
	p.Description = description.String
	p.Published = published == 1
	p.CustomDomain = domain.String
	p.PasswordHash = passwordHash.String
	p.Theme = theme.String
	p.LogoURL = logo.String
	p.FaviconURL = favicon.String
	p.PrimaryColor = colour.String
	p.FooterText = footer.String
	p.CustomCSS = css.String
	p.ShowUptimePercentage = uptimePct == 1
	p.ShowResponseTimeChart = chart == 1
	p.ShowPoweredBy = poweredBy == 1
	p.SubscriptionsEnabled = subscriptions == 1
	p.GoogleAnalyticsID = analytics.String
	p.CreatedAt = fromMillis(createdAt)
	p.UpdatedAt = fromMillis(updatedAt)
	return p, nil
}
