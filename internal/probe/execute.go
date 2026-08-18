package probe

import (
	"context"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// latenessBudget is the fraction of a monitor's interval a check may start late
// before it is shed instead. Past it, running the check would produce a result
// timestamped from the past, which is worse than no result because it looks fine.
const latenessBudget = 0.5

// schedule is the probe's main loop: wait for the earliest due check, run it,
// reschedule it.
func (s *Session) schedule(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		now := time.Now()
		t, wait, ready := s.sched.pop(now)
		if !ready {
			if wait == 0 {
				// Nothing scheduled at all: sleep until something is assigned.
				wait = time.Minute
			}
			select {
			case <-ctx.Done():
				return
			case <-s.sched.wake:
			case <-time.After(wait):
			}
			continue
		}

		a, assigned := s.assignment(t.monitorID)
		if !assigned {
			// Unassigned between scheduling and now. Nothing to run and nothing
			// to report: the control plane already knows, it made the change.
			continue
		}

		lateness := now.Sub(t.due)
		budget := time.Duration(float64(interval(a)) * latenessBudget)
		if lateness > budget {
			s.shed(a, t, lateness)
			continue
		}

		select {
		case s.workers <- struct{}{}:
			go s.run(ctx, a, t)
		default:
			// Worker pool full. Overload sheds, it never queues: an unbounded
			// queue turns overload into a memory leak and then emits results
			// timestamped from the past.
			s.shed(a, t, lateness)
		}
	}
}

// shed records a check that never started. It is a probe capacity signal, not a
// monitor failure — the invariant this whole path exists to protect is that
// probe overload must never look like target downtime.
func (s *Session) shed(a *probev1.Assignment, t task, lateness time.Duration) {
	s.skipped.Add(1)

	s.emit(a, t, check.Observation{
		Status:  model.StatusSkipped,
		Class:   check.ClassNone,
		Message: "shed under load: " + lateness.Round(time.Millisecond).String() + " past due",
	}, time.Now())

	s.reschedule(a, t, model.StatusSkipped)
}

// run executes one check attempt.
func (s *Session) run(ctx context.Context, a *probev1.Assignment, t task) {
	defer func() { <-s.workers }()

	s.started.Add(1)
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	defer s.completed.Add(1)

	checker, ok := s.registry.Lookup(a.GetType())
	if !ok {
		s.emit(a, t, check.Observation{
			Status:  model.StatusUnknown,
			Class:   check.ClassCapability,
			Message: "no checker for type " + a.GetType(),
		}, time.Now())
		s.reschedule(a, t, model.StatusUnknown)
		return
	}

	timeout := time.Duration(a.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	startedAt := time.Now()
	obs := checker.Check(runCtx, a.GetConfig())

	// upside_down inverts the verdict after assertions, and only up and down:
	// unknown and skipped are statements about the probe, and an operator asking
	// to invert a monitor did not ask to invert their probe's health.
	if a.GetUpsideDown() {
		switch obs.Status {
		case model.StatusUp:
			obs.Status = model.StatusDown
			obs.Message = "check succeeded, but the monitor is upside down"
		case model.StatusDown:
			obs.Status = model.StatusUp
			obs.Message = ""
		}
	}

	s.emit(a, t, obs, startedAt)
	s.reschedule(a, t, obs.Status)
}

// emit turns an observation into a buffered result.
func (s *Session) emit(a *probev1.Assignment, t task, obs check.Observation, at time.Time) {
	key := assignmentKey(a)
	outcome := toOutcome(obs.Status)

	s.mu.Lock()
	changed := s.lastOutcome[key] != outcome
	s.lastOutcome[key] = outcome
	s.mu.Unlock()

	id := model.NewID()
	r := &probev1.Result{
		ResultId:       id[:],
		MonitorId:      a.GetMonitorId(),
		TimeUnixMicros: at.UnixMicro(),
		Outcome:        outcome,
		Code:           obs.Code,
		Message:        obs.Message,
		Attempt:        t.attempt,
		// A shedding hint, not heartbeats.important: the control plane owns the
		// stored value, which needs state this probe does not have.
		OutcomeChanged: changed,
		ErrorClass:     toErrorClass(obs.Class),
	}
	if obs.ResponseTime != nil {
		r.ResponseTimeMs = float64(obs.ResponseTime.Microseconds()) / 1000.0
	}

	s.buf.Add(r)
}

// reschedule puts the monitor back on the heap.
//
// Retries run probe-side at retry_interval_seconds, and each attempt is its own
// result — which is what heartbeats.attempt anticipates. What a run of failures
// *means* is the control plane's call, so this only decides when to look again.
func (s *Session) reschedule(a *probev1.Assignment, t task, status model.Status) {
	key := assignmentKey(a)

	if status == model.StatusDown && t.attempt <= a.GetRetries() {
		retryIn := time.Duration(a.GetRetryIntervalSeconds()) * time.Second
		if retryIn <= 0 {
			retryIn = interval(a)
		}
		s.sched.push(task{monitorID: key, due: time.Now().Add(retryIn), attempt: t.attempt + 1})
		return
	}

	// Keep the phase rather than scheduling from now: dispersal is what stops
	// 5,000 monitors converging on the same second, and drifting off it by the
	// check duration every cycle would slowly undo that.
	next := t.due.Add(interval(a))
	if now := time.Now(); next.Before(now) {
		next = firstDue(key, interval(a), now)
	}
	s.sched.push(task{monitorID: key, due: next, attempt: 1})
}

func toOutcome(s model.Status) probev1.Outcome {
	switch s {
	case model.StatusUp:
		return probev1.Outcome_OUTCOME_UP
	case model.StatusDown:
		return probev1.Outcome_OUTCOME_DOWN
	case model.StatusSkipped:
		return probev1.Outcome_OUTCOME_SKIPPED
	default:
		return probev1.Outcome_OUTCOME_UNKNOWN
	}
}

func toErrorClass(c check.ErrorClass) probev1.ErrorClass {
	switch c {
	case check.ClassAssertion:
		return probev1.ErrorClass_ERROR_CLASS_ASSERTION
	case check.ClassTimeout:
		return probev1.ErrorClass_ERROR_CLASS_TIMEOUT
	case check.ClassDNS:
		return probev1.ErrorClass_ERROR_CLASS_DNS
	case check.ClassTLS:
		return probev1.ErrorClass_ERROR_CLASS_TLS
	case check.ClassNetwork:
		return probev1.ErrorClass_ERROR_CLASS_NETWORK
	case check.ClassConfig:
		return probev1.ErrorClass_ERROR_CLASS_CONFIG
	case check.ClassCapability:
		return probev1.ErrorClass_ERROR_CLASS_CAPABILITY
	default:
		return probev1.ErrorClass_ERROR_CLASS_UNSPECIFIED
	}
}
