package maintenance

import (
	"context"
	"log/slog"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Store is what the sweep needs from persistence, named by the consumer so this
// package never learns which backend is underneath (ADR-002).
type Store interface {
	// DueMaintenanceWindows returns everything whose schedule needs
	// re-evaluating: never evaluated, or its next occurrence has arrived.
	DueMaintenanceWindows(ctx context.Context, now time.Time) ([]model.MaintenanceWindow, error)

	// SetNextOccurrence materialises the evaluated schedule.
	SetNextOccurrence(ctx context.Context, id model.ID, next *time.Time) error

	// MonitorsUnderWindows resolves target sets into monitors.
	MonitorsUnderWindows(ctx context.Context, windowIDs []model.ID) ([]model.ID, error)

	// ApplyMaintenance moves monitors in and out, and touches only the
	// maintenance reason.
	ApplyMaintenance(ctx context.Context, under []model.ID) (entered, exited int64, err error)
}

// Sweeper evaluates schedules and keeps monitor_state in step with them.
//
// It is the only writer of the 'maintenance' suppression reason. Ingest owns
// 'dependency' and null, and the two never write each other's values — which is
// what lets a sweep and a result batch land in either order without one undoing
// the other.
type Sweeper struct {
	store Store
	log   *slog.Logger
}

// NewSweeper returns a sweeper bound to one store.
func NewSweeper(store Store, log *slog.Logger) *Sweeper {
	return &Sweeper{store: store, log: log}
}

// Result is what one sweep did, for the caller's logging.
type Result struct {
	Active   int
	Entered  int64
	Exited   int64
	Rejected int
}

// Sweep evaluates every due window and applies the result.
//
// The set of currently active windows is computed from the due set rather than
// from a second query, and that is sound rather than a shortcut: an active
// occurrence started in the past, so its next_occurrence_at is at or before now
// and it is due by construction. A window that has just ended is due for the
// same reason, which is what makes this pass able to notice the end.
func (s *Sweeper) Sweep(ctx context.Context, now time.Time) (Result, error) {
	windows, err := s.store.DueMaintenanceWindows(ctx, now)
	if err != nil {
		return Result{}, err
	}

	var (
		result Result
		active []model.ID
	)

	for _, w := range windows {
		occurrence, ok, err := Next(w, now)
		if err != nil {
			// Validation refuses these at the API, so a window that cannot be
			// evaluated here means something wrote around it. Left in the due
			// set on purpose: a schedule nobody can read should keep saying so
			// rather than quietly dropping out of the sweep.
			result.Rejected++
			s.log.Warn("maintenance window has an unusable schedule",
				"window", w.ID.String(), "title", w.Title, "error", err)
			continue
		}

		var next *time.Time
		if ok {
			start := occurrence.Start
			next = &start
		}
		if err := s.store.SetNextOccurrence(ctx, w.ID, next); err != nil {
			return result, err
		}

		if !ok || !occurrence.Covers(now) {
			continue
		}
		result.Active++

		// A window with suppress_notifications off is an annotation, not a
		// suppression. It still appears on a status page and still has a
		// schedule; what it must not do is rewrite the monitor's history as
		// maintenance, because that would exclude the period from uptime while
		// still paging — the worst of both answers.
		if w.SuppressNotifications {
			active = append(active, w.ID)
		}
	}

	under, err := s.store.MonitorsUnderWindows(ctx, active)
	if err != nil {
		return result, err
	}

	entered, exited, err := s.store.ApplyMaintenance(ctx, under)
	if err != nil {
		return result, err
	}
	result.Entered, result.Exited = entered, exited
	return result, nil
}

// Trigger wakes the sweep out of band.
//
// A window created to start "now" has to suppress the check that is about to
// run, not the one after it — waiting out a tick would let the first check of a
// planned outage page somebody. Depth one on purpose: the sweep recomputes
// everything from the store, so a second pending signal would tell it nothing
// the first one does not.
type Trigger struct{ signal chan struct{} }

// NewTrigger returns a trigger nobody is waiting on yet.
func NewTrigger() *Trigger { return &Trigger{signal: make(chan struct{}, 1)} }

// Notify asks for a sweep. Never blocks.
func (t *Trigger) Notify() {
	select {
	case t.signal <- struct{}{}:
	default:
	}
}

// C is the channel a sweep loop selects on.
func (t *Trigger) C() <-chan struct{} { return t.signal }
