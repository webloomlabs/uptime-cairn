package controlplane

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Suppression: what a maintenance window and a downed dependency each do to a
// result, and — the part that matters — how the two differ.
//
// They are not the same operation wearing different labels. A window says the
// observation is not about the target, so the heartbeat is recorded as
// maintenance and the uptime ratio never sees it. A dependency says the
// observation is real and nobody needs waking, so the heartbeat is recorded as
// down and only the page is withheld. Collapsing the two would either hide a
// real outage from the SLA figure or count planned downtime against it.

func maintenanceServer(t *testing.T, status, suppressedBy string) (*Server, *fakeStore, *recordingAlerter) {
	t.Helper()

	monitor := monitorFor(t)
	store := &fakeStore{
		monitor: monitor,
		state: model.MonitorState{
			MonitorID: monitor.ID, OrgID: monitor.OrgID,
			Status: status, SuppressedBy: suppressedBy,
		},
	}
	alerts := &recordingAlerter{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, NewPublisher(), alerts, nil, log, model.EmbeddedProbeID, model.SentinelOrgID), store, alerts
}

func TestMaintenanceRecordsTheResultWithoutCountingIt(t *testing.T) {
	t.Parallel()

	server, store, alerts := maintenanceServer(t, model.MonitorStatusMaintenance, model.SuppressedByMaintenance)

	failure := result(store.monitor.ID, probev1.Outcome_OUTCOME_DOWN, time.Now().UTC())
	failure.Message = "connection refused"
	failure.Code = "503"
	failure.ResponseTimeMs = 12.5

	if _, err := server.ingest(context.Background(), []*probev1.Result{failure}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if len(store.written) != 1 {
		t.Fatalf("%d heartbeats written", len(store.written))
	}
	beat := store.written[0]

	// Recorded as maintenance, which is what makes /uptime's three-way
	// maintenance choice implementable at all.
	if beat.Status != model.StatusMaintenance {
		t.Errorf("status = %s, want maintenance", beat.Status)
	}
	if !beat.Suppressed || beat.SuppressionReason != model.SuppressionMaintenance {
		t.Errorf("suppressed = %v reason = %d", beat.Suppressed, beat.SuppressionReason)
	}
	// The check still ran and its result is still there — that is the whole
	// difference between "we did not look" and "we looked and it was planned".
	if beat.Message != "connection refused" || beat.Code != "503" {
		t.Errorf("the observation was discarded: message=%q code=%q", beat.Message, beat.Code)
	}
	if beat.ResponseTime == nil {
		t.Error("the response time was discarded")
	}

	// Nobody is paged, and the failure count does not move: a window says
	// nothing about the target either way.
	if got := alerts.types(); len(got) != 0 {
		t.Errorf("events = %v, want none — planned downtime that pages somebody is not planned downtime", got)
	}
	if store.state.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want it frozen", store.state.ConsecutiveFailures)
	}
	if store.state.Status != model.MonitorStatusMaintenance {
		t.Errorf("status = %s", store.state.Status)
	}
}

func TestMaintenanceSurvivesASuccessfulCheckToo(t *testing.T) {
	t.Parallel()

	server, store, _ := maintenanceServer(t, model.MonitorStatusMaintenance, model.SuppressedByMaintenance)

	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(store.monitor.ID, probev1.Outcome_OUTCOME_UP, time.Now().UTC())}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if store.written[0].Status != model.StatusMaintenance {
		t.Errorf("status = %s: an up check inside a window is still inside the window", store.written[0].Status)
	}
	if store.state.Status != model.MonitorStatusMaintenance {
		t.Errorf("a successful check pulled the monitor out of maintenance: %s", store.state.Status)
	}
}

// The counting has to resume where it left off, not from zero and not from
// wherever the window's own failures took it.
func TestFailureCountResumesAfterMaintenance(t *testing.T) {
	t.Parallel()

	server, store, alerts := maintenanceServer(t, model.MonitorStatusMaintenance, model.SuppressedByMaintenance)
	store.monitor.Retries = 1
	store.state.ConsecutiveFailures = 1

	now := time.Now().UTC()
	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(store.monitor.ID, probev1.Outcome_OUTCOME_DOWN, now)}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if store.state.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive_failures = %d during the window, want 1", store.state.ConsecutiveFailures)
	}

	// The sweep ends the window.
	store.state.SuppressedBy = ""
	store.state.Status = model.MonitorStatusPending

	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(store.monitor.ID, probev1.Outcome_OUTCOME_DOWN, now.Add(time.Minute))}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if store.state.ConsecutiveFailures != 2 {
		t.Errorf("consecutive_failures = %d after the window, want it to resume at 2", store.state.ConsecutiveFailures)
	}
	if store.state.Status != model.MonitorStatusDown {
		t.Errorf("status = %s, want down once the retry budget is spent", store.state.Status)
	}
	if got := alerts.types(); len(got) != 1 || got[0] != model.EventMonitorDown {
		t.Errorf("events = %v, want one monitor.down after the window", got)
	}
}

