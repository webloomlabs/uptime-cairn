package maintenance

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// fakeStore records what the sweep decided, which is the half of the feature no
// schedule test can reach.
type fakeStore struct {
	windows  []model.MaintenanceWindow
	next     map[model.ID]*time.Time
	resolved []model.ID
	applied  []model.ID
	applies  int
}

func newFakeStore(windows ...model.MaintenanceWindow) *fakeStore {
	return &fakeStore{windows: windows, next: map[model.ID]*time.Time{}}
}

func (f *fakeStore) DueMaintenanceWindows(_ context.Context, now time.Time) ([]model.MaintenanceWindow, error) {
	var due []model.MaintenanceWindow
	for _, w := range f.windows {
		if w.CancelledAt != nil {
			continue
		}
		if w.NextOccurrenceAt == nil || !w.NextOccurrenceAt.After(now) {
			due = append(due, w)
		}
	}
	return due, nil
}

func (f *fakeStore) SetNextOccurrence(_ context.Context, id model.ID, next *time.Time) error {
	f.next[id] = next
	for i := range f.windows {
		if f.windows[i].ID == id {
			f.windows[i].NextOccurrenceAt = next
		}
	}
	return nil
}

func (f *fakeStore) MonitorsUnderWindows(_ context.Context, windowIDs []model.ID) ([]model.ID, error) {
	f.resolved = windowIDs
	// One synthetic monitor per active window, which is enough to tell "the
	// sweep resolved something" from "the sweep resolved nothing".
	out := make([]model.ID, 0, len(windowIDs))
	for range windowIDs {
		out = append(out, model.NewID())
	}
	return out, nil
}

func (f *fakeStore) ApplyMaintenance(_ context.Context, under []model.ID) (int64, int64, error) {
	f.applies++
	f.applied = under
	return int64(len(under)), 0, nil
}

func sweeper(store Store) *Sweeper {
	return NewSweeper(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func windowAt(t *testing.T, start, duration time.Duration, now time.Time) model.MaintenanceWindow {
	t.Helper()

	w := window(model.StrategyRecurringDaily)
	w.ID = model.NewID()
	w.StartsAt = now.Add(start)
	w.Duration = duration
	return w
}

func TestSweepAppliesActiveWindowsOnly(t *testing.T) {
	t.Parallel()

	now := utc(t, "2026-08-19 12:00")
	active := windowAt(t, -30*time.Minute, time.Hour, now) // started half an hour ago
	future := windowAt(t, 6*time.Hour, time.Hour, now)     // starts this evening
	store := newFakeStore(active, future)

	result, err := sweeper(store).Sweep(context.Background(), now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Active != 1 {
		t.Errorf("active = %d, want 1", result.Active)
	}
	if len(store.resolved) != 1 || store.resolved[0] != active.ID {
		t.Errorf("resolved = %v, want just the running window", store.resolved)
	}
	if len(store.applied) != 1 {
		t.Errorf("applied %d monitors", len(store.applied))
	}
}

// This is what next_occurrence_at is for: a window months out drops out of the
// due query until its time comes, instead of having its recurrence rule
// evaluated on every tick forever.
func TestSweepMaterialisesTheNextOccurrence(t *testing.T) {
	t.Parallel()

	now := utc(t, "2026-08-19 12:00")
	future := windowAt(t, 6*time.Hour, time.Hour, now)
	store := newFakeStore(future)

	if _, err := sweeper(store).Sweep(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	next := store.next[future.ID]
	if next == nil {
		t.Fatal("no occurrence was materialised")
	}
	if !next.Equal(now.Add(6 * time.Hour)) {
		t.Errorf("next = %s, want this evening", next)
	}

	// And on the next tick it is no longer due.
	due, _ := store.DueMaintenanceWindows(context.Background(), now.Add(time.Minute))
	if len(due) != 0 {
		t.Errorf("%d windows still due", len(due))
	}
}

// An active window stays due, which is what lets the following pass notice that
// it ended.
func TestSweepNoticesAWindowEnding(t *testing.T) {
	t.Parallel()

	now := utc(t, "2026-08-19 12:00")
	active := windowAt(t, -30*time.Minute, time.Hour, now)
	store := newFakeStore(active)

	if _, err := sweeper(store).Sweep(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(store.applied) != 1 {
		t.Fatalf("the window was not applied")
	}

	// Half an hour later the occurrence is over.
	after := now.Add(45 * time.Minute)
	result, err := sweeper(store).Sweep(context.Background(), after)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Active != 0 {
		t.Errorf("active = %d after the occurrence ended", result.Active)
	}
	if len(store.applied) != 0 {
		t.Errorf("applied = %v, want the set cleared", store.applied)
	}
}

// suppress_notifications off means the window is an annotation, not a
// suppression. Marking it as maintenance anyway would exclude the period from
// uptime while still paging — the worst of both answers.
func TestWindowThatDoesNotSuppressIsNotApplied(t *testing.T) {
	t.Parallel()

	now := utc(t, "2026-08-19 12:00")
	announcement := windowAt(t, -30*time.Minute, time.Hour, now)
	announcement.SuppressNotifications = false
	store := newFakeStore(announcement)

	result, err := sweeper(store).Sweep(context.Background(), now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Active != 1 {
		t.Errorf("active = %d: it is still a running window", result.Active)
	}
	if len(store.applied) != 0 {
		t.Errorf("applied = %v, want nothing suppressed", store.applied)
	}
	// Its schedule is still materialised, because the status page will want it.
	if store.next[announcement.ID] == nil {
		t.Error("the schedule was not materialised")
	}
}

// A schedule nobody can read should keep saying so rather than quietly dropping
// out of the sweep.
func TestUnusableScheduleIsReportedAndSkipped(t *testing.T) {
	t.Parallel()

	now := utc(t, "2026-08-19 12:00")
	broken := windowAt(t, -time.Hour, time.Hour, now)
	broken.Strategy = model.StrategyCron
	broken.Recurrence.Cron = "not a cron expression"
	healthy := windowAt(t, -30*time.Minute, time.Hour, now)
	store := newFakeStore(broken, healthy)

	result, err := sweeper(store).Sweep(context.Background(), now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Rejected != 1 {
		t.Errorf("rejected = %d", result.Rejected)
	}
	// The healthy one is unaffected: one bad window must not take the sweep down.
	if result.Active != 1 || len(store.applied) != 1 {
		t.Errorf("active = %d applied = %d", result.Active, len(store.applied))
	}
	// Left due on purpose, so it keeps complaining.
	if next, ok := store.next[broken.ID]; ok {
		t.Errorf("the broken window's schedule was materialised as %v", next)
	}
}

// Every sweep applies, even when nothing is active — that is how a monitor gets
// out of maintenance when its last window is deleted.
func TestSweepAlwaysApplies(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	if _, err := sweeper(store).Sweep(context.Background(), utc(t, "2026-08-19 12:00")); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if store.applies != 1 {
		t.Errorf("applies = %d, want the empty set to be applied", store.applies)
	}
}

func TestTriggerIsSingleSlotAndNeverBlocks(t *testing.T) {
	t.Parallel()

	trigger := NewTrigger()
	for i := 0; i < 100; i++ {
		trigger.Notify()
	}

	select {
	case <-trigger.C():
	default:
		t.Fatal("no signal was delivered")
	}
	select {
	case <-trigger.C():
		t.Error("a second signal was queued; one wake-up recomputes everything")
	default:
	}
}
