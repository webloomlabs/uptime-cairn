package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
)

// Delivery to status page subscribers.
//
// A separate relay rather than a fourteenth channel type, and the reason is who
// is on the other end. A notification channel is aimed at the operator: it is
// configured by them, it can be as loud as they like, and if it breaks they are
// the one who finds out. A status page subscriber is a stranger who typed their
// address into somebody else's website. That difference decides everything here:
//
//   - Nothing is sent to an unconfirmed address, ever. Double opt-in is not a
//     setting.
//   - Every message carries a working one-click unsubscribe link. A message
//     without one is not sent at all — a mail somebody cannot get out of is how a
//     status page ends up on a blocklist, taking the operator's domain with it.
//   - The address is opened for exactly as long as it takes to hand it to the
//     mail server, and never logged. What the log holds is a subscriber id.
//   - Delivery is recorded whether it worked or not, like every other delivery in
//     this package, because "did my customers hear about the outage" is a
//     question somebody asks afterwards.

// Fan-out policy.
const (
	// bulletinQueue is how many bulletins can be waiting. Two orders of
	// magnitude smaller than the alert queue and deliberately so: a bulletin is
	// something a human typed into an incident timeline, so the arrival rate is
	// human, while each one can expand into thousands of messages.
	bulletinQueue = 256

	// bulletinWorkers is how many bulletins expand at once. Kept low because the
	// expansion below is where the real concurrency is, and because two incident
	// updates landing in the same second is already an unusual day.
	bulletinWorkers = 2

	// fanOut is how many recipients of one bulletin are in flight together. An
	// SMTP conversation can take thirty seconds and a page can have thousands of
	// subscribers, so this is the number that decides whether a large list takes
	// minutes or hours — and how many sockets the mail server sees at once.
	fanOut = 8

	// bulletinAttempts includes the first, and is one fewer than a channel gets.
	// A retry on this path is multiplied by the size of the subscriber list, so
	// the third attempt a channel can afford is a thousand extra messages here.
	// A permanent failure is recorded against the subscriber and the operator
	// re-posts the update, which is a decision they should be making anyway.
	bulletinAttempts = 2

	// bulletinRetryAfter is the pause before the second attempt.
	bulletinRetryAfter = 5 * time.Second
)

// eventSubscriptionConfirmation is the event_type a confirmation delivery is
// recorded under.
//
// Deliberately not in model.AllEventTypes: that list mirrors the frozen spec's
// EventType enum, which is what a notification channel subscribes to, and a
// confirmation is not something anybody can subscribe to. It is only ever
// written into the delivery log, whose column is free text.
const eventSubscriptionConfirmation = "subscription.confirmation"

// SubscriberStore is what subscriber delivery needs from persistence. Declared
// here, by the consumer, so this package never names a backend (ADR-002).
type SubscriberStore interface {
	GetStatusPage(ctx context.Context, id model.ID) (model.StatusPage, error)

	// ConfirmedSubscribers returns only the addresses that completed double
	// opt-in. The filter belongs to the query rather than to this package —
	// see the note on the implementation.
	ConfirmedSubscribers(ctx context.Context, pageID model.ID) ([]model.Subscriber, error)

	// ReissueUnsubscribeToken replaces both halves of a subscriber's token, for
	// rows written before the envelope column existed.
	ReissueUnsubscribeToken(ctx context.Context, id model.ID, hash, sealed []byte) error

	RecordDelivery(ctx context.Context, d model.NotificationDelivery) error
}

// Relay delivers to status page subscribers.
type Relay struct {
	store  SubscriberStore
	vault  *secrets.Vault
	sender *Sender
	log    *slog.Logger

	queue chan bulletin
	wg    sync.WaitGroup

	mu      sync.Mutex
	dropped uint64

	// backoff is the pause before the second attempt, indirected so a test can
	// exercise the retry path without waiting out a real one.
	backoff func() time.Duration
}

// NewRelay builds one. Start must be called before anything is delivered.
//
// The vault is the same one the API seals a subscriber's address with, so the
// AAD binding an envelope to (org, subscribers, target, id) is computed in one
// place — a blob moved onto another subscriber's row fails to open rather than
// unsubscribing the wrong person.
func NewRelay(store SubscriberStore, vault *secrets.Vault, sender *Sender, log *slog.Logger) *Relay {
	return &Relay{
		store:  store,
		vault:  vault,
		sender: sender,
		log:    log,
		queue:  make(chan bulletin, bulletinQueue),

		backoff: func() time.Duration { return bulletinRetryAfter },
	}
}

