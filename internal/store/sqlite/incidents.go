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

// Incidents and their timelines.
//
// The join tables — incident_monitors, incident_status_pages — are rewritten
// wholesale inside the incident's transaction rather than diffed. An incident
// touches a handful of monitors and one or two pages, so the delete-and-reinsert
// costs nothing and removes the class of bug where a partial diff leaves a stale
// row pointing at a page the incident no longer appears on.

const incidentColumns = `
	id, org_id, title, state, impact, started_at, resolved_at, auto_opened,
	acknowledged_at, acknowledged_by, assigned_to, detected_at, created_at, updated_at`

// CreateIncident writes the incident and its associations in one transaction.
func (s *Store) CreateIncident(ctx context.Context, in model.Incident) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO incidents (`+incidentColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.ID[:], in.OrgID[:], in.Title, in.State, in.Impact, millis(in.StartedAt),
		nullMillis(in.ResolvedAt), boolToInt(in.AutoOpened), nullMillis(in.AcknowledgedAt),
		nullID(in.AcknowledgedBy), nullID(in.AssignedTo), nullMillis(in.DetectedAt),
		millis(in.CreatedAt), millis(in.UpdatedAt),
	); err != nil {
		return fmt.Errorf("insert incident: %w", err)
	}
	if err := writeIncidentLinks(ctx, tx, in); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateIncident rewrites the metadata and the associations. State is included
// because the timeline handler advances it through this same call — there is one
// write path, so a state change without a timeline entry is prevented by the
// handler above rather than by two diverging statements here.
func (s *Store) UpdateIncident(ctx context.Context, in model.Incident) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE incidents
		SET title = ?, state = ?, impact = ?, started_at = ?, resolved_at = ?,
		    acknowledged_at = ?, acknowledged_by = ?, assigned_to = ?, updated_at = ?
		WHERE id = ?`,
		in.Title, in.State, in.Impact, millis(in.StartedAt), nullMillis(in.ResolvedAt),
		nullMillis(in.AcknowledgedAt), nullID(in.AcknowledgedBy), nullID(in.AssignedTo),
		millis(in.UpdatedAt), in.ID[:])
	if err != nil {
		return fmt.Errorf("update incident: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	if err := writeIncidentLinks(ctx, tx, in); err != nil {
		return err
	}
	return tx.Commit()
}

func writeIncidentLinks(ctx context.Context, tx *sql.Tx, in model.Incident) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM incident_monitors WHERE incident_id = ?`, in.ID[:]); err != nil {
		return fmt.Errorf("clear incident monitors: %w", err)
	}
	for _, id := range in.MonitorIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO incident_monitors (incident_id, monitor_id, org_id) VALUES (?,?,?)`,
			in.ID[:], id[:], in.OrgID[:]); err != nil {
			return fmt.Errorf("link incident monitor: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM incident_status_pages WHERE incident_id = ?`, in.ID[:]); err != nil {
		return fmt.Errorf("clear incident pages: %w", err)
	}
	for _, id := range in.StatusPageIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO incident_status_pages (incident_id, status_page_id, org_id) VALUES (?,?,?)`,
			in.ID[:], id[:], in.OrgID[:]); err != nil {
			return fmt.Errorf("link incident page: %w", err)
		}
	}
	return nil
}

// GetIncident returns one incident with its associations and its timeline.
func (s *Store) GetIncident(ctx context.Context, id model.ID) (model.Incident, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = ?`, id[:])

	in, err := scanIncident(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Incident{}, ErrNotFound
	} else if err != nil {
		return model.Incident{}, err
	}
	if err := s.loadIncidentLinks(ctx, []*model.Incident{&in}); err != nil {
		return model.Incident{}, err
	}
	updates, err := s.ListIncidentUpdates(ctx, id)
	if err != nil {
		return model.Incident{}, err
	}
	in.Updates = updates
	return in, nil
}

// IncidentFilter narrows an incident listing.
type IncidentFilter = store.IncidentFilter

