package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

func testGroup(name string) model.Group {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return model.Group{
		ID: model.NewID(), OrgID: model.SentinelOrgID, Name: name,
		CreatedAt: now, UpdatedAt: now,
	}
}

func testTag(name string) model.Tag {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return model.Tag{
		ID: model.NewID(), OrgID: model.SentinelOrgID, Name: name,
		Slug: model.Slugify(name), Color: model.DefaultTagColor,
		CreatedAt: now, UpdatedAt: now,
	}
}

// monitorIn creates a monitor in a group with a given status, which is what the
// group summaries are computed from.
func monitorIn(t *testing.T, s *Store, name string, groupID *model.ID, status string) model.Monitor {
	t.Helper()

	m := testMonitor(name)
	m.GroupID = groupID
	if err := s.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if status != "" {
		state, err := s.GetState(t.Context(), m.ID)
		if err != nil {
			t.Fatalf("get state: %v", err)
		}
		state.Status = status
		if err := s.SaveState(t.Context(), state); err != nil {
			t.Fatalf("save state: %v", err)
		}
	}
	return m
}

func TestGroupRoundTrip(t *testing.T) {
	t.Parallel()

	s := open(t)
	parent := testGroup("Production")
	if err := s.CreateGroup(t.Context(), parent); err != nil {
		t.Fatalf("create: %v", err)
	}

	child := testGroup("Production / EU")
	child.ParentGroupID = &parent.ID
	child.Description = "Frankfurt"
	if err := s.CreateGroup(t.Context(), child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	got, err := s.GetGroup(t.Context(), child.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Group.Name != child.Name || got.Group.Description != "Frankfurt" {
		t.Errorf("group = %+v", got.Group)
	}
	if got.Group.ParentGroupID == nil || *got.Group.ParentGroupID != parent.ID {
		t.Errorf("parent = %v", got.Group.ParentGroupID)
	}
}

// A parent group showing zero monitors and no status while the child underneath
// it is down would be a dashboard that goes green during an outage.
func TestGroupSummaryIncludesChildren(t *testing.T) {
	t.Parallel()

	s := open(t)
	parent := testGroup("Production")
	if err := s.CreateGroup(t.Context(), parent); err != nil {
		t.Fatalf("create: %v", err)
	}
	child := testGroup("Production / EU")
	child.ParentGroupID = &parent.ID
	if err := s.CreateGroup(t.Context(), child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	monitorIn(t, s, "direct", &parent.ID, model.MonitorStatusUp)
	monitorIn(t, s, "nested", &child.ID, model.MonitorStatusDown)
	monitorIn(t, s, "outside", nil, model.MonitorStatusDown)

	got, err := s.GetGroup(t.Context(), parent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MonitorCount != 2 {
		t.Errorf("monitor_count = %d, want the direct member and the nested one", got.MonitorCount)
	}
	if got.Status != model.MonitorStatusDown {
		t.Errorf("status = %q, want down from the child", got.Status)
	}

	// And the child answers for itself.
	nested, _ := s.GetGroup(t.Context(), child.ID)
	if nested.MonitorCount != 1 || nested.Status != model.MonitorStatusDown {
		t.Errorf("child summary = %d / %q", nested.MonitorCount, nested.Status)
	}
}

// An empty group has no status. Null is a different statement from "up", and
// rendering it green would be the dashboard inventing health.
func TestEmptyGroupHasNoStatus(t *testing.T) {
	t.Parallel()

	s := open(t)
	group := testGroup("Empty")
	if err := s.CreateGroup(t.Context(), group); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetGroup(t.Context(), group.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MonitorCount != 0 || got.Status != "" {
		t.Errorf("summary = %d / %q, want no monitors and no status", got.MonitorCount, got.Status)
	}
}

// Deleting a container must never delete what it contained.
func TestDeletingAGroupUngroupsRatherThanDeletes(t *testing.T) {
	t.Parallel()

	s := open(t)
	parent := testGroup("Production")
	if err := s.CreateGroup(t.Context(), parent); err != nil {
		t.Fatalf("create: %v", err)
	}
	child := testGroup("Production / EU")
	child.ParentGroupID = &parent.ID
	if err := s.CreateGroup(t.Context(), child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	monitor := monitorIn(t, s, "api", &parent.ID, model.MonitorStatusUp)

	if err := s.DeleteGroup(t.Context(), parent.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	survived, err := s.GetMonitor(t.Context(), monitor.ID)
	if err != nil {
		t.Fatalf("the monitor went with the group: %v", err)
	}
	if survived.Monitor.GroupID != nil {
		t.Errorf("group_id = %v, want the monitor ungrouped", survived.Monitor.GroupID)
	}

	orphan, err := s.GetGroup(t.Context(), child.ID)
	if err != nil {
		t.Fatalf("the child group went with its parent: %v", err)
	}
	if orphan.Group.ParentGroupID != nil {
		t.Errorf("parent = %v, want the child promoted to top level", orphan.Group.ParentGroupID)
	}
}

func TestTagRoundTripAndCount(t *testing.T) {
	t.Parallel()

	s := open(t)
	tag := testTag("Customer facing")
	tag.Color = "#ff0000"
	if err := s.CreateTag(t.Context(), tag); err != nil {
		t.Fatalf("create: %v", err)
	}

	monitor := monitorIn(t, s, "api", nil, "")
	if err := s.SetMonitorTags(t.Context(), monitor.ID, monitor.OrgID, []model.ID{tag.ID}); err != nil {
		t.Fatalf("tag: %v", err)
	}

	got, err := s.GetTag(t.Context(), tag.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	switch {
	case got.Tag.Slug != "customer-facing":
		t.Errorf("slug = %q", got.Tag.Slug)
	case got.Tag.Color != "#ff0000":
		t.Errorf("color = %q", got.Tag.Color)
	case got.MonitorCount != 1:
		t.Errorf("monitor_count = %d", got.MonitorCount)
	}
}

func TestDuplicateSlugIsAConflict(t *testing.T) {
	t.Parallel()

	s := open(t)
	if err := s.CreateTag(t.Context(), testTag("Production")); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A different name that reduces to the same slug is the case worth catching:
	// two tags rendering identically in a list are two nobody can tell apart.
	lookalike := testTag("PRODUCTION")
	if err := s.CreateTag(t.Context(), lookalike); !errors.Is(err, store.ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestRenamingATagOntoAnotherSlugIsAConflict(t *testing.T) {
	t.Parallel()

	s := open(t)
	first := testTag("Production")
	second := testTag("Staging")
	for _, tag := range []model.Tag{first, second} {
		if err := s.CreateTag(t.Context(), tag); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	second.Name, second.Slug = "Production", "production"
	if err := s.UpdateTag(t.Context(), second); !errors.Is(err, store.ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}

	// But renaming a tag onto its own slug is fine.
	first.Description = "unchanged slug"
	if err := s.UpdateTag(t.Context(), first); err != nil {
		t.Errorf("renaming a tag to itself failed: %v", err)
	}
}

// The tag goes; the monitors do not.
func TestDeletingATagUntagsRatherThanDeletes(t *testing.T) {
	t.Parallel()

	s := open(t)
	tag := testTag("Production")
	if err := s.CreateTag(t.Context(), tag); err != nil {
		t.Fatalf("create: %v", err)
	}
	monitor := monitorIn(t, s, "api", nil, "")
	if err := s.SetMonitorTags(t.Context(), monitor.ID, monitor.OrgID, []model.ID{tag.ID}); err != nil {
		t.Fatalf("tag: %v", err)
	}

	if err := s.DeleteTag(t.Context(), tag.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetMonitor(t.Context(), monitor.ID); err != nil {
		t.Fatalf("the monitor went with the tag: %v", err)
	}
	ids, err := s.TagIDsForMonitor(t.Context(), monitor.ID)
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("tags = %v, want none", ids)
	}
}

func TestSetMonitorTagsReplaces(t *testing.T) {
	t.Parallel()

	s := open(t)
	first, second := testTag("one"), testTag("two")
	for _, tag := range []model.Tag{first, second} {
		if err := s.CreateTag(t.Context(), tag); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	monitor := monitorIn(t, s, "api", nil, "")

	if err := s.SetMonitorTags(t.Context(), monitor.ID, monitor.OrgID, []model.ID{first.ID, second.ID}); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if err := s.SetMonitorTags(t.Context(), monitor.ID, monitor.OrgID, []model.ID{second.ID}); err != nil {
		t.Fatalf("retag: %v", err)
	}

	ids, _ := s.TagIDsForMonitor(t.Context(), monitor.ID)
	if len(ids) != 1 || ids[0] != second.ID {
		t.Errorf("tags = %v, want just the second", ids)
	}
}

// Filtering to a parent group and getting nothing back while the child holds
// every monitor is a filter nobody trusts twice.
func TestMonitorFilterByGroupReachesChildren(t *testing.T) {
	t.Parallel()

	s := open(t)
	parent := testGroup("Production")
	if err := s.CreateGroup(t.Context(), parent); err != nil {
		t.Fatalf("create: %v", err)
	}
	child := testGroup("Production / EU")
	child.ParentGroupID = &parent.ID
	if err := s.CreateGroup(t.Context(), child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	monitorIn(t, s, "direct", &parent.ID, "")
	monitorIn(t, s, "nested", &child.ID, "")
	monitorIn(t, s, "outside", nil, "")

	got, _, err := s.ListMonitors(t.Context(), nil, 50, MonitorFilter{GroupIDs: []model.ID{parent.ID}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("%d monitors, want the direct one and the nested one", len(got))
	}
}

// Repeated array parameters combine with OR within the parameter, per the spec.
func TestMonitorFilterByTagIsAUnion(t *testing.T) {
	t.Parallel()

	s := open(t)
	first, second := testTag("edge"), testTag("database")
	for _, tag := range []model.Tag{first, second} {
		if err := s.CreateTag(t.Context(), tag); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	edge := monitorIn(t, s, "edge", nil, "")
	db := monitorIn(t, s, "db", nil, "")
	monitorIn(t, s, "untagged", nil, "")

	if err := s.SetMonitorTags(t.Context(), edge.ID, edge.OrgID, []model.ID{first.ID}); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if err := s.SetMonitorTags(t.Context(), db.ID, db.OrgID, []model.ID{second.ID}); err != nil {
		t.Fatalf("tag: %v", err)
	}

	one, _, err := s.ListMonitors(t.Context(), nil, 50, MonitorFilter{TagIDs: []model.ID{first.ID}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(one) != 1 {
		t.Errorf("one tag matched %d monitors", len(one))
	}

	both, _, err := s.ListMonitors(t.Context(), nil, 50, MonitorFilter{TagIDs: []model.ID{first.ID, second.ID}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(both) != 2 {
		t.Errorf("two tags matched %d monitors, want the union", len(both))
	}
}

// Filters combine with AND across parameters.
func TestMonitorFiltersCombineWithAnd(t *testing.T) {
	t.Parallel()

	s := open(t)
	group := testGroup("Production")
	if err := s.CreateGroup(t.Context(), group); err != nil {
		t.Fatalf("create: %v", err)
	}
	tag := testTag("edge")
	if err := s.CreateTag(t.Context(), tag); err != nil {
		t.Fatalf("create: %v", err)
	}

	both := monitorIn(t, s, "both", &group.ID, "")
	groupOnly := monitorIn(t, s, "group only", &group.ID, "")
	tagOnly := monitorIn(t, s, "tag only", nil, "")

	for _, m := range []model.Monitor{both, tagOnly} {
		if err := s.SetMonitorTags(t.Context(), m.ID, m.OrgID, []model.ID{tag.ID}); err != nil {
			t.Fatalf("tag: %v", err)
		}
	}
	_ = groupOnly

	got, _, err := s.ListMonitors(t.Context(), nil, 50,
		MonitorFilter{GroupIDs: []model.ID{group.ID}, TagIDs: []model.ID{tag.ID}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Monitor.ID != both.ID {
		t.Errorf("%d monitors, want only the one matching both", len(got))
	}
}