// Start launches the workers and returns immediately.
func (r *Relay) Start(ctx context.Context) {
	for i := 0; i < bulletinWorkers; i++ {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case b := <-r.queue:
					r.run(ctx, b)
				}
			}
		}()
	}
}

// Wait blocks until the workers have stopped.
func (r *Relay) Wait() { r.wg.Wait() }

// Dropped is how many bulletins the queue could not hold since start.
func (r *Relay) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Confirmation is the double opt-in message, and the only one a relay sends to
// an unconfirmed address.
type Confirmation struct {
	Page       model.StatusPage
	Subscriber model.Subscriber

	// Target is the plaintext address, held by the caller because it has just
	// arrived in a request body. It is not read back out of the store for this:
	// the envelope has been written but the subscription has not been confirmed,
	// and opening it again would be work for nothing.
	Target string

	// Token and UnsubscribeToken are the plaintexts, which exist only in the
	// request that generated them. The store holds a hash of the first and an
	// envelope around the second.
	Token            string
	UnsubscribeToken string

	BaseURL string
}

// Announcement is one thing the pages an incident is attached to should tell
// their subscribers.
type Announcement struct {
	EventType string

	// PageIDs are the status pages the incident names. A subscriber hears about
	// an incident because their page was attached to it, not because the
	// incident touched a monitor they cannot see.
	PageIDs  []model.ID
	Incident Incident

	// Update is the timeline entry the operator wrote. It is the only part of
	// the message that could not have been composed in advance, so it is quoted
	// rather than summarised.
	Update string

	OccurredAt time.Time
	BaseURL    string
}

// bulletin is one queued unit of work: exactly one of the two is set.
type bulletin struct {
	confirmation *Confirmation
	announcement *Announcement
}

// Confirm queues the confirmation message for a new subscription.
//
// Never blocks, and never reports back. The subscribe endpoint answers the same
// way whether the address was new, already subscribed, or undeliverable — an
// endpoint that answers differently is a membership oracle for somebody else's
// customer list.
func (r *Relay) Confirm(c Confirmation) { r.enqueue(bulletin{confirmation: &c}) }

// Announce queues an incident update for every confirmed subscriber of the pages
// it names.
func (r *Relay) Announce(a Announcement) { r.enqueue(bulletin{announcement: &a}) }

func (r *Relay) enqueue(b bulletin) {
	select {
	case r.queue <- b:
	default:
		r.mu.Lock()
		r.dropped++
		total := r.dropped
		r.mu.Unlock()
		r.log.Error("status page bulletin queue full, message dropped", "dropped_total", total)
	}
}

func (r *Relay) run(ctx context.Context, b bulletin) {
	switch {
	case b.confirmation != nil:
		r.confirm(ctx, *b.confirmation)
	case b.announcement != nil:
		r.announce(ctx, *b.announcement)
	}
}

func (r *Relay) confirm(ctx context.Context, c Confirmation) {
	if c.BaseURL == "" {
		r.suppress(ctx, c.Subscriber, eventSubscriptionConfirmation, nil, noBaseURL)
		return
	}

	page := c.Page
	body := confirmationBody(page, c)
	r.send(ctx, page, c.Subscriber, bulletinMessage{
		eventType: eventSubscriptionConfirmation,
		subject:   "Confirm your subscription to " + page.Title,
		body:      body,
		payload: map[string]any{
			"event":           eventSubscriptionConfirmation,
			"status_page":     pagePayload(page, c.BaseURL),
			"confirm_url":     confirmURL(c.BaseURL, c.Token),
			"unsubscribe_url": unsubscribeURL(c.BaseURL, c.UnsubscribeToken),
			"occurred_at":     c.Subscriber.CreatedAt.UTC().Format(time.RFC3339),
		},
		unsubscribeURL: unsubscribeURL(c.BaseURL, c.UnsubscribeToken),
		at:             c.Subscriber.CreatedAt,
		target:         c.Target,
	})
}

