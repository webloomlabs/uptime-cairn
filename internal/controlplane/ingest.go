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

		beat, err := apply(monitor, state, r, s.probeID)
		if err != nil {
			return nil, err
		}
		beats = append(beats, beat)
	}

	if err := s.store.WriteBatch(ctx, beats); err != nil {
		return nil, fmt.Errorf("write heartbeats: %w", err)
	}
	for _, state := range states {
		if err := s.store.SaveState(ctx, *state); err != nil {
			return nil, fmt.Errorf("save state %s: %w", state.MonitorID, err)
		}
	}

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

// apply records one result against a monitor's state and returns the heartbeat
// to store. It mutates state in place; the caller persists it once per batch
// rather than once per result.
func apply(monitor model.Monitor, state *model.MonitorState, r *probev1.Result, probeID model.ID) (model.Heartbeat, error) {
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
		return model.Heartbeat{}, fmt.Errorf("unhandled outcome %s", r.GetOutcome())
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

	if state.Status != previous {
		beat.Important = true
		state.LastStatusChangeAt = &at
	}

	state.LastCheckAt = &at
	next := at.Add(monitor.Interval)
	state.NextCheckAt = &next
	state.LastMessage = beat.Message
	if beat.ResponseTime != nil {
		ms := float64(beat.ResponseTime.Microseconds()) / 1000.0
		state.LastResponseTimeMs = &ms
	}

	return beat, nil
}
