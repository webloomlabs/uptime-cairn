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

// Report templates: the saved definition of what to report on.
//
// The scope is stored as a rule, not as a resolved list, and that is the single
// most consequential choice on this table. An agency that adds a monitor to a
// client's tag expects it in that client's next report without editing the
// report; a list of ids flattened at save time silently excludes it, and the
// omission is invisible until the client notices a service missing from a
// document they are paying for.

const reportTemplateColumns = `
	id, org_id, name, description, type, scope, period, period_style, sla_target,
	response_time_target_ms, maintenance_handling, comparison, brand_profile_id,
	sections, formats, created_at, updated_at`

// scopeJSON is the stored shape of a scope.
//
// Ids are written as canonical UUID strings rather than as raw bytes, because
// this column is read by a human with a SQLite shell at least as often as by
// this code — during a support conversation about why a monitor is or is not in
// somebody's report. Base64 of sixteen bytes would answer that question in a way
// nobody can check.
type scopeJSON struct {
	MonitorIDs []string `json:"monitor_ids,omitempty"`
	GroupIDs   []string `json:"group_ids,omitempty"`
	TagIDs     []string `json:"tag_ids,omitempty"`
	IncidentID string   `json:"incident_id,omitempty"`
}

type comparisonJSON struct {
	Mode       string   `json:"mode,omitempty"`
	MonitorIDs []string `json:"monitor_ids,omitempty"`
	GroupIDs   []string `json:"group_ids,omitempty"`
}

