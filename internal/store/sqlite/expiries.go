package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The expiry calendar.
//
// The data has existed since migration 0003 and this is a report over it, which
// is exactly what the phase plan says it is. Two tables, deliberately shaped
// differently — a registration has a registrar and a source, and no subject,
// chain or serial — unioned into one ordered list here rather than merged in the
// schema, because the calendar is the only reader that wants them together.
//
// Both halves seek their own index: `idx_monitor_certificates_expiry` and
// `idx_monitor_domain_expiry` are both `(org_id, <the date>)`. Checked with
// EXPLAIN QUERY PLAN rather than assumed, and the plan is stated honestly rather
// than claimed clean — it is a `SEARCH` on the index followed by a temporary
// B-tree for the *last* term of the ordering. The index supplies the date order
// and only the monitor-id tiebreak is sorted, so what gets sorted is the set of
// rows sharing an expiry millisecond. Making that disappear would mean an index
// on `(org_id, valid_to, monitor_id)` on both tables, which is two more indexes
// to keep for a tiebreak nobody pages through.

// ExpiryFilter narrows the calendar.
type ExpiryFilter = store.ExpiryFilter

// ListUpcomingExpiries pages the calendar, soonest first.
//
// # Ordering, and the one collision the cursor cannot see
//
// The keyset is `(expires_at, monitor_id)`, the same two-part shape every other
// collection uses. It is unique unless one monitor holds **both** a certificate
// and a domain registration expiring in the same millisecond, which is the only
// way two rows can tie on both parts. That row would be skipped at a page
// boundary. It is stated rather than defended against, because the alternative is
// a third cursor component on a type shared by every collection in the product,
// bought to cover a coincidence measured in milliseconds.
//
// # Already-expired entries are included
//
// `within_days` bounds the future and not the past. Something that expired
// eleven days ago is the most urgent row on the calendar and is precisely what
// somebody opened the page to find; filtering it out would leave the screen
// looking calm on the worst possible day.
func (s *Store) ListUpcomingExpiries(
	ctx context.Context,
	after *Cursor,
	limit int,
	filter ExpiryFilter,
	now time.Time,
) ([]model.UpcomingExpiry, bool, error) {
	if limit <= 0 {
		limit = 25
	}

	certificates := `
		SELECT 'certificate' AS kind, c.monitor_id, m.name,
		       c.subject, c.issuer, c.valid_to AS expires_at, c.observed_at
		FROM monitor_certificates c
		JOIN monitors m ON m.id = c.monitor_id
		WHERE c.org_id = ?`
	domains := `
		SELECT 'domain' AS kind, d.monitor_id, m.name,
		       d.domain AS subject, d.registrar AS issuer, d.expires_at, d.observed_at
		FROM monitor_domain_expiry d
		JOIN monitors m ON m.id = d.monitor_id
		WHERE d.org_id = ?`

	certArgs := []any{model.SentinelOrgID[:]}
	domainArgs := []any{model.SentinelOrgID[:]}

	if filter.WithinDays != nil {
		cutoff := millis(now.AddDate(0, 0, *filter.WithinDays))
		certificates += ` AND c.valid_to <= ?`
		domains += ` AND d.expires_at <= ?`
		certArgs = append(certArgs, cutoff)
		domainArgs = append(domainArgs, cutoff)
	}

	// Tags narrow both halves identically. EXISTS rather than a join, so a
	// monitor carrying two of the requested tags appears once rather than twice
	// — the same rule MonitorsInScope follows and for the same reason.
	if len(filter.TagIDs) > 0 {
		clause := ` AND EXISTS (SELECT 1 FROM monitor_tags mt WHERE mt.monitor_id = %s.monitor_id
		            AND mt.tag_id IN (` + placeholders(len(filter.TagIDs)) + `))`
		certificates += fmt.Sprintf(clause, "c")
		domains += fmt.Sprintf(clause, "d")
		for _, id := range filter.TagIDs {
			certArgs = append(certArgs, id[:])
			domainArgs = append(domainArgs, id[:])
		}
	}

	// The kind filter drops a whole branch rather than filtering the union, so
	// an operator asking only for domains does not read the certificate index at
	// all.
	wantCert, wantDomain := true, true
	if len(filter.Kinds) > 0 {
		wantCert, wantDomain = false, false
		for _, kind := range filter.Kinds {
			switch kind {
			case model.ExpiryCertificate:
				wantCert = true
			case model.ExpiryDomain:
				wantDomain = true
			}
		}
	}
	if !wantCert && !wantDomain {
		return nil, false, nil
	}

	var query string
	var args []any
	switch {
	case wantCert && wantDomain:
		query = "SELECT * FROM (" + certificates + " UNION ALL " + domains + ")"
		args = append(append([]any{}, certArgs...), domainArgs...)
	case wantCert:
		query = "SELECT * FROM (" + certificates + ")"
		args = certArgs
	default:
		query = "SELECT * FROM (" + domains + ")"
		args = domainArgs
	}

	if after != nil {
		query += ` WHERE (expires_at, monitor_id) > (?, ?)`
		args = append(args, millis(after.UpdatedAt), after.ID[:])
	}
	// One more than asked for, which is how every collection here answers
	// has_more without a second count query.
	query += ` ORDER BY expires_at, monitor_id LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list upcoming expiries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.UpcomingExpiry
	for rows.Next() {
		var (
			e                   model.UpcomingExpiry
			monitorID           []byte
			subject, issuer     sql.NullString
			expiresAt, observed int64
		)
		if err := rows.Scan(&e.Kind, &monitorID, &e.MonitorName, &subject, &issuer,
			&expiresAt, &observed); err != nil {
			return nil, false, fmt.Errorf("scan expiry: %w", err)
		}
		copy(e.MonitorID[:], monitorID)
		e.Subject, e.Issuer = subject.String, issuer.String
		e.ExpiresAt = time.UnixMilli(expiresAt).UTC()
		e.ObservedAt = time.UnixMilli(observed).UTC()
		e.DaysRemaining = daysUntil(now, e.ExpiresAt)
		out = append(out, e)
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

// daysUntil is whole days from now to the expiry, signed.
//
// **Signed rather than floored at zero**, because "expired eleven days ago" is
// the row somebody most needs to see and flooring it would file that beside
// "expires today". Rounded down towards the expiry so that a certificate with
// twenty-three hours left reports zero rather than one — the pessimistic
// direction, which is the right one for a number somebody schedules work
// against.
func daysUntil(now, expires time.Time) int {
	hours := expires.Sub(now).Hours() / 24
	return int(math.Floor(hours))
}
