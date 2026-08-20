package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Groups and tags, and the join table that carries the second.
//
// The counts are the interesting part. A list of groups is read far more often
// than it is written and every row wants two derived figures — how many monitors
// it holds and the worst thing any of them is doing — so both come back from the
// same query as the page rather than from a round trip per row.

const groupColumns = `g.id, g.org_id, g.name, g.description, g.parent_group_id, g.created_at, g.updated_at`
const tagColumns = `t.id, t.org_id, t.name, t.slug, t.color, t.description, t.created_at, t.updated_at`

// CreateGroup inserts a group.
func (s *Store) CreateGroup(ctx context.Context, g model.Group) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO groups (id, org_id, name, description, parent_group_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`,
		g.ID[:], g.OrgID[:], g.Name, nullString(g.Description), nullID(g.ParentGroupID),
		millis(g.CreatedAt), millis(g.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert group: %w", err)
	}
	return nil
}

// UpdateGroup replaces the mutable columns.
func (s *Store) UpdateGroup(ctx context.Context, g model.Group) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE groups SET name = ?, description = ?, parent_group_id = ?, updated_at = ?
		WHERE id = ?`,
		g.Name, nullString(g.Description), nullID(g.ParentGroupID), millis(g.UpdatedAt), g.ID[:])
	if err != nil {
		return fmt.Errorf("update group: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetGroup returns one group with its counts.
func (s *Store) GetGroup(ctx context.Context, id model.ID) (model.GroupSummary, error) {
	row := s.ro.QueryRowContext(ctx, `SELECT `+groupColumns+` FROM groups g WHERE g.id = ?`, id[:])

	g, err := scanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.GroupSummary{}, ErrNotFound
	}
	if err != nil {
		return model.GroupSummary{}, err
	}

	summaries, err := s.groupSummaries(ctx, []model.Group{g})
	if err != nil {
		return model.GroupSummary{}, err
	}
	return summaries[0], nil
}

// ListGroups returns one page with counts.
func (s *Store) ListGroups(ctx context.Context, after *Cursor, limit int, search string) ([]model.GroupSummary, bool, error) {
	query := `SELECT ` + groupColumns + ` FROM groups g WHERE g.org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if after != nil {
		query += ` AND (g.updated_at, g.id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	if search != "" {
		query += ` AND g.name LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(search)+"%")
	}
	query += ` ORDER BY g.updated_at DESC, g.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var groups []model.Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, false, err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(groups) > limit
	if hasMore {
		groups = groups[:limit]
	}
	summaries, err := s.groupSummaries(ctx, groups)
	return summaries, hasMore, err
}

// groupSummaries attaches the monitor count and the worst status to a page of
// groups in one query.
//
// A parent counts its children's monitors as its own. A parent group showing
// zero monitors and no status while the child underneath it is down would be a
// dashboard that goes green during an outage, which is the single worst thing a
// monitoring tool can do.
//
// The statuses come back as a short distinct list rather than as a precomputed
// worst, so the ordering lives in exactly one place — model.WorstStatus — rather
// than in a CASE expression here that has to be kept in step with it.
func (s *Store) groupSummaries(ctx context.Context, groups []model.Group) ([]model.GroupSummary, error) {
	out := make([]model.GroupSummary, 0, len(groups))
	for _, g := range groups {
		out = append(out, model.GroupSummary{Group: g})
	}
	if len(groups) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(groups))
	for _, g := range groups {
		args = append(args, g.ID[:])
	}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT g.id, COUNT(m.id), GROUP_CONCAT(DISTINCT st.status)
		FROM groups g
		LEFT JOIN monitors m
		  ON m.group_id = g.id
		  OR m.group_id IN (SELECT c.id FROM groups c WHERE c.parent_group_id = g.id)
		LEFT JOIN monitor_state st ON st.monitor_id = m.id
		WHERE g.id IN (`+placeholders(len(groups))+`)
		GROUP BY g.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("summarise groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := map[model.ID]int{}
	statuses := map[model.ID]string{}
	for rows.Next() {
		var raw []byte
		var count int
		var concatenated sql.NullString
		if err := rows.Scan(&raw, &count, &concatenated); err != nil {
			return nil, err
		}
		var id model.ID
		copy(id[:], raw)
		counts[id] = count
		statuses[id] = concatenated.String
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		id := out[i].Group.ID
		out[i].MonitorCount = counts[id]
		if list := statuses[id]; list != "" {
			out[i].Status = model.WorstStatus(strings.Split(list, ","))
		}
	}
	return out, nil
}

// DeleteGroup removes the group. Its monitors become ungrouped and its child
// groups become top-level, both through the schema's ON DELETE SET NULL —
// deleting a container must never delete what it contained.
func (s *Store) DeleteGroup(ctx context.Context, id model.ID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, id[:])
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateTag inserts a tag, refusing a slug that is already taken.
//
// Checked inside the transaction rather than by catching a constraint violation
// and matching on its message: a driver's error text is not an interface, and
// the single writer makes the read-then-write exact.
func (s *Store) CreateTag(ctx context.Context, t model.Tag) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var taken int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE org_id = ? AND slug = ?`, t.OrgID[:], t.Slug).Scan(&taken); err != nil {
		return fmt.Errorf("check tag slug: %w", err)
	}
	if taken > 0 {
		return store.ErrConflict
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tags (id, org_id, name, slug, color, description, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		t.ID[:], t.OrgID[:], t.Name, t.Slug, t.Color, nullString(t.Description),
		millis(t.CreatedAt), millis(t.UpdatedAt)); err != nil {
		return fmt.Errorf("insert tag: %w", err)
	}
	return tx.Commit()
}

// UpdateTag replaces the mutable columns, including the slug when the name
// changes.
func (s *Store) UpdateTag(ctx context.Context, t model.Tag) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var taken int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE org_id = ? AND slug = ? AND id != ?`,
		t.OrgID[:], t.Slug, t.ID[:]).Scan(&taken); err != nil {
		return fmt.Errorf("check tag slug: %w", err)
	}
	if taken > 0 {
		return store.ErrConflict
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE tags SET name = ?, slug = ?, color = ?, description = ?, updated_at = ?
		WHERE id = ?`,
		t.Name, t.Slug, t.Color, nullString(t.Description), millis(t.UpdatedAt), t.ID[:])
	if err != nil {
		return fmt.Errorf("update tag: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// GetTag returns one tag with its monitor count.
func (s *Store) GetTag(ctx context.Context, id model.ID) (model.TagSummary, error) {
	row := s.ro.QueryRowContext(ctx, `
		SELECT `+tagColumns+`, (SELECT COUNT(*) FROM monitor_tags mt WHERE mt.tag_id = t.id)
		FROM tags t WHERE t.id = ?`, id[:])

	out, err := scanTagSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.TagSummary{}, ErrNotFound
	}
	return out, err
}

// ListTags returns one page with counts.
func (s *Store) ListTags(ctx context.Context, after *Cursor, limit int, search string) ([]model.TagSummary, bool, error) {
	query := `
		SELECT ` + tagColumns + `, (SELECT COUNT(*) FROM monitor_tags mt WHERE mt.tag_id = t.id)
		FROM tags t WHERE t.org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if after != nil {
		query += ` AND (t.updated_at, t.id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	if search != "" {
		// Matched against the name and the slug: somebody searching "prod" for a
		// tag named "Production" should find it either way round.
		// Bound twice rather than referenced as ?1: this query is assembled from
		// optional clauses, so numbered and positional parameters would have to
		// agree about a count that changes with the filters.
		query += ` AND (t.name LIKE ? ESCAPE '\' OR t.slug LIKE ? ESCAPE '\')`
		pattern := "%" + escapeLike(search) + "%"
		args = append(args, pattern, pattern)
	}
	query += ` ORDER BY t.updated_at DESC, t.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.TagSummary
	for rows.Next() {
		t, err := scanTagSummary(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, t)
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

// DeleteTag removes the tag from every monitor carrying it, through the
// schema's cascade on monitor_tags. The monitors are untouched.
func (s *Store) DeleteTag(ctx context.Context, id model.ID) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, id[:])
	if err != nil {
		return fmt.Errorf("delete tag: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMonitorTags replaces a monitor's whole tag set, for the same reason a
// channel assignment is replaced: a request carrying two ids means those two.
func (s *Store) SetMonitorTags(ctx context.Context, monitorID, orgID model.ID, tagIDs []model.ID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM monitor_tags WHERE monitor_id = ?`, monitorID[:]); err != nil {
		return fmt.Errorf("clear monitor tags: %w", err)
	}
	for _, id := range tagIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO monitor_tags (monitor_id, tag_id, org_id) VALUES (?,?,?)`,
			monitorID[:], id[:], orgID[:]); err != nil {
			return fmt.Errorf("tag monitor with %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// TagIDsForMonitor is the assignment list a monitor read returns.
func (s *Store) TagIDsForMonitor(ctx context.Context, monitorID model.ID) ([]model.ID, error) {
	byMonitor, err := s.TagIDsForMonitors(ctx, []model.ID{monitorID})
	if err != nil {
		return nil, err
	}
	return byMonitor[monitorID], nil
}

// TagIDsForMonitors answers for a whole page in one query. A monitor list is the
// hottest read in the product; one round trip per row for a decorative field
// would cost more than everything else on it.
func (s *Store) TagIDsForMonitors(ctx context.Context, monitorIDs []model.ID) (map[model.ID][]model.ID, error) {
	out := make(map[model.ID][]model.ID, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(monitorIDs))
	for _, id := range monitorIDs {
		args = append(args, id[:])
	}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT monitor_id, tag_id FROM monitor_tags
		WHERE monitor_id IN (`+placeholders(len(monitorIDs))+`)
		ORDER BY monitor_id, tag_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("load monitor tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var monitorRaw, tagRaw []byte
		if err := rows.Scan(&monitorRaw, &tagRaw); err != nil {
			return nil, err
		}
		var monitorID, tagID model.ID
		copy(monitorID[:], monitorRaw)
		copy(tagID[:], tagRaw)
		out[monitorID] = append(out[monitorID], tagID)
	}
	return out, rows.Err()
}

func scanGroup(row scanner) (model.Group, error) {
	var (
		g                    model.Group
		id, orgID, parentID  []byte
		description          sql.NullString
		createdAt, updatedAt int64
	)
	if err := row.Scan(&id, &orgID, &g.Name, &description, &parentID, &createdAt, &updatedAt); err != nil {
		return model.Group{}, err
	}
	copy(g.ID[:], id)
	copy(g.OrgID[:], orgID)
	g.Description = description.String
	g.ParentGroupID = idFromBytes(parentID)
	g.CreatedAt = fromMillis(createdAt)
	g.UpdatedAt = fromMillis(updatedAt)
	return g, nil
}

func scanTagSummary(row scanner) (model.TagSummary, error) {
	var (
		out                  model.TagSummary
		id, orgID            []byte
		description          sql.NullString
		createdAt, updatedAt int64
	)
	if err := row.Scan(&id, &orgID, &out.Tag.Name, &out.Tag.Slug, &out.Tag.Color,
		&description, &createdAt, &updatedAt, &out.MonitorCount); err != nil {
		return model.TagSummary{}, err
	}
	copy(out.Tag.ID[:], id)
	copy(out.Tag.OrgID[:], orgID)
	out.Tag.Description = description.String
	out.Tag.CreatedAt = fromMillis(createdAt)
	out.Tag.UpdatedAt = fromMillis(updatedAt)
	return out, nil
}

// GroupHasChildren reports whether anything nests under this group.
//
// Phase 1 allows one level, and the two rules that enforce it are "a parent must
// itself have no parent" and "a group with children cannot be given one". This
// answers the second.
func (s *Store) GroupHasChildren(ctx context.Context, id model.ID) (bool, error) {
	var count int
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM groups WHERE parent_group_id = ?`, id[:]).Scan(&count); err != nil {
		return false, fmt.Errorf("check group children: %w", err)
	}
	return count > 0, nil
}
