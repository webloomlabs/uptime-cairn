package controlplane

import (
	"strconv"
	"sync"

	"github.com/webloomlabs/uptime-cairn/internal/protocol"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Publisher is how a change to a monitor reaches the probe.
//
// The API calls Notify after any write; each open WatchAssignments stream wakes,
// recomputes, and sends a delta. Deliberately a signal rather than a payload: the
// stream reloads from the store, so a burst of edits collapses into one delta
// instead of a queue of them, and no monitor state is duplicated in a channel.
type Publisher struct {
	mu   sync.Mutex
	rev  uint64
	subs map[chan struct{}]struct{}
}

// NewPublisher returns a publisher with no subscribers.
func NewPublisher() *Publisher {
	return &Publisher{subs: make(map[chan struct{}]struct{})}
}

// Notify wakes every open assignment stream and advances the revision.
func (p *Publisher) Notify() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.rev++
	for ch := range p.subs {
		// Non-blocking: the channel is a signal with a depth of one. A subscriber
		// that has not drained it yet will already recompute from the store when
		// it wakes, so a second signal would tell it nothing new.
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Revision is the current opaque set version.
func (p *Publisher) Revision() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rev
}

// Subscribe returns a signal channel and the function that releases it.
func (p *Publisher) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	p.mu.Lock()
	p.subs[ch] = struct{}{}
	p.mu.Unlock()

	return ch, func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}
}

// setVersion renders a revision for the wire. Opaque and monotonic per session,
// compared by equality only — the probe must never parse it.
func setVersion(rev uint64) string { return strconv.FormatUint(rev, 10) }

// digest delegates to the one shared implementation, because the probe computes
// the same hash and two copies of it would silently diverge.
func digest(assignments []*probev1.Assignment) string {
	return protocol.AssignmentDigest(assignments)
}
