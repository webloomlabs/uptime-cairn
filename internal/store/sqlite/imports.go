package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Import jobs and their reports.
//
// The report is the artefact a migrating user reads to confirm nothing was
// lost, so the entries table is append-only and the job row carries no counts:
// the summary is computed from the entries every time it is rendered, which is
// what stops a tally and its own detail disagreeing.

func (s *Store) CreateImportJob(ctx context.Context, j model.ImportJob) error {
	sources, err := json.Marshal(j.Sources)
	if err != nil {
		return fmt.Errorf("encode import sources: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO import_jobs (id, org_id, source, state, dry_run, options, source_meta,
		                         error, started_at, finished_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID[:], j.OrgID[:], j.Source, j.State, boolToInt(j.DryRun),
		nullJSON(j.Options), string(sources), nullString(j.Error),
		nullMillis(j.StartedAt), nullMillis(j.FinishedAt),
		millis(j.CreatedAt), millis(j.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert import job: %w", err)
	}
	return nil
}

func (s *Store) UpdateImportJob(ctx context.Context, j model.ImportJob) error {
	sources, err := json.Marshal(j.Sources)
	if err != nil {
		return fmt.Errorf("encode import sources: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE import_jobs
		SET state = ?, source_meta = ?, error = ?, started_at = ?, finished_at = ?, updated_at = ?
		WHERE id = ?`,
		j.State, string(sources), nullString(j.Error),
		nullMillis(j.StartedAt), nullMillis(j.FinishedAt), millis(j.UpdatedAt), j.ID[:])
	if err != nil {
		return fmt.Errorf("update import job: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AddImportEntries appends a batch of per-entity outcomes.
//
// Batched, in one transaction, because a thousand-monitor import produces a
// thousand of these and a round trip each would make the report cost more than
// the import. Written as the import progresses rather than at the end, so a job
// that dies halfway still has a report explaining how far it got.
func (s *Store) AddImportEntries(ctx context.Context, entries []model.ImportEntry) error {
	if len(entries) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO import_entries (id, job_id, org_id, source_file, source_type, source_id,
		                            source_name, outcome, reason, entity_type, entity_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare import entry: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, e := range entries {
		var entityID any
		if e.TargetID != nil {
			entityID = e.TargetID[:]
		}
		if _, err := stmt.ExecContext(ctx, e.ID[:], e.JobID[:], e.OrgID[:], e.SourceFile,
			e.EntityType, nullString(e.SourceID), nullString(e.SourceName), e.Result,
			nullString(e.Detail), nullString(e.EntityType), entityID, millis(e.CreatedAt)); err != nil {
			return fmt.Errorf("insert import entry: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) GetImportJob(ctx context.Context, id model.ID) (model.ImportJob, []model.ImportEntry, error) {
	var (
		j                    model.ImportJob
		rowID, orgID         []byte
		options, sources     sql.NullString
		failure              sql.NullString
		dryRun               int64
		started, finished    sql.NullInt64
		createdAt, updatedAt int64
	)
	err := s.ro.QueryRowContext(ctx, `
		SELECT id, org_id, source, state, dry_run, options, source_meta, error,
		       started_at, finished_at, created_at, updated_at
		FROM import_jobs WHERE id = ? AND org_id = ?`, id[:], model.SentinelOrgID[:]).
		Scan(&rowID, &orgID, &j.Source, &j.State, &dryRun, &options, &sources, &failure,
			&started, &finished, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ImportJob{}, nil, ErrNotFound
	} else if err != nil {
		return model.ImportJob{}, nil, fmt.Errorf("read import job: %w", err)
	}

	copy(j.ID[:], rowID)
	copy(j.OrgID[:], orgID)
	j.DryRun = dryRun == 1
	j.Error = failure.String
	j.StartedAt = nullableTime(started)
	j.FinishedAt = nullableTime(finished)
	j.CreatedAt = fromMillis(createdAt)
	j.UpdatedAt = fromMillis(updatedAt)
	if options.Valid {
		j.Options = json.RawMessage(options.String)
	}
	if sources.Valid {
		_ = json.Unmarshal([]byte(sources.String), &j.Sources)
	}

	entries, err := s.importEntries(ctx, id)
	if err != nil {
		return model.ImportJob{}, nil, err
	}
	return j, entries, nil
}

func (s *Store) importEntries(ctx context.Context, jobID model.ID) ([]model.ImportEntry, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT id, job_id, org_id, source_file, source_type, source_id, source_name,
		       outcome, reason, entity_id, created_at
		FROM import_entries WHERE job_id = ? ORDER BY id`, jobID[:])
	if err != nil {
		return nil, fmt.Errorf("read import entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ImportEntry
	for rows.Next() {
		var (
			e                            model.ImportEntry
			id, job, org, entity         []byte
			sourceID, sourceName, reason sql.NullString
			createdAt                    int64
		)
		if err := rows.Scan(&id, &job, &org, &e.SourceFile, &e.EntityType, &sourceID,
			&sourceName, &e.Result, &reason, &entity, &createdAt); err != nil {
			return nil, err
		}
		copy(e.ID[:], id)
		copy(e.JobID[:], job)
		copy(e.OrgID[:], org)
		e.SourceID = sourceID.String
		e.SourceName = sourceName.String
		e.Detail = reason.String
		e.TargetID = idFromBytes(entity)
		e.CreatedAt = fromMillis(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// nullJSON stores a raw JSON document, or NULL when there is none. Distinct
// from storing "null" as text, which the column's json_valid CHECK would accept
// and every reader would then have to special-case.
func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