// announce expands one bulletin over the pages the incident names.
//
// A page whose subscriptions_enabled has since been turned off still notifies
// the people already on its list. That switch closes the door; it does not evict
// the people inside — they asked, they confirmed, and every message tells them
// how to leave. Silently dropping them would mean an operator turning off a
// signup form and, without being told, also cancelling notifications their
// customers are relying on.
func (r *Relay) announce(ctx context.Context, a Announcement) {
	for _, pageID := range a.PageIDs {
		page, err := r.store.GetStatusPage(ctx, pageID)
		if err != nil {
			r.log.Error("load status page for a bulletin", "page", pageID.String(), "error", err)
			continue
		}

		subscribers, err := r.store.ConfirmedSubscribers(ctx, pageID)
		if err != nil {
			r.log.Error("load subscribers for a bulletin", "page", page.Slug, "error", err)
			continue
		}
		if len(subscribers) == 0 {
			continue
		}

		r.log.Info("announcing to status page subscribers",
			"page", page.Slug, "event", a.EventType, "subscribers", len(subscribers))

		subject, body := announcementText(page, a)
		payload := map[string]any{
			"event":       a.EventType,
			"status_page": pagePayload(page, a.BaseURL),
			"incident":    incidentPayload(a.Incident),
			"update":      a.Update,
			"occurred_at": a.OccurredAt.UTC().Format(time.RFC3339),
		}

		incidentID := a.Incident.ID
		r.fanOut(ctx, page, subscribers, bulletinMessage{
			eventType:  a.EventType,
			incidentID: &incidentID,
			subject:    subject,
			body:       body,
			payload:    payload,
			at:         a.OccurredAt,
			baseURL:    a.BaseURL,
		})
	}
}

// fanOut delivers one bulletin to a page's subscribers, bounded.
//
// The message is shared by value and personalised in prepareAndSend, which is
// where the one field that differs per recipient — the unsubscribe link — is
// filled in. Rendering the whole message per subscriber would be the same text a
// thousand times over; rendering the link once would send a thousand people the
// same link, and the second mistake is the one nobody notices until somebody
// else is unsubscribed.
func (r *Relay) fanOut(ctx context.Context, page model.StatusPage, subscribers []model.Subscriber, msg bulletinMessage) {
	work := make(chan model.Subscriber)
	var wg sync.WaitGroup

	for i := 0; i < fanOut; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sub := range work {
				r.prepareAndSend(ctx, page, sub, msg)
			}
		}()
	}

	for _, sub := range subscribers {
		select {
		case <-ctx.Done():
		case work <- sub:
			continue
		}
		break
	}
	close(work)
	wg.Wait()
}

// prepareAndSend opens what this recipient needs and hands it on.
//
// Two things are opened here and nowhere else: the address, and the unsubscribe
// token. Both are envelopes bound to this subscriber's row, and both stay on
// this stack — the message that goes out carries them, and the delivery record
// that comes back does not.
func (r *Relay) prepareAndSend(ctx context.Context, page model.StatusPage, sub model.Subscriber, msg bulletinMessage) {
	if msg.baseURL == "" {
		r.suppress(ctx, sub, msg.eventType, msg.incidentID, noBaseURL)
		return
	}

	target, err := r.vault.Open(sub.OrgID[:], sub.ID[:], sub.SealedTarget)
	if err != nil {
		// Almost always a restored database without its key file. Recorded
		// against the subscriber rather than logged with their address, which is
		// the value we could not read in the first place.
		r.fail(ctx, sub, msg, "cannot read this subscriber's stored address: "+err.Error())
		return
	}

	token, err := r.unsubscribeToken(ctx, sub)
	if err != nil {
		// Deliberately fatal for this message. Sending anyway would mean a
		// notification with no way out of it, which is the one thing this relay
		// will not do.
		r.fail(ctx, sub, msg, "cannot render an unsubscribe link: "+err.Error())
		return
	}

	msg.target = string(target)
	msg.unsubscribeURL = unsubscribeURL(msg.baseURL, token)
	msg.body = strings.ReplaceAll(msg.body, unsubscribePlaceholder, msg.unsubscribeURL)
	if msg.payload != nil {
		// Copied rather than mutated: the payload map is shared across every
		// recipient of this bulletin, and the unsubscribe URL is the one field
		// that is not.
		personal := make(map[string]any, len(msg.payload)+1)
		for key, value := range msg.payload {
			personal[key] = value
		}
		personal["unsubscribe_url"] = msg.unsubscribeURL
		msg.payload = personal
	}

	r.send(ctx, page, sub, msg)
}

