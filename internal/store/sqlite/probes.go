package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Probes are read-only in this build.
//
// Enrolment, revocation, and everything else that writes to this table is Phase
// 4 work behind the protocol's credential exchange. What Phase 1 needs is the
// ability to name a probe, because a `docker` monitor asks a question only one
// host can answer and the pin has to reference something (protocol §6.4).
//
// token_hash is not in any query here. It is a credential, nothing that renders
// a probe wants it, and the surest way to keep it out of a response is to never
// load it.
const probeColumns = `id, org_id, name, region, mode, version, last_seen_at, enabled, created_at`

// ListProbes returns every probe, ordered so the embedded one comes first.
//
// Ordered rather than left to the storage engine because this list is what a
// picker renders, and in solo mode it has exactly one entry that should not
// move around between requests.
func (s *Store) ListProbes(ctx context.Context) ([]model.Probe, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT `+probeColumns+` FROM probes
		WHERE org_id = ?
		ORDER BY mode = 'embedded' DESC, name, id`, model.SentinelOrgID[:])
	if err != nil {
		return nil, fmt.Errorf("list probes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []model.Probe
	for rows.Next() {
		p, err := scanProbe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProbe reads one probe. ErrNotFound is what the pin validation turns into a
// field error naming /probe_id, rather than a foreign-key failure surfacing as
// a 500 three layers away.
func (s *Store) GetProbe(ctx context.Context, id model.ID) (model.Probe, error) {
	row := s.ro.QueryRowContext(ctx, `
		SELECT `+probeColumns+` FROM probes WHERE id = ? AND org_id = ?`,
		id[:], model.SentinelOrgID[:])

	p, err := scanProbe(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Probe{}, ErrNotFound
	}
	return p, err
}

// CountEnabledProbes is what decides whether an unpinned host-local monitor is
// ambiguous. One probe is solo mode and the pin is implied; more than one and
// the operator has to say which host they meant, because guessing would report
// a container missing that was never meant to be there.
func (s *Store) CountEnabledProbes(ctx context.Context) (int, error) {
	var n int
	err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM probes WHERE org_id = ? AND enabled = 1`,
		model.SentinelOrgID[:]).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count probes: %w", err)
	}
	return n, nil
}

func scanProbe(row scanner) (model.Probe, error) {
	var (
		p                 model.Probe
		id, orgID         []byte
		region, agentVers sql.NullString
		lastSeen          sql.NullInt64
		enabled           int64
		createdAt         int64
	)
	if err := row.Scan(&id, &orgID, &p.Name, &region, &p.Mode, &agentVers,
		&lastSeen, &enabled, &createdAt); err != nil {
		return model.Probe{}, err
	}
	copy(p.ID[:], id)
	copy(p.OrgID[:], orgID)
	p.Region = region.String
	p.Version = agentVers.String
	p.LastSeenAt = nullableTime(lastSeen)
	p.Enabled = enabled == 1
	p.CreatedAt = fromMillis(createdAt)
	return p, nil
}
