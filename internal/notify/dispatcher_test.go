package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
)

// fakeChannelStore records what the dispatcher decided, which is the half of
// alerting that no provider can tell you about.
type fakeChannelStore struct {
	mu         sync.Mutex
	channels   []model.NotificationChannel
	deliveries []model.NotificationDelivery
	lastErrors map[model.ID]string
	recorded   chan struct{}
}

func newFakeChannelStore(channels ...model.NotificationChannel) *fakeChannelStore {
	return &fakeChannelStore{
		channels:   channels,
		lastErrors: map[model.ID]string{},
		recorded:   make(chan struct{}, 64),
	}
}

func (s *fakeChannelStore) ChannelsForMonitor(context.Context, model.ID) ([]model.NotificationChannel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.NotificationChannel(nil), s.channels...), nil
}

func (s *fakeChannelStore) RecordDelivery(_ context.Context, d model.NotificationDelivery) error {
	s.mu.Lock()
	s.deliveries = append(s.deliveries, d)
	s.mu.Unlock()

	select {
	case s.recorded <- struct{}{}:
	default:
	}
	return nil
}

func (s *fakeChannelStore) MarkChannelResult(_ context.Context, id model.ID, _ time.Time, failure string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErrors[id] = failure
	return nil
}

func (s *fakeChannelStore) snapshot() []model.NotificationDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.NotificationDelivery(nil), s.deliveries...)
}

func (s *fakeChannelStore) lastError(id model.ID) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErrors[id]
}

// waitForDeliveries blocks until n rows have been written, or fails. Waiting on
// the recording rather than on a sleep is what keeps this test honest under
// -race.
func (s *fakeChannelStore) waitForDeliveries(t *testing.T, n int) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		if len(s.snapshot()) >= n {
			return
		}
		select {
		case <-s.recorded:
		case <-deadline:
			t.Fatalf("only %d of %d deliveries were recorded", len(s.snapshot()), n)
		}
	}
}

