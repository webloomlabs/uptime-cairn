package probe

import (
	"bytes"
	"sync"

	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Buffer holds results until the control plane acknowledges them.
//
// This is ADR-001's HA claim made concrete: a probe whose control plane goes
// away keeps checking and keeps its results, and replays them when it returns.
// Nothing is freed on send — only on acknowledgement — because a batch that was
// sent and never written is a batch that must be sent again.
type Buffer struct {
	mu       sync.Mutex
	items    []*probev1.Result
	sent     int    // how many of items have been handed to the stream
	bytes    int    // approximate encoded size of items
	shed     uint64 // monotonic, reported in ProbeHealth
	maxItems int
	maxBytes int
}

// Buffer defaults. At 5,000 monitors on the 20-second floor and roughly 100
// bytes per result, 64 MB is on the order of 40 minutes of outage coverage —
// estimated, not measured (probe-plan §10).
const (
	defaultMaxItems = 250_000
	defaultMaxBytes = 64 << 20
)

// NewBuffer returns an empty buffer with the default bounds.
func NewBuffer() *Buffer {
	return &Buffer{maxItems: defaultMaxItems, maxBytes: defaultMaxBytes}
}

// approxSize is a cheap stand-in for the encoded size. Marshalling every result
// twice to bound a buffer would cost more than the bound saves.
func approxSize(r *probev1.Result) int {
	return 64 + len(r.GetResultId()) + len(r.GetMonitorId()) + len(r.GetCode()) + len(r.GetMessage())
}

// Add appends a result, shedding if the buffer is full.
//
// On overflow the oldest results whose outcome did not change are dropped first,
// so state changes survive an outage that raw heartbeats do not: losing a tick
// costs resolution, losing a transition loses the event that alerting and the
// incident timeline are built from.
func (b *Buffer) Add(r *probev1.Result) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.items = append(b.items, r)
	b.bytes += approxSize(r)

	for len(b.items) > b.maxItems || b.bytes > b.maxBytes {
		if !b.shedOneLocked() {
			return
		}
	}
}

// shedOneLocked drops the oldest sheddable result and reports whether it found
// one. When everything left is a state change, nothing is shed: at that point
// the buffer is holding only events, and dropping those would lose the outage
// rather than its resolution.
func (b *Buffer) shedOneLocked() bool {
	for i, r := range b.items {
		if r.GetOutcomeChanged() {
			continue
		}
		b.bytes -= approxSize(r)
		b.items = append(b.items[:i], b.items[i+1:]...)
		if i < b.sent {
			b.sent--
		}
		b.shed++
		return true
	}
	return false
}

// Next returns up to n unsent results and marks them sent. They stay in the
// buffer until Ack.
func (b *Buffer) Next(n int) []*probev1.Result {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.sent >= len(b.items) {
		return nil
	}
	end := min(b.sent+n, len(b.items))

	batch := make([]*probev1.Result, end-b.sent)
	copy(batch, b.items[b.sent:end])
	b.sent = end
	return batch
}

// Ack frees everything at or below the acknowledged result id, comparing ids as
// unsigned bytes — which works because result ids are UUIDv7 and generated in
// send order.
func (b *Buffer) Ack(through []byte) {
	if len(through) == 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	keep := 0
	for keep < len(b.items) && bytes.Compare(b.items[keep].GetResultId(), through) <= 0 {
		b.bytes -= approxSize(b.items[keep])
		keep++
	}
	if keep == 0 {
		return
	}
	b.items = append([]*probev1.Result(nil), b.items[keep:]...)
	b.sent = max(b.sent-keep, 0)
}

// Rewind marks everything unsent, which is what a dropped stream requires: the
// control plane may or may not have written what was in flight, and the only
// safe assumption is that it did not.
func (b *Buffer) Rewind() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = 0
}

// Stats reports depth and shed count for ProbeHealth. A probe quietly dropping
// data must be visible in the UI, and this is how it gets there.
func (b *Buffer) Stats() (items int, size int, shed uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items), b.bytes, b.shed
}
