package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// ingest turns a batch of probe results into heartbeats and state transitions.
//
// This is the half of the system the probe deliberately does not do: it counts
// consecutive failures, decides up from down from pending, and marks the
// heartbeat that changed things. The probe cannot, because every one of those
// needs knowledge of a check other than the one it just ran
// (ADR-005 decision 1).
func (s *Server) ingest(ctx context.Context, results []*probev1.Result) (*probev1.ResultAck, error) {
	if len(results) == 0 {
		return &probev1.ResultAck{}, nil
	}

	// Results are sent in time order, but a batch that was resent after a
	// reconnect can interleave with newer ones. Sorting is cheap and makes the
	// transition sequence deterministic regardless.
	ordered := make([]*probev1.Result, len(results))
	copy(ordered, results)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].GetTimeUnixMicros() < ordered[j].GetTimeUnixMicros()
	})

	var (
		beats    []model.Heartbeat
		pending  []pendingAlert
		rejected uint32
		states   = make(map[model.ID]*model.MonitorState)
		monitors = make(map[model.ID]model.Monitor)
	)

	for _, r := range ordered {
		outcome := r.GetOutcome()
		if outcome == probev1.Outcome_OUTCOME_UNSPECIFIED {
			// A probe emitting this is a bug. Rejected results are dropped, never
			// retried: resending something already refused is an infinite loop.
			rejected++
			continue
		}

		var id model.ID
		copy(id[:], r.GetMonitorId())

		monitor, ok := monitors[id]
		if !ok {
			loaded, err := s.store.LoadMonitor(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				// Deleted between assignment and result. Unattributable, so it is
				// rejected rather than written against a monitor that no longer
				// exists.
				rejected++
				continue
			} else if err != nil {
				return nil, fmt.Errorf("load monitor %s: %w", id, err)
			}
			monitors[id] = loaded
			monitor = loaded
		}

		state, ok := states[id]
		if !ok {
			loaded, err := s.store.GetState(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("load state %s: %w", id, err)
			}
			state = &loaded
			states[id] = state
		}

		beat, raised, err := apply(monitor, state, r, s.probeID)
		if err != nil {
			return nil, err
		}
		beats = append(beats, beat)
		if raised.fire {
			// Collected rather than published here: an event must not be sent
			// for a heartbeat that then fails to persist. Publishing happens
			// after the write below returns.
			pending = append(pending, pendingAlert{monitor: monitor, beat: beat, alert: raised})
		}
	}

	if err := s.store.WriteBatch(ctx, beats); err != nil {
		return nil, fmt.Errorf("write heartbeats: %w", err)
	}
	for _, state := range states {
		if err := s.store.SaveState(ctx, *state); err != nil {
			return nil, fmt.Errorf("save state %s: %w", state.MonitorID, err)
		}
	}

	// Only now, with the heartbeats and the states durable. An alert about a
	// transition that was not recorded would be an alert nobody can corroborate
	// afterwards, and the delivery log would disagree with the history.
	s.raise(pending)

	// The high-water mark: every result at or below this id is durable. Results
	// are ordered by result_id within a session, so the last one sent is the
	// mark — and it is only sent after the write above returned.
	last := results[len(results)-1].GetResultId()

	return &probev1.ResultAck{
		AcknowledgedThroughResultId: last,
		Accepted:                    uint32(len(beats)),
		Rejected:                    rejected,
	}, nil
}

// alert is what one result should tell the world, decided by the same pass that
// decides the state transition. Kept next to that decision rather than derived
// afterwards: "did this change" and "should this notify" answer to the same two
// variables, and computing them apart is how the two drift.
type alert struct {
	fire      bool
	eventType string
	previous  string
}

type pendingAlert struct {
	monitor model.Monitor
	beat    model.Heartbeat
	alert   alert
}

