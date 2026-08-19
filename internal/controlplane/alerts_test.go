package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

var errWriteFailed = errors.New("disk is full")

// Which transitions raise an alert, and which deliberately do not.
//
// This is the table that decides who gets woken up. Getting it wrong in one
// direction is a missed outage; in the other it is the alert fatigue that makes
// people filter the whole channel, which is a missed outage with extra steps.

func newAlertingServer(t *testing.T, monitor model.Monitor) (*Server, *fakeStore, *recordingAlerter) {
	t.Helper()

	store := &fakeStore{
		monitor: monitor,
		state: model.MonitorState{
			MonitorID: monitor.ID, OrgID: monitor.OrgID, Status: model.MonitorStatusPending,
		},
	}
	alerts := &recordingAlerter{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, NewPublisher(), alerts, nil, log, model.EmbeddedProbeID, model.SentinelOrgID), store, alerts
}

func monitorFor(t *testing.T) model.Monitor {
	t.Helper()
	return model.Monitor{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Name: "API gateway", Type: model.TypeHTTP, Target: "https://api.example.com/health",
		Interval: time.Minute, NotifyOnRecovery: true,
	}
}

func feed(t *testing.T, server *Server, id model.ID, outcomes ...probev1.Outcome) {
	t.Helper()

	at := time.Now().UTC().Truncate(time.Millisecond)
	for i, outcome := range outcomes {
		if _, err := server.ingest(context.Background(),
			[]*probev1.Result{result(id, outcome, at.Add(time.Duration(i)*time.Minute))}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
}

func TestAlertsRaisedOnTransitions(t *testing.T) {
	t.Parallel()

	const (
		up      = probev1.Outcome_OUTCOME_UP
		down    = probev1.Outcome_OUTCOME_DOWN
		unknown = probev1.Outcome_OUTCOME_UNKNOWN
	)

	tests := []struct {
		name             string
		retries          int
		resendAfter      int
		notifyOnRecovery bool
		outcomes         []probev1.Outcome
		want             []string
	}{
		{
			name:     "a failure with no retry budget alerts once",
			outcomes: []probev1.Outcome{down},
			want:     []string{model.EventMonitorDown},
		},
		{
			name:     "continued downtime does not repeat by default",
			outcomes: []probev1.Outcome{down, down, down},
			want:     []string{model.EventMonitorDown},
		},
		{
			name:             "recovery from down alerts",
			notifyOnRecovery: true,
			outcomes:         []probev1.Outcome{down, up},
			want:             []string{model.EventMonitorDown, model.EventMonitorUp},
		},
		{
			name:     "recovery is silent when the monitor says so",
			outcomes: []probev1.Outcome{down, up},
			want:     []string{model.EventMonitorDown},
		},
		{
			// A monitor that has never reported and then comes up did not
			// recover from anything. Alerting here would mean every newly
			// created monitor announces itself.
			name:             "coming up from pending says nothing",
			notifyOnRecovery: true,
			outcomes:         []probev1.Outcome{up},
			want:             nil,
		},
		{
			// From up, because a monitor that has never reported is already
			// pending and moving to pending is not a transition.
			name:     "the pending step is its own event",
			retries:  2,
			outcomes: []probev1.Outcome{up, down, down, down},
			want:     []string{model.EventMonitorPending, model.EventMonitorDown},
		},
		{
			// The whole reason the unknown outcome exists: a probe whose egress
			// dies must not page anybody about the targets it can no longer
			// reach.
			name:     "a probe failure alerts nobody",
			outcomes: []probev1.Outcome{up, unknown, unknown},
			want:     nil,
		},
		{
			name:        "resend_after repeats while still down",
			resendAfter: 2,
			outcomes:    []probev1.Outcome{down, down, down, down, down},
			want: []string{
				model.EventMonitorDown, // the transition
				model.EventMonitorDown, // two failures later
				model.EventMonitorDown, // and two after that
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			monitor := monitorFor(t)
			monitor.Retries = tc.retries
			monitor.ResendAfter = tc.resendAfter
			monitor.NotifyOnRecovery = tc.notifyOnRecovery

			server, _, alerts := newAlertingServer(t, monitor)
			feed(t, server, monitor.ID, tc.outcomes...)

			got := alerts.types()
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

// resend_after counts from the failure that produced the verdict, not from the
// first one, so it is a period of continued downtime rather than "retries plus
// resend_after".
func TestResendCountsFromTheVerdict(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	monitor.Retries = 3
	monitor.ResendAfter = 2

	server, _, alerts := newAlertingServer(t, monitor)
	// Four failures reach the verdict; the fifth and sixth are the first resend
	// period, so the resend lands on the sixth.
	feed(t, server, monitor.ID, probev1.Outcome_OUTCOME_UP)
	feed(t, server, monitor.ID,
		probev1.Outcome_OUTCOME_DOWN, probev1.Outcome_OUTCOME_DOWN,
		probev1.Outcome_OUTCOME_DOWN, probev1.Outcome_OUTCOME_DOWN,
		probev1.Outcome_OUTCOME_DOWN)

	if got := alerts.types(); len(got) != 2 {
		t.Fatalf("events = %v, want pending then down", got)
	}
	feed(t, server, monitor.ID, probev1.Outcome_OUTCOME_DOWN)

	want := []string{model.EventMonitorPending, model.EventMonitorDown, model.EventMonitorDown}
	if got := alerts.types(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// The event has to carry enough for the alert to be worth reading: the check's
// own message, the target, and what the monitor was before.
func TestEventCarriesTheDetailAnAlertNeeds(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	server, _, alerts := newAlertingServer(t, monitor)

	at := time.Now().UTC().Truncate(time.Millisecond)
	failure := result(monitor.ID, probev1.Outcome_OUTCOME_DOWN, at)
	failure.Message = "unexpected status 503"
	failure.Code = "503"
	failure.ResponseTimeMs = 412.5

	if _, err := server.ingest(context.Background(), []*probev1.Result{failure}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	alerts.mu.Lock()
	defer alerts.mu.Unlock()
	if len(alerts.events) != 1 {
		t.Fatalf("%d events", len(alerts.events))
	}

	ev := alerts.events[0]
	switch {
	case ev.Monitor.Name != "API gateway":
		t.Errorf("name = %q", ev.Monitor.Name)
	case ev.Monitor.Target != "https://api.example.com/health":
		t.Errorf("target = %q — the most useful line in the alert", ev.Monitor.Target)
	case ev.PreviousStatus != model.MonitorStatusPending:
		t.Errorf("previous = %q", ev.PreviousStatus)
	case ev.Monitor.Status != model.MonitorStatusDown:
		t.Errorf("status = %q", ev.Monitor.Status)
	case ev.Heartbeat == nil:
		t.Fatal("no heartbeat on the event")
	case ev.Heartbeat.Message != "unexpected status 503":
		t.Errorf("message = %q", ev.Heartbeat.Message)
	case ev.Heartbeat.ResponseTimeMs == nil || *ev.Heartbeat.ResponseTimeMs != 412.5:
		t.Errorf("response time = %v", ev.Heartbeat.ResponseTimeMs)
	}
}

// An alert about a transition that was never recorded is an alert nobody can
// corroborate afterwards.
func TestNothingIsRaisedWhenTheWriteFails(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	server, store, alerts := newAlertingServer(t, monitor)
	store.writeErr = errWriteFailed

	at := time.Now().UTC()
	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(monitor.ID, probev1.Outcome_OUTCOME_DOWN, at)}); err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if got := alerts.types(); len(got) != 0 {
		t.Errorf("events = %v, want none: the heartbeat was never stored", got)
	}
}
