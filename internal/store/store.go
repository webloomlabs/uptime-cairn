// Package store defines the persistence seam.
//
// ADR-002 requires business logic to speak to interfaces rather than to SQL, so
// that the SQLite install on a Raspberry Pi and the PostgreSQL + TimescaleDB
// install serving an agency are the same product with different wiring. The
// upgrade between them is a config change and a migration, never a reinstall,
// and that promise is only as good as this boundary.
//
// What the two backends genuinely differ on, and therefore what must never leak
// past these interfaces (data model §7.1):
//
//   - Rollup computation: continuous aggregate versus scheduled job.
//   - Retention: policy versus delete loop.
//   - Batch insert shape: COPY versus a multi-row INSERT in one transaction.
//   - Idempotent batch insert, which is where they diverge hardest — SQLite
//     takes INSERT … ON CONFLICT DO NOTHING directly, while COPY has no conflict
//     clause at all.
//   - Type marshalling for uuid, timestamp, boolean, and JSON.
//
// Only internal/app constructs a concrete implementation. Any other package
// importing internal/store/sqlite is a bug in the dependency graph, not a
// shortcut.
package store

import (
	"context"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// HeartbeatStore owns the hottest write path in the system.
//
// Batch, not single-row, because the storage layer's primary operation is a
// batch write (data model §5.1) and the probe protocol's result frame is shaped
// to hand almost straight to it.
type HeartbeatStore interface {
	// WriteBatch is idempotent. Delivery from a probe is at-least-once and
	// several probes may check one monitor by design, so a resent batch must be
	// a no-op rather than a duplicate row — the natural key
	// (org_id, monitor_id, time, probe_id) is what makes it one
	// (ADR-005 decision 16, data model §11.8).
	WriteBatch(ctx context.Context, beats []model.Heartbeat) error
}

// The remaining interfaces — monitors, rollups, notifications, status pages,
// incidents, maintenance windows, API keys, audit log — are deliberately not
// declared yet. Each one's method set follows from the OpenAPI operations that
// use it, and inventing signatures before the handlers exist produces an
// interface shaped by guesswork that everything then has to implement.
//
// The rule when they are added: a method returns domain types from
// internal/model, takes a context, and never takes or returns anything that
// names a backend.
