package main

import (
	"context"
	"errors"
	"time"
)

// ErrNotImplemented is returned by targets that cannot run yet.
var ErrNotImplemented = errors.New("not implemented")

// ErrNotSupported is returned by a target that could never do something, as
// distinct from one that cannot do it yet. The partition phase is the case: the
// SQLite target has no engine to react to a failing host, and pretending
// otherwise would produce a number with nothing behind it.
var ErrNotSupported = errors.New("not supported by this target")

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

// Cursor is ADR-004's keyset position.
//
// It carries both forms because the two targets legitimately have different
// ones. The SQLite target seeks on the (updated_at, id) pair directly, because
// it is the index. The HTTP target only ever sees the opaque token the API
// hands back — deliberately opaque, so nobody builds one by hand and then
// depends on its shape. A harness that reconstructed the token from the pair
// would be asserting an encoding the API does not promise.
type Cursor struct {
	UpdatedAt time.Time
	ID        []byte

	// Token is the opaque `next_cursor` from a previous page, used by targets
	// that page through the API rather than the index.
	Token string
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

// WriteResult is what the sustained-write phase measured, and — just as
// important — how.
//
// The two targets measure genuinely different things and a report that presented
// one number for both would be lying. The SQLite target *drives* batches as fast
// as the write path will take them, which is a ceiling: "this is what the
// storage layer can absorb". The HTTP target *observes* the rate the running
// engine achieves, which is a floor imposed by arithmetic: N monitors on an
// I-second interval produce N/I results a second and cannot produce more,
// because there is nothing else to check.
//
// So Method travels with Rate, and the gate applies a different assertion to
// each. Conflating them is how a harness ends up reporting 49,000 writes a
// second for an engine that is quietly ten minutes behind schedule.
type WriteResult struct {
	Rate   float64
	Method string

	// Expected is the rate the configuration implies, for an observed
	// measurement. Zero when the measurement is a ceiling rather than a target.
	Expected float64

	// Shed and Rejected are what did not make it. A write rate quoted without
	// them is a rate that could have been achieved by throwing work away.
	Shed     uint64
	Rejected uint64

	// Redelivered is results offered to the write path beyond the rows that
	// resulted. It is correct behaviour — delivery is at-least-once and the
	// natural key absorbs the repeat — and it is still work being done twice,
	// which is worth a number rather than a shrug.
	Redelivered uint64

	// TargetRequests is how many times the checked endpoint was actually hit,
	// counted on the other side of the network by the harness itself.
	//
	// It is the one number here the engine cannot fake. Every other figure comes
	// from the engine's own counters, and a counter that is wrong reports a
	// system that is fine; this one says independently that the checks really
	// happened, and a large gap between it and the heartbeat count means results
	// were produced and never stored.
	TargetRequests uint64
}

// EngineCounters is the subset of the engine's self-report the gate asserts on.
// Read from /metrics, which is the same endpoint an operator scrapes — a harness
// with a private back door measures a system nobody else can see.
type EngineCounters struct {
	HeartbeatsWritten uint64
	ResultsIngested   uint64
	ResultsRejected   uint64
	AlertsPublished   uint64
	AlertsDropped     uint64

	ProbeShedResults   uint64
	ProbeSkippedChecks uint64
	ProbeChecksStarted uint64
	ProbeDueQueueDepth uint64
	ProbeBufferedItems uint64

	WebhookEventsDropped uint64
}

// Target is the seam between the scenarios and whatever is being measured.
//
// Two targets exist. The SQLite one exercises the schema directly and answers
// "is the data model right"; the HTTP one drives the real API against a running
// engine and answers "does the product hold up". They share the scenarios, which
// is the whole reason this interface exists rather than the scenarios talking to
// a database handle.
type Target interface {
	// Name identifies the target in the report.
	Name() string

	// Setup prepares the target and loads the workload into it.
	//
	// It may rewrite the workload's identifiers: the HTTP target creates
	// monitors through the API and the server assigns their ids, so the
	// scenarios have to be pointed at what actually exists rather than at what
	// the generator invented. It also fills w.DeepCursor.
	Setup(ctx context.Context, w *Workload, rollupHours int) error

	// MeasureWrites runs the sustained-write phase for the duration given.
	MeasureWrites(ctx context.Context, w *Workload, seconds int) (WriteResult, error)

	// ListMonitors performs one cursor-paginated, filtered page fetch.
	ListMonitors(ctx context.Context, q ListQuery) (ListResult, error)

	// Membership returns ADR-004's reconciliation signal for a filter.
	Membership(ctx context.Context, q ListQuery) (MembershipResult, error)

	// History reads rolled-up history for one monitor over a range, returning
	// the number of buckets read.
	History(ctx context.Context, monitorID []byte, from, to time.Time) (int, error)

	Close() error
}

// Disruptor is the optional half: a target that can break the thing its monitors
// are watching, and read back what the engine did about it.
//
// Optional rather than part of Target because the SQLite target could never
// implement it honestly. There is no engine underneath it — no scheduler, no
// probe, no ingest — so a partition would be the harness writing rows that say
// "down" and then reading them back, which measures nothing.
type Disruptor interface {
	// Partition makes every monitored target start failing, or recover.
	Partition(ctx context.Context, healthy bool) error

	// Counters reads the engine's self-report.
	Counters(ctx context.Context) (EngineCounters, error)

	// Deliveries is how many outbound webhook deliveries the harness has
	// received. The partition phase's real question: a burst that marks several
	// thousand monitors down inside one scheduler tick is exactly what the
	// delivery queue is sized against, and that size has been an argument rather
	// than a measurement.
	Deliveries() int
}
