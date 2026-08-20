package probe

import (
	"context"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/protocol"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// watchOnce opens the assignment stream and applies what arrives, returning when
// it drops so the caller can reconnect.
func (s *Session) watchOnce(ctx context.Context) error {
	// Always resume from empty in this build. The probe holds assignments in
	// memory only (ADR-005 decision 9), and after a reconnect the cheapest
	// correct thing is a full set: a resumption token would claim knowledge the
	// process may have lost.
	stream, err := s.client.WatchAssignments(ctx, &probev1.WatchAssignmentsRequest{
		KnownSetVersion: "",
	})
	if err != nil {
		return fmt.Errorf("open assignment stream: %w", err)
	}

	var staging []*probev1.Assignment

	for {
		update, err := stream.Recv()
		if err != nil {
			return err
		}

		switch u := update.GetUpdate().(type) {
		case *probev1.AssignmentUpdate_Set:
			// Chunks accumulate and are applied atomically on final. A stream
			// that fails mid-set is discarded rather than half-applied: half a
			// set is a monitoring gap neither side knows about.
			staging = append(staging, u.Set.GetAssignments()...)
			if u.Set.GetFinal() {
				s.applyFullSet(staging)
				staging = nil
			}

		case *probev1.AssignmentUpdate_Delta:
			s.applyDelta(u.Delta)

		case *probev1.AssignmentUpdate_Reconcile:
			local := protocol.AssignmentDigest(s.snapshot())
			if local == u.Reconcile.GetAssignmentDigest() {
				continue
			}
			// Different: close the stream and reopen it, which forces a full
			// set. WatchAssignments is server-streaming, so reopening *is* the
			// request — there is no other way to ask.
			s.log.Warn("assignment digest mismatch, resyncing",
				"local", local, "control_plane", u.Reconcile.GetAssignmentDigest())
			return fmt.Errorf("assignment digest mismatch")
		}
	}
}

// applyFullSet replaces everything and rebuilds the schedule.
func (s *Session) applyFullSet(set []*probev1.Assignment) {
	accepted := make(map[string]*probev1.Assignment, len(set))
	var rejected []*probev1.AssignmentRejection

	for _, a := range set {
		if r := s.validate(a); r != nil {
			rejected = append(rejected, r)
			continue
		}
		accepted[assignmentKey(a)] = a
	}

	s.mu.Lock()
	previous := s.assignments
	s.assignments = accepted
	s.rejections = append(s.rejections, rejected...)
	s.mu.Unlock()

	for key := range previous {
		s.sched.drop(key)
	}
	now := time.Now()
	for key, a := range accepted {
		s.sched.push(task{monitorID: key, due: firstDue(key, interval(a), now), attempt: 1})
	}

	s.log.Info("assignment set applied", "monitors", len(accepted), "rejected", len(rejected))
}

// applyDelta adds, updates, and removes without disturbing anything else — in
// particular without resetting the schedule of monitors it does not mention.
func (s *Session) applyDelta(delta *probev1.AssignmentDelta) {
	now := time.Now()

	for _, a := range delta.GetAdded() {
		if r := s.validate(a); r != nil {
			s.addRejection(r)
			continue
		}
		key := assignmentKey(a)
		s.setAssignment(key, a)
		s.sched.push(task{monitorID: key, due: firstDue(key, interval(a), now), attempt: 1})
	}

	for _, a := range delta.GetUpdated() {
		key := assignmentKey(a)
		if r := s.validate(a); r != nil {
			s.removeAssignment(key)
			s.sched.drop(key)
			s.addRejection(r)
			continue
		}
		s.setAssignment(key, a)
		// Reschedule: the interval may have changed, and a monitor moved from
		// 12 hours to 20 seconds should not wait 12 hours to notice.
		s.sched.drop(key)
		s.sched.push(task{monitorID: key, due: firstDue(key, interval(a), now), attempt: 1})
	}

	for _, id := range delta.GetRemovedMonitorIds() {
		var monitorID model.ID
		copy(monitorID[:], id)
		key := monitorID.String()
		s.removeAssignment(key)
		s.sched.drop(key)
	}
}

// validate runs the checker's own validation at assignment time, not check time.
// A config the probe cannot honour is reported once, immediately, rather than
// discovered on every tick.
func (s *Session) validate(a *probev1.Assignment) *probev1.AssignmentRejection {
	reject := func(reason probev1.AssignmentRejection_Reason, msg string) *probev1.AssignmentRejection {
		return &probev1.AssignmentRejection{
			MonitorId:     a.GetMonitorId(),
			ConfigVersion: a.GetConfigVersion(),
			Reason:        reason,
			Message:       msg,
		}
	}

	checker, ok := s.registry.Lookup(a.GetType())
	if !ok {
		return reject(probev1.AssignmentRejection_REASON_UNSUPPORTED_TYPE,
			fmt.Sprintf("this probe has no checker for type %q", a.GetType()))
	}
	if a.GetIntervalSeconds() == 0 {
		return reject(probev1.AssignmentRejection_REASON_INVALID_CONFIG, "interval_seconds is zero")
	}
	if err := checker.Validate(a.GetConfig()); err != nil {
		return reject(probev1.AssignmentRejection_REASON_INVALID_CONFIG, err.Error())
	}
	return nil
}

func (s *Session) setAssignment(key string, a *probev1.Assignment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assignments[key] = a
}

func (s *Session) removeAssignment(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.assignments, key)
	delete(s.lastOutcome, key)
	delete(s.lastSeen, key)
}

func (s *Session) assignment(key string) (*probev1.Assignment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assignments[key]
	return a, ok
}

func (s *Session) snapshot() []*probev1.Assignment {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*probev1.Assignment, 0, len(s.assignments))
	for _, a := range s.assignments {
		out = append(out, a)
	}
	return out
}

func (s *Session) addRejection(r *probev1.AssignmentRejection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejections = append(s.rejections, r)
}

// takeRejections drains the pending rejections onto the next result batch.
func (s *Session) takeRejections() []*probev1.AssignmentRejection {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.rejections) == 0 {
		return nil
	}
	out := s.rejections
	s.rejections = nil
	return out
}

// assignmentKey is the probe-side identity of a monitor within this session.
// Phase 4 makes it (session, monitor_id); today there is one session, and the
// key is already scoped to it by construction rather than by convention.
func assignmentKey(a *probev1.Assignment) string {
	var id model.ID
	copy(id[:], a.GetMonitorId())
	return id.String()
}

func interval(a *probev1.Assignment) time.Duration {
	return time.Duration(a.GetIntervalSeconds()) * time.Second
}
