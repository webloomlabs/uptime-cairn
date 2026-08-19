package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	"github.com/webloomlabs/uptime-cairn/internal/telemetry"
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
	ack, _, err := s.ingestResults(ctx, results)
	return ack, err
}

// ingestResults is ingest with the heartbeats it wrote handed back, which the
// check-now endpoint needs: it has to answer with the result of the check it
// just ran, and the heartbeat is not a thing the caller can look up afterwards
// without racing the next scheduled check.
func (s *Server) ingestResults(ctx context.Context, results []*probev1.Result) (*probev1.ResultAck, []model.Heartbeat, error) {
	if len(results) == 0 {
		return &probev1.ResultAck{}, nil, nil
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
				return nil, nil, fmt.Errorf("load monitor %s: %w", id, err)
			}
			monitors[id] = loaded
			monitor = loaded
		}

		state, ok := states[id]
		if !ok {
			loaded, err := s.store.GetState(ctx, id)
			if err != nil {
				return nil, nil, fmt.Errorf("load state %s: %w", id, err)
			}
			state = &loaded
			states[id] = state
		}

		beat, raised, err := apply(monitor, state, r, s.probeID, s.suppression(ctx, monitor, state, states))
		if err != nil {
			return nil, nil, err
		}
		beats = append(beats, beat)
		if raised.fire {
			// Collected rather than published here: an event must not be sent
			// for a heartbeat that then fails to persist. Publishing happens
			// after the write below returns.
			pending = append(pending, pendingAlert{monitor: monitor, beat: beat, alert: raised})
		}
	}

	written, err := s.store.WriteBatch(ctx, beats)
	if err != nil {
		return nil, nil, fmt.Errorf("write heartbeats: %w", err)
	}
	// Counted after the write returns, never before: a counter that moves on
	// intent rather than on completion reports a healthy system while it is
	// losing data. Rows and results are counted separately because they differ
	// under redelivery, and the gap is the only visible sign of it.
	telemetry.Engine.HeartbeatsWritten.Add(uint64(written))
	telemetry.Engine.ResultsIngested.Add(uint64(len(beats)))
	telemetry.Engine.ResultsRejected.Add(uint64(rejected))
	for _, state := range states {
		if err := s.store.SaveState(ctx, *state); err != nil {
			return nil, nil, fmt.Errorf("save state %s: %w", state.MonitorID, err)
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
	}, beats, nil
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

// suppression decides whether this result should be silent, and why.
//
// Two sources, and they are computed differently on purpose. Maintenance is a
// property of the clock, so the sweep materialises it onto monitor_state and
// this only has to read it. A dependency is a property of another monitor's
// current state, so it is derived here, at the moment the result lands — a
// sweep would be up to one interval stale, and the case that matters most is a
// parent and its children failing within the same second.
func (s *Server) suppression(ctx context.Context, monitor model.Monitor, state *model.MonitorState, batch map[model.ID]*model.MonitorState) int {
	if state.SuppressedBy == model.SuppressedByMaintenance {
		return model.SuppressionMaintenance
	}
	if s.ancestorUnavailable(ctx, monitor, batch) {
		return model.SuppressionDependency
	}
	return model.SuppressionNone
}

// maxDependencyDepth bounds the walk up the parent chain.
//
// Creation rejects cycles, so a cycle should be impossible; the bound is here
// because "should be impossible" and "cannot happen" are different, and the
// difference is an infinite loop on the hottest path in the system.
const maxDependencyDepth = 10

// ancestorUnavailable reports whether anything this monitor depends on is
// already known to be unavailable — down, or inside a maintenance window.
//
// Maintenance counts, and the case that forces it is worth stating: taking the
// router down for a firmware upgrade is the most known problem there is, and
// being paged about the forty services behind it is exactly what the operator
// scheduled the window to avoid. The alternative is making them target every
// descendant by hand, which is a list that goes stale the first time somebody
// adds a service.
//
// Transitive on purpose, for the same reason: suppressing only the router's
// immediate children would still page thirty-nine times.
func (s *Server) ancestorUnavailable(ctx context.Context, monitor model.Monitor, batch map[model.ID]*model.MonitorState) bool {
	parentID := monitor.ParentMonitorID

	for depth := 0; parentID != nil && depth < maxDependencyDepth; depth++ {
		// The batch first: a parent that went down earlier in this same batch is
		// already down as far as its children are concerned, and reading the
		// stored row would miss it by one flush.
		state, ok := batch[*parentID]
		if !ok {
			loaded, err := s.store.GetState(ctx, *parentID)
			if err != nil {
				// A dangling parent suppresses nothing. Failing open is the
				// right direction here: the cost is an alert that should have
				// been silent, and the alternative is silence that should have
				// been an alert.
				return false
			}
			state = &loaded
		}
		switch state.Status {
		case model.MonitorStatusDown, model.MonitorStatusMaintenance:
			return true
		}

		parent, err := s.store.LoadMonitor(ctx, *parentID)
		if err != nil {
			return false
		}
		parentID = parent.ParentMonitorID
	}
	return false
}

// apply records one result against a monitor's state and returns the heartbeat
// to store. It mutates state in place; the caller persists it once per batch
// rather than once per result.
func apply(monitor model.Monitor, state *model.MonitorState, r *probev1.Result, probeID model.ID, suppressedBy int) (model.Heartbeat, alert, error) {
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

	if suppressedBy != model.SuppressionNone {
		beat.Suppressed = true
		beat.SuppressionReason = suppressedBy
	}

	// Under maintenance the check still runs and its result is still recorded —
	// the message, the code, and the response time are all kept — but the
	// verdict is recorded as maintenance rather than as up or down. That is what
	// makes /uptime's three-way maintenance choice implementable: an operator
	// can exclude planned downtime from an SLA figure only if the rows carrying
	// it are distinguishable from the rows that are not.
	//
	// The failure count is frozen rather than reset. A window says nothing about
	// the target either way, so the monitor resumes counting from where it was
	// when the window opened.
	if suppressedBy == model.SuppressionMaintenance {
		beat.Status = model.StatusMaintenance

		// Not marked important, and not because it does not matter. The move
		// into maintenance is made by the sweep at the instant the window opens,
		// when no check is running and there is no heartbeat to mark. It is
		// carried by state_version and by the window itself, both of which the
		// API can show; inventing a heartbeat for it would put a check in the
		// history that never ran.
		state.Status = model.MonitorStatusMaintenance
		state.LastCheckAt = &at
		next := at.Add(monitor.Interval)
		state.NextCheckAt = &next
		state.LastMessage = beat.Message
		if beat.ResponseTime != nil {
			ms := float64(beat.ResponseTime.Microseconds()) / 1000.0
			state.LastResponseTimeMs = &ms
		}
		// No alert either way. Planned downtime that pages somebody is not
		// planned downtime.
		return beat, alert{}, nil
	}

	// Dependency suppression is different in kind, and deliberately so. A child
	// whose parent is down really is unreachable, so the transition is recorded
	// and the status changes and the uptime figure moves. The only thing
	// withheld is the page.
	state.SuppressedBy = ""
	if suppressedBy == model.SuppressionDependency {
		state.SuppressedBy = model.SuppressedByDependency
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

	// The whole point of the feature: the transition is recorded, and nobody is
	// woken up about it. A suppressed heartbeat is still important, so the
	// activity feed and important_only still show the change — what is withheld
	// is the alert, not the history.
	if suppressedBy != model.SuppressionNone {
		raised.fire = false
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
