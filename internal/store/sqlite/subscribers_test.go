package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Subscriber storage, and the one query whose mistake is invisible from inside
// the process: ConfirmedSubscribers returning somebody who never confirmed.
// "Everyone who asked" and "everyone who agreed" are one clause apart in SQL and
// a different product in a mailbox.

func testPage(t *testing.T, s *Store, slug string) model.StatusPage {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Millisecond)
	page := model.StatusPage{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Slug: slug, Title: "Acme Status", Published: true,
		Visibility: model.VisibilityPublic, Timezone: "UTC",
		SubscriptionsEnabled: true, UptimeBarDays: 90,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateStatusPage(t.Context(), page); err != nil {
		t.Fatalf("create status page: %v", err)
	}
	return page
}

func testSubscriber(page model.StatusPage, target string, confirmed bool) model.Subscriber {
	sub := model.Subscriber{
		ID:                     model.NewID(),
		StatusPageID:           page.ID,
		OrgID:                  page.OrgID,
		Channel:                model.SubscriberEmail,
		SealedTarget:           []byte("sealed:" + target),
		TargetHash:             []byte(target),
		SealedUnsubscribeToken: []byte("sealed-token:" + target),
		CreatedAt:              time.Now().UTC().Truncate(time.Millisecond),
	}
	// Token hashes are per-subscriber rather than per-address: both columns are
	// uniquely indexed, and real tokens are 256 random bits, so two rows sharing
	// one is a thing that cannot happen outside a fixture.
	sub.ConfirmTokenHash = []byte("confirm:" + sub.ID.String())
	sub.UnsubscribeTokenHash = []byte("unsubscribe:" + sub.ID.String())
	if confirmed {
		at := time.Now().UTC().Truncate(time.Millisecond)
		sub.ConfirmedAt = &at
	}
	return sub
}

