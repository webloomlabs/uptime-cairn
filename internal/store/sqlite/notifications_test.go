package sqlite

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

func testChannel(name string) model.NotificationChannel {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return model.NotificationChannel{
		ID:        model.NewID(),
		OrgID:     model.SentinelOrgID,
		Name:      name,
		Type:      model.ChannelSlack,
		Config:    json.RawMessage(`{"channel":"#ops"}`),
		Secrets:   []byte{0x01, 0x02, 0x03},
		Enabled:   true,
		Events:    []string{},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestChannelRoundTrip(t *testing.T) {
	t.Parallel()

	s := open(t)
	want := testChannel("Ops Slack")
	want.Events = []string{model.EventMonitorDown}
	want.IsDefault = true

	if err := s.CreateChannel(t.Context(), want); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetChannel(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	switch {
	case got.Channel.Name != want.Name:
		t.Errorf("name = %q", got.Channel.Name)
	case got.Channel.Type != want.Type:
		t.Errorf("type = %q", got.Channel.Type)
	case string(got.Channel.Config) != string(want.Config):
		t.Errorf("config = %s", got.Channel.Config)
	case string(got.Channel.Secrets) != string(want.Secrets):
		t.Errorf("secrets did not round-trip")
	case !got.Channel.IsDefault:
		t.Error("is_default was lost")
	case len(got.Channel.Events) != 1 || got.Channel.Events[0] != model.EventMonitorDown:
		t.Errorf("events = %v", got.Channel.Events)
	case got.MonitorCount != 0:
		t.Errorf("monitor_count = %d", got.MonitorCount)
	}
}

func TestGetMissingChannelIsNotFound(t *testing.T) {
	t.Parallel()

	s := open(t)
	if _, err := s.GetChannel(t.Context(), model.NewID()); err != store.ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestChannelListFilters(t *testing.T) {
	t.Parallel()

	s := open(t)
	slack := testChannel("Ops Slack")
	email := testChannel("Ops Email")
	email.Type = model.ChannelEmail
	disabled := testChannel("Retired webhook")
	disabled.Type = model.ChannelWebhook
	disabled.Enabled = false

	for _, c := range []model.NotificationChannel{slack, email, disabled} {
		if err := s.CreateChannel(t.Context(), c); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	enabled := true
	cases := []struct {
		name   string
		filter store.ChannelFilter
		want   int
	}{
		{"everything", store.ChannelFilter{}, 3},
		{"by type", store.ChannelFilter{Types: []string{model.ChannelEmail}}, 1},
		{"by two types", store.ChannelFilter{Types: []string{model.ChannelEmail, model.ChannelSlack}}, 2},
		{"by enabled", store.ChannelFilter{Enabled: &enabled}, 2},
		{"by search", store.ChannelFilter{Search: "Ops"}, 2},
		{"search finds nothing", store.ChannelFilter{Search: "nonexistent"}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := s.ListChannels(t.Context(), nil, 50, tc.filter)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("%d channels, want %d", len(got), tc.want)
			}
		})
	}
}

// A search term containing a LIKE wildcard must match that term, not everything.
func TestChannelSearchEscapesWildcards(t *testing.T) {
	t.Parallel()

	s := open(t)
	for _, name := range []string{"100% uptime", "Ops Slack"} {
		c := testChannel(name)
		if err := s.CreateChannel(t.Context(), c); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	got, _, err := s.ListChannels(t.Context(), nil, 50, store.ChannelFilter{Search: "100%"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Channel.Name != "100% uptime" {
		t.Errorf("search for a literal %% matched %d channels", len(got))
	}
}

func TestChannelAssignmentAndCount(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("api")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	slack := testChannel("Slack")
	pager := testChannel("Pager")
	pager.Type = model.ChannelPagerDuty
	quiet := testChannel("Disabled")
	quiet.Enabled = false

	for _, c := range []model.NotificationChannel{slack, pager, quiet} {
		if err := s.CreateChannel(t.Context(), c); err != nil {
			t.Fatalf("create channel: %v", err)
		}
	}

	if err := s.SetMonitorChannels(t.Context(), monitor.ID, monitor.OrgID,
		[]model.ID{slack.ID, pager.ID, quiet.ID}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	ids, err := s.ChannelIDsForMonitor(t.Context(), monitor.ID)
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("%d assignments, want 3", len(ids))
	}

	// Delivery only ever sees the enabled ones.
	channels, err := s.ChannelsForMonitor(t.Context(), monitor.ID)
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	if len(channels) != 2 {
		t.Errorf("%d deliverable channels, want 2 — a disabled channel must not be delivered to", len(channels))
	}

	got, err := s.GetChannel(t.Context(), slack.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MonitorCount != 1 {
		t.Errorf("monitor_count = %d, want 1", got.MonitorCount)
	}

	// Replace, not merge: a PATCH that sends one id means one.
	if err := s.SetMonitorChannels(t.Context(), monitor.ID, monitor.OrgID, []model.ID{pager.ID}); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	ids, _ = s.ChannelIDsForMonitor(t.Context(), monitor.ID)
	if len(ids) != 1 || ids[0] != pager.ID {
		t.Errorf("assignments = %v, want just the pager", ids)
	}
}

func TestChannelIDsForMonitorsIsOneQueryForAPage(t *testing.T) {
	t.Parallel()

	s := open(t)
	channel := testChannel("Slack")
	if err := s.CreateChannel(t.Context(), channel); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	var ids []model.ID
	for i := 0; i < 3; i++ {
		monitor := testMonitor("m")
		if err := s.CreateMonitor(t.Context(), monitor); err != nil {
			t.Fatalf("create monitor: %v", err)
		}
		ids = append(ids, monitor.ID)
		if i < 2 {
			if err := s.SetMonitorChannels(t.Context(), monitor.ID, monitor.OrgID, []model.ID{channel.ID}); err != nil {
				t.Fatalf("assign: %v", err)
			}
		}
	}

	assignments, err := s.ChannelIDsForMonitors(t.Context(), ids)
	if err != nil {
		t.Fatalf("bulk assignments: %v", err)
	}
	if len(assignments) != 2 {
		t.Errorf("%d monitors with assignments, want 2", len(assignments))
	}
	if got := assignments[ids[2]]; len(got) != 0 {
		t.Errorf("an unassigned monitor came back with %v", got)
	}
}

func TestDefaultChannelIDs(t *testing.T) {
	t.Parallel()

	s := open(t)
	ordinary := testChannel("Ordinary")
	fallback := testChannel("Default")
	fallback.IsDefault = true

	for _, c := range []model.NotificationChannel{ordinary, fallback} {
		if err := s.CreateChannel(t.Context(), c); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	ids, err := s.DefaultChannelIDs(t.Context(), model.SentinelOrgID)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if len(ids) != 1 || ids[0] != fallback.ID {
		t.Errorf("defaults = %v", ids)
	}
}

func TestMissingChannelsNamesTheOnesThatAreNotThere(t *testing.T) {
	t.Parallel()

	s := open(t)
	real := testChannel("Real")
	if err := s.CreateChannel(t.Context(), real); err != nil {
		t.Fatalf("create: %v", err)
	}
	ghost := model.NewID()

	missing, err := s.MissingChannels(t.Context(), model.SentinelOrgID, []model.ID{real.ID, ghost})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(missing) != 1 || missing[0] != ghost {
		t.Errorf("missing = %v, want just the ghost", missing)
	}
}

// The delivery log outlives its destination: the history of what was sent has to
// survive deleting the channel, or an after-the-fact question about an incident
// has no answer.
func TestDeliveryLogSurvivesChannelDeletion(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("api")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	channel := testChannel("Slack")
	if err := s.CreateChannel(t.Context(), channel); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := s.SetMonitorChannels(t.Context(), monitor.ID, monitor.OrgID, []model.ID{channel.ID}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	duration := 42.5
	if err := s.RecordDelivery(t.Context(), model.NotificationDelivery{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		MonitorID: &monitor.ID, ChannelID: &channel.ID,
		EventType: model.EventMonitorDown, Outcome: model.DeliverySucceeded,
		DurationMs: &duration, Attempt: 1, RenderedPayload: `{"text":"down"}`,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := s.DeleteChannel(t.Context(), channel.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var rows, nulled int
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*), SUM(channel_id IS NULL) FROM notification_deliveries`).Scan(&rows, &nulled); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d delivery rows survived, want 1", rows)
	}
	if nulled != 1 {
		t.Error("the delivery still points at a channel that no longer exists")
	}

	// The assignment, by contrast, goes with the channel.
	ids, _ := s.ChannelIDsForMonitor(t.Context(), monitor.ID)
	if len(ids) != 0 {
		t.Errorf("assignments = %v after the channel was deleted", ids)
	}
}

// A channel that broke and recovered must stop looking broken.
func TestMarkChannelResultClearsOnSuccess(t *testing.T) {
	t.Parallel()

	s := open(t)
	channel := testChannel("Slack")
	if err := s.CreateChannel(t.Context(), channel); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.MarkChannelResult(t.Context(), channel.ID, now, "403 Forbidden: invalid_auth"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, _ := s.GetChannel(t.Context(), channel.ID)
	if got.Channel.LastError == "" {
		t.Fatal("the failure was not recorded")
	}
	if got.Channel.LastUsedAt == nil {
		t.Error("last_used_at was not set")
	}

	if err := s.MarkChannelResult(t.Context(), channel.ID, now.Add(time.Minute), ""); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, _ = s.GetChannel(t.Context(), channel.ID)
	if got.Channel.LastError != "" {
		t.Errorf("last_error = %q after a success", got.Channel.LastError)
	}
}

// Deleting a monitor must take its assignments with it, or the channel's
// monitor_count counts monitors that no longer exist.
func TestDeletingAMonitorDetachesItsChannels(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("api")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	channel := testChannel("Slack")
	if err := s.CreateChannel(t.Context(), channel); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := s.SetMonitorChannels(t.Context(), monitor.ID, monitor.OrgID, []model.ID{channel.ID}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	if err := s.DeleteMonitor(t.Context(), monitor.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got, err := s.GetChannel(t.Context(), channel.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MonitorCount != 0 {
		t.Errorf("monitor_count = %d after the monitor was deleted", got.MonitorCount)
	}
}
