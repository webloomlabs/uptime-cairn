package live

import (
	"context"
	"log/slog"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Summariser is the global header count, as its own channel.
//
// ADR-004 is explicit that this must not be computed by the client summing
// whatever it happens to be subscribed to: that would couple a global number to
// viewport state, and a filtered view would show "3 down" because three of the
// down monitors were on this page. So it is computed server-side, from the same
// query /overview reads, and pushed to every subscriber regardless of scope.
//
// It wraps a Bus rather than sitting beside it so the control plane has one
// thing to publish to. Publish forwards the diff and, when the diff is a
// transition, notes that the counts may have moved.
//
// # Why it is debounced rather than recomputed per change
//
// The counts come from a GROUP BY over monitor_state. That is cheap once and
// not cheap five thousand times: the case this has to survive is the one the
// load-test harness constructs deliberately, where a burst breaks every
// monitored endpoint at once and several thousand monitors transition inside a
// single scheduler tick. Recomputing per transition would put a full scan of
// the state table on the ingest path once per monitor, at the exact moment
// ingest is under the most pressure — which is how a monitoring tool falls over
// during the outage it exists to report.
//
// So a transition marks the summary dirty and the loop publishes at most once
// per interval. The number on screen is therefore at most one interval behind,
// which is invisible against the twenty-second interval floor and is the same
// staleness bound the ADR already accepts for filtered-view membership.
type Summariser struct {
	bus    Bus
	counts func(context.Context) (map[string]int, error)
	every  time.Duration
	log    *slog.Logger

	dirty chan struct{}
}

// NewSummariser returns a summariser over bus. counts is the same read
// /overview performs; passing it in rather than taking a store keeps this
// package free of the persistence layer, which is what lets the probe-side and
// the API-side builds share it.
func NewSummariser(bus Bus, counts func(context.Context) (map[string]int, error), every time.Duration, log *slog.Logger) *Summariser {
	if every <= 0 {
		every = 2 * time.Second
	}
	return &Summariser{
		bus:    bus,
		counts: counts,
		every:  every,
		log:    log,
		// Depth one, and a non-blocking send: the signal means "recompute soon",
		// so a second one while the first is outstanding says nothing new. This
		// is the same shape as the assignment publisher's, for the same reason.
		dirty: make(chan struct{}, 1),
	}
}

// Publish forwards a monitor diff, and marks the summary dirty when the diff is
// a transition.
//
// Only on transitions. An up-to-up check cannot change any status count by
// definition, and treating every heartbeat as a reason to recount would mean
// 250 scans a second at the install size this product is built for — for a
// number that had not moved.
func (s *Summariser) Publish(u Update) {
	s.bus.Publish(u)
	if u.Important {
		s.Touch()
	}
}

// PublishSummary forwards, so a caller holding a Summariser can still push one
// directly. Used by the handler that sends the current counts to a stream the
// moment it opens.
func (s *Summariser) PublishSummary(sum Summary) { s.bus.PublishSummary(sum) }

// Subscribe forwards, so a Summariser satisfies Bus whole and can be handed to
// anything that wants one.
func (s *Summariser) Subscribe(monitorIDs []model.ID) *Subscription {
	return s.bus.Subscribe(monitorIDs)
}

// Subscribers forwards, when the wrapped bus can count. Without this the
// wrapper would hide the one metric that measures the cost dimension the
// 5,000-monitor gate does not exercise — a decorator quietly dropping an
// interface is the classic way that happens.
func (s *Summariser) Subscribers() int {
	if counter, ok := s.bus.(Counter); ok {
		return counter.Subscribers()
	}
	return 0
}

// Touch marks the counts as possibly changed. Safe from any goroutine, and
// never blocks.
func (s *Summariser) Touch() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// Run publishes the counts whenever they are dirty, at most once per interval.
// It returns when ctx is done.
func (s *Summariser) Run(ctx context.Context) {
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		select {
		case <-s.dirty:
		default:
			// Nothing transitioned since the last publish, so the number on
			// screen is still right. A tick that republishes an unchanged count
			// is traffic to every connected browser for no information.
			continue
		}

		counts, err := s.counts(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("could not recompute the live summary", "error", err)
				// Left dirty, so the next tick tries again rather than the
				// header staying stale until something else transitions.
				s.Touch()
			}
			continue
		}
		s.bus.PublishSummary(Summary{Counts: counts, At: time.Now().UTC()})
	}
}