// apply records one result against a monitor's state and returns the heartbeat
// to store. It mutates state in place; the caller persists it once per batch
// rather than once per result.
func apply(monitor model.Monitor, state *model.MonitorState, r *probev1.Result, probeID model.ID) (model.Heartbeat, alert, error) {
	at := time.UnixMicro(r.GetTimeUnixMicros()).UTC()

	// The probe id comes from the session, not from the result: the connection
	// already says which probe this is, and repeating it on every row would be
	// 16 bytes per heartbeat restating what the control plane knows.
	beat := model.Heartbeat{
		Time:      at,
		MonitorID: state.MonitorID,
		OrgID:     state.OrgID,
		ProbeID:   probeID,
		Code:      r.GetCode(),
		Message:   r.GetMessage(),
		Attempt:   int(r.GetAttempt()),
	}

	switch r.GetOutcome() {
	case probev1.Outcome_OUTCOME_UP:
		beat.Status = model.StatusUp
	case probev1.Outcome_OUTCOME_DOWN:
		beat.Status = model.StatusDown
	case probev1.Outcome_OUTCOME_UNKNOWN:
		beat.Status = model.StatusUnknown
	case probev1.Outcome_OUTCOME_SKIPPED:
		beat.Status = model.StatusSkipped
	default:
		return model.Heartbeat{}, alert{}, fmt.Errorf("unhandled outcome %s", r.GetOutcome())
	}

	if ms := r.GetResponseTimeMs(); ms > 0 {
		d := time.Duration(ms * float64(time.Millisecond))
		beat.ResponseTime = &d
	}

	previous := state.Status
	switch beat.Status {
	case model.StatusUp:
		state.Status = model.MonitorStatusUp
		state.ConsecutiveFailures = 0

	case model.StatusDown:
		state.ConsecutiveFailures++
		// retries is the number of consecutive failures tolerated before the
		// verdict. Zero means the first failure counts; while below the
		// threshold the monitor is pending, which is "no verdict yet" rather
		// than "fine".
		if state.ConsecutiveFailures > monitor.Retries {
			state.Status = model.MonitorStatusDown
		} else {
			state.Status = model.MonitorStatusPending
		}

	default:
		// unknown and skipped say something about the probe, not the target, so
		// they leave the verdict and the failure count exactly where they were.
		// This is the whole reason those two outcomes exist: a probe whose egress
		// dies must not report every monitor assigned to it as failing.
	}

	raised := alert{previous: previous}
	if state.Status != previous {
		beat.Important = true
		state.LastStatusChangeAt = &at

		switch state.Status {
		case model.MonitorStatusDown:
			raised.fire, raised.eventType = true, model.EventMonitorDown
		case model.MonitorStatusUp:
			// A monitor that was pending and came up never went down, so there
			// is nothing to recover from and nothing to say. Alerting on it
			// would mean every new monitor announces itself.
			if previous == model.MonitorStatusDown && monitor.NotifyOnRecovery {
				raised.fire, raised.eventType = true, model.EventMonitorUp
			}
		case model.MonitorStatusPending:
			raised.fire, raised.eventType = true, model.EventMonitorPending
		}
	} else if shouldResend(monitor, state) {
		// Still down, and far enough past the last notification to say so again.
		raised.fire, raised.eventType = true, model.EventMonitorDown
	}

	state.LastCheckAt = &at
	next := at.Add(monitor.Interval)
	state.NextCheckAt = &next
	state.LastMessage = beat.Message
	if beat.ResponseTime != nil {
		ms := float64(beat.ResponseTime.Microseconds()) / 1000.0
		state.LastResponseTimeMs = &ms
	}

	return beat, raised, nil
}

// shouldResend implements resend_after: re-notify after this many consecutive
// failed checks while still down, zero disabling it entirely.
//
// Counted from the failure that produced the verdict rather than from the first
// one, so resend_after is a period of continued downtime and not "retries plus
// resend_after". Derived from consecutive_failures, which the schema already
// records — a last_notified_at column would be a second source of truth for
// something the first one already answers.
func shouldResend(monitor model.Monitor, state *model.MonitorState) bool {
	if monitor.ResendAfter <= 0 || state.Status != model.MonitorStatusDown {
		return false
	}
	since := state.ConsecutiveFailures - (monitor.Retries + 1)
	return since > 0 && since%monitor.ResendAfter == 0
}
