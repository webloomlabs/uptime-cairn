package probe

import (
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

func testResult(changed bool) *probev1.Result {
	id := model.NewID()
	return &probev1.Result{ResultId: id[:], OutcomeChanged: changed}
}

// Nothing is freed on send. A batch that was sent and never written must be sent
// again, and this is the property that makes at-least-once delivery work.
func TestBufferFreesOnlyOnAck(t *testing.T) {
	t.Parallel()

	b := NewBuffer()
	first, second := testResult(false), testResult(false)
	b.Add(first)
	b.Add(second)

	batch := b.Next(10)
	if len(batch) != 2 {
		t.Fatalf("Next returned %d results, want 2", len(batch))
	}
	if depth, _, _ := b.Stats(); depth != 2 {
		t.Errorf("depth after send = %d, want 2 — send must not free", depth)
	}

	b.Ack(first.GetResultId())
	if depth, _, _ := b.Stats(); depth != 1 {
		t.Errorf("depth after acking the first = %d, want 1", depth)
	}

	b.Ack(second.GetResultId())
	if depth, _, _ := b.Stats(); depth != 0 {
		t.Errorf("depth after acking both = %d, want 0", depth)
	}
}

// A dropped stream means the control plane may or may not have written what was
// in flight. The only safe assumption is that it did not.
func TestBufferRewindResends(t *testing.T) {
	t.Parallel()

	b := NewBuffer()
	b.Add(testResult(false))
	b.Add(testResult(false))

	_ = b.Next(10)
	if got := b.Next(10); got != nil {
		t.Fatalf("Next returned %d results before rewind, want none", len(got))
	}

	b.Rewind()
	if got := b.Next(10); len(got) != 2 {
		t.Fatalf("Next returned %d results after rewind, want 2", len(got))
	}
}

// Under pressure, ticks are shed and state changes are kept: losing a heartbeat
// costs resolution, losing a transition loses the event alerting is built from.
func TestBufferShedsTicksBeforeTransitions(t *testing.T) {
	t.Parallel()

	b := NewBuffer()
	b.maxItems = 3

	transition := testResult(true)
	b.Add(testResult(false))
	b.Add(transition)
	b.Add(testResult(false))
	b.Add(testResult(false)) // one over: the oldest tick goes

	depth, _, shed := b.Stats()
	if depth != 3 {
		t.Errorf("depth = %d, want 3", depth)
	}
	if shed != 1 {
		t.Errorf("shed = %d, want 1", shed)
	}

	for _, r := range b.Next(10) {
		if string(r.GetResultId()) == string(transition.GetResultId()) {
			return
		}
	}
	t.Error("the state change was shed; ticks should go first")
}

// When only transitions are left there is nothing safe to drop, so the buffer
// holds rather than losing the events.
func TestBufferKeepsTransitionsWhenFull(t *testing.T) {
	t.Parallel()

	b := NewBuffer()
	b.maxItems = 2

	for range 4 {
		b.Add(testResult(true))
	}

	depth, _, shed := b.Stats()
	if depth != 4 {
		t.Errorf("depth = %d, want 4 — transitions must not be shed", depth)
	}
	if shed != 0 {
		t.Errorf("shed = %d, want 0", shed)
	}
}
