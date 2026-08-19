package telemetry

import (
	"sync"
	"sync/atomic"
	"time"
)

// The process's view of itself.
//
// This package deliberately imports nothing from the rest of the product. That
// is what lets the probe and the control plane both publish into it without
// either importing the other, which ADR-001 forbids: in scaled mode they are
// separate processes, and a shared counter type that reached across the seam
// would be a dependency waiting to become a compile error.
//
// A package-level registry rather than a value threaded through constructors.
// There is exactly one process, the numbers are a property of it rather than of
// any object in it, and the alternative — a Metrics parameter on the control
// plane, the probe session, two dispatchers, and the scheduler — is five
// signatures changed to observe something none of them are about.

// Engine holds the counters the control plane owns: what actually reached
// storage, and what did not.
var Engine engineMetrics

type engineMetrics struct {
	// HeartbeatsWritten counts rows durably written. Incremented after the
	// write returns, never before: a counter that moves on intent rather than
	// on completion is a counter that says the system is fine while it is
	// losing data.
	HeartbeatsWritten atomic.Uint64

	// ResultsIngested counts results offered to the write path. It exceeds
	// HeartbeatsWritten exactly when a probe redelivers, because the natural key
	// absorbs the repeat — which is correct behaviour and still worth seeing.
	// One counter for both would make "the probe is resending" and "the system
	// is doing twice the work" indistinguishable.
	ResultsIngested atomic.Uint64

	// ResultsRejected counts results that could not be attributed — a monitor
	// deleted between assignment and result, or an unspecified outcome, which is
	// a probe bug. Rejections are dropped rather than retried, so this is the
	// only place they are visible.
	ResultsRejected atomic.Uint64

	// ChecksRunInline counts checks the API ran itself for POST .../check. They
	// go through the same ingest and are counted separately, so a spike in
	// heartbeats can be attributed to somebody holding down a button rather than
	// to the scheduler.
	ChecksRunInline atomic.Uint64

	// AlertsPublished and AlertsDropped are the notification path. Dropped is
	// the number that matters: the dispatcher sheds rather than blocking, because
	// alerting must never become backpressure on ingest, and a queue that
	// silently sheds is indistinguishable from one nobody is using.
	AlertsPublished atomic.Uint64
	AlertsDropped   atomic.Uint64

	startedAt atomic.Int64
}

// MarkStart records process start, so uptime is reported from the same clock as
// everything else here.
func MarkStart(at time.Time) { Engine.startedAt.Store(at.UnixMilli()) }

// Uptime reports how long the process has been running, or zero before
// MarkStart.
func Uptime(now time.Time) time.Duration {
	started := Engine.startedAt.Load()
	if started == 0 {
		return 0
	}
	return now.Sub(time.UnixMilli(started))
}

// ProbeHealth is a probe's self-report, flattened out of the protocol message
// so this package does not import the generated protobuf types — the same
// reason it imports nothing else.
//
// A probe has no inbound port to scrape, so these ride the result stream and the
// control plane republishes them here (docs/probe/protocol.md §8). In solo mode
// the probe is in this process and the path is the same, which is the point: the
// numbers an operator reads are produced by the code a remote probe runs.
type ProbeHealth struct {
	ProbeID string
	Name    string

	ReportedAt time.Time

	Assigned             uint32
	InFlight             uint32
	MaxConcurrent        uint32
	DueQueueDepth        uint32
	BufferedResults      uint64
	BufferedBytes        uint64
	ShedResultsTotal     uint64
	SkippedChecksTotal   uint64
	ChecksStartedTotal   uint64
	ChecksCompletedTotal uint64
	UptimeSeconds        uint64
	ClockOffsetMicros    int64
}

var probes struct {
	mu     sync.RWMutex
	latest map[string]ProbeHealth
}

// RecordProbeHealth stores the most recent report from one probe.
//
// Last value wins rather than accumulating: these are gauges plus monotonic
// counters, and the useful question is "what is this probe doing now", not "what
// was the average". Keeping a history here would be building a time-series
// database inside the process that already has one.
func RecordProbeHealth(h ProbeHealth) {
	probes.mu.Lock()
	defer probes.mu.Unlock()

	if probes.latest == nil {
		probes.latest = make(map[string]ProbeHealth, 4)
	}
	probes.latest[h.ProbeID] = h
}

// Probes returns the current reports, in a stable order by probe id so a scrape
// does not reshuffle its own output between calls.
func Probes() []ProbeHealth {
	probes.mu.RLock()
	defer probes.mu.RUnlock()

	out := make([]ProbeHealth, 0, len(probes.latest))
	for _, h := range probes.latest {
		out = append(out, h)
	}
	sortByID(out)
	return out
}

// Reset clears everything. For tests only: package-level state that no test can
// put back is package-level state that makes tests order-dependent.
func Reset() {
	Engine.HeartbeatsWritten.Store(0)
	Engine.ResultsIngested.Store(0)
	Engine.ResultsRejected.Store(0)
	Engine.ChecksRunInline.Store(0)
	Engine.AlertsPublished.Store(0)
	Engine.AlertsDropped.Store(0)
	Engine.startedAt.Store(0)

	probes.mu.Lock()
	probes.latest = nil
	probes.mu.Unlock()
}

// sortByID is an insertion sort. The slice has one element on every install this
// release supports and a handful in Phase 4, so pulling in sort for it would be
// more machinery than the problem.
func sortByID(out []ProbeHealth) {
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ProbeID < out[j-1].ProbeID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
}
