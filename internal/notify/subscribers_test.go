package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
)

// Status page subscriber delivery.
//
// The rules worth a test are the ones whose failure is silent and lands in
// somebody else's inbox: mail to an address that never confirmed, mail with no
// way to unsubscribe, and one subscriber's unsubscribe link reaching a different
// subscriber. None of the three is visible from inside the process.

type fakeSubscriberStore struct {
	page model.StatusPage

	mu          sync.Mutex
	subscribers []model.Subscriber
	deliveries  []model.NotificationDelivery
	reissued    map[model.ID][]byte
	recorded    chan struct{}
}

func newFakeSubscriberStore(page model.StatusPage, subs ...model.Subscriber) *fakeSubscriberStore {
	return &fakeSubscriberStore{
		page:        page,
		subscribers: subs,
		reissued:    map[model.ID][]byte{},
		recorded:    make(chan struct{}, 64),
	}
}

func (s *fakeSubscriberStore) GetStatusPage(context.Context, model.ID) (model.StatusPage, error) {
	return s.page, nil
}

// ConfirmedSubscribers filters here as the real one does in SQL, so a test that
// hands it an unconfirmed row exercises the same rule the query enforces.
func (s *fakeSubscriberStore) ConfirmedSubscribers(context.Context, model.ID) ([]model.Subscriber, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []model.Subscriber
	for _, sub := range s.subscribers {
		if sub.Confirmed() {
			out = append(out, sub)
		}
	}
	return out, nil
}

func (s *fakeSubscriberStore) ReissueUnsubscribeToken(_ context.Context, id model.ID, _, sealed []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reissued[id] = sealed
	return nil
}

func (s *fakeSubscriberStore) RecordDelivery(_ context.Context, d model.NotificationDelivery) error {
	s.mu.Lock()
	s.deliveries = append(s.deliveries, d)
	s.mu.Unlock()

	select {
	case s.recorded <- struct{}{}:
	default:
	}
	return nil
}

func (s *fakeSubscriberStore) snapshot() []model.NotificationDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.NotificationDelivery(nil), s.deliveries...)
}

func (s *fakeSubscriberStore) waitForDeliveries(t *testing.T, n int) []model.NotificationDelivery {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		if got := s.snapshot(); len(got) >= n {
			return got
		}
		select {
		case <-s.recorded:
		case <-deadline:
			t.Fatalf("only %d of %d deliveries were recorded", len(s.snapshot()), n)
		}
	}
}

