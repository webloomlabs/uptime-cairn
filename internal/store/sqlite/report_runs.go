package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Runs, artifacts, and the delivery log.
//
// A run is a record rather than a job that happened to leave a row behind, and
// almost every decision in this file follows from that. It outlives the template
// and the schedule that produced it (soft delete, migration 0008), its artifact
// rows outlive the bytes they point at (the `expired` tombstone), and its
// delivery log records what was skipped as well as what was sent — because
// silence with no row behind it is indistinguishable from a system that is not
// running.

// ReportRunFilter narrows a run listing.
type ReportRunFilter = store.ReportRunFilter

const reportRunColumns = `
	id, org_id, report_template_id, report_schedule_id, state, period_start,
	period_end, timezone, late, error, started_at, finished_at, created_at`

const reportArtifactColumns = `
	id, org_id, report_run_id, format, state, path, size_bytes, sha256, error,
	expires_at, created_at`

// CreateReportRun writes a queued run.
func (s *Store) CreateReportRun(ctx context.Context, r model.ReportRun) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO report_runs (`+reportRunColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID[:], r.OrgID[:], r.ReportTemplateID[:], nullID(r.ReportScheduleID), r.State,
		millis(r.PeriodStart), millis(r.PeriodEnd), r.Timezone, boolToInt(r.Late),
		nullString(r.Error), nullMillis(r.StartedAt), nullMillis(r.FinishedAt),
		millis(r.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert report run: %w", err)
	}
	return nil
}

// StartReportRun moves a queued run to running.
//
// Conditional on the run still being queued, and that condition is the whole
// point: it is what makes a worker pool safe without a lock. Two workers that
// pick up the same run both issue this, one updates a row and one does not, and
// the loser sees ErrConflict rather than rendering a duplicate.
func (s *Store) StartReportRun(ctx context.Context, id model.ID, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE report_runs SET state = ?, started_at = ?
		WHERE id = ? AND org_id = ? AND state = ?`,
		model.RunRunning, millis(at), id[:], model.SentinelOrgID[:], model.RunQueued)
	if err != nil {
		return fmt.Errorf("start report run: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrConflict
	}
	return nil
}

// FinishReportRun records the terminal state.
//
// The state is the caller's to decide because only the caller knows what
// happened to each format: succeeded when everything rendered, partial when one
// did and another did not, failed when nothing did. Deriving it here from the
// artifact rows was considered and rejected — a run that failed before it wrote
// any artifact and a run whose only format failed would then be
// indistinguishable, and they are not the same thing to whoever is reading the
// error.
func (s *Store) FinishReportRun(ctx context.Context, id model.ID, state, failure string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE report_runs SET state = ?, error = ?, finished_at = ?
		WHERE id = ? AND org_id = ?`,
		state, nullString(failure), millis(at), id[:], model.SentinelOrgID[:])
	if err != nil {
		return fmt.Errorf("finish report run: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkReportRunLate records that the run started materially after it was due.
//
// A separate call rather than a field on create, because whether a run is late
// is known by the scheduler comparing the due instant against the clock, and the
// run is created by the thing that then executes it. A missed schedule is late,
// not lost, and the UI has to be able to say which.
func (s *Store) MarkReportRunLate(ctx context.Context, id model.ID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE report_runs SET late = 1 WHERE id = ? AND org_id = ?`, id[:], model.SentinelOrgID[:])
	if err != nil {
		return fmt.Errorf("mark report run late: %w", err)
	}
	return nil
}

// GetReportRun reads one run.
func (s *Store) GetReportRun(ctx context.Context, id model.ID) (model.ReportRun, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+reportRunColumns+` FROM report_runs WHERE id = ? AND org_id = ?`,
		id[:], model.SentinelOrgID[:])
	return scanReportRun(row)
}