// unsubscribeToken renders the subscriber's token, issuing a new one where the
// stored row predates the envelope column.
//
// The reissue is a write on the delivery path, which is worth being explicit
// about: it happens at most once per legacy subscriber, and the alternative is
// either a message with no unsubscribe link or a subscriber who silently never
// hears anything again.
func (r *Relay) unsubscribeToken(ctx context.Context, sub model.Subscriber) (string, error) {
	if len(sub.SealedUnsubscribeToken) > 0 {
		plain, err := r.vault.Open(sub.OrgID[:], sub.ID[:], sub.SealedUnsubscribeToken)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}

	token, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	sealed, err := r.vault.Seal(sub.OrgID[:], sub.ID[:], []byte(token))
	if err != nil {
		return "", err
	}
	if err := r.store.ReissueUnsubscribeToken(ctx, sub.ID, auth.HashToken(token), sealed); err != nil {
		return "", err
	}
	r.log.Info("issued an unsubscribe token to a subscription that predates one", "subscriber", sub.ID.String())
	return token, nil
}

// bulletinMessage is one rendered bulletin for one recipient.
type bulletinMessage struct {
	eventType  string
	incidentID *model.ID
	subject    string
	body       string
	payload    map[string]any

	target         string
	unsubscribeURL string
	baseURL        string
	at             time.Time
}

// send performs the attempts for one recipient and records the outcome.
func (r *Relay) send(ctx context.Context, page model.StatusPage, sub model.Subscriber, msg bulletinMessage) {
	started := time.Now()

	var (
		err     error
		receipt Receipt
		attempt int
	)
	for attempt = 1; attempt <= bulletinAttempts; attempt++ {
		receipt, err = r.attempt(ctx, sub, msg)
		if err == nil || !retryable(err) {
			break
		}
		if attempt == bulletinAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.backoff()):
		}
	}

	elapsed := float64(time.Since(started).Microseconds()) / 1000.0
	outcome, failure := model.DeliverySucceeded, ""
	switch {
	case errors.Is(err, ErrNotApplicable):
		// Nothing is wrong with this subscriber: the install has no relay to
		// send from. Recorded as suppressed rather than failed, because a row
		// saying "failed" sends the operator to debug a mail server they never
		// configured.
		outcome, failure = model.DeliverySuppressed, noRelay
		r.log.Warn("status page bulletin suppressed",
			"page", page.Slug, "subscriber", sub.ID.String(), "reason", noRelay)
	case err != nil:
		outcome, failure = model.DeliveryFailed, err.Error()
		// The page and the subscriber id, never the address: the log is the one
		// place a customer's address must not end up, and it is the easiest
		// place for it to.
		r.log.Error("status page bulletin failed",
			"page", page.Slug, "subscriber", sub.ID.String(),
			"channel", sub.Channel, "attempts", attempt, "error", err)
	}

	r.record(ctx, model.NotificationDelivery{
		ID:              model.NewID(),
		OrgID:           sub.OrgID,
		IncidentID:      msg.incidentID,
		EventType:       msg.eventType,
		Outcome:         outcome,
		Error:           failure,
		DurationMs:      &elapsed,
		Attempt:         attempt,
		RenderedPayload: truncate(receipt.Payload, maxRecordedPayload),
		CreatedAt:       time.Now().UTC(),
	})
}

// attempt is one delivery over whichever transport this subscriber chose.
func (r *Relay) attempt(ctx context.Context, sub model.Subscriber, msg bulletinMessage) (Receipt, error) {
	recorded := recordedPayload(sub.Channel, msg.target, msg.subject)

	switch sub.Channel {
	case model.SubscriberWebhook:
		payload, err := json.Marshal(msg.payload)
		if err != nil {
			return Receipt{}, err
		}
		return r.sender.do(ctx, request{
			url:         msg.target,
			contentType: "application/json",
			body:        payload,
			verifyTLS:   true,
			record:      recorded,
		})

	default:
		endpoint, ok := instanceRelay()
		if !ok {
			// Not a failure of this message so much as of the install: nothing
			// can be delivered until an operator configures a relay. Said in
			// those words, because the delivery log is where they will look.
			return Receipt{}, ErrNotApplicable
		}
		if err := deliverSMTP(ctx, endpoint, []string{msg.target}, composeBulletin(endpoint, msg)); err != nil {
			return Receipt{}, err
		}
		return Receipt{Payload: recorded}, nil
	}
}