func testSubscriberVault(t *testing.T) *secrets.Vault {
	t.Helper()

	key, err := secrets.NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := secrets.NewKeeper(1, map[uint32][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	return secrets.NewVault(keeper, "subscribers", "target")
}

func testPage() model.StatusPage {
	return model.StatusPage{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Slug: "acme", Title: "Acme Status", SubscriptionsEnabled: true,
	}
}

// subscriber builds a stored row with both envelopes sealed, which is the only
// state the relay ever sees one in.
func subscriber(t *testing.T, vault *secrets.Vault, page model.StatusPage, channel, target string, confirmed bool) model.Subscriber {
	t.Helper()

	sub := model.Subscriber{
		ID:           model.NewID(),
		StatusPageID: page.ID,
		OrgID:        page.OrgID,
		Channel:      channel,
		TargetHash:   auth.HashToken(target),
		CreatedAt:    time.Now().UTC(),
	}
	if confirmed {
		at := time.Now().UTC()
		sub.ConfirmedAt = &at
	}

	sealed, err := vault.Seal(sub.OrgID[:], sub.ID[:], []byte(target))
	if err != nil {
		t.Fatal(err)
	}
	sub.SealedTarget = sealed

	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	sub.UnsubscribeTokenHash = auth.HashToken(token)
	if sub.SealedUnsubscribeToken, err = vault.Seal(sub.OrgID[:], sub.ID[:], []byte(token)); err != nil {
		t.Fatal(err)
	}
	return sub
}

func testRelay(t *testing.T, store SubscriberStore, vault *secrets.Vault) *Relay {
	t.Helper()

	relay := NewRelay(store, vault, NewSender(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	// The retry path is exercised, not waited out.
	relay.backoff = func() time.Duration { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		relay.Wait()
	})
	relay.Start(ctx)
	return relay
}

// useFakeSMTP points the instance relay at a fake server. The instance relay is
// package state, so these tests do not run in parallel with each other.
func useFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()

	server := newFakeSMTP(t)
	SetInstanceSMTP(InstanceSMTP{
		Host: server.host, Port: server.port, Encryption: "none",
		FromAddress: "status@example.com", FromName: "Acme Status",
	})
	t.Cleanup(func() { SetInstanceSMTP(InstanceSMTP{}) })
	return server
}

func TestConfirmationCarriesBothLinks(t *testing.T) {
	server := useFakeSMTP(t)
	vault := testSubscriberVault(t)
	page := testPage()
	store := newFakeSubscriberStore(page)
	relay := testRelay(t, store, vault)

	sub := subscriber(t, vault, page, model.SubscriberEmail, "reader@example.com", false)
	relay.Confirm(Confirmation{
		Page:             page,
		Subscriber:       sub,
		Target:           "reader@example.com",
		Token:            "confirm-token",
		UnsubscribeToken: "unsubscribe-token",
		BaseURL:          "https://cairn.example.com",
	})

	deliveries := store.waitForDeliveries(t, 1)
	if deliveries[0].Outcome != model.DeliverySucceeded {
		t.Fatalf("outcome = %s (%s)", deliveries[0].Outcome, deliveries[0].Error)
	}
	if deliveries[0].EventType != eventSubscriptionConfirmation {
		t.Errorf("event_type = %q", deliveries[0].EventType)
	}

	// The delivery log holds a masked address and the subject, and neither the
	// full address nor the unsubscribe token: that column is plaintext and
	// subject to retention, and both of those belong to the key hierarchy.
	logged := deliveries[0].RenderedPayload
	if strings.Contains(logged, "reader@example.com") || strings.Contains(logged, "unsubscribe-token") {
		t.Errorf("the delivery log recorded a subscriber's address or their token: %q", logged)
	}
	if !strings.Contains(logged, "Confirm your subscription to Acme Status") {
		t.Errorf("the delivery log says nothing about what was sent: %q", logged)
	}

	sent := server.sent()
	if len(sent) != 1 {
		t.Fatalf("the server accepted %d messages, want 1", len(sent))
	}
	message := sent[0].data
	if len(sent[0].recipients) != 1 || sent[0].recipients[0] != "reader@example.com" {
		t.Errorf("recipients = %v", sent[0].recipients)
	}
	// The header that decides whether a mail client offers the one-click
	// button, and whether the message reads as bulk mail somebody consented to.
	if !strings.Contains(message, "List-Unsubscribe: <https://cairn.example.com/subscriptions/unsubscribe/unsubscribe-token>") {
		t.Errorf("no List-Unsubscribe header:\n%s", message)
	}

	body := decodeMailBody(t, message)
	if !strings.Contains(body, "https://cairn.example.com/subscriptions/confirm/confirm-token") {
		t.Errorf("no confirmation link in the body:\n%s", body)
	}
	// The one sentence that matters to somebody who did not subscribe.
	if !strings.Contains(body, "ignore this message") {
		t.Errorf("the body does not tell a stranger they can ignore it:\n%s", body)
	}
	if !strings.Contains(body, "https://cairn.example.com/status/acme") {
		t.Errorf("no link to the page itself:\n%s", body)
	}

}

// The rule this whole feature is arranged around: an address that has not
// completed double opt-in hears nothing but its own confirmation.
func TestAnnouncementSkipsUnconfirmedSubscribers(t *testing.T) {
	server := useFakeSMTP(t)
	vault := testSubscriberVault(t)
	page := testPage()

	confirmed := subscriber(t, vault, page, model.SubscriberEmail, "yes@example.com", true)
	pending := subscriber(t, vault, page, model.SubscriberEmail, "never-agreed@example.com", false)
	store := newFakeSubscriberStore(page, confirmed, pending)
	relay := testRelay(t, store, vault)

	relay.Announce(sampleAnnouncement(page))

	deliveries := store.waitForDeliveries(t, 1)
	if len(deliveries) != 1 {
		t.Fatalf("recorded %d deliveries, want 1", len(deliveries))
	}
	sent := server.sent()
	if len(sent) != 1 {
		t.Fatalf("the server accepted %d messages, want 1", len(sent))
	}
	if sent[0].recipients[0] != "yes@example.com" {
		t.Errorf("recipients = %v, want only the confirmed subscriber", sent[0].recipients)
	}
	if strings.Contains(server.transcript(), "never-agreed@example.com") {
		t.Fatal("an unconfirmed address was mailed")
	}
}

// Every recipient gets their own link. The body and the payload are rendered
// once per bulletin and the link once per recipient, and getting that seam wrong
// unsubscribes the wrong person — from a link that works, which is what makes it
// silent.
func TestEachSubscriberGetsTheirOwnUnsubscribeLink(t *testing.T) {
	server := useFakeSMTP(t)
	vault := testSubscriberVault(t)
	page := testPage()

	first := subscriber(t, vault, page, model.SubscriberEmail, "one@example.com", true)
	second := subscriber(t, vault, page, model.SubscriberEmail, "two@example.com", true)
	store := newFakeSubscriberStore(page, first, second)
	relay := testRelay(t, store, vault)

	relay.Announce(sampleAnnouncement(page))
	for _, d := range store.waitForDeliveries(t, 2) {
		if d.Outcome != model.DeliverySucceeded {
			t.Fatalf("outcome = %s (%s)", d.Outcome, d.Error)
		}
	}

	sent := server.sent()
	if len(sent) != 2 {
		t.Fatalf("the server accepted %d messages, want 2", len(sent))
	}

	links := map[string]bool{}
	for _, message := range sent {
		link := unsubscribeLinkOf(t, message.data)
		if links[link] {
			t.Fatalf("two subscribers were given the same unsubscribe link: %s", link)
		}
		links[link] = true

		// And the body carries it too: a mail client that does not honour
		// List-Unsubscribe leaves the footer as the only way out.
		if !strings.Contains(decodeMailBody(t, message.data), link) {
			t.Errorf("the link is in the header but not in the body:\n%s", message.data)
		}
	}
	if len(links) != 2 {
		t.Fatalf("got %d distinct links for 2 subscribers", len(links))
	}
}

// A subscription written before the envelope column existed can be verified
// against but not rendered from. Sending without the link is the one thing this
// relay will not do, so it issues a fresh token instead.
func TestUnsubscribeTokenIsReissuedForOlderSubscriptions(t *testing.T) {
	server := useFakeSMTP(t)
	vault := testSubscriberVault(t)
	page := testPage()

	legacy := subscriber(t, vault, page, model.SubscriberEmail, "old@example.com", true)
	legacy.SealedUnsubscribeToken = nil
	store := newFakeSubscriberStore(page, legacy)
	relay := testRelay(t, store, vault)

	relay.Announce(sampleAnnouncement(page))
	deliveries := store.waitForDeliveries(t, 1)

	if deliveries[0].Outcome != model.DeliverySucceeded {
		t.Fatalf("outcome = %s (%s)", deliveries[0].Outcome, deliveries[0].Error)
	}
	sent := server.sent()
	if len(sent) != 1 {
		t.Fatalf("the server accepted %d messages, want 1", len(sent))
	}
	if link := unsubscribeLinkOf(t, sent[0].data); !strings.Contains(link, "/subscriptions/unsubscribe/") {
		t.Errorf("no usable unsubscribe link: %q", link)
	}

	store.mu.Lock()
	sealed := store.reissued[legacy.ID]
	store.mu.Unlock()
	if len(sealed) == 0 {
		t.Fatal("the new token was not written back, so the next message would issue another one")
	}
	// Written as an envelope bound to this row, not as plaintext.
	if plain, err := vault.Open(legacy.OrgID[:], legacy.ID[:], sealed); err != nil {
		t.Errorf("the reissued token does not open: %v", err)
	} else if len(plain) == 0 {
		t.Error("the reissued token is empty")
	}
}

// Without a base URL every link in the message would be broken, and deriving one
// from whichever hostname the operator's browser used would put an internal name
// in a customer's inbox. Neither is sent; both are recorded.
func TestBulletinIsSuppressedWithoutABaseURL(t *testing.T) {
	server := useFakeSMTP(t)
	vault := testSubscriberVault(t)
	page := testPage()

	sub := subscriber(t, vault, page, model.SubscriberEmail, "reader@example.com", true)
	store := newFakeSubscriberStore(page, sub)
	relay := testRelay(t, store, vault)

	announcement := sampleAnnouncement(page)
	announcement.BaseURL = ""
	relay.Announce(announcement)

	deliveries := store.waitForDeliveries(t, 1)
	if deliveries[0].Outcome != model.DeliverySuppressed {
		t.Errorf("outcome = %s, want suppressed", deliveries[0].Outcome)
	}
	// The reason names the setting, because the delivery log is where the
	// operator looks and "failed" would send them to the mail server.
	if !strings.Contains(deliveries[0].Error, "base_url") {
		t.Errorf("the reason does not name the setting: %q", deliveries[0].Error)
	}
	if sent := server.sent(); len(sent) != 0 {
		t.Errorf("a message went out anyway: %v", sent)
	}
}

// With no relay configured there is nowhere to send from. Recorded as suppressed
// rather than failed: nothing is wrong with the subscriber, and a row saying
// "failed" would have the operator debugging a mail server they never set up.
func TestBulletinIsSuppressedWithoutARelay(t *testing.T) {
	SetInstanceSMTP(InstanceSMTP{})
	vault := testSubscriberVault(t)
	page := testPage()

	sub := subscriber(t, vault, page, model.SubscriberEmail, "reader@example.com", true)
	store := newFakeSubscriberStore(page, sub)
	relay := testRelay(t, store, vault)

	relay.Announce(sampleAnnouncement(page))

	deliveries := store.waitForDeliveries(t, 1)
	if deliveries[0].Outcome != model.DeliverySuppressed {
		t.Errorf("outcome = %s, want suppressed (%s)", deliveries[0].Outcome, deliveries[0].Error)
	}
}

func TestWebhookSubscriberGetsThePayload(t *testing.T) {
	useFakeSMTP(t)
	vault := testSubscriberVault(t)
	page := testPage()

	var (
		mu       sync.Mutex
		received map[string]any
	)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(body, &received)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()

	sub := subscriber(t, vault, page, model.SubscriberWebhook, endpoint.URL, true)
	store := newFakeSubscriberStore(page, sub)
	relay := testRelay(t, store, vault)

	relay.Announce(sampleAnnouncement(page))
	deliveries := store.waitForDeliveries(t, 1)
	if deliveries[0].Outcome != model.DeliverySucceeded {
		t.Fatalf("outcome = %s (%s)", deliveries[0].Outcome, deliveries[0].Error)
	}

	mu.Lock()
	defer mu.Unlock()
	if received["event"] != model.EventIncidentUpdated {
		t.Errorf("event = %v", received["event"])
	}
	if received["update"] != "We have identified the cause." {
		t.Errorf("update = %v", received["update"])
	}
	// A machine receiver needs the way out as much as a person does, and it is
	// the field a receiver has to be able to find without parsing prose.
	link, _ := received["unsubscribe_url"].(string)
	if !strings.Contains(link, "/subscriptions/unsubscribe/") {
		t.Errorf("unsubscribe_url = %v", received["unsubscribe_url"])
	}
	incident, _ := received["incident"].(map[string]any)
	if incident == nil || incident["title"] != "Checkout is failing" {
		t.Errorf("incident = %v", received["incident"])
	}
	statusPage, _ := received["status_page"].(map[string]any)
	if statusPage == nil || statusPage["url"] != "https://cairn.example.com/status/acme" {
		t.Errorf("status_page = %v", received["status_page"])
	}
}

// A failed delivery is recorded against the subscriber rather than swallowed:
// "did my customers hear about the outage" is asked afterwards, and the answer
// has to be in a row somewhere.
func TestFailedBulletinIsRecorded(t *testing.T) {
	useFakeSMTP(t)
	vault := testSubscriberVault(t)
	page := testPage()

	// A URL that refuses the connection: the webhook path, because it fails
	// fast, where an unreachable SMTP host would spend the dial timeout.
	sub := subscriber(t, vault, page, model.SubscriberWebhook, "http://127.0.0.1:1/hook", true)
	store := newFakeSubscriberStore(page, sub)
	relay := testRelay(t, store, vault)

	relay.Announce(sampleAnnouncement(page))
	deliveries := store.waitForDeliveries(t, 1)

	if deliveries[0].Outcome != model.DeliveryFailed {
		t.Errorf("outcome = %s, want failed", deliveries[0].Outcome)
	}
	if deliveries[0].Error == "" {
		t.Error("a failed delivery recorded no reason")
	}
	// Filed against the incident, which is the only thing on the row that says
	// what this message was about.
	if deliveries[0].IncidentID == nil {
		t.Error("the delivery is not attributable to its incident")
	}
}

// The subject is read in a list of subject lines, so it has to lead with what
// changed rather than with the page's name doing all the work.
func TestAnnouncementSubjects(t *testing.T) {
	page := testPage()
	a := sampleAnnouncement(page)

	for _, tc := range []struct {
		event string
		want  string
	}{
		{model.EventIncidentOpened, "[Acme Status] Checkout is failing"},
		{model.EventIncidentUpdated, "[Acme Status] Update: Checkout is failing"},
		{model.EventIncidentResolved, "[Acme Status] Resolved: Checkout is failing"},
	} {
		a.EventType = tc.event
		if subject, _ := announcementText(page, a); subject != tc.want {
			t.Errorf("%s: subject = %q, want %q", tc.event, subject, tc.want)
		}
	}
}

// A page on its own hostname links to itself, not through the install's base
// URL — that is what the custom domain is for, and a customer following a link
// to somebody's internal dashboard hostname is the failure it prevents.
func TestPageURLPrefersTheCustomDomain(t *testing.T) {
	page := testPage()
	if got := pageURL("https://cairn.example.com", page); got != "https://cairn.example.com/status/acme" {
		t.Errorf("page URL = %q", got)
	}

	page.CustomDomain = "status.acme.example"
	if got := pageURL("https://cairn.example.com", page); got != "https://status.acme.example" {
		t.Errorf("custom domain page URL = %q", got)
	}
}

func sampleAnnouncement(page model.StatusPage) Announcement {
	started := time.Now().UTC().Add(-time.Hour)
	return Announcement{
		EventType: model.EventIncidentUpdated,
		PageIDs:   []model.ID{page.ID},
		Incident: Incident{
			ID:        model.NewID(),
			Title:     "Checkout is failing",
			State:     "identified",
			Impact:    "major",
			StartedAt: started,
		},
		Update:     "We have identified the cause.",
		OccurredAt: time.Now().UTC(),
		BaseURL:    "https://cairn.example.com",
	}
}

func decodeMailBody(t *testing.T, message string) string {
	t.Helper()

	_, encoded, found := strings.Cut(message, "\r\n\r\n")
	if !found {
		t.Fatalf("no header/body separator in:\n%s", message)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(encoded), "\r\n", ""))
	if err != nil {
		t.Fatalf("body is not base64: %v", err)
	}
	return string(decoded)
}

func unsubscribeLinkOf(t *testing.T, message string) string {
	t.Helper()

	for _, line := range strings.Split(message, "\r\n") {
		if rest, ok := strings.CutPrefix(line, "List-Unsubscribe: <"); ok {
			return strings.TrimSuffix(rest, ">")
		}
	}
	t.Fatalf("no List-Unsubscribe header in:\n%s", message)
	return ""
}