// ListReportRuns pages run history, newest first.
//
// Ordered on created_at rather than the updated_at the other collections use,
// because a run has no updated_at column and because the question is "what ran,
// most recently" — a run whose state changed an hour after it was queued has not
// become newer history.
func (s *Store) ListReportRuns(ctx context.Context, after *Cursor, limit int, filter ReportRunFilter) ([]model.ReportRun, bool, error) {
	query := `SELECT ` + reportRunColumns + ` FROM report_runs WHERE org_id = ?`
	args := []any{model.SentinelOrgID[:]}

	if filter.ReportTemplateID != nil {
		query += ` AND report_template_id = ?`
		args = append(args, filter.ReportTemplateID[:])
	}
	if len(filter.States) > 0 {
		query += ` AND state IN (` + placeholders(len(filter.States)) + `)`
		for _, state := range filter.States {
			args = append(args, state)
		}
	}
	if after != nil {
		query += ` AND (created_at, id) < (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list report runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ReportRun
	for rows.Next() {
		r, err := scanReportRun(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, r)
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

// CreateReportArtifact records one rendered file.
//
// Called **after** the bytes are on disk and fsynced (ADR-008). That ordering is
// the whole crash-consistency story: a crash between the two leaves an inert
// orphan for the sweeper, whereas the reverse order leaves a row the UI offers
// and the disk cannot supply — a download that 500s on an artifact the client
// was told exists.
func (s *Store) CreateReportArtifact(ctx context.Context, a model.ReportArtifact) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO report_artifacts (`+reportArtifactColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID[:], a.OrgID[:], a.ReportRunID[:], a.Format, a.State, nullString(a.Path),
		nullInt64(a.SizeBytes), nullString(a.SHA256), nullString(a.Error),
		nullMillis(a.ExpiresAt), millis(a.CreatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("insert report artifact: %w", err)
	}
	return nil
}

// ArtifactsForRun lists a run's artifacts, including the failed and expired ones.
//
// Including, deliberately. A format that did not render is why the run is
// partial, and an expired one is why a bookmarked link answers 410 rather than
// 404; hiding either would leave the run's own state unexplained on the page
// that shows it.
func (s *Store) ArtifactsForRun(ctx context.Context, runID model.ID) ([]model.ReportArtifact, error) {
	byRun, err := s.ArtifactsForRuns(ctx, []model.ID{runID})
	if err != nil {
		return nil, err
	}
	return byRun[runID], nil
}

// ArtifactsForRuns fetches artifacts for a page of runs in one query.
//
// One query rather than one per run: a page of fifty runs is fifty round trips
// otherwise, on a screen an operator opens to see what went out this morning.
func (s *Store) ArtifactsForRuns(ctx context.Context, runIDs []model.ID) (map[model.ID][]model.ReportArtifact, error) {
	out := map[model.ID][]model.ReportArtifact{}
	if len(runIDs) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(runIDs)+1)
	args = append(args, model.SentinelOrgID[:])
	for _, id := range runIDs {
		args = append(args, id[:])
	}

	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+reportArtifactColumns+`
		 FROM report_artifacts
		 WHERE org_id = ? AND report_run_id IN (`+placeholders(len(runIDs))+`)
		 ORDER BY report_run_id, format`, args...)
	if err != nil {
		return nil, fmt.Errorf("list report artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		a, err := scanReportArtifact(rows)
		if err != nil {
			return nil, err
		}
		out[a.ReportRunID] = append(out[a.ReportRunID], a)
	}
	return out, rows.Err()
}

// GetReportArtifact reads one artifact by its own id, which is how the
// artifact-addressed download path finds it.
func (s *Store) GetReportArtifact(ctx context.Context, id model.ID) (model.ReportArtifact, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+reportArtifactColumns+` FROM report_artifacts WHERE id = ? AND org_id = ?`,
		id[:], model.SentinelOrgID[:])
	return scanReportArtifact(row)
}

// ArtifactByFormat finds a run's artifact in one format, which is what
// /report-runs/{id}/download?format= resolves.
func (s *Store) ArtifactByFormat(ctx context.Context, runID model.ID, format string) (model.ReportArtifact, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+reportArtifactColumns+`
		 FROM report_artifacts WHERE report_run_id = ? AND format = ? AND org_id = ?`,
		runID[:], format, model.SentinelOrgID[:])
	return scanReportArtifact(row)
}

