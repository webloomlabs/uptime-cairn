// Package live is the browser-facing update channel.
//
// It is the second half of ADR-004. The first half — server-side pagination,
// filtering, and a cheap membership signal polled on an interval — is what
// stops the client ever being sent full state. This is what makes the rows it
// *is* holding update without waiting for the next poll.
//
// # The shape, and why it is this shape
//
// A subscriber names the monitor ids currently on its screen, and receives
// diffs for exactly those. Nothing else reaches it. That is the whole
// mechanism, and it is what makes the Kuma failure mode structurally
// impossible rather than merely tuned around: push volume to any client is
// bounded by that client's viewport, so there is no monitor count at which it
// falls over. Fifty rows on screen is fifty subscriptions whether the install
// has eighty monitors or eight thousand.
//
// The global summary is a separate channel, delivered to every subscriber
// regardless of scope. ADR-004 is explicit that it must not be computed by the
// client summing whatever it happens to be subscribed to, which would silently
// couple a global number to viewport state — a filtered view would show "3
// down" because three of the down monitors were on this page.
//
// # Solo mode and scaled mode
//
// This is the in-process bus, which is what solo mode's live-update path runs
// on: there is no multi-process fan-out to solve inside one binary, and
// requiring a broker for it would break "solo mode keeps zero required external
// dependencies". Scaled mode's path is NATS, with subjects shaped
// updates.{org_id}.{monitor_id}.status.
//
// The two share this package's interface exactly, which is ADR-004's open
// follow-up written down as a type: the API handler talks to Bus, and the
// frontend cannot tell which implementation is underneath. Scope changes are an
// Update call either way — a set of ids in, a set of subscriptions out — which
// is the operation NATS subject matching does natively and the reason the ADR
// chose ID-scoped subjects in the first place.
package live

import (
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Update is one monitor's change, as a subscriber sees it.
//
// Deliberately small. It is a diff rather than a monitor: the client already
// holds the row, and re-sending the configuration on every heartbeat is the
// full-state broadcast this design exists to avoid, in miniature. What changes
// on a check is the status and the numbers beside it, and that is what is here.
// It carries no JSON tags on purpose. This is a domain type, and the wire shape
// is rendered by internal/api like every other response in this project —
// model.ID is sixteen bytes and serialises as an array of numbers unless
// somebody converts it, which is the kind of thing that ships and is then
// impossible to change.
type Update struct {
	MonitorID model.ID
	Status    string
	At        time.Time

	// ResponseTimeMs is nil when nothing was measured, which is every failure
	// that never reached the target.
	ResponseTimeMs *float64

	// Important marks a transition. The client uses it to decide whether to
	// draw attention to a row rather than merely refresh it; it is the same
	// flag the stored heartbeat carries, so the two cannot disagree.
	Important bool

	// Message is only populated on failures and transitions, mirroring the
	// heartbeat rule. An up-to-up check carries nothing to say.
	Message string

	// StateVersion is the monitor's version after this change, so a client can
	// discard an update it has already applied — which happens on the seam
	// between a page refetch and the stream that was running during it.
	StateVersion int64
}

// Summary is the global header count, pushed to every subscriber.
//
// Computed server-side and independent of any subscription, per ADR-004: a
// filtered view showing "3 down" because three of the down monitors happened to
// be on this page is the specific bug this separation prevents.
type Summary struct {
	Counts map[string]int
	At     time.Time
}

// Message is what travels down one subscription. Exactly one field is set.
type Message struct {
	Update  *Update
	Summary *Summary
}

// Bus is the contract the API handler is written against, and the contract a
// NATS-backed implementation has to satisfy unchanged.
type Bus interface {
	// Subscribe opens a subscription with an initial scope. The returned
	// channel is closed when Close is called on the subscription.
	Subscribe(monitorIDs []model.ID) *Subscription

	// Publish delivers a monitor's change to the subscribers holding it.
	Publish(u Update)

	// PublishSummary delivers the global counts to every subscriber.
	PublishSummary(s Summary)
}

// bufferDepth is how many messages a slow subscriber may fall behind before it
// starts losing them.
//
// It is small on purpose, and dropping is the correct behaviour rather than a
// concession. A browser that cannot keep up with its own viewport's updates is
// a browser that has been backgrounded or throttled, and the right thing to
// send it when it comes back is the current state — which the membership poll
// and a page refetch already deliver. Queueing instead would make one stalled
// tab hold memory proportional to how long it stalled, and blocking would put
// a browser's scheduling on the heartbeat ingest path, which is the one thing
// ADR-005 says must never happen.
//
// Sixty-four is roughly three seconds of updates for a fifty-row viewport at
// the twenty-second interval floor, which is far longer than any scheduling
// hiccup and far shorter than a backgrounded tab.
const bufferDepth = 64

// Subscription is one open client channel.
type Subscription struct {
	// C carries the messages. Closed by Close.
	C chan Message

	bus    *bus
	mu     sync.RWMutex
	scope  map[model.ID]struct{}
	closed bool

	// dropped counts messages this subscriber was too slow to take. Exposed so
	// the handler can log it: a subscriber dropping updates is invisible from
	// the browser, which sees a row that simply stopped moving.
	dropped uint64
}

// Update replaces the subscription's scope in place.
//
// In place, rather than by closing and reopening the stream, is the property
// that makes this usable: paginating a list is the most ordinary thing a user
// does, and a channel that has to be torn down and re-established on every page
// turn would spend its life reconnecting. It is also exactly what an ID-scoped
// subject model gives for free — subscribe to the new ids, drop the old — which
// is why ADR-004 chose one.
func (s *Subscription) Update(monitorIDs []model.ID) {
	next := make(map[model.ID]struct{}, len(monitorIDs))
	for _, id := range monitorIDs {
		next[id] = struct{}{}
	}

	s.mu.Lock()
	s.scope = next
	s.mu.Unlock()
}

// Dropped reports how many messages this subscriber has missed.
func (s *Subscription) Dropped() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dropped
}

