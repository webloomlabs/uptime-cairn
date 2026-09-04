package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Share links: an unauthenticated URL onto one run.
//
// # The token is stored twice, and the duplication is the point
//
// "Hash what you verify, encrypt what you replay" (data model §12.1) decides
// this one twice over, because a share token is both. It is **verified** when a
// client follows the link, which wants a hash and a unique index; and it is
// **replayed** when the operator comes back to copy the URL again, which wants an
// envelope, because a hash cannot be un-hashed and a link nobody can re-read is a
// link that has to be revoked and recreated to be sent twice.
//
// This is the shape `subscribers.unsubscribe_token` already uses, adopted rather
// than reinvented — ADR-008 item 14 names it as the precedent.
//
// # One live link per run
//
// Enforced by a partial unique index rather than by a handler check, which is
// what makes it an invariant of the database instead of a convention two code
// paths have to agree on. Creating a second link while one lives is a conflict
// the caller reports; revocation is a column rather than a delete, so a
// withdrawn link answers "this was withdrawn" instead of looking like a typo.

const reportShareColumns = `
	id, org_id, report_run_id, token_hash, token_encrypted, expires_at,
	revoked_at, last_accessed_at, created_at`

// CreateReportShareLink stores a new link.
//
// Returns ErrConflict when the run already has a live one, from the partial
// unique index rather than from a read-then-write — two requests that race are
// then resolved by the database, and exactly one of them wins.
func (s *Store) CreateReportShareLink(ctx context.Context, link model.ReportShareLink) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO report_share_links (`+reportShareColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		link.ID[:], link.OrgID[:], link.ReportRunID[:], link.TokenHash, link.TokenSealed,
		nullMillis(link.ExpiresAt), nullMillis(link.RevokedAt),
		nullMillis(link.LastAccessedAt), millis(link.CreatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("insert report share link: %w", err)
	}
	return nil
}

// ReportShareLinkForRun returns the live link for a run, if there is one.
//
// Live means not revoked. Expiry is deliberately **not** filtered here: an
// expired link is still the link this run has, and the operator's screen has to
// be able to say "this expired on Tuesday" rather than offering to create a
// second one and then hitting the unique index. The public read path applies
// expiry itself, where the answer is a status code rather than a sentence.
func (s *Store) ReportShareLinkForRun(ctx context.Context, runID model.ID) (model.ReportShareLink, error) {
	row := s.ro.QueryRowContext(ctx, `
		SELECT `+reportShareColumns+`
		FROM report_share_links
		WHERE report_run_id = ? AND revoked_at IS NULL`, runID[:])
	return scanReportShareLink(row)
}

// ReportShareLinksForRuns is the listing's batch read: one query for a page of
// runs rather than one per row.
//
// The same shape ArtifactsForRuns takes, and for the same reason — a run listing
// showing twenty-five runs would otherwise issue twenty-five extra queries, which
// is the N+1 that makes a page slow at exactly the scale the product promises to
// stay fast at.
func (s *Store) ReportShareLinksForRuns(ctx context.Context, runIDs []model.ID) (map[model.ID]model.ReportShareLink, error) {
	out := make(map[model.ID]model.ReportShareLink, len(runIDs))
	if len(runIDs) == 0 {
		return out, nil
	}

	query := `SELECT ` + reportShareColumns + `
		FROM report_share_links
		WHERE revoked_at IS NULL AND report_run_id IN (` + placeholders(len(runIDs)) + `)`
	args := make([]any, 0, len(runIDs))
	for _, id := range runIDs {
		args = append(args, id[:])
	}

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list report share links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		link, err := scanReportShareLink(rows)
		if err != nil {
			return nil, err
		}
		out[link.ReportRunID] = link
	}
	return out, rows.Err()
}

// ReportShareLinkByTokenHash is the unauthenticated lookup.
//
// **By hash, against a unique index**, which is the property that makes guessing
// cost a single indexed probe rather than a walk of every share link on the
// instance — and the reason the index exists at all. The token itself never
// reaches this layer; the caller hashes it, so a plaintext credential is not one
// query log away from being on disk.
//
// Revoked links are returned rather than filtered, because "this was withdrawn"
// and "no such link" are different answers to somebody holding a bookmark and the
// caller has to be able to give the right one.
func (s *Store) ReportShareLinkByTokenHash(ctx context.Context, hash []byte) (model.ReportShareLink, error) {
	row := s.ro.QueryRowContext(ctx, `
		SELECT `+reportShareColumns+`
		FROM report_share_links WHERE token_hash = ?`, hash)
	return scanReportShareLink(row)
}

// RevokeReportShareLink withdraws a run's live link.
//
// Conditional on it still being live, so two concurrent revocations do not both
// claim to have done it — and so a revocation cannot silently reopen the window
// on a link that was already withdrawn by moving its timestamp forward.
func (s *Store) RevokeReportShareLink(ctx context.Context, runID model.ID, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE report_share_links SET revoked_at = ?
		WHERE report_run_id = ? AND revoked_at IS NULL`, millis(at), runID[:])
	if err != nil {
		return fmt.Errorf("revoke report share link: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchReportShareLink records that somebody opened the link.
//
// "Has the client opened it yet" is the first thing anybody asks after sending
// one. Best-effort by design: the caller ignores the error, because failing a
// public read over a statistics column would take a client's report offline to
// protect a timestamp.
func (s *Store) TouchReportShareLink(ctx context.Context, id model.ID, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE report_share_links SET last_accessed_at = ? WHERE id = ?`, millis(at), id[:])
	if err != nil {
		return fmt.Errorf("touch report share link: %w", err)
	}
	return nil
}

func scanReportShareLink(row scanner) (model.ReportShareLink, error) {
	var (
		link              model.ReportShareLink
		id, orgID, runID  []byte
		tokenHash, sealed []byte
		expires, revoked  sql.NullInt64
		accessed          sql.NullInt64
		createdAt         int64
	)

	if err := row.Scan(&id, &orgID, &runID, &tokenHash, &sealed, &expires,
		&revoked, &accessed, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.ReportShareLink{}, ErrNotFound
		}
		return model.ReportShareLink{}, fmt.Errorf("scan report share link: %w", err)
	}

	copy(link.ID[:], id)
	copy(link.OrgID[:], orgID)
	copy(link.ReportRunID[:], runID)
	// Copied rather than aliased: the driver may reuse the backing array for the
	// next row, and a map of links sharing one buffer is a bug that shows up only
	// under a listing.
	link.TokenHash = append([]byte(nil), tokenHash...)
	link.TokenSealed = append([]byte(nil), sealed...)
	link.ExpiresAt = nullableTime(expires)
	link.RevokedAt = nullableTime(revoked)
	link.LastAccessedAt = nullableTime(accessed)
	link.CreatedAt = fromMillis(createdAt)
	return link, nil
}