func testVault(t *testing.T) *Vault {
	t.Helper()

	key, err := secrets.NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := secrets.NewKeeper(1, map[uint32][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	return NewVault(keeper)
}

// channel builds a stored channel with its secrets already sealed, which is the
// only state the dispatcher ever sees one in.
func channel(t *testing.T, vault *Vault, name, channelType, rawConfig string, events ...string) model.NotificationChannel {
	t.Helper()

	public, secret := Split(channelType, cfg(rawConfig))
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}

	c := model.NotificationChannel{
		ID:      model.NewID(),
		OrgID:   model.SentinelOrgID,
		Name:    name,
		Type:    channelType,
		Config:  encoded,
		Enabled: true,
		Events:  events,
	}
	sealed, err := vault.Seal(c.OrgID, c.ID, secret)
	if err != nil {
		t.Fatal(err)
	}
	c.Secrets = sealed
	return c
}

func testDispatcher(t *testing.T, store ChannelStore, sender *Sender) *Dispatcher {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := NewDispatcher(store, testVault(t), sender, Instance{Name: "Test", Version: "test"}, log)
	d.backoff = func(int) time.Duration { return time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	t.Cleanup(func() {
		cancel()
		d.Wait()
	})
	return d
}

func TestDispatcherFansOutToEveryAttachedChannel(t *testing.T) {
	t.Parallel()

	sender, _ := testSender(t, http.StatusOK, `{}`)
	store := newFakeChannelStore()
	d := testDispatcher(t, store, sender)

	// Sealed with the dispatcher's own vault, because that is the only state a
	// stored channel is ever in.
	store.channels = []model.NotificationChannel{
		channel(t, d.vault, "slack", "slack", `{"webhook_url":"https://hooks.slack.com/x"}`),
		channel(t, d.vault, "discord", "discord", `{"webhook_url":"https://discord.com/api/webhooks/1/x"}`),
	}

	d.Publish(sampleEvent())
	store.waitForDeliveries(t, 2)

	for _, delivery := range store.snapshot() {
		if delivery.Outcome != model.DeliverySucceeded {
			t.Errorf("outcome = %s: %s", delivery.Outcome, delivery.Error)
		}
		if delivery.RenderedPayload == "" {
			t.Error("nothing was recorded as sent, so an operator cannot see what went out")
		}
	}
}

// A channel that subscribes to recoveries must not be woken by failures, and the
// empty subscription must not mean "everything".
func TestEventSubscriptionIsHonoured(t *testing.T) {
	t.Parallel()

	sender, _ := testSender(t, http.StatusOK, `{}`)
	store := newFakeChannelStore()
	d := testDispatcher(t, store, sender)

	recoveries := channel(t, d.vault, "recoveries", "slack", `{"webhook_url":"https://hooks.slack.com/x"}`, model.EventMonitorUp)
	everything := channel(t, d.vault, "default", "slack", `{"webhook_url":"https://hooks.slack.com/y"}`)
	disabled := channel(t, d.vault, "off", "slack", `{"webhook_url":"https://hooks.slack.com/z"}`)
	disabled.Enabled = false
	store.channels = []model.NotificationChannel{recoveries, everything, disabled}

	d.Publish(sampleEvent())
	store.waitForDeliveries(t, 1)

	// Give the other two a chance to be wrong.
	time.Sleep(100 * time.Millisecond)

	deliveries := store.snapshot()
	if len(deliveries) != 1 {
		t.Fatalf("%d deliveries, want 1", len(deliveries))
	}
	if *deliveries[0].ChannelID != everything.ID {
		t.Error("the wrong channel was notified")
	}
}

// The retry that matters: a receiver that was restarting thirty seconds ago
// usually is not now.
func TestTransientFailureIsRetriedAndRecovers(t *testing.T) {
	t.Parallel()

	var attempts int
	var mu sync.Mutex
	server := flakyServer(t, func() int {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return http.StatusBadGateway
		}
		return http.StatusOK
	})

	store := newFakeChannelStore()
	d := testDispatcher(t, store, server)
	c := channel(t, d.vault, "slack", "slack", `{"webhook_url":"https://hooks.slack.com/x"}`)
	store.channels = []model.NotificationChannel{c}

	d.Publish(sampleEvent())
	store.waitForDeliveries(t, 2)

	deliveries := store.snapshot()
	if deliveries[0].Outcome != model.DeliveryFailed || deliveries[0].Attempt != 1 {
		t.Errorf("first delivery = %s attempt %d", deliveries[0].Outcome, deliveries[0].Attempt)
	}
	if deliveries[1].Outcome != model.DeliverySucceeded || deliveries[1].Attempt != 2 {
		t.Errorf("second delivery = %s attempt %d", deliveries[1].Outcome, deliveries[1].Attempt)
	}

	// A channel that broke and recovered must stop looking broken, or the field
	// trains people to ignore it.
	if failure := store.lastError(c.ID); failure != "" {
		t.Errorf("last_error = %q after a successful retry", failure)
	}
}

// Retrying a wrong credential three times produces three identical failures and
// delays the moment the operator is told which it was.
func TestPermanentFailureIsNotRetriedAndIsVisible(t *testing.T) {
	t.Parallel()

	sender, _ := testSender(t, http.StatusUnauthorized, `{"error":"invalid_auth"}`)
	store := newFakeChannelStore()
	d := testDispatcher(t, store, sender)
	c := channel(t, d.vault, "slack", "slack", `{"webhook_url":"https://hooks.slack.com/x"}`)
	store.channels = []model.NotificationChannel{c}

	d.Publish(sampleEvent())
	store.waitForDeliveries(t, 1)
	time.Sleep(100 * time.Millisecond)

	if n := len(store.snapshot()); n != 1 {
		t.Errorf("%d attempts, want 1", n)
	}
	failure := store.lastError(c.ID)
	if failure == "" {
		t.Fatal("a permanently broken channel does not say so")
	}
	if !strings.Contains(failure, "invalid_auth") {
		t.Errorf("last_error does not carry the provider's words: %q", failure)
	}
}

// suppressed is a first-class outcome: "we chose not to" and "it worked" are
// different answers to the only question anybody asks after an incident.
func TestSuppressedDeliveryIsRecordedAsSuch(t *testing.T) {
	t.Parallel()

	sender, _ := testSender(t, http.StatusAccepted, `{}`)
	store := newFakeChannelStore()
	d := testDispatcher(t, store, sender)
	c := channel(t, d.vault, "pd", "pagerduty", `{"integration_key":"k","auto_resolve":false}`, model.EventMonitorUp)
	store.channels = []model.NotificationChannel{c}

	up := sampleEvent()
	up.Type = model.EventMonitorUp
	d.Publish(up)
	store.waitForDeliveries(t, 1)

	delivery := store.snapshot()[0]
	if delivery.Outcome != model.DeliverySuppressed {
		t.Errorf("outcome = %s, want suppressed", delivery.Outcome)
	}
	// A suppression says nothing about the channel's health either way.
	if failure := store.lastError(c.ID); failure != "" {
		t.Errorf("last_error = %q after a suppression", failure)
	}
}

func TestTestFireRecordsADelivery(t *testing.T) {
	t.Parallel()

	sender, recorded := testSender(t, http.StatusOK, `{}`)
	store := newFakeChannelStore()
	d := testDispatcher(t, store, sender)
	c := channel(t, d.vault, "slack", "slack", `{"webhook_url":"https://hooks.slack.com/x"}`)

	if _, err := d.Test(context.Background(), c, model.EventMonitorDown); err != nil {
		t.Fatalf("test fire: %v", err)
	}
	if len(store.snapshot()) != 1 {
		t.Fatalf("%d deliveries recorded", len(store.snapshot()))
	}
	if !strings.Contains(recorded.get().body, "Sample monitor") {
		t.Errorf("the test did not send the sample event: %s", recorded.get().body)
	}
}

// A dropped alert is the failure this package exists to prevent, so the count is
// visible rather than silent.
func TestFullQueueIsCountedNotSwallowed(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender, _ := testSender(t, http.StatusOK, `{}`)
	d := NewDispatcher(newFakeChannelStore(), testVault(t), sender, Instance{}, log)
	// Deliberately not started: nothing drains the queue.

	for i := 0; i < queueDepth+5; i++ {
		d.Publish(sampleEvent())
	}
	if dropped := d.Dropped(); dropped != 5 {
		t.Errorf("dropped = %d, want 5", dropped)
	}
}

// A restored database without its key file is the case this must not paper over.
func TestUnreadableSecretsFailLoudly(t *testing.T) {
	t.Parallel()

	sender, _ := testSender(t, http.StatusOK, `{}`)
	store := newFakeChannelStore()
	d := testDispatcher(t, store, sender)

	// Sealed under a different key than the dispatcher holds.
	c := channel(t, testVault(t), "slack", "slack", `{"webhook_url":"https://hooks.slack.com/x"}`)
	store.channels = []model.NotificationChannel{c}

	d.Publish(sampleEvent())
	store.waitForDeliveries(t, 1)

	delivery := store.snapshot()[0]
	if delivery.Outcome != model.DeliveryFailed {
		t.Errorf("outcome = %s", delivery.Outcome)
	}
	if !strings.Contains(delivery.Error, "credentials") {
		t.Errorf("the error does not say what is wrong: %q", delivery.Error)
	}
}

// flakyServer answers with whatever status the caller's function returns, so a
// test can script a transient failure.
func flakyServer(t *testing.T, status func() int) *Sender {
	t.Helper()

	sender, _ := testSenderFunc(t, status, `{}`)
	return sender
}