// CreateReportTemplate writes a definition.
func (s *Store) CreateReportTemplate(ctx context.Context, t model.ReportTemplate) error {
	args, err := templateArgs(t)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO report_templates (`+reportTemplateColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...); err != nil {
		return fmt.Errorf("insert report template: %w", err)
	}
	return nil
}

// UpdateReportTemplate rewrites a definition.
//
// Wholesale rather than field by field: a template is edited in one form and
// saved once, and a partial update path would have to decide what an absent
// scope means — "leave it" or "clear it" — which is a question the API layer
// answers by sending the whole object.
func (s *Store) UpdateReportTemplate(ctx context.Context, t model.ReportTemplate) error {
	scope, comparison, sections, formats, err := templateJSON(t)
	if err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE report_templates
		SET name = ?, description = ?, type = ?, scope = ?, period = ?, period_style = ?,
		    sla_target = ?, response_time_target_ms = ?, maintenance_handling = ?,
		    comparison = ?, brand_profile_id = ?, sections = ?, formats = ?, updated_at = ?
		WHERE id = ? AND org_id = ? AND deleted_at IS NULL`,
		t.Name, nullString(t.Description), t.Type, scope, t.Period, t.PeriodStyle,
		nullFloat(t.SLATarget), nullInt(t.ResponseTimeTargetMS), t.MaintenanceHandling,
		comparison, nullID(t.BrandProfileID), sections, formats,
		millis(t.UpdatedAt), t.ID[:], t.OrgID[:])
	if err != nil {
		return fmt.Errorf("update report template: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetReportTemplate reads one live definition. A deleted template is not found,
// which is what every caller acting on behalf of a request wants.
func (s *Store) GetReportTemplate(ctx context.Context, id model.ID) (model.ReportTemplate, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+reportTemplateColumns+`
		 FROM report_templates WHERE id = ? AND org_id = ? AND deleted_at IS NULL`,
		id[:], model.SentinelOrgID[:])
	return scanReportTemplate(row)
}

// ReportTemplateForRun reads a definition whether or not it has been deleted.
//
// This is the method soft delete exists for. A run is a record of what a client
// was sent, and the first question asked of one is "sent under what definition?"
// — which a read that hides deleted rows cannot answer, and which is precisely
// the question that gets asked after somebody tidies up the template. Every
// other read path uses GetReportTemplate; this one is for showing a run.
func (s *Store) ReportTemplateForRun(ctx context.Context, id model.ID) (model.ReportTemplate, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+reportTemplateColumns+` FROM report_templates WHERE id = ? AND org_id = ?`,
		id[:], model.SentinelOrgID[:])
	return scanReportTemplate(row)
}

// ListReportTemplates pages the organisation's definitions, newest change first.
func (s *Store) ListReportTemplates(ctx context.Context, after *Cursor, limit int) ([]model.ReportTemplate, bool, error) {
	query := `SELECT ` + reportTemplateColumns + `
		FROM report_templates WHERE org_id = ? AND deleted_at IS NULL`
	args := []any{model.SentinelOrgID[:]}

	if after != nil {
		query += ` AND (updated_at, id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list report templates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ReportTemplate
	for rows.Next() {
		t, err := scanReportTemplate(rows)
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

// DeleteReportTemplate hides a definition and stops its schedules, keeping every
// run it produced.
//
// A soft delete, on the maintainer's ruling of 2026-08-31 and recorded in the
// migration header. The earlier cascade deleted the record of what each client
// had been sent, which contradicted the frozen spec's own promise that
// "already-generated runs and their artefacts are retained".
//
// The schedules go with it, in the same transaction. A schedule whose template
// is hidden has nothing to render, and one left enabled would keep firing — a
// deleted report that keeps arriving in a client's inbox is worse than either
// deleting it or not.
//
// Deleting an already-deleted template is ErrNotFound rather than a silent
// success, because the caller is a DELETE handler that has to choose between 204
// and 404 and the answer is the same one it would give for an id that never
// existed.
func (s *Store) DeleteReportTemplate(ctx context.Context, id model.ID, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE report_templates SET deleted_at = ?, updated_at = ?
		WHERE id = ? AND org_id = ? AND deleted_at IS NULL`,
		millis(at), millis(at), id[:], model.SentinelOrgID[:])
	if err != nil {
		return fmt.Errorf("delete report template: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE report_schedules SET deleted_at = ?, enabled = 0, next_run_at = NULL, updated_at = ?
		WHERE report_template_id = ? AND org_id = ? AND deleted_at IS NULL`,
		millis(at), millis(at), id[:], model.SentinelOrgID[:]); err != nil {
		return fmt.Errorf("delete report template schedules: %w", err)
	}
	return tx.Commit()
}

func templateArgs(t model.ReportTemplate) ([]any, error) {
	scope, comparison, sections, formats, err := templateJSON(t)
	if err != nil {
		return nil, err
	}
	return []any{
		t.ID[:], t.OrgID[:], t.Name, nullString(t.Description), t.Type, scope,
		t.Period, t.PeriodStyle, nullFloat(t.SLATarget), nullInt(t.ResponseTimeTargetMS),
		t.MaintenanceHandling, comparison, nullID(t.BrandProfileID), sections, formats,
		millis(t.CreatedAt), millis(t.UpdatedAt),
	}, nil
}

func templateJSON(t model.ReportTemplate) (scope string, comparison any, sections, formats string, err error) {
	scopeBytes, err := json.Marshal(scopeJSON{
		MonitorIDs: idStrings(t.Scope.MonitorIDs),
		GroupIDs:   idStrings(t.Scope.GroupIDs),
		TagIDs:     idStrings(t.Scope.TagIDs),
		IncidentID: idString(t.Scope.IncidentID),
	})
	if err != nil {
		return "", nil, "", "", fmt.Errorf("encode report scope: %w", err)
	}

	if t.Comparison != nil {
		b, err := json.Marshal(comparisonJSON{
			Mode:       t.Comparison.Mode,
			MonitorIDs: idStrings(t.Comparison.MonitorIDs),
			GroupIDs:   idStrings(t.Comparison.GroupIDs),
		})
		if err != nil {
			return "", nil, "", "", fmt.Errorf("encode report comparison: %w", err)
		}
		comparison = string(b)
	}

	// Empty slices marshal as [] rather than null, which the CHECK requires and
	// which also makes "no sections chosen" and "sections not set" the same
	// stored value — they are the same thing, and two encodings of one state is
	// how a read path acquires a branch nobody tests.
	sectionsBytes, err := json.Marshal(orEmpty(t.Sections))
	if err != nil {
		return "", nil, "", "", fmt.Errorf("encode report sections: %w", err)
	}
	formatsBytes, err := json.Marshal(orEmpty(t.Formats))
	if err != nil {
		return "", nil, "", "", fmt.Errorf("encode report formats: %w", err)
	}
	return string(scopeBytes), comparison, string(sectionsBytes), string(formatsBytes), nil
}

func scanReportTemplate(row scanner) (model.ReportTemplate, error) {
	var (
		t                        model.ReportTemplate
		id, orgID, brandID       []byte
		description              sql.NullString
		comparison               sql.NullString
		slaTarget                sql.NullFloat64
		responseTarget           sql.NullInt64
		scope, sections, formats string
		createdAt, updatedAt     int64
	)

	if err := row.Scan(&id, &orgID, &t.Name, &description, &t.Type, &scope, &t.Period,
		&t.PeriodStyle, &slaTarget, &responseTarget, &t.MaintenanceHandling,
		&comparison, &brandID, &sections, &formats, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ReportTemplate{}, ErrNotFound
		}
		return model.ReportTemplate{}, fmt.Errorf("scan report template: %w", err)
	}

	copy(t.ID[:], id)
	copy(t.OrgID[:], orgID)
	t.Description = description.String
	t.SLATarget = nullableFloat(slaTarget)
	if responseTarget.Valid {
		v := int(responseTarget.Int64)
		t.ResponseTimeTargetMS = &v
	}
	t.BrandProfileID = idFromBytes(brandID)

	var sc scopeJSON
	if err := json.Unmarshal([]byte(scope), &sc); err != nil {
		return model.ReportTemplate{}, fmt.Errorf("decode report scope: %w", err)
	}
	t.Scope = model.ReportScope{
		MonitorIDs: parseIDs(sc.MonitorIDs),
		GroupIDs:   parseIDs(sc.GroupIDs),
		TagIDs:     parseIDs(sc.TagIDs),
	}
	if id, ok := model.ParseID(sc.IncidentID); ok {
		t.Scope.IncidentID = &id
	}

	if comparison.Valid && comparison.String != "" {
		var cj comparisonJSON
		if err := json.Unmarshal([]byte(comparison.String), &cj); err != nil {
			return model.ReportTemplate{}, fmt.Errorf("decode report comparison: %w", err)
		}
		t.Comparison = &model.ReportComparison{
			Mode:       cj.Mode,
			MonitorIDs: parseIDs(cj.MonitorIDs),
			GroupIDs:   parseIDs(cj.GroupIDs),
		}
	}

	if err := json.Unmarshal([]byte(sections), &t.Sections); err != nil {
		return model.ReportTemplate{}, fmt.Errorf("decode report sections: %w", err)
	}
	if err := json.Unmarshal([]byte(formats), &t.Formats); err != nil {
		return model.ReportTemplate{}, fmt.Errorf("decode report formats: %w", err)
	}

	t.CreatedAt = fromMillis(createdAt)
	t.UpdatedAt = fromMillis(updatedAt)
	return t, nil
}

// parseIDs drops anything unparseable rather than failing the read.
//
// A scope that has acquired a malformed id — by a hand edit, or by a future
// version writing a shape this one does not know — should still produce a report
// over the ids that are valid. The alternative is a template that cannot be
// opened or run, which is a worse answer to the same corruption.
func parseIDs(in []string) []model.ID {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.ID, 0, len(in))
	for _, s := range in {
		if id, ok := model.ParseID(s); ok {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func idStrings(ids []model.ID) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func idString(id *model.ID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