// ExpirableArtifacts lists the artifacts whose bytes may now be reclaimed.
//
// Returns rows rather than deleting them, because the sweeper has to unlink a
// file before it can honestly mark the row expired — and the file is the half
// this package does not own.
func (s *Store) ExpirableArtifacts(ctx context.Context, now time.Time, limit int) ([]model.ReportArtifact, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+reportArtifactColumns+`
		 FROM report_artifacts
		 WHERE state = ? AND expires_at IS NOT NULL AND expires_at <= ?
		 ORDER BY expires_at LIMIT ?`,
		model.ArtifactRendered, millis(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list expirable artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ReportArtifact
	for rows.Next() {
		a, err := scanReportArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkArtifactExpired turns a row into a tombstone: the bytes are gone, the row
// stays.
//
// The row stays so that a bookmarked share link answers "this existed and is
// gone" with a 410 rather than "no such thing" with a 404. The two are different
// facts and a client chasing a missing report deserves the first one. The path
// is cleared with the size and digest, because a path that no longer resolves is
// worse than no path — it invites the next reader to go looking.
func (s *Store) MarkArtifactExpired(ctx context.Context, id model.ID) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE report_artifacts
		SET state = ?, path = NULL, size_bytes = NULL, sha256 = NULL
		WHERE id = ? AND org_id = ?`,
		model.ArtifactExpired, id[:], model.SentinelOrgID[:])
	if err != nil {
		return fmt.Errorf("expire report artifact: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// LiveArtifactPaths is every path the database believes in.
//
// The orphan sweeper's other half: ADR-008 writes the file before the row, so a
// crash in between leaves bytes nothing references. Comparing the directory
// against this set is how they are found, and it is the only correct direction —
// walking the rows and checking the disk finds missing files, not extra ones.
func (s *Store) LiveArtifactPaths(ctx context.Context) (map[string]bool, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT path FROM report_artifacts WHERE path IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("list artifact paths: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out[path] = true
	}
	return out, rows.Err()
}

// RecordReportDelivery appends one attempt to the log.
func (s *Store) RecordReportDelivery(ctx context.Context, d model.ReportDelivery) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO report_deliveries (id, org_id, report_run_id,
		    report_schedule_delivery_id, type, outcome, error, attempt, target,
		    delivered_at, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID[:], d.OrgID[:], d.ReportRunID[:], nullID(d.ReportScheduleDeliveryID),
		d.Type, d.Outcome, nullString(d.Error), d.Attempt, nullString(d.Target),
		nullMillis(d.DeliveredAt), millis(d.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert report delivery: %w", err)
	}
	return nil
}

// DeliveriesForRun is the log shown against a run, oldest attempt first so that
// a retry reads as a sequence rather than as a list somebody has to re-sort.
func (s *Store) DeliveriesForRun(ctx context.Context, runID model.ID) ([]model.ReportDelivery, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT id, org_id, report_run_id, report_schedule_delivery_id, type, outcome,
		       error, attempt, target, delivered_at, created_at
		FROM report_deliveries WHERE report_run_id = ? AND org_id = ?
		ORDER BY created_at, attempt`, runID[:], model.SentinelOrgID[:])
	if err != nil {
		return nil, fmt.Errorf("list report deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.ReportDelivery
	for rows.Next() {
		var (
			d                     model.ReportDelivery
			id, orgID, runIDBytes []byte
			targetID              []byte
			failure, target       sql.NullString
			delivered             sql.NullInt64
			attempt, createdAt    int64
		)
		if err := rows.Scan(&id, &orgID, &runIDBytes, &targetID, &d.Type, &d.Outcome,
			&failure, &attempt, &target, &delivered, &createdAt); err != nil {
			return nil, fmt.Errorf("scan report delivery: %w", err)
		}
		copy(d.ID[:], id)
		copy(d.OrgID[:], orgID)
		copy(d.ReportRunID[:], runIDBytes)
		d.ReportScheduleDeliveryID = idFromBytes(targetID)
		d.Error = failure.String
		d.Attempt = int(attempt)
		d.Target = target.String
		d.DeliveredAt = nullableTime(delivered)
		d.CreatedAt = fromMillis(createdAt)
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanReportRun(row scanner) (model.ReportRun, error) {
	var (
		r                      model.ReportRun
		id, orgID, templateID  []byte
		scheduleID             []byte
		failure                sql.NullString
		started, finished      sql.NullInt64
		late                   int64
		periodStart, periodEnd int64
		createdAt              int64
	)

	if err := row.Scan(&id, &orgID, &templateID, &scheduleID, &r.State, &periodStart,
		&periodEnd, &r.Timezone, &late, &failure, &started, &finished, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ReportRun{}, ErrNotFound
		}
		return model.ReportRun{}, fmt.Errorf("scan report run: %w", err)
	}

	copy(r.ID[:], id)
	copy(r.OrgID[:], orgID)
	copy(r.ReportTemplateID[:], templateID)
	r.ReportScheduleID = idFromBytes(scheduleID)
	r.PeriodStart = fromMillis(periodStart)
	r.PeriodEnd = fromMillis(periodEnd)
	r.Late = late == 1
	r.Error = failure.String
	r.StartedAt = nullableTime(started)
	r.FinishedAt = nullableTime(finished)
	r.CreatedAt = fromMillis(createdAt)
	return r, nil
}

func scanReportArtifact(row scanner) (model.ReportArtifact, error) {
	var (
		a                  model.ReportArtifact
		id, orgID, runID   []byte
		path, sha, failure sql.NullString
		size, expires      sql.NullInt64
		createdAt          int64
	)

	if err := row.Scan(&id, &orgID, &runID, &a.Format, &a.State, &path, &size,
		&sha, &failure, &expires, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ReportArtifact{}, ErrNotFound
		}
		return model.ReportArtifact{}, fmt.Errorf("scan report artifact: %w", err)
	}

	copy(a.ID[:], id)
	copy(a.OrgID[:], orgID)
	copy(a.ReportRunID[:], runID)
	a.Path = path.String
	a.SizeBytes = size.Int64
	a.SHA256 = sha.String
	a.Error = failure.String
	a.ExpiresAt = nullableTime(expires)
	a.CreatedAt = fromMillis(createdAt)
	return a, nil
}

// isUniqueViolation is matched on the message because modernc.org/sqlite does not
// export a typed constraint error. Narrow on purpose: a broader match would turn
// an unrelated failure into a 409 that tells the caller to change a name they
// have not used twice.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
