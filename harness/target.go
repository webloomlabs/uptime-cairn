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

	// Include is the `include=` embed list, verbatim. It exists so a scenario
	// can ask for exactly what the dashboard asks for: the strip of recent
	// checks and the uptime figures are the expensive part of the real request,
	// and a benchmark that measured the bare listing would be measuring a page
	// nobody loads.
	Include string

	// HeartbeatsLimit sets `heartbeats_limit=` when Include names heartbeats.
	HeartbeatsLimit int
}

// ListResult reports what a page fetch returned. Rows is the count actually
// handed back — the number ADR-004 requires to stay bounded by viewport size
// rather than growing with the total monitor count.
type ListResult struct {
	Rows int
	Next *Cursor

	// Bytes is the response body's size on the wire.
	//
	// This is the half of ADR-004's second invariant that latency does not
	// cover, and it is the half a frontend can break on its own. "Client-side
	// payload size and render cost stay bounded by viewport size, never by total
	// monitor count" is two claims; a page that returns 25 rows in 4 KB at 500
	// monitors and 25 rows in 400 KB at 5,000 has held the row count and broken
	// the invariant, and every latency figure in this report would still pass.
	//
	// Zero on a target that has no wire — the SQLite one reads rows out of the
	// schema and there is no serialised response to measure. The assertion is
	// skipped rather than reported as a claim about zero bytes.
	Bytes int
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

	// WriterWaits is how many times a statement had to queue for the store's
	// write connection, and WriterWaitSeconds how long it spent queued.
	//
	// These are what separate "this is slow" from "this is behind something
	// else", and the harness has needed that distinction since it first reported
	// that creating monitors gets slower as the install grows. A creation rate
	// that falls while the wait counter stays flat is work getting harder; the
	// same rate with the counter climbing is a queue, and only one of those is
	// fixed by moving reads off the write connection.
	//
	// Absent from a backend that has no pool, in which case they stay zero and
	// the line reporting them is skipped rather than printed as a claim.
	WriterWaits       uint64
	WriterWaitSeconds float64
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

// LiveResult is what one window of the browser update channel carried.
//
// The cost this measures is the one the 5,000-monitor gate does not exercise at
// all: it scales with connected clients and with viewport size, and not with
// how many monitors exist. That is precisely ADR-004's claim, and it is the
// claim that stops being true the first time somebody makes the stream carry
// everything "because it is simpler".
type LiveResult struct {
	// Clients is how many streams were open.
	Clients int

	// Scoped is how many monitors each stream subscribed to.
	Scoped int

	// Updates is the total monitor diffs delivered across every stream.
	Updates int

	// Foreign is diffs delivered for a monitor the receiving stream had not
	// subscribed to. Any at all is a failure: it means the channel is not
	// scoped, and the whole design rests on it being scoped.
	Foreign int

	// Bytes is the total delivered across every stream, summaries included.
	Bytes int

	// Seconds is the measurement window.
	Seconds float64
}

// PerClientRate is updates per second per open stream, which is the figure that
// must not move with monitor count.
func (l LiveResult) PerClientRate() float64 {
	if l.Clients == 0 || l.Seconds <= 0 {
		return 0
	}
	return float64(l.Updates) / float64(l.Clients) / l.Seconds
}

// Streamer is the optional half a target implements when it has a browser-facing
// update channel to measure.
type Streamer interface {
	// MeasureLive opens `clients` streams, each scoped to `scoped` monitors
	// drawn from the workload, and reports what arrived over the window.
	MeasureLive(ctx context.Context, w *Workload, clients, scoped, seconds int) (LiveResult, error)
}

// ReportResult is what the concurrent report-run phase measured.
//
// # The question, and why it needs two windows rather than one
//
// The phase plan's exit criterion for the report worker pool is that "fifty PDFs
// at 09:00 on the 1st must not delay a single check". That is a claim about
// *interference*, and interference cannot be read off one measurement: an
// install whose checks are 200ms late during a report burst has either a
// reporting problem or a slow runner, and only a comparison against the same
// install a minute earlier tells you which.
//
// So every figure here is a pair. The baseline is taken with the engine in
// steady state and nothing rendering; the burst is taken with `Submitted` runs
// in flight. The gate asserts on the *delta*, which is the only form of the
// assertion that survives a noisy shared runner.
//
// # Lateness rather than throughput
//
// The heartbeat write rate is the harness's existing steady-state measure and it
// is the wrong one here. A pool that delays every check by ten seconds and then
// catches up has an unchanged rate over a sixty-second window and has broken the
// promise exactly as stated. Check lateness — how long past its due time each
// monitor was last checked, as a fraction of its own interval — is the thing the
// criterion is actually about, and it is visible in
// `cairn_monitor_last_check_timestamp_seconds`, which the engine already
// publishes for operators.
type ReportResult struct {
	// Submitted is how many runs were requested, and Accepted how many the API
	// took. The difference is the pool refusing, which is correct behaviour and
	// not a failure: a bounded queue that answers 503 is the design.
	Submitted int
	Accepted  int
	Refused   int

	// Failed is a run the engine accepted and could not complete. Unlike a
	// refusal, this is a defect.
	Failed int

	// Completed is how many reached a terminal state inside the phase's
	// deadline, and Elapsed how long the whole burst took to drain.
	Completed int
	Elapsed   time.Duration

	// LatenessBefore and LatenessDuring are the p95 of (now - last checked) as a
	// fraction of the monitor's own interval, over every monitor in the
	// workload. 1.0 means "checked exactly one interval ago", which is the
	// steady state of a healthy install; 2.0 means a whole interval was missed.
	LatenessBefore float64
	LatenessDuring float64

	// SkippedDuring and ShedDuring are the probe's own counters over the burst.
	// A probe that sheds is protecting itself and is reported rather than
	// treated as a pass.
	SkippedDuring uint64
	ShedDuring    uint64

	// WriteRateBefore and WriteRateDuring bracket the throughput measure too,
	// because a pool that starved the scheduler entirely would show there first.
	WriteRateBefore float64
	WriteRateDuring float64
}

// Reporter is the optional half a target implements when it has a reporting
// subsystem to put under load.
//
// Optional for the same reason Disruptor is: the SQLite target has no engine, no
// worker pool and no renderer, so a report burst against it would be the harness
// timing its own loop.
type Reporter interface {
	// MeasureReports fires `runs` report generations concurrently and measures
	// what they did to check scheduling.
	MeasureReports(ctx context.Context, w *Workload, runs int) (ReportResult, error)
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