// dependencyGraph wires child -> parent -> grandparent, so the transitive case
// is exercised rather than assumed: a router, a switch behind it, and the
// services behind that is the shape the feature exists for.
func dependencyGraph(t *testing.T, grandparentStatus, parentStatus string) (*Server, *fakeStore, *recordingAlerter, model.Monitor) {
	t.Helper()

	grandparent := monitorFor(t)
	grandparent.Name = "Core router"

	parent := monitorFor(t)
	parent.Name = "Edge switch"
	parent.ParentMonitorID = &grandparent.ID

	child := monitorFor(t)
	child.Name = "API gateway"
	child.ParentMonitorID = &parent.ID

	state := func(m model.Monitor, status string) *model.MonitorState {
		return &model.MonitorState{MonitorID: m.ID, OrgID: m.OrgID, Status: status}
	}

	store := &fakeStore{
		graph: map[model.ID]model.Monitor{
			grandparent.ID: grandparent, parent.ID: parent, child.ID: child,
		},
		graphState: map[model.ID]*model.MonitorState{
			grandparent.ID: state(grandparent, grandparentStatus),
			parent.ID:      state(parent, parentStatus),
			child.ID:       state(child, model.MonitorStatusUp),
		},
	}
	alerts := &recordingAlerter{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, NewPublisher(), alerts, nil, log, model.EmbeddedProbeID, model.SentinelOrgID),
		store, alerts, child
}

// The headline case: the router being down should page once, not forty times.
func TestChildIsSilentWhileItsParentIsDown(t *testing.T) {
	t.Parallel()

	server, store, alerts, child := dependencyGraph(t, model.MonitorStatusUp, model.MonitorStatusDown)

	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(child.ID, probev1.Outcome_OUTCOME_DOWN, time.Now().UTC())}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	beat := store.written[0]
	// The child really is unreachable, so the observation stands and the uptime
	// figure moves. Only the page is withheld.
	if beat.Status != model.StatusDown {
		t.Errorf("status = %s, want down: the service was genuinely unavailable", beat.Status)
	}
	if !beat.Suppressed || beat.SuppressionReason != model.SuppressionDependency {
		t.Errorf("suppressed = %v reason = %d", beat.Suppressed, beat.SuppressionReason)
	}
	if !beat.Important {
		t.Error("the transition was not marked important: suppression withholds the alert, not the history")
	}
	if got := alerts.types(); len(got) != 0 {
		t.Errorf("events = %v, want none", got)
	}

	state := store.graphState[child.ID]
	if state.Status != model.MonitorStatusDown {
		t.Errorf("status = %s, want down", state.Status)
	}
	if state.SuppressedBy != model.SuppressedByDependency {
		t.Errorf("suppressed_by = %q, want dependency cached for the list view", state.SuppressedBy)
	}
}

func TestSuppressionIsTransitiveUpTheChain(t *testing.T) {
	t.Parallel()

	// The grandparent is down; the parent has not noticed yet.
	server, store, alerts, child := dependencyGraph(t, model.MonitorStatusDown, model.MonitorStatusUp)

	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(child.ID, probev1.Outcome_OUTCOME_DOWN, time.Now().UTC())}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !store.written[0].Suppressed {
		t.Error("a grandparent outage did not reach the grandchild")
	}
	if got := alerts.types(); len(got) != 0 {
		t.Errorf("events = %v, want none", got)
	}
}

func TestHealthyParentSuppressesNothing(t *testing.T) {
	t.Parallel()

	server, store, alerts, child := dependencyGraph(t, model.MonitorStatusUp, model.MonitorStatusUp)

	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(child.ID, probev1.Outcome_OUTCOME_DOWN, time.Now().UTC())}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if store.written[0].Suppressed {
		t.Error("a healthy chain suppressed a real outage")
	}
	if got := alerts.types(); len(got) != 1 || got[0] != model.EventMonitorDown {
		t.Errorf("events = %v, want one monitor.down", got)
	}
	if store.graphState[child.ID].SuppressedBy != "" {
		t.Errorf("suppressed_by = %q, want it cleared", store.graphState[child.ID].SuppressedBy)
	}
}

