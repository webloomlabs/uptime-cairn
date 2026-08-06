package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotImplemented is returned by targets that cannot run yet.
var ErrNotImplemented = errors.New("not implemented")

// Monitor is the harness's view of a monitor: only the fields the scenarios
// actually exercise, not the full API entity.
type Monitor struct {
	ID        []byte
	Name      string
	Type      string
	Target    string
	Config    string
	GroupID   []byte
	TagIDs    [][]byte
	Status    string
	Interval  int
	Timeout   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Heartbeat is one check result. Status is the integer encoding from the data
// model (§5.2): 0=down, 1=up, 2=pending, 3=maintenance.
type Heartbeat struct {
	Time       time.Time
	MonitorID  []byte
	Status     int
	ResponseMS float64
	Important  bool
}

// Cursor is ADR-004's keyset position: the (updated_at, id) pair every list view
// pages on.
type Cursor struct {
	UpdatedAt time.Time
	ID        []byte
}

// ListQuery mirrors the filter and cursor parameters of GET /api/v1/monitors.
type ListQuery struct {
	Limit   int
	Cursor  *Cursor
	Status  string
	TagID   []byte
	GroupID []byte
}

// ListResult reports what a page fetch returned. Rows is the count actually
// handed back — the number ADR-004 requires to stay bounded by viewport size
// rather than growing with the total monitor count.
type ListResult struct {
	Rows int
	Next *Cursor
}

// MembershipResult is the signal behind GET /api/v1/monitors/membership.
type MembershipResult struct {
	Version int64
	Count   int64
}

// Target is the seam between the scenarios and whatever is being measured.
//
// Today only the SQLite target exists, exercising the schema directly, because
// there is no server to point at. Phase 1 adds an HTTP target that drives the
// real /api/v1 endpoints; the scenarios in scenario.go do not change when it
// does. That is the whole reason this interface exists rather than the scenarios
// talking to a database handle.
type Target interface {
	// Name identifies the target in the report.
	Name() string

	// Setup prepares the target and loads the workload into it.
	Setup(ctx context.Context, w *Workload, rollupHours int) error

	// WriteHeartbeats writes one batch. Batching is the contract, not an
	// optimisation: the data model (§5.1) requires heartbeats be written one
	// transaction per scheduler tick, because SQLite in WAL mode is a single
	// writer with an fsync per transaction and 250 individual commits per second
	// will not hold up on a Pi.
	WriteHeartbeats(ctx context.Context, batch []Heartbeat) error

	// ListMonitors performs one cursor-paginated, filtered page fetch.
	ListMonitors(ctx context.Context, q ListQuery) (ListResult, error)

	// Membership returns ADR-004's reconciliation signal for a filter.
	Membership(ctx context.Context, q ListQuery) (MembershipResult, error)

	// History reads rolled-up history for one monitor over a range, returning
	// the number of buckets read.
	History(ctx context.Context, monitorID []byte, from, to time.Time) (int, error)

	Close() error
}

// HTTPTarget will drive the real API in Phase 1. It exists now so the seam is
// real rather than hypothetical, and so that pointing the harness at a server
// fails with an explanation instead of a nil dereference.
//
// It deliberately does not silently succeed. A load-test gate that passes
// because it measured nothing is worse than no gate: it lets the Phase 0 exit
// criterion be ticked while asserting nothing.
type HTTPTarget struct {
	BaseURL string
}

func (t *HTTPTarget) Name() string { return fmt.Sprintf("http(%s)", t.BaseURL) }

func (t *HTTPTarget) Setup(context.Context, *Workload, int) error {
	return fmt.Errorf(
		"%w: the HTTP target drives /api/v1 against a running server, and Phase 0 has none. "+
			"It lands with the API server in Phase 1; use -target sqlite until then",
		ErrNotImplemented)
}

func (t *HTTPTarget) WriteHeartbeats(context.Context, []Heartbeat) error {
	return ErrNotImplemented
}

func (t *HTTPTarget) ListMonitors(context.Context, ListQuery) (ListResult, error) {
	return ListResult{}, ErrNotImplemented
}

func (t *HTTPTarget) Membership(context.Context, ListQuery) (MembershipResult, error) {
	return MembershipResult{}, ErrNotImplemented
}

func (t *HTTPTarget) History(context.Context, []byte, time.Time, time.Time) (int, error) {
	return 0, ErrNotImplemented
}

func (t *HTTPTarget) Close() error { return nil }