// recordedPayload is what a subscriber delivery puts in the delivery log, and it
// is deliberately not the message.
//
// A rendered bulletin carries two things that must not be written to
// notification_deliveries, which is a plaintext column subject to retention
// rather than to the key hierarchy: the subscriber's address, which is encrypted
// at rest precisely because it is replayed (data model §12.1) and which §12.5
// keeps out of any plaintext index on the instance; and their unsubscribe token,
// which is a live credential. Logging the message would undo both, and the
// delivery log is not where anybody would think to look for either.
//
// What is kept is what the question actually needs. "Did this go out, and what
// did it say" is answered by the subject and by enough of the address for an
// operator to recognise their own subscriber — the same mask the subscriber list
// endpoint returns, for the same reason.
func recordedPayload(channel, target, subject string) string {
	return "to " + model.MaskTarget(channel, target) + "\nsubject: " + subject
}

// noBaseURL is the reason a bulletin is suppressed when the install does not
// know its own address.
//
// Every message this relay sends carries two links a stranger has to be able to
// follow — the page, and the way off the list — and neither can be built from a
// relative path in a mailbox. Deriving one from the request that happened to
// trigger the incident would put whatever hostname the operator's browser used
// into a customer's inbox, which is worse than not sending: an internal hostname
// in a public message is a leak, and a broken link is a support ticket.
const noBaseURL = "the instance base URL is not configured, so a subscriber " +
	"message would carry no working links: set general.base_url in /settings"

// noRelay is the reason when there is no mail server to send through. Said in
// these words because the delivery log is where an operator looks first, and
// "connection refused" would be a lie about a connection nobody attempted.
const noRelay = "no instance SMTP relay is configured, so nothing can be " +
	"delivered to an email subscriber: set the relay in /settings"

func (r *Relay) suppress(ctx context.Context, sub model.Subscriber, eventType string, incidentID *model.ID, reason string) {
	r.log.Warn("status page bulletin suppressed", "subscriber", sub.ID.String(), "reason", reason)
	r.record(ctx, model.NotificationDelivery{
		ID:         model.NewID(),
		OrgID:      sub.OrgID,
		IncidentID: incidentID,
		EventType:  eventType,
		Outcome:    model.DeliverySuppressed,
		Error:      reason,
		Attempt:    1,
		CreatedAt:  time.Now().UTC(),
	})
}

func (r *Relay) fail(ctx context.Context, sub model.Subscriber, msg bulletinMessage, reason string) {
	r.log.Error("status page bulletin not sent", "subscriber", sub.ID.String(), "error", reason)
	r.record(ctx, model.NotificationDelivery{
		ID:         model.NewID(),
		OrgID:      sub.OrgID,
		IncidentID: msg.incidentID,
		EventType:  msg.eventType,
		Outcome:    model.DeliveryFailed,
		Error:      reason,
		Attempt:    1,
		CreatedAt:  time.Now().UTC(),
	})
}

func (r *Relay) record(ctx context.Context, d model.NotificationDelivery) {
	// Detached from the request or the worker's context on purpose, for the same
	// reason the channel path does it: the record of a delivery that has already
	// happened must not be lost because the thing that triggered it went away.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := r.store.RecordDelivery(recordCtx, d); err != nil {
		r.log.Error("record subscriber delivery", "error", err)
	}
}

// --------------------------------------------------------------------------
// Composition
// --------------------------------------------------------------------------

// unsubscribePlaceholder stands in the shared body text until the per-recipient
// link is known. The body is rendered once per bulletin and the link once per
// recipient, and this is the seam between them.
const unsubscribePlaceholder = "\x00unsubscribe\x00"

func confirmURL(baseURL, token string) string {
	return strings.TrimSuffix(baseURL, "/") + "/subscriptions/confirm/" + token
}

func unsubscribeURL(baseURL, token string) string {
	return strings.TrimSuffix(baseURL, "/") + "/subscriptions/unsubscribe/" + token
}

func pageURL(baseURL string, page model.StatusPage) string {
	if page.CustomDomain != "" {
		return "https://" + page.CustomDomain
	}
	return strings.TrimSuffix(baseURL, "/") + "/status/" + page.Slug
}

func pagePayload(page model.StatusPage, baseURL string) map[string]any {
	return map[string]any{
		"id":    page.ID.String(),
		"slug":  page.Slug,
		"title": page.Title,
		"url":   pageURL(baseURL, page),
	}
}

