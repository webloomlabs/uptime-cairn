package controlplane

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// A push monitor is the one type no probe runs, so these transitions are the
// only thing standing between "nothing called in" and an alert nobody gets.

func pushServer(t *testing.T, config string, retries int, lastCheck *time.Time) (*Server, *fakeStore) {
	t.Helper()

	created := time.Now().Add(-time.Hour).UTC()
	store := &fakeStore{
		monitor: model.Monitor{
			ID:        model.NewID(),
			OrgID:     model.SentinelOrgID,
			Type:      model.TypePush,
			Config:    []byte(config),
			Interval:  60 * time.Second,
			Retries:   retries,
			Enabled:   true,
			CreatedAt: created,
		},
		state: model.MonitorState{
			Status:      model.MonitorStatusPending,
			LastCheckAt: lastCheck,
		},
	}
	store.state.MonitorID = store.monitor.ID
	store.state.OrgID = store.monitor.OrgID

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, NewPublisher(), &recordingAlerter{}, log, model.EmbeddedProbeID, model.SentinelOrgID), store
}

func TestPushDeadline(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	monitor := model.Monitor{CreatedAt: created}

	// Defaults from the spec: 60 seconds expected, 30 seconds grace.
	monitor.Config = []byte(`{}`)
	deadline, err := pushDeadline(monitor, model.MonitorState{})
	if err != nil {
		t.Fatalf("pushDeadline: %v", err)
	}
	if want := created.Add(90 * time.Second); !deadline.Equal(want) {
		t.Errorf("deadline = %s, want %s", deadline, want)
	}

	// A monitor that has been pushed counts from the last push, not from
	// creation.
	last := created.Add(10 * time.Minute)
	monitor.Config = []byte(`{"expected_interval_seconds":300,"grace_period_seconds":60}`)
	deadline, err = pushDeadline(monitor, model.MonitorState{LastCheckAt: &last})
	if err != nil {
		t.Fatalf("pushDeadline: %v", err)
	}
	if want := last.Add(360 * time.Second); !deadline.Equal(want) {
		t.Errorf("deadline = %s, want %s", deadline, want)
	}

	// Zero grace is a real choice and must not fall back to the default.
	monitor.Config = []byte(`{"expected_interval_seconds":300,"grace_period_seconds":0}`)
	deadline, _ = pushDeadline(monitor, model.MonitorState{LastCheckAt: &last})
	if want := last.Add(300 * time.Second); !deadline.Equal(want) {
		t.Errorf("deadline with zero grace = %s, want %s", deadline, want)
	}
}

func TestSweepPushMarksSilenceDown(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	server, store := pushServer(t, `{"expected_interval_seconds":60,"grace_period_seconds":30}`, 0, nil)

	// Not yet overdue: the monitor was created an hour ago but its deadline is
	// computed from creation, so wind the clock back to just inside it.
	inTime := store.monitor.CreatedAt.Add(60 * time.Second)
	moved, err := server.SweepPush(ctx, inTime)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if moved != 0 {
		t.Errorf("swept %d monitors before the deadline, want 0", moved)
	}
	if store.state.Status != model.MonitorStatusPending {
		t.Errorf("status = %s, want pending", store.state.Status)
	}

	// Past the deadline, with retries at zero, the first miss is the verdict.
	overdue := store.monitor.CreatedAt.Add(120 * time.Second)
	if moved, err = server.SweepPush(ctx, overdue); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if moved != 1 {
		t.Errorf("swept %d monitors past the deadline, want 1", moved)
	}
	if store.state.Status != model.MonitorStatusDown {
		t.Errorf("status = %s, want down", store.state.Status)
	}
	if len(store.written) != 1 || !store.written[0].Important {
		t.Error("the transition into down was not marked important, so no alert would fire")
	}

	// Recording a miss pushes the next deadline out by a full interval, so
	// sweeping again immediately writes nothing. Without this the sweep interval
	// — not the monitor's interval — would set the heartbeat rate.
	if moved, err = server.SweepPush(ctx, overdue.Add(time.Second)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if moved != 0 {
		t.Errorf("swept %d monitors immediately after recording one, want 0", moved)
	}
}

// Retries apply to push exactly as they do everywhere else: below the threshold
// the monitor is pending, which is "no verdict yet" rather than "fine".
func TestSweepPushHonoursRetries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	server, store := pushServer(t, `{"expected_interval_seconds":60,"grace_period_seconds":0}`, 1, nil)

	at := store.monitor.CreatedAt.Add(120 * time.Second)
	if _, err := server.SweepPush(ctx, at); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if store.state.Status != model.MonitorStatusPending {
		t.Errorf("status after one miss = %s, want pending", store.state.Status)
	}

	at = at.Add(120 * time.Second)
	if _, err := server.SweepPush(ctx, at); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if store.state.Status != model.MonitorStatusDown {
		t.Errorf("status after two misses = %s, want down", store.state.Status)
	}
}

func TestPushHeartbeatRecovers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	server, store := pushServer(t, `{"expected_interval_seconds":60}`, 0, nil)

	// Go down first.
	if _, err := server.SweepPush(ctx, store.monitor.CreatedAt.Add(10*time.Minute)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if store.state.Status != model.MonitorStatusDown {
		t.Fatalf("setup: status = %s, want down", store.state.Status)
	}

	rtt := 42 * time.Millisecond
	if err := server.PushHeartbeat(ctx, store.monitor, true, "cron finished", &rtt); err != nil {
		t.Fatalf("push: %v", err)
	}
	if store.state.Status != model.MonitorStatusUp {
		t.Errorf("status = %s, want up", store.state.Status)
	}
	if store.state.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0", store.state.ConsecutiveFailures)
	}

	last := store.written[len(store.written)-1]
	if !last.Important {
		t.Error("the recovery was not marked important, so no recovery notification would fire")
	}
	if last.ResponseTime == nil || *last.ResponseTime != rtt {
		t.Errorf("response time = %v, want %v", last.ResponseTime, rtt)
	}
	if last.Message != "cron finished" {
		t.Errorf("message = %q, want the caller's", last.Message)
	}
}

// A pusher reporting its own failure is taken at its word — that is the point of
// the status parameter.
func TestPushHeartbeatAcceptsSelfReportedFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	server, store := pushServer(t, `{}`, 0, nil)

	if err := server.PushHeartbeat(ctx, store.monitor, false, "backup failed", nil); err != nil {
		t.Fatalf("push: %v", err)
	}
	if store.state.Status != model.MonitorStatusDown {
		t.Errorf("status = %s, want down", store.state.Status)
	}
}

// An unreadable config is unknown, not down: the target may be pushing
// perfectly, and this build simply cannot read what it was asked to enforce.
func TestSweepPushBadConfigIsUnknown(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	server, store := pushServer(t, `{"expected_interval_seconds":"soon"}`, 0, nil)

	if _, err := server.SweepPush(ctx, time.Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if store.state.Status != model.MonitorStatusPending {
		t.Errorf("status = %s; an unreadable config must leave the verdict alone", store.state.Status)
	}
	if len(store.written) != 1 || store.written[0].Status != model.StatusUnknown {
		t.Error("no unknown heartbeat recorded for the unreadable config")
	}
}