func TestConfirmedSubscribersOnlyReturnsConfirmedOnes(t *testing.T) {
	t.Parallel()

	s := open(t)
	page := testPage(t, s, "acme")
	other := testPage(t, s, "other")

	yes := testSubscriber(page, "yes@example.com", true)
	no := testSubscriber(page, "never-agreed@example.com", false)
	elsewhere := testSubscriber(other, "elsewhere@example.com", true)
	for _, sub := range []model.Subscriber{yes, no, elsewhere} {
		if err := s.CreateSubscriber(t.Context(), sub); err != nil {
			t.Fatalf("create subscriber: %v", err)
		}
	}

	confirmed, err := s.ConfirmedSubscribers(t.Context(), page.ID)
	if err != nil {
		t.Fatalf("confirmed subscribers: %v", err)
	}
	if len(confirmed) != 1 {
		t.Fatalf("got %d confirmed subscribers, want 1", len(confirmed))
	}
	if confirmed[0].ID != yes.ID {
		t.Error("the wrong subscriber came back")
	}

	// Both envelopes travel with the row: without them the relay has an address
	// it cannot read and a link it cannot render.
	if string(confirmed[0].SealedTarget) != "sealed:yes@example.com" {
		t.Errorf("sealed target = %q", confirmed[0].SealedTarget)
	}
	if string(confirmed[0].SealedUnsubscribeToken) != "sealed-token:yes@example.com" {
		t.Errorf("sealed unsubscribe token = %q", confirmed[0].SealedUnsubscribeToken)
	}

	// And the list a page's operator reads is not filtered: they are entitled to
	// see who has not confirmed yet, which is the whole reason that column is on
	// the response.
	all, err := s.ListSubscribers(t.Context(), page.ID, 50)
	if err != nil {
		t.Fatalf("list subscribers: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("the operator's list returned %d of 2 subscribers", len(all))
	}
}

// The token round-trips through the confirmation lookup, which is the path an
// unauthenticated stranger takes with the token as their whole credential.
func TestSubscriberByToken(t *testing.T) {
	t.Parallel()

	s := open(t)
	page := testPage(t, s, "acme")
	sub := testSubscriber(page, "reader@example.com", false)
	if err := s.CreateSubscriber(t.Context(), sub); err != nil {
		t.Fatalf("create subscriber: %v", err)
	}

	found, err := s.SubscriberByToken(t.Context(), sub.ConfirmTokenHash, sub.ConfirmTokenHash)
	if err != nil {
		t.Fatalf("by confirm token: %v", err)
	}
	if found.ID != sub.ID || found.Confirmed() {
		t.Errorf("resolved to %v, confirmed=%v", found.ID, found.Confirmed())
	}

	if err := s.ConfirmSubscriber(t.Context(), sub.ID, time.Now().UTC()); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// Confirming burns the token, so the same link cannot be replayed.
	if _, err := s.SubscriberByToken(t.Context(), sub.ConfirmTokenHash, sub.ConfirmTokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a spent confirmation token still resolves: %v", err)
	}

	// The unsubscribe token survives confirmation, because it has to work for
	// as long as the subscription does.
	found, err = s.SubscriberByToken(t.Context(), sub.UnsubscribeTokenHash, sub.UnsubscribeTokenHash)
	if err != nil {
		t.Fatalf("by unsubscribe token: %v", err)
	}
	if !found.Confirmed() {
		t.Error("the row came back unconfirmed after being confirmed")
	}
}

// A subscription written before migration 0005 has a hash and no envelope, so
// its link cannot be rendered. Delivery issues a fresh token rather than sending
// a message with no way out of it, and both halves have to move together.
func TestReissueUnsubscribeToken(t *testing.T) {
	t.Parallel()

	s := open(t)
	page := testPage(t, s, "acme")
	sub := testSubscriber(page, "old@example.com", true)
	sub.SealedUnsubscribeToken = nil
	if err := s.CreateSubscriber(t.Context(), sub); err != nil {
		t.Fatalf("create subscriber: %v", err)
	}

	if err := s.ReissueUnsubscribeToken(t.Context(), sub.ID, []byte("new-hash"), []byte("new-envelope")); err != nil {
		t.Fatalf("reissue: %v", err)
	}

	// The new hash resolves.
	found, err := s.SubscriberByToken(t.Context(), []byte("new-hash"), []byte("new-hash"))
	if err != nil {
		t.Fatalf("by reissued token: %v", err)
	}
	if string(found.SealedUnsubscribeToken) != "new-envelope" {
		t.Errorf("sealed token = %q", found.SealedUnsubscribeToken)
	}

	// And the old one does not: a token that still worked after being replaced
	// would mean two live unsubscribe links for one subscription, only one of
	// which anybody knows about.
	if _, err := s.SubscriberByToken(t.Context(), sub.UnsubscribeTokenHash, sub.UnsubscribeTokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the replaced token still resolves: %v", err)
	}

	if err := s.ReissueUnsubscribeToken(t.Context(), model.NewID(), []byte("x"), []byte("y")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("reissuing for a missing subscriber = %v, want ErrNotFound", err)
	}
}

// One address per page, and the second attempt is a conflict rather than a
// second row — which is what stops a stranger mailing somebody repeatedly by
// re-submitting the form.
func TestSubscriberUniquenessPerPage(t *testing.T) {
	t.Parallel()

	s := open(t)
	page := testPage(t, s, "acme")
	other := testPage(t, s, "other")

	first := testSubscriber(page, "reader@example.com", false)
	if err := s.CreateSubscriber(t.Context(), first); err != nil {
		t.Fatalf("create subscriber: %v", err)
	}

	again := testSubscriber(page, "reader@example.com", false)
	if err := s.CreateSubscriber(t.Context(), again); !errors.Is(err, store.ErrConflict) {
		t.Errorf("second subscription = %v, want ErrConflict", err)
	}

	// The same address on a different page is a different subscription.
	elsewhere := testSubscriber(other, "reader@example.com", false)
	if err := s.CreateSubscriber(t.Context(), elsewhere); err != nil {
		t.Errorf("the same address on another page was refused: %v", err)
	}
}
