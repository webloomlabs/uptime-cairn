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

// ErrConflict is the other one: the write is well-formed and refers to real
// things, and the current state will not have it — a tag slug already taken, a
// referenced row still in use. Distinct from ErrNotFound because the caller's
// answer is different: a 409 asks the user to choose another name, a 404 tells
// them the thing is not there.
var ErrConflict = errors.New("conflict")

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

// ChannelWithCount is a notification channel plus how many monitors point at
// it. The count comes from the same query rather than a second round trip,
// because a channel list is exactly where an operator asks "is anything using
// this?" before deleting one.
type ChannelWithCount struct {
	Channel      model.NotificationChannel
	MonitorCount int
}

// MonitorFilter narrows a monitor listing. Every field is optional; the zero
// value lists everything.
//
// Only the two taxonomy filters are here so far. status, type, enabled, and
// search are specified and not implemented, and they belong in this struct when
// they are — the shape is the point, so that adding one is a clause rather than
// a signature change.
type MonitorFilter struct {
	// GroupIDs match monitors in any of these groups, or in a child of one.
	// A parent group filtering to nothing while its children hold the monitors
	// would be the same lie the monitor count avoids.
	GroupIDs []model.ID

	// TagIDs match monitors carrying any of these tags.
	TagIDs []model.ID
}

// Empty reports whether the filter narrows anything.
func (f MonitorFilter) Empty() bool { return len(f.GroupIDs)+len(f.TagIDs) == 0 }

// ChannelFilter narrows a channel listing. Every field is optional; the zero
// value lists everything.
type ChannelFilter struct {
	// Search matches the channel name, case-insensitively.
	Search string

	// Types restricts to these channel types. Empty means all.
	Types []string

	// Enabled restricts to enabled or disabled channels. Nil means both.
	Enabled *bool
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

// HistoryBucket is one aggregated interval, whatever produced it — a rollup tier
// or a direct aggregation of raw heartbeats. The API renders it; nothing here
// computes a ratio, because the caller's maintenance policy decides what the
// denominator is (data model §5.3).
type HistoryBucket struct {
	Start time.Time

	Up, Down, Pending, Maintenance int

	// Unknown and Skipped are carried even though the API's HistoryBucket schema
	// has no field for them: they are what makes a null uptime_ratio explicable
	// rather than mysterious, and the ratio is computed from these counts.
	Unknown, Skipped int

	// Sum and count rather than an average, all the way to the edge of the
	// system. The API divides; nothing before it does.
	ResponseTimeSum   float64
	ResponseTimeCount int

	ResponseTimeMin *float64
	ResponseTimeMax *float64

	// ResponseTimeP95 is nil unless it is a real percentile. A p95 quoted
	// without its method is worse than no p95 (§11.5), and the API schema has no
	// field in which to say "approximate" — so an approximation is reported as
	// absent instead.
	ResponseTimeP95 *float64
}

// Observed reports whether this bucket has anything to compute a ratio from.
// unknown and skipped never count: they are gaps in observation, not
// observations of failure.
func (b HistoryBucket) Observed() int { return b.Up + b.Down }
