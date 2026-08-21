package live

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

func drain(t *testing.T, s *Subscription) []Message {
	t.Helper()

	var out []Message
	for {
		select {
		case m := <-s.C:
			out = append(out, m)
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
}

// The central claim of ADR-004, expressed as the smallest test that can falsify
// it: a monitor nobody is looking at produces no traffic to anybody. This is
// what makes the Kuma failure mode structurally impossible rather than tuned
// around — there is no monitor count at which an unwatched monitor starts
// costing a connected browser anything.
func TestAMonitorOffScreenReachesNobody(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	watched, unwatched := model.NewID(), model.NewID()

	s := bus.Subscribe([]model.ID{watched})
	defer s.Close()

	bus.Publish(Update{MonitorID: unwatched, Status: "down"})
	if got := drain(t, s); len(got) != 0 {
		t.Fatalf("a monitor outside the subscription produced %d messages", len(got))
	}

	bus.Publish(Update{MonitorID: watched, Status: "down"})
	got := drain(t, s)
	if len(got) != 1 || got[0].Update == nil || got[0].Update.MonitorID != watched {
		t.Fatalf("the watched monitor produced %v", got)
	}
}

// Scope changes in place. A channel that had to be torn down and re-established
// on every page turn would spend its life reconnecting, which is precisely the
// objection ADR-004 raises against the framings it rejects.
func TestScopeChangesWithoutReopeningTheStream(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	first, second := model.NewID(), model.NewID()

	s := bus.Subscribe([]model.ID{first})
	defer s.Close()

	s.Update([]model.ID{second})

	bus.Publish(Update{MonitorID: first, Status: "down"})
	if got := drain(t, s); len(got) != 0 {
		t.Errorf("the old page's monitor still reaches the stream: %v", got)
	}

	bus.Publish(Update{MonitorID: second, Status: "up"})
	if got := drain(t, s); len(got) != 1 {
		t.Errorf("the new page's monitor produced %d messages, want 1", len(got))
	}

	// The channel itself was never closed, which is the property under test.
	select {
	case _, open := <-s.C:
		if !open {
			t.Error("the stream closed on a scope change")
		}
	default:
	}
}

// The summary goes to everybody regardless of scope. ADR-004 is explicit that
// it must not be summed client-side from whatever happens to be subscribed,
// which would make a filtered view report "3 down" because three of the down
// monitors were on this page.
func TestTheSummaryIgnoresScope(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	empty := bus.Subscribe(nil)
	defer empty.Close()
	scoped := bus.Subscribe([]model.ID{model.NewID()})
	defer scoped.Close()

	bus.PublishSummary(Summary{Counts: map[string]int{"up": 12, "down": 3}})

	for name, s := range map[string]*Subscription{"empty": empty, "scoped": scoped} {
		got := drain(t, s)
		if len(got) != 1 || got[0].Summary == nil {
			t.Fatalf("%s subscription received %v, want one summary", name, got)
		}
		if got[0].Summary.Counts["down"] != 3 {
			t.Errorf("%s subscription saw %v", name, got[0].Summary.Counts)
		}
	}
}

// A browser that cannot keep up must never become backpressure on ingest. It
// loses messages instead, and the loss is counted, because from the browser's
// side a dropped update is a row that simply stopped moving.
func TestASlowSubscriberLosesMessagesRatherThanBlocking(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	id := model.NewID()
	s := bus.Subscribe([]model.ID{id})
	defer s.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < bufferDepth*4; i++ {
			bus.Publish(Update{MonitorID: id, Status: "up"})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a subscriber that was not reading; ingest would stall behind a browser")
	}

	if s.Dropped() == 0 {
		t.Error("nothing was counted as dropped, so the loss would be invisible")
	}
}

// Only transitions recount. An up-to-up check cannot move a status count, and
// treating every heartbeat as a reason to scan monitor_state would be 250 scans
// a second at the size this product is built for, for a number that had not
// changed.
func TestTheSummaryIsRecomputedOnlyOnTransitions(t *testing.T) {
	t.Parallel()

	// Atomic because the recount happens on the summariser's goroutine and is
	// read on this one; the subject of the test is the debounce, not the
	// counter.
	var recounts atomic.Int64
	counts := func(context.Context) (map[string]int, error) {
		recounts.Add(1)
		return map[string]int{"up": 1}, nil
	}

	bus := NewBus()
	sum := NewSummariser(bus, counts, 10*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go sum.Run(ctx)

	sum.Publish(Update{MonitorID: model.NewID(), Status: "up", Important: false})
	time.Sleep(60 * time.Millisecond)
	if got := recounts.Load(); got != 0 {
		t.Errorf("an unremarkable heartbeat caused %d recounts", got)
	}

	sum.Publish(Update{MonitorID: model.NewID(), Status: "down", Important: true})
	deadline := time.Now().Add(time.Second)
	for recounts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if recounts.Load() == 0 {
		t.Fatal("a transition did not cause a recount")
	}

	// And a burst collapses. This is the case the load-test harness constructs
	// on purpose: several thousand monitors transitioning inside one scheduler
	// tick must not become several thousand scans of monitor_state on the
	// ingest path.
	before := recounts.Load()
	for i := 0; i < 500; i++ {
		sum.Publish(Update{MonitorID: model.NewID(), Status: "down", Important: true})
	}
	time.Sleep(60 * time.Millisecond)
	if got := recounts.Load() - before; got > 5 {
		t.Errorf("500 transitions produced %d recounts, want the debounce to collapse them", got)
	}
}
