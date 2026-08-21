package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/protocol"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Push monitors are the one type no probe ever runs.
//
// Everything else asks a question and records the answer; a push monitor records
// the absence of a question being answered, which only the side holding the
// clock and the last heartbeat can do. So the control plane produces the results
// itself — and produces them as ordinary probev1.Result values fed through the
// same ingest path, so consecutive failures, pending-versus-down, and the
// important flag are decided by exactly one state machine rather than two that
// have to be kept in agreement.

// pushConfig is the part of PushConfig this evaluation needs.
type pushConfig struct {
	ExpectedIntervalSeconds *int `json:"expected_interval_seconds"`
	GracePeriodSeconds      *int `json:"grace_period_seconds"`
}

const (
	defaultExpectedInterval = 60 * time.Second
	defaultGracePeriod      = 30 * time.Second
)

// PushHeartbeat records an inbound call to the push endpoint. The caller has
// already resolved the token to a monitor; this turns it into a result.
func (s *Server) PushHeartbeat(ctx context.Context, monitor model.Monitor, up bool, message string, responseTime *time.Duration) error {
	outcome := probev1.Outcome_OUTCOME_UP
	if !up {
		outcome = probev1.Outcome_OUTCOME_DOWN
	}

	result := s.newResult(monitor, outcome, message, time.Now().UTC())
	if responseTime != nil {
		result.ResponseTimeMs = float64(responseTime.Microseconds()) / 1000.0
	}

	_, err := s.ingest(ctx, []*probev1.Result{result})
	return err
}

// SweepPush marks every push monitor that has gone quiet past its deadline.
// Returns how many it moved, for the caller's logging.
//
// It is safe to call at any cadence: a monitor is only touched once its deadline
// has passed, and recording that pushes the next deadline out by another full
// interval. Sweeping every five seconds therefore does not write twelve times as
// many heartbeats as sweeping every minute — it just notices sooner.
func (s *Server) SweepPush(ctx context.Context, now time.Time) (int, error) {
	monitors, err := s.store.ListPushMonitors(ctx)
	if err != nil {
		return 0, fmt.Errorf("list push monitors: %w", err)
	}

	var overdue []*probev1.Result
	for _, m := range monitors {
		deadline, err := pushDeadline(m.Monitor, m.State)
		if err != nil {
			// A config this build cannot read is reported as unknown once per
			// deadline, not as an outage: the target may be pushing perfectly.
			overdue = append(overdue, s.newResult(m.Monitor, probev1.Outcome_OUTCOME_UNKNOWN, err.Error(), now))
			continue
		}
		if now.Before(deadline) {
			continue
		}
		overdue = append(overdue, s.newResult(m.Monitor, probev1.Outcome_OUTCOME_DOWN,
			fmt.Sprintf("no push received since %s", describeSince(m.State.LastCheckAt, m.Monitor.CreatedAt)), now))
	}

	if len(overdue) == 0 {
		return 0, nil
	}
	if _, err := s.ingest(ctx, overdue); err != nil {
		return 0, err
	}
	return len(overdue), nil
}

// pushDeadline is when silence becomes an outage: the last heartbeat plus the
// expected interval plus the grace period. A monitor that has never been pushed
// counts from its creation, so a freshly created monitor is given one full
// interval to be wired up rather than going down on the next sweep.
func pushDeadline(monitor model.Monitor, state model.MonitorState) (time.Time, error) {
	var cfg pushConfig
	if len(monitor.Config) > 0 {
		if err := json.Unmarshal(monitor.Config, &cfg); err != nil {
			return time.Time{}, fmt.Errorf("push config: %w", err)
		}
	}

	expected := defaultExpectedInterval
	if cfg.ExpectedIntervalSeconds != nil && *cfg.ExpectedIntervalSeconds > 0 {
		expected = time.Duration(*cfg.ExpectedIntervalSeconds) * time.Second
	}
	grace := defaultGracePeriod
	if cfg.GracePeriodSeconds != nil && *cfg.GracePeriodSeconds >= 0 {
		grace = time.Duration(*cfg.GracePeriodSeconds) * time.Second
	}

	from := monitor.CreatedAt
	if state.LastCheckAt != nil {
		from = *state.LastCheckAt
	}
	return from.Add(expected + grace), nil
}

// newResult builds a control-plane-authored result. The result id is fresh every
// time, so the heartbeat write's idempotency key never collides with a probe's.
func (s *Server) newResult(monitor model.Monitor, outcome probev1.Outcome, message string, at time.Time) *probev1.Result {
	id := model.NewID()
	return &probev1.Result{
		ResultId:       id[:],
		MonitorId:      monitor.ID[:],
		TimeUnixMicros: at.UnixMicro(),
		Outcome:        outcome,
		Message:        message,
		Attempt:        1,
	}
}

func describeSince(last *time.Time, created time.Time) string {
	if last == nil {
		return "creation, " + time.Since(created).Round(time.Second).String() + " ago"
	}
	return last.UTC().Format(time.RFC3339) + ", " + time.Since(*last).Round(time.Second).String() + " ago"
}

// RecordCheck ingests a check the API ran itself, and returns the heartbeat it
// produced.
//
// The API runs the checker because the control plane must not import
// probe/check — that is the ADR-001 seam, and breaking it here would tie a
// control plane serving remote probes to the checker binaries it does not run.
// What comes back through this door is an observation, in the same vocabulary a
// probe would have used, and from here on a manual check is indistinguishable
// from a scheduled one: same transition table, same consecutive-failure count,
// same alerts, and the same certificate and registration rows. A "check now"
// that took a shortcut would be testing the shortcut.
//
// The certificate is the part that used to be dropped here, and dropping it had
// a visible cost: pressing "check now" after installing a new certificate
// refreshed the verdict and left the expiry panel showing the certificate that
// had just been replaced, until the next scheduled check caught up. The
// observation is unconditional rather than rate-limited on this path — the
// hourly resend interval exists because a probe's buffer is a fixed size and
// several hundred bytes on every result cuts its outage coverage to a fifth,
// and a manual check has no buffer and happens when somebody presses a button.
func (s *Server) RecordCheck(ctx context.Context, monitor model.Monitor, c protocol.Check) (model.Heartbeat, error) {
	result := s.newResult(monitor, outcomeFor(c.Status), c.Message, time.Now().UTC())
	result.Code = c.Code
	result.Certificate = c.Certificate
	result.Domain = c.Domain
	if c.ResponseTime != nil {
		result.ResponseTimeMs = float64(c.ResponseTime.Microseconds()) / 1000.0
	}

	_, beats, err := s.ingestResults(ctx, []*probev1.Result{result})
	if err != nil {
		return model.Heartbeat{}, err
	}
	if len(beats) == 0 {
		// The only way to get here is a result ingest rejected — a monitor
		// deleted between the read and the check. Reported rather than returned
		// as a zero heartbeat, which would render as a down verdict nobody made.
		return model.Heartbeat{}, fmt.Errorf("check for monitor %s was not recorded", monitor.ID)
	}
	return beats[0], nil
}

// outcomeFor maps the stored status vocabulary onto the protocol's.
//
// The two enums genuinely differ — proto3 must start at zero with UNSPECIFIED,
// and the stored values are the schema's and cannot be renumbered — so the
// mapping is spelled out rather than cast. Casting one to the other silently
// inverts up and down (docs/probe/protocol.md §6.2).
func outcomeFor(status model.Status) probev1.Outcome {
	switch status {
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
