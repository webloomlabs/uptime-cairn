package sqlite

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

func testWindow(title string, targets model.MaintenanceTargets) model.MaintenanceWindow {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return model.MaintenanceWindow{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Title: title, Strategy: model.StrategyRecurringWeekly, Timezone: "Europe/London",
		StartsAt: now, Duration: 2 * time.Hour,
		Recurrence:            model.Recurrence{Weekdays: []int{0}},
		SuppressNotifications: true, ShowOnStatusPages: true,
		Targets:   targets,
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestMaintenanceWindowRoundTrip(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("api")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	until := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Millisecond)
	want := testWindow("Sunday patching", model.MaintenanceTargets{MonitorIDs: []model.ID{monitor.ID}})
	want.Description = "Kernel updates"
	want.Recurrence.Until = &until

	if err := s.CreateMaintenanceWindow(t.Context(), want, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetMaintenanceWindow(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	switch {
	case got.Title != want.Title:
		t.Errorf("title = %q", got.Title)
	case got.Description != want.Description:
		t.Errorf("description = %q", got.Description)
	case got.Timezone != "Europe/London":
		t.Errorf("timezone = %q — an offset would have lost the DST rule", got.Timezone)
	case got.Duration != 2*time.Hour:
		t.Errorf("duration = %s", got.Duration)
	case len(got.Recurrence.Weekdays) != 1 || got.Recurrence.Weekdays[0] != 0:
		t.Errorf("weekdays = %v", got.Recurrence.Weekdays)
	case got.Recurrence.Until == nil || !got.Recurrence.Until.Equal(until):
		t.Errorf("until = %v", got.Recurrence.Until)
	case len(got.Targets.MonitorIDs) != 1 || got.Targets.MonitorIDs[0] != monitor.ID:
		t.Errorf("targets = %+v", got.Targets)
	}
}

// A target set is replaced, not merged: "these two monitors" has to be able to
// mean exactly two.
func TestUpdateReplacesTheWholeTargetSet(t *testing.T) {
	t.Parallel()

	s := open(t)
	first := testMonitor("first")
	second := testMonitor("second")
	for _, m := range []model.Monitor{first, second} {
		if err := s.CreateMonitor(t.Context(), m); err != nil {
			t.Fatalf("create monitor: %v", err)
		}
	}

	w := testWindow("Patching", model.MaintenanceTargets{MonitorIDs: []model.ID{first.ID, second.ID}})
	if err := s.CreateMaintenanceWindow(t.Context(), w, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	w.Targets = model.MaintenanceTargets{MonitorIDs: []model.ID{second.ID}}
	if err := s.UpdateMaintenanceWindow(t.Context(), w, nil); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _ := s.GetMaintenanceWindow(t.Context(), w.ID)
	if len(got.Targets.MonitorIDs) != 1 || got.Targets.MonitorIDs[0] != second.ID {
		t.Errorf("targets = %+v, want just the second monitor", got.Targets)
	}
}

// Targeting by tag is what makes a window survive monitors being added later.
// Resolving through a snapshot of ids would not.
func TestTargetsResolveThroughTagsAndGroups(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	tagID, groupID := model.NewID(), model.NewID()
	if _, err := s.db.ExecContext(t.Context(),
		`INSERT INTO tags (id, org_id, name, slug, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		tagID[:], model.SentinelOrgID[:], "production", "production", millis(now), millis(now)); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(),
		`INSERT INTO groups (id, org_id, name, created_at, updated_at) VALUES (?,?,?,?,?)`,
		groupID[:], model.SentinelOrgID[:], "edge", millis(now), millis(now)); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	direct := testMonitor("direct")
	tagged := testMonitor("tagged")
	grouped := testMonitor("grouped")
	grouped.GroupID = &groupID
	unrelated := testMonitor("unrelated")
	disabled := testMonitor("disabled")
	disabled.GroupID = &groupID
	disabled.Enabled = false

	for _, m := range []model.Monitor{direct, tagged, grouped, unrelated, disabled} {
		if err := s.CreateMonitor(t.Context(), m); err != nil {
			t.Fatalf("create monitor: %v", err)
		}
	}
	if _, err := s.db.ExecContext(t.Context(),
		`INSERT INTO monitor_tags (monitor_id, tag_id, org_id) VALUES (?,?,?)`,
		tagged.ID[:], tagID[:], model.SentinelOrgID[:]); err != nil {
		t.Fatalf("tag monitor: %v", err)
	}

	w := testWindow("Everything edge", model.MaintenanceTargets{
		MonitorIDs: []model.ID{direct.ID},
		GroupIDs:   []model.ID{groupID},
		TagIDs:     []model.ID{tagID},
	})
	if err := s.CreateMaintenanceWindow(t.Context(), w, nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	under, err := s.MonitorsUnderWindows(t.Context(), []model.ID{w.ID})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	covered := map[model.ID]bool{}
	for _, id := range under {
		covered[id] = true
	}
	for name, m := range map[string]model.Monitor{"direct": direct, "tagged": tagged, "grouped": grouped} {
		if !covered[m.ID] {
			t.Errorf("%s monitor is not covered", name)
		}
	}
	if covered[unrelated.ID] {
		t.Error("an unrelated monitor was swept in")
	}
	// A paused monitor is not being checked, so calling it under maintenance
	// replaces one true statement with a less true one.
	if covered[disabled.ID] {
		t.Error("a disabled monitor was put under maintenance")
	}
}

func TestApplyMaintenanceEntersAndLeaves(t *testing.T) {
	t.Parallel()

	s := open(t)
	inside := testMonitor("inside")
	outside := testMonitor("outside")
	for _, m := range []model.Monitor{inside, outside} {
		if err := s.CreateMonitor(t.Context(), m); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	entered, exited, err := s.ApplyMaintenance(t.Context(), []model.ID{inside.ID})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if entered != 1 || exited != 0 {
		t.Fatalf("entered=%d exited=%d", entered, exited)
	}

	state, _ := s.GetState(t.Context(), inside.ID)
	if state.Status != model.MonitorStatusMaintenance || state.SuppressedBy != model.SuppressedByMaintenance {
		t.Errorf("state = %s / %q", state.Status, state.SuppressedBy)
	}
	untouched, _ := s.GetState(t.Context(), outside.ID)
	if untouched.SuppressedBy != "" {
		t.Errorf("an untargeted monitor was flagged: %q", untouched.SuppressedBy)
	}

	// Idempotent: a second pass with the same set moves nothing.
	entered, exited, err = s.ApplyMaintenance(t.Context(), []model.ID{inside.ID})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if entered != 0 || exited != 0 {
		t.Errorf("the second pass moved entered=%d exited=%d", entered, exited)
	}

	// And the window ends.
	_, exited, err = s.ApplyMaintenance(t.Context(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if exited != 1 {
		t.Errorf("exited = %d", exited)
	}
	state, _ = s.GetState(t.Context(), inside.ID)
	if state.Status != model.MonitorStatusPending {
		t.Errorf("status = %s, want pending: the last real observation predates the window", state.Status)
	}
	if state.SuppressedBy != "" {
		t.Errorf("suppressed_by = %q", state.SuppressedBy)
	}
}

// The sweep owns 'maintenance' and ingest owns 'dependency'. Neither may write
// the other's value, which is what lets them land in either order.
func TestApplyMaintenanceLeavesDependencySuppressionAlone(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("child")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create: %v", err)
	}

	state, _ := s.GetState(t.Context(), monitor.ID)
	state.Status = model.MonitorStatusDown
	state.SuppressedBy = model.SuppressedByDependency
	if err := s.SaveState(t.Context(), state); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, _, err := s.ApplyMaintenance(t.Context(), nil); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, _ := s.GetState(t.Context(), monitor.ID)
	if got.SuppressedBy != model.SuppressedByDependency {
		t.Errorf("suppressed_by = %q, want the sweep to have left it alone", got.SuppressedBy)
	}
	if got.Status != model.MonitorStatusDown {
		t.Errorf("status = %s", got.Status)
	}
}

// A sweep landing between a result being read and written must win: a monitor
// the operator has declared under maintenance cannot be dragged back out by a
// check that was already in flight.
func TestSaveStateCannotClobberAMaintenanceFlag(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("api")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create: %v", err)
	}

	stale, _ := s.GetState(t.Context(), monitor.ID)
	if _, _, err := s.ApplyMaintenance(t.Context(), []model.ID{monitor.ID}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	stale.Status = model.MonitorStatusUp
	stale.SuppressedBy = ""
	if err := s.SaveState(t.Context(), stale); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, _ := s.GetState(t.Context(), monitor.ID)
	if got.Status != model.MonitorStatusMaintenance || got.SuppressedBy != model.SuppressedByMaintenance {
		t.Errorf("state = %s / %q, want the sweep to have won", got.Status, got.SuppressedBy)
	}
}

// next_occurrence_at is what stops the sweep evaluating every cron expression on
// every tick, so the query that reads it has to actually narrow.
func TestDueWindowsUsesTheMaterialisedOccurrence(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("api")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create: %v", err)
	}
	targets := model.MaintenanceTargets{MonitorIDs: []model.ID{monitor.ID}}

	now := time.Now().UTC().Truncate(time.Millisecond)
	fresh := testWindow("never evaluated", targets)
	soon := testWindow("due", targets)
	past := now.Add(-time.Minute)
	soon.NextOccurrenceAt = &past
	later := testWindow("months away", targets)
	future := now.Add(60 * 24 * time.Hour)
	later.NextOccurrenceAt = &future
	cancelledAt := now
	cancelled := testWindow("cancelled", targets)
	cancelled.CancelledAt = &cancelledAt

	for _, w := range []model.MaintenanceWindow{fresh, soon, later, cancelled} {
		if err := s.CreateMaintenanceWindow(t.Context(), w, nil); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	due, err := s.DueMaintenanceWindows(t.Context(), now)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	titles := map[string]bool{}
	for _, w := range due {
		titles[w.Title] = true
		if len(w.Targets.MonitorIDs) != 1 {
			t.Errorf("%s came back without its targets", w.Title)
		}
	}
	if !titles["never evaluated"] || !titles["due"] {
		t.Errorf("due = %v, want the unevaluated and the arrived", titles)
	}
	if titles["months away"] {
		t.Error("a window months out was evaluated anyway")
	}
	if titles["cancelled"] {
		t.Error("a cancelled window was evaluated")
	}
}

func TestListMaintenanceWindowsByMonitor(t *testing.T) {
	t.Parallel()

	s := open(t)
	covered := testMonitor("covered")
	other := testMonitor("other")
	for _, m := range []model.Monitor{covered, other} {
		if err := s.CreateMonitor(t.Context(), m); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	mine := testWindow("mine", model.MaintenanceTargets{MonitorIDs: []model.ID{covered.ID}})
	theirs := testWindow("theirs", model.MaintenanceTargets{MonitorIDs: []model.ID{other.ID}})
	for _, w := range []model.MaintenanceWindow{mine, theirs} {
		if err := s.CreateMaintenanceWindow(t.Context(), w, nil); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	got, _, err := s.ListMaintenanceWindows(t.Context(), nil, 50, "", &covered.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Title != "mine" {
		t.Errorf("%d windows: %v", len(got), got)
	}
}

// Deleting the schedule must not rewrite history: past uptime figures cannot
// silently change because somebody tidied up a window.
func TestDeletingAWindowLeavesItsHeartbeatsAnnotated(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("api")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create: %v", err)
	}
	w := testWindow("Patching", model.MaintenanceTargets{MonitorIDs: []model.ID{monitor.ID}})
	if err := s.CreateMaintenanceWindow(t.Context(), w, nil); err != nil {
		t.Fatalf("create window: %v", err)
	}

	beat := model.Heartbeat{
		Time: time.Now().UTC(), MonitorID: monitor.ID, OrgID: monitor.OrgID,
		ProbeID: model.EmbeddedProbeID, Status: model.StatusMaintenance, Attempt: 1,
		Suppressed: true, SuppressionReason: model.SuppressionMaintenance,
	}
	if _, err := s.WriteBatch(t.Context(), []model.Heartbeat{beat}); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := s.DeleteMaintenanceWindow(t.Context(), w.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	beats, _, err := s.ListHeartbeats(t.Context(), monitor.ID, nil, 10, false)
	if err != nil {
		t.Fatalf("list heartbeats: %v", err)
	}
	if len(beats) != 1 {
		t.Fatalf("%d heartbeats", len(beats))
	}
	if beats[0].Status != model.StatusMaintenance || !beats[0].Suppressed {
		t.Errorf("the annotation was lost: %+v", beats[0])
	}
	if beats[0].SuppressionReason != model.SuppressionMaintenance {
		t.Errorf("reason = %d", beats[0].SuppressionReason)
	}
}