// ListIncidents returns one page, newest-updated first.
func (s *Store) ListIncidents(ctx context.Context, after *Cursor, limit int, filter IncidentFilter) ([]model.Incident, bool, error) {
	query := `SELECT ` + incidentColumns + ` FROM incidents WHERE org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if len(filter.States) > 0 {
		query += ` AND state IN (` + placeholders(len(filter.States)) + `)`
		for _, v := range filter.States {
			args = append(args, v)
		}
	}
	if len(filter.Impacts) > 0 {
		query += ` AND impact IN (` + placeholders(len(filter.Impacts)) + `)`
		for _, v := range filter.Impacts {
			args = append(args, v)
		}
	}
	if filter.MonitorID != nil {
		query += ` AND EXISTS (SELECT 1 FROM incident_monitors im
		            WHERE im.incident_id = incidents.id AND im.monitor_id = ?)`
		args = append(args, filter.MonitorID[:])
	}
	if filter.StatusPageID != nil {
		query += ` AND EXISTS (SELECT 1 FROM incident_status_pages ip
		            WHERE ip.incident_id = incidents.id AND ip.status_page_id = ?)`
		args = append(args, filter.StatusPageID[:])
	}
	if filter.From != nil {
		query += ` AND started_at >= ?`
		args = append(args, millis(*filter.From))
	}
	if filter.To != nil {
		// Half-open, like every other range in this system: the bucket
		// boundaries, the maintenance occurrences, and this.
		query += ` AND started_at < ?`
		args = append(args, millis(*filter.To))
	}
	if filter.Search != "" {
		query += ` AND title LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(filter.Search)+"%")
	}
	if after != nil {
		query += ` AND (updated_at, id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list incidents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	pointers := make([]*model.Incident, len(out))
	for i := range out {
		pointers[i] = &out[i]
	}
	if err := s.loadIncidentLinks(ctx, pointers); err != nil {
		return nil, false, err
	}
	return out, hasMore, nil
}

// loadIncidentLinks fills MonitorIDs and StatusPageIDs for a page of incidents
// in two queries rather than two per incident.
func (s *Store) loadIncidentLinks(ctx context.Context, incidents []*model.Incident) error {
	if len(incidents) == 0 {
		return nil
	}

	index := make(map[model.ID]*model.Incident, len(incidents))
	args := make([]any, 0, len(incidents))
	for _, in := range incidents {
		index[in.ID] = in
		args = append(args, in.ID[:])
	}
	list := placeholders(len(incidents))

	for _, q := range []struct {
		query  string
		assign func(*model.Incident, model.ID)
	}{
		{`SELECT incident_id, monitor_id FROM incident_monitors WHERE incident_id IN (` + list + `)`,
			func(in *model.Incident, id model.ID) { in.MonitorIDs = append(in.MonitorIDs, id) }},
		{`SELECT incident_id, status_page_id FROM incident_status_pages WHERE incident_id IN (` + list + `)`,
			func(in *model.Incident, id model.ID) { in.StatusPageIDs = append(in.StatusPageIDs, id) }},
	} {
		rows, err := s.db.QueryContext(ctx, q.query, args...)
		if err != nil {
			return fmt.Errorf("load incident links: %w", err)
		}
		for rows.Next() {
			var incidentID, targetID []byte
			if err := rows.Scan(&incidentID, &targetID); err != nil {
				_ = rows.Close()
				return err
			}
			var key, value model.ID
			copy(key[:], incidentID)
			copy(value[:], targetID)
			if in, ok := index[key]; ok {
				q.assign(in, value)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// DeleteIncident removes the incident; the timeline and the joins cascade.
func (s *Store) DeleteIncident(ctx context.Context, id model.ID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM incidents WHERE id = ?`, id[:])
	if err != nil {
		return fmt.Errorf("delete incident: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AddIncidentUpdate appends a timeline entry and, when it carries a state,
// advances the incident to it in the same transaction.
//
// One transaction because the two are one act: an incident whose state moved
// without the entry explaining it, or an entry claiming a state the incident is
// not in, are both worse than the write failing.
func (s *Store) AddIncidentUpdate(ctx context.Context, u model.IncidentUpdate, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM incidents WHERE id = ?`, u.IncidentID[:]).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("load incident: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO incident_updates (id, incident_id, org_id, state, body, author_id,
		                              notified_subscribers, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		u.ID[:], u.IncidentID[:], u.OrgID[:], nullString(u.State), u.Body,
		nullID(u.AuthorID), boolToInt(u.NotifiedSubscribers), millis(u.CreatedAt),
	); err != nil {
		return fmt.Errorf("insert incident update: %w", err)
	}

	if u.State != "" {
		// resolved_at is stamped here and cleared if the incident is reopened, so
		// the column always agrees with the state rather than recording the first
		// time somebody thought it was over.
		var resolvedAt any
		if u.State == model.IncidentResolved {
			resolvedAt = millis(at)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE incidents SET state = ?, resolved_at = ?, updated_at = ? WHERE id = ?`,
			u.State, resolvedAt, millis(at), u.IncidentID[:]); err != nil {
			return fmt.Errorf("advance incident state: %w", err)
		}
	}
	return tx.Commit()
}