// Close releases the subscription. Safe to call twice, because a handler
// unwinding through a defer and an explicit close on the error path is the
// normal way this ends.
func (s *Subscription) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.bus.remove(s)
	close(s.C)
}

// wants reports whether this subscription is holding a monitor.
func (s *Subscription) wants(id model.ID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.scope[id]
	return ok
}

// deliver hands a message over without blocking. See bufferDepth.
func (s *Subscription) deliver(m Message) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	select {
	case s.C <- m:
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
}

// bus is the in-process implementation.
type bus struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewBus returns an in-process bus with no subscribers.
func NewBus() Bus { return &bus{subs: make(map[*Subscription]struct{})} }

func (b *bus) Subscribe(monitorIDs []model.ID) *Subscription {
	s := &Subscription{
		C:     make(chan Message, bufferDepth),
		bus:   b,
		scope: make(map[model.ID]struct{}, len(monitorIDs)),
	}
	for _, id := range monitorIDs {
		s.scope[id] = struct{}{}
	}

	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *bus) remove(s *Subscription) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

// Publish walks the subscribers rather than an index from monitor id to
// subscriber.
//
// An index would be the obvious optimisation and is the wrong trade here: it
// has to be rebuilt on every scope change, and scope changes happen on every
// page turn while publishes happen on every check. What bounds this loop is the
// number of connected browsers — tens, in the install this product is sized
// for — not the monitor count, which is the dimension ADR-004 requires to stay
// flat. Five thousand monitors with three tabs open is three map lookups per
// heartbeat either way.
func (b *bus) Publish(u Update) {
	b.mu.RLock()
	targets := make([]*Subscription, 0, len(b.subs))
	for s := range b.subs {
		if s.wants(u.MonitorID) {
			targets = append(targets, s)
		}
	}
	b.mu.RUnlock()

	message := Message{Update: &u}
	for _, s := range targets {
		s.deliver(message)
	}
}

func (b *bus) PublishSummary(sum Summary) {
	b.mu.RLock()
	targets := make([]*Subscription, 0, len(b.subs))
	for s := range b.subs {
		targets = append(targets, s)
	}
	b.mu.RUnlock()

	message := Message{Summary: &sum}
	for _, s := range targets {
		s.deliver(message)
	}
}

// Subscribers reports how many channels are open. Read by /metrics, because a
// cost that scales with connected clients is a cost somebody has to be able to
// see — it is the dimension the 5,000-monitor gate does not exercise at all.
func (b *bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Counter is the optional half of Bus, for the metrics endpoint. Separate from
// Bus so an implementation that cannot cheaply count — a NATS-backed one, where
// subscriptions live in the broker — is not forced to lie about it.
type Counter interface{ Subscribers() int }
