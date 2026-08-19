package outbound

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
)

// fakeStore is the one dependency worth faking here: what is under test is the
// delivery decision, and a real database would only prove that SQLite works.
type fakeStore struct {
	mu         sync.Mutex
	hooks      []model.Webhook
	headers    [][]byte
	deliveries []model.WebhookDelivery
	disabled   bool
}

func (f *fakeStore) EnabledWebhooks(context.Context) ([]model.Webhook, [][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disabled {
		return nil, nil, nil
	}
	return f.hooks, f.headers, nil
}

func (f *fakeStore) RecordWebhookDelivery(_ context.Context, d model.WebhookDelivery, maxFailures int, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries = append(f.deliveries, d)
	if d.Outcome == model.DeliveryFailed && len(f.deliveries) >= maxFailures {
		f.disabled = true
	}
	return nil
}

func (f *fakeStore) recorded() []model.WebhookDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.WebhookDelivery(nil), f.deliveries...)
}

// plainVault stands in for the key hierarchy: the envelope is the plaintext.
// Crypto is tested where it lives, in internal/secrets.
type plainVault struct{}

func (plainVault) Open(_, _, envelope []byte) ([]byte, error) { return envelope, nil }

func testDispatcher(t *testing.T, store Store) *Dispatcher {
	t.Helper()

	d := New(store, plainVault{}, notify.Instance{Name: "Test", Version: "test"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	// No real waiting: five attempts at the production schedule is over a
	// minute, and what is under test is the decision to retry rather than the
	// arithmetic that spaces them.
	d.backoff = func(int) time.Duration { return time.Millisecond }
	return d
}

func downEvent() notify.Event {
	return notify.NewEvent(model.EventMonitorDown, notify.Instance{Name: "Test", Version: "test"},
		model.Monitor{ID: model.NewID(), Name: "Checkout", Type: "http", Target: "https://example.com"},
		model.MonitorStatusUp, nil, model.MonitorStatusDown, time.Now().UTC())
}

func TestSignatureIsOverTheExactBytesSent(t *testing.T) {
	t.Parallel()

	var (
		received  []byte
		signature string
		done      = make(chan struct{})
	)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		signature = r.Header.Get(signatureHeader)
		close(done)
	}))
	defer receiver.Close()

	store := &fakeStore{hooks: []model.Webhook{{
		ID: model.NewID(), OrgID: model.SentinelOrgID, URL: receiver.URL,
		Events: []string{model.EventMonitorDown}, Enabled: true, VerifyTLS: true,
		SecretEncrypted: []byte("shared-secret"),
	}}, headers: [][]byte{nil}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d := testDispatcher(t, store)
	d.Start(ctx)
	d.fanOut(ctx, downEvent())

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery arrived")
	}

	// A receiver verifies by recomputing over the raw body it read. If the
	// signature were computed over anything else — a re-marshalled struct, a
	// canonicalised form — every receiver in the world would reject every
	// delivery, and it would work perfectly in a test that reused the same
	// serialiser.
	if want := Sign([]byte("shared-secret"), received); signature != want {
		t.Fatalf("signature = %q, want %q", signature, want)
	}
}

func TestAConfiguredHeaderCannotReplaceTheEventIdentity(t *testing.T) {
	t.Parallel()

	var seen http.Header
	done := make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		close(done)
	}))
	defer receiver.Close()

	headers, _ := json.Marshal(map[string]string{
		"X-Team":        "ops",
		eventIDHeader:   "not-the-event-id",
		signatureHeader: "sha256=forged",
	})
	store := &fakeStore{hooks: []model.Webhook{{
		ID: model.NewID(), OrgID: model.SentinelOrgID, URL: receiver.URL,
		Events: []string{model.EventMonitorDown}, Enabled: true,
		SecretEncrypted: []byte("shared-secret"),
	}}, headers: [][]byte{headers}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d := testDispatcher(t, store)
	d.Start(ctx)
	event := downEvent()
	d.fanOut(ctx, event)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery arrived")
	}

	// The operator's header goes on, and the reserved ones win. A receiver whose
	// deduplication key can be changed by a typo in a settings field is a
	// receiver with no deduplication.
	if seen.Get("X-Team") != "ops" {
		t.Errorf("X-Team = %q, want the configured header to arrive", seen.Get("X-Team"))
	}
	if seen.Get(eventIDHeader) != event.ID.String() {
		t.Errorf("%s = %q, want the real event id", eventIDHeader, seen.Get(eventIDHeader))
	}
	if seen.Get(signatureHeader) == "sha256=forged" {
		t.Error("a configured header replaced the signature")
	}
}