func incidentPayload(in Incident) map[string]any {
	out := map[string]any{
		"id":         in.ID.String(),
		"title":      in.Title,
		"state":      in.State,
		"impact":     in.Impact,
		"started_at": in.StartedAt.UTC().Format(time.RFC3339),
	}
	if in.ResolvedAt != nil {
		out["resolved_at"] = in.ResolvedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func confirmationBody(page model.StatusPage, c Confirmation) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Somebody asked for updates from %s to be sent to this address.\n\n", page.Title)
	b.WriteString("Confirm the subscription:\n")
	b.WriteString(confirmURL(c.BaseURL, c.Token) + "\n\n")
	// Said plainly rather than politely. The person reading this may not have
	// asked for it, and the useful thing to tell them is that ignoring it works.
	b.WriteString("If that was not you, ignore this message. Nothing further is sent\n")
	b.WriteString("to this address unless the link above is followed.\n\n")
	fmt.Fprintf(&b, "Status page: %s\n", pageURL(c.BaseURL, page))
	return b.String()
}

// announcementText renders the message body once per bulletin. The subject leads
// with what changed, because it is read in a list of subject lines.
func announcementText(page model.StatusPage, a Announcement) (subject, body string) {
	in := a.Incident

	switch a.EventType {
	case model.EventIncidentResolved:
		subject = fmt.Sprintf("[%s] Resolved: %s", page.Title, in.Title)
	case model.EventIncidentUpdated:
		subject = fmt.Sprintf("[%s] Update: %s", page.Title, in.Title)
	default:
		subject = fmt.Sprintf("[%s] %s", page.Title, in.Title)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", in.Title)
	fmt.Fprintf(&b, "Status: %s\nImpact: %s\nStarted: %s\n",
		in.State, in.Impact, in.StartedAt.UTC().Format(time.RFC3339))
	if in.ResolvedAt != nil {
		fmt.Fprintf(&b, "Resolved: %s\n", in.ResolvedAt.UTC().Format(time.RFC3339))
	}
	if a.Update != "" {
		fmt.Fprintf(&b, "\n%s\n", a.Update)
	}
	fmt.Fprintf(&b, "\nFollow this incident: %s\n", pageURL(a.BaseURL, page))
	fmt.Fprintf(&b, "Unsubscribe: %s\n", unsubscribePlaceholder)
	return subject, b.String()
}

// composeBulletin builds one text/plain message for one subscriber.
//
// Close to composeMail and deliberately not the same function. That one is built
// from an Event and addresses a list of operators; this one addresses exactly one
// stranger and carries List-Unsubscribe, which is the header that decides whether
// a mail client offers the one-click button — and whether the message is treated
// as bulk mail somebody consented to or as something to be filtered.
func composeBulletin(endpoint relay, msg bulletinMessage) string {
	from := endpoint.from
	if endpoint.fromName != "" {
		from = (&mail.Address{Name: endpoint.fromName, Address: endpoint.from}).String()
	}

	at := msg.at
	if at.IsZero() {
		at = time.Now()
	}

	var headers strings.Builder
	fmt.Fprintf(&headers, "From: %s\r\n", from)
	fmt.Fprintf(&headers, "To: %s\r\n", msg.target)
	fmt.Fprintf(&headers, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.subject))
	fmt.Fprintf(&headers, "Date: %s\r\n", at.Format(time.RFC1123Z))
	fmt.Fprintf(&headers, "Message-ID: <%s@uptime-cairn>\r\n", model.NewID().String())
	if msg.incidentID != nil {
		// Threads every update to one incident into one conversation, which is
		// how a subscriber reads a two-day outage as a story rather than as
		// eleven unrelated messages.
		fmt.Fprintf(&headers, "References: <incident-%s@uptime-cairn>\r\n", msg.incidentID.String())
	}
	// An http link rather than RFC 8058 one-click, and that is not an oversight:
	// one-click POSTs to the URL it is given, and on this API a POST to a
	// subscription token is the *confirm* operation. A client helpfully
	// unsubscribing somebody would confirm them instead.
	fmt.Fprintf(&headers, "List-Unsubscribe: <%s>\r\n", msg.unsubscribeURL)
	headers.WriteString("MIME-Version: 1.0\r\n")
	headers.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	headers.WriteString("Content-Transfer-Encoding: base64\r\n")
	headers.WriteString("Auto-Submitted: auto-generated\r\n")
	headers.WriteString("\r\n")

	headers.WriteString(base64Lines(msg.subject + "\n\n" + msg.body))
	return headers.String()
}
