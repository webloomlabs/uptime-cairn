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
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// ErrNotFound is the one error every backend must report the same way. It lives
// here rather than in an implementation so a caller can handle a missing row
// without importing a database driver — which is exactly the leak this package
// exists to prevent.
var ErrNotFound = errors.New("not found")

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

// MonitorWithState is what every read path actually wants: the configuration and
// what it is currently doing, joined once rather than fetched twice. It lives
// here rather than in an implementation because the API speaks it too.
type MonitorWithState struct {
	Monitor model.Monitor
	State   model.MonitorState
}

// Cursor is ADR-004's pagination key: (updated_at, id), applied uniformly with
// no small-install exception where the full set is sent because it happens to
// fit today.
//
// Opaque to clients — base64 so nobody builds one by hand and then depends on
// the shape, which would make the shape a compatibility promise.
type Cursor struct {
	UpdatedAt time.Time
	ID        model.ID
}

// Encode renders the cursor for the wire.
func (c Cursor) Encode() string {
	raw := strconv.FormatInt(c.UpdatedAt.UnixMilli(), 10) + "." + hex.EncodeToString(c.ID[:])
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses one back. A malformed cursor is a client error rather than
// a silent reset to page one, which would loop forever.
func DecodeCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("malformed cursor: %w", err)
	}
	msText, idHex, found := strings.Cut(string(raw), ".")
	if !found {
		return Cursor{}, errors.New("malformed cursor")
	}
	ms, err := strconv.ParseInt(msText, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("malformed cursor: %w", err)
	}
	var c Cursor
	idBytes, err := hex.DecodeString(idHex)
	if err != nil || len(idBytes) != len(c.ID) {
		return Cursor{}, errors.New("malformed cursor")
	}
	c.UpdatedAt = time.UnixMilli(ms).UTC()
	copy(c.ID[:], idBytes)
	return c, nil
}

// EncodeTimeCursor renders a heartbeat page cursor. Opaque, like every other
// cursor in the API — the client passes it back and does not read it.
func EncodeTimeCursor(t time.Time) string {
	return base64.RawURLEncoding.EncodeToString([]byte("t." + strconv.FormatInt(t.UnixMicro(), 10)))
}

// DecodeTimeCursor parses one back.
func DecodeTimeCursor(s string) (time.Time, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("malformed cursor: %w", err)
	}
	value, ok := strings.CutPrefix(string(raw), "t.")
	if !ok {
		return time.Time{}, fmt.Errorf("malformed cursor")
	}
	us, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("malformed cursor: %w", err)
	}
	return time.UnixMicro(us).UTC(), nil
}