func TestOnlySubscribedEventsAreDelivered(t *testing.T) {
	t.Parallel()

	var count int
	var mu sync.Mutex
	receiver := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
	}))
	defer receiver.Close()

	store := &fakeStore{hooks: []model.Webhook{{
		ID: model.NewID(), OrgID: model.SentinelOrgID, URL: receiver.URL,
		Events: []string{model.EventIncidentOpened}, Enabled: true,
	}}, headers: [][]byte{nil}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d := testDispatcher(t, store)
	d.Start(ctx)
	d.fanOut(ctx, downEvent())

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("received %d deliveries for an event this webhook did not subscribe to", count)
	}
}

func TestAFailingReceiverIsRetriedAndThenGivenUpOn(t *testing.T) {
	t.Parallel()

	var attempts int
	var mu sync.Mutex
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer receiver.Close()

	store := &fakeStore{hooks: []model.Webhook{{
		ID: model.NewID(), OrgID: model.SentinelOrgID, URL: receiver.URL,
		Events: []string{model.EventMonitorDown}, Enabled: true,
	}}, headers: [][]byte{nil}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d := testDispatcher(t, store)
	d.Start(ctx)
	d.fanOut(ctx, downEvent())

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		got := attempts
		mu.Unlock()
		if got >= maxAttempts {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("attempts = %d, want %d", got, maxAttempts)
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Bounded. A dead endpoint retried forever is a queue that never drains.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if attempts > maxAttempts {
		t.Fatalf("attempts = %d, want it to stop at %d", attempts, maxAttempts)
	}

	// Every attempt is logged, whether or not anybody is watching.
	if got := len(store.recorded()); got < maxAttempts {
		t.Fatalf("recorded %d deliveries, want one per attempt", got)
	}
	for _, delivery := range store.recorded() {
		if delivery.Outcome != model.DeliveryFailed {
			t.Errorf("outcome = %q, want failed", delivery.Outcome)
		}
		if delivery.ResponseStatus == nil || *delivery.ResponseStatus != http.StatusInternalServerError {
			t.Errorf("response_status = %v, want 500", delivery.ResponseStatus)
		}
	}
}

func TestAChattyErrorResponseIsTruncated(t *testing.T) {
	t.Parallel()

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		// A receiver returning a megabyte of HTML on error must not turn one
		// failing webhook into a disk-space incident.
		for range 100 {
			_, _ = w.Write([]byte("<html><body>something went terribly wrong</body></html>"))
		}
	}))
	defer receiver.Close()

	store := &fakeStore{hooks: []model.Webhook{{
		ID: model.NewID(), OrgID: model.SentinelOrgID, URL: receiver.URL,
		Events: []string{model.EventMonitorDown}, Enabled: true,
	}}, headers: [][]byte{nil}}

	d := testDispatcher(t, store)
	delivery := d.send(t.Context(), job{
		hook: store.hooks[0], eventID: model.NewID(), event: model.EventMonitorDown,
		body: []byte(`{}`), attempt: 1,
	})

	if len(delivery.ResponseBody) > maxResponseBody {
		t.Fatalf("response body kept %d bytes, want at most %d", len(delivery.ResponseBody), maxResponseBody)
	}
	if delivery.Outcome != model.DeliveryFailed {
		t.Fatalf("outcome = %q, want failed", delivery.Outcome)
	}
}

func TestEnvelopeShapeFollowsTheEventType(t *testing.T) {
	t.Parallel()

	raw, err := Envelope(downEvent())
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["type"] != model.EventMonitorDown {
		t.Errorf("type = %v", decoded["type"])
	}

	data := decoded["data"].(map[string]any)
	if _, ok := data["monitor"]; !ok {
		t.Error("a monitor event carries no monitor")
	}
	// A single struct with omitempty would send both halves with one null,
	// which is a contract that says less. A receiver branches on the type and
	// finds only what that type carries.
	if _, present := data["incident"]; present {
		t.Error("a monitor event carries an incident")
	}
	if data["previous_status"] != model.MonitorStatusUp {
		t.Errorf("previous_status = %v, want up", data["previous_status"])
	}

	incident := notify.NewIncidentEvent(model.EventIncidentOpened,
		notify.Instance{Name: "Test"},
		notify.Incident{ID: model.NewID(), Title: "Outage", State: model.IncidentInvestigating, Impact: model.ImpactMajor},
		time.Now().UTC())
	raw, err = Envelope(incident)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	decoded = nil
	_ = json.Unmarshal(raw, &decoded)
	data = decoded["data"].(map[string]any)
	if _, ok := data["incident"]; !ok {
		t.Error("an incident event carries no incident")
	}
	if _, present := data["monitor"]; present {
		t.Error("an incident event carries a monitor")
	}
}

func TestRetryScheduleBacksOffAndIsCapped(t *testing.T) {
	t.Parallel()

	previous := time.Duration(0)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		delay := retryAfter(attempt)
		if delay < previous {
			t.Fatalf("attempt %d waits %s, less than the previous %s", attempt, delay, previous)
		}
		if delay > 2*time.Minute {
			t.Fatalf("attempt %d waits %s, past the cap", attempt, delay)
		}
		previous = delay
	}
}