// ListIncidentUpdates returns the timeline, oldest first — the order it is read
// in, and the order a status page renders.
func (s *Store) ListIncidentUpdates(ctx context.Context, incidentID model.ID) ([]model.IncidentUpdate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, incident_id, org_id, state, body, author_id, notified_subscribers, created_at
		FROM incident_updates WHERE incident_id = ? ORDER BY created_at, id`, incidentID[:])
	if err != nil {
		return nil, fmt.Errorf("list incident updates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.IncidentUpdate
	for rows.Next() {
		var (
			u                 model.IncidentUpdate
			id, incident, org []byte
			author            []byte
			state             sql.NullString
			notified          int64
			created           int64
		)
		if err := rows.Scan(&id, &incident, &org, &state, &u.Body, &author, &notified, &created); err != nil {
			return nil, err
		}
		copy(u.ID[:], id)
		copy(u.IncidentID[:], incident)
		copy(u.OrgID[:], org)
		u.State = state.String
		u.AuthorID = idFromBytes(author)
		u.NotifiedSubscribers = notified == 1
		u.CreatedAt = fromMillis(created)
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountOpenIncidents is the overview's figure: anything not yet resolved.
func (s *Store) CountOpenIncidents(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE org_id = ? AND resolved_at IS NULL`,
		model.SentinelOrgID[:]).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count open incidents: %w", err)
	}
	return count, nil
}

// IncidentsForStatusPage returns the incidents published on a page, split into
// those still open and those resolved inside the window.
//
// Two calls would have been simpler to read and would have doubled the query
// count on the one endpoint that is unauthenticated and public.
func (s *Store) IncidentsForStatusPage(ctx context.Context, pageID model.ID, since time.Time, limit int) ([]model.Incident, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+incidentColumns+`
		FROM incidents
		WHERE id IN (SELECT incident_id FROM incident_status_pages WHERE status_page_id = ?)
		  AND (resolved_at IS NULL OR resolved_at >= ?)
		ORDER BY started_at DESC LIMIT ?`, pageID[:], millis(since), limit)
	if err != nil {
		return nil, fmt.Errorf("status page incidents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pointers := make([]*model.Incident, len(out))
	for i := range out {
		pointers[i] = &out[i]
	}
	if err := s.loadIncidentLinks(ctx, pointers); err != nil {
		return nil, err
	}
	for i := range out {
		updates, err := s.ListIncidentUpdates(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Updates = updates
	}
	return out, nil
}

func scanIncident(row scanner) (model.Incident, error) {
	var (
		in                               model.Incident
		id, orgID                        []byte
		acknowledgedBy, assignedTo       []byte
		resolved, acknowledged, detected sql.NullInt64
		autoOpened                       int64
		started, created, updated        int64
	)
	if err := row.Scan(&id, &orgID, &in.Title, &in.State, &in.Impact, &started,
		&resolved, &autoOpened, &acknowledged, &acknowledgedBy, &assignedTo,
		&detected, &created, &updated); err != nil {
		return model.Incident{}, err
	}

	copy(in.ID[:], id)
	copy(in.OrgID[:], orgID)
	in.StartedAt = fromMillis(started)
	in.ResolvedAt = nullableTime(resolved)
	in.AutoOpened = autoOpened == 1
	in.AcknowledgedAt = nullableTime(acknowledged)
	in.AcknowledgedBy = idFromBytes(acknowledgedBy)
	in.AssignedTo = idFromBytes(assignedTo)
	in.DetectedAt = nullableTime(detected)
	in.CreatedAt = fromMillis(created)
	in.UpdatedAt = fromMillis(updated)
	return in, nil
}