// A parent that goes down earlier in the same batch is already down as far as
// its children are concerned; reading the stored row would miss it by one flush,
// which is precisely the burst this feature exists for.
func TestParentGoingDownInTheSameBatchSuppressesItsChild(t *testing.T) {
	t.Parallel()

	server, store, alerts, child := dependencyGraph(t, model.MonitorStatusUp, model.MonitorStatusUp)
	parentID := *store.graph[child.ID].ParentMonitorID

	now := time.Now().UTC()
	if _, err := server.ingest(context.Background(), []*probev1.Result{
		result(parentID, probev1.Outcome_OUTCOME_DOWN, now),
		result(child.ID, probev1.Outcome_OUTCOME_DOWN, now.Add(time.Second)),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	var childBeat model.Heartbeat
	for _, beat := range store.written {
		if beat.MonitorID == child.ID {
			childBeat = beat
		}
	}
	if !childBeat.Suppressed {
		t.Error("the child was paged about an outage its parent caused in the same batch")
	}
	// The parent itself is not suppressed — somebody has to be told.
	if got := alerts.types(); len(got) != 1 || got[0] != model.EventMonitorDown {
		t.Errorf("events = %v, want exactly one monitor.down — the parent's", got)
	}
}

// A monitor with no parent must not pay for the walk, and a parent that has been
// deleted must fail open: the cost of a stray alert is far below the cost of
// silence.
func TestMissingParentDoesNotSuppress(t *testing.T) {
	t.Parallel()

	child := monitorFor(t)
	ghost := model.NewID()
	child.ParentMonitorID = &ghost

	store := &fakeStore{
		graph:      map[model.ID]model.Monitor{child.ID: child},
		graphState: map[model.ID]*model.MonitorState{child.ID: {MonitorID: child.ID, OrgID: child.OrgID, Status: model.MonitorStatusUp}},
	}
	alerts := &recordingAlerter{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(store, NewPublisher(), alerts, nil, log, model.EmbeddedProbeID, model.SentinelOrgID)

	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(child.ID, probev1.Outcome_OUTCOME_DOWN, time.Now().UTC())}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if store.written[0].Suppressed {
		t.Error("a dangling parent suppressed an alert")
	}
	if got := alerts.types(); len(got) != 1 {
		t.Errorf("events = %v, want the outage to be reported", got)
	}
}

// Maintenance wins when both apply. It is the more specific statement: "we
// planned this" rather than "something upstream broke".
func TestMaintenanceTakesPrecedenceOverDependency(t *testing.T) {
	t.Parallel()

	server, store, _, child := dependencyGraph(t, model.MonitorStatusUp, model.MonitorStatusDown)
	store.graphState[child.ID].SuppressedBy = model.SuppressedByMaintenance
	store.graphState[child.ID].Status = model.MonitorStatusMaintenance

	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(child.ID, probev1.Outcome_OUTCOME_DOWN, time.Now().UTC())}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	beat := store.written[0]
	if beat.SuppressionReason != model.SuppressionMaintenance {
		t.Errorf("reason = %d, want maintenance", beat.SuppressionReason)
	}
	if beat.Status != model.StatusMaintenance {
		t.Errorf("status = %s, want maintenance", beat.Status)
	}
}

// Taking the router down for a firmware upgrade is the most known problem there
// is. Being paged about the forty services behind it is exactly what the window
// was scheduled to avoid, so a parent under maintenance suppresses its children
// as surely as a parent that is down.
func TestParentUnderMaintenanceSuppressesItsChildren(t *testing.T) {
	t.Parallel()

	server, store, alerts, child := dependencyGraph(t, model.MonitorStatusUp, model.MonitorStatusMaintenance)

	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{result(child.ID, probev1.Outcome_OUTCOME_DOWN, time.Now().UTC())}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	beat := store.written[0]
	if !beat.Suppressed {
		t.Error("a child paged while its parent was under maintenance")
	}
	// The child is not itself in a window, so the reason is the dependency, not
	// the maintenance. It stays a real down in its own history.
	if beat.SuppressionReason != model.SuppressionDependency {
		t.Errorf("reason = %d, want dependency", beat.SuppressionReason)
	}
	if beat.Status != model.StatusDown {
		t.Errorf("status = %s, want down: the child is genuinely unavailable", beat.Status)
	}
	if got := alerts.types(); len(got) != 0 {
		t.Errorf("events = %v, want none", got)
	}
}
