// Package outbound delivers events to subscribed HTTP endpoints.
//
// It is the machine-readable half of alerting, and it is a separate package from
// internal/notify on purpose. A notification channel is aimed at a person: it
// renders a sentence, chooses a colour, and knows what Slack's block kit wants.
// A webhook is aimed at a program, so it posts the event envelope verbatim,
// signs it, and logs every attempt whether or not anybody is watching. The two
// share the event stream and nothing else, and collapsing them would mean either
// the program receives prose or the person receives JSON.
//
// Three properties are load-bearing:
//
//   - Signed. Every delivery carries an HMAC-SHA256 over the raw body, computed
//     with a per-webhook secret that is encrypted at rest rather than hashed —
//     the distinction in data model §12.1, which only bites at the first
//     delivery if it is got wrong.
//   - Deduplicable. The event id is stable across retries and travels in a
//     header, so a receiver that processed a delivery whose response was lost
//     can recognise the repeat.
//   - Self-limiting. A subscription that has failed enough times in a row
//     disables itself and records when, because a dead endpoint retried forever
//     is a queue that never drains and a bill somebody pays.
package outbound

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
)

// Store is what delivery needs from persistence, named by the consumer.
type Store interface {
	EnabledWebhooks(ctx context.Context) ([]model.Webhook, [][]byte, error)
	RecordWebhookDelivery(ctx context.Context, d model.WebhookDelivery, maxFailures int, at time.Time) error
}

// Vault opens the encrypted secret and header blobs.
type Vault interface {
	Open(orgID, rowID, envelope []byte) ([]byte, error)
}

const (
	// queueDepth is the same bound the notification dispatcher uses and for the
	// same reason: a partition that marks several thousand monitors down inside
	// one scheduler tick is the burst this is sized against, and dropping the
	// tail loudly beats becoming backpressure on heartbeat ingest.
	queueDepth = 4096

	workers     = 4
	maxAttempts = 5

	// maxFailures is how many consecutive failures auto-disable a subscription.
	// Ten is roughly an hour of a receiver being down at the retry schedule
	// below — long enough to survive a deploy, short enough not to be forever.
	maxFailures = 10

	// maxResponseBody is what is kept from a receiver's reply. A receiver that
	// returns a megabyte of HTML on error must not turn one failing webhook into
	// a disk-space incident.
	maxResponseBody = 2048

	requestTimeout = 15 * time.Second
)

// signatureHeader and its companions are the contract a receiver verifies
// against. Named with a vendor prefix because a receiver may be behind a proxy
// that already sets X-Signature for something else.
const (
	signatureHeader = "X-Cairn-Signature"
	eventIDHeader   = "X-Cairn-Event-Id"
	eventTypeHeader = "X-Cairn-Event"
	deliveryHeader  = "X-Cairn-Delivery-Id"
	timestampHeader = "X-Cairn-Timestamp"
)

type job struct {
	hook    model.Webhook
	headers map[string]string
	eventID model.ID
	event   string
	body    []byte
	attempt int
}

// Dispatcher fans events out to subscribed endpoints.
type Dispatcher struct {
	store    Store
	vault    Vault
	client   *http.Client
	insecure *http.Client
	log      *slog.Logger
	instance notify.Instance

	queue chan job
	wg    sync.WaitGroup

	// dropped counts events shed because the queue was full. Counted rather than
	// blocked on: alerting must never become backpressure on ingest.
	dropped atomic.Uint64

	// backoff is injectable so a test does not sleep through five attempts.
	backoff func(attempt int) time.Duration
}

// New returns a dispatcher. Start must be called before it delivers anything.
func New(store Store, vault Vault, instance notify.Instance, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:    store,
		vault:    vault,
		instance: instance,
		log:      log,
		client:   &http.Client{Timeout: requestTimeout},
		insecure: &http.Client{
			Timeout: requestTimeout,
			// One client per TLS policy rather than a transport rebuilt per
			// request: a webhook with verify_tls off is a deliberate choice for
			// an internal endpoint with a private CA, and it should not cost a
			// new connection pool every time it fires.
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // opted into per webhook
		},
		queue:   make(chan job, queueDepth),
		backoff: retryAfter,
	}
}

// Instance identifies the sending install, so the API can build an event with
// the same envelope this delivers.
func (d *Dispatcher) Instance() notify.Instance { return d.instance }

// Start launches the worker pool.
func (d *Dispatcher) Start(ctx context.Context) {
	for range workers {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-d.queue:
					d.attempt(ctx, j)
				}
			}
		}()
	}
}

// Wait blocks until the workers have stopped.
func (d *Dispatcher) Wait() { d.wg.Wait() }

// Publish fans one event out to every subscription that wants it.
//
// Fire-and-forget, like the notification dispatcher: the caller is the ingest
// path, and a slow receiver must not slow down the recording of heartbeats. The
// fan-out itself happens on a goroutine because it reads the subscription list,
// which is a database round trip.
func (d *Dispatcher) Publish(ev notify.Event) {
	go d.fanOut(context.Background(), ev)
}

func (d *Dispatcher) fanOut(ctx context.Context, ev notify.Event) {
	hooks, sealedHeaders, err := d.store.EnabledWebhooks(ctx)
	if err != nil {
		d.log.Error("load webhooks", "error", err)
		return
	}

	var body []byte
	for i, hook := range hooks {
		if !hook.WantsEvent(ev.Type) {
			continue
		}
		if body == nil {
			// Rendered once for the whole fan-out. Every subscriber gets byte
			// -identical bytes, which is what makes the signature verifiable
			// and the event id meaningful as a deduplication key.
			if body, err = Envelope(ev); err != nil {
				d.log.Error("render event envelope", "error", err, "event", ev.Type)
				return
			}
		}

		headers, err := d.openHeaders(hook, sealedHeaders[i])
		if err != nil {
			d.log.Error("open webhook headers", "error", err, "webhook", hook.ID.String())
			continue
		}

		select {
		case d.queue <- job{hook: hook, headers: headers, eventID: ev.ID, event: ev.Type, body: body, attempt: 1}:
		default:
			d.dropped.Add(1)
			d.log.Error("webhook queue full, event dropped",
				"webhook", hook.ID.String(), "event", ev.Type, "dropped_total", d.dropped.Load())
		}
	}
}

// Dropped reports how many deliveries were shed. Surfaced through /metrics,
// because a queue that silently sheds is indistinguishable from one nobody is
// using.
func (d *Dispatcher) Dropped() uint64 { return d.dropped.Load() }

// Redeliver resends a logged delivery's exact body.
//
// Exact rather than regenerated: the reason to press the button is that the
// receiver was broken and has been fixed, and a payload rebuilt from current
// state would describe a world the original event did not. It runs synchronously
// because the caller is a request that wants the outcome.
func (d *Dispatcher) Redeliver(ctx context.Context, hook model.Webhook, sealedHeaders []byte, delivery model.WebhookDelivery) (model.WebhookDelivery, error) {
	// The URL and the headers come from the subscription as it is now, not from
	// the delivery record: a redelivery goes wherever the webhook points today,
	// which is the whole point of having fixed the receiver.
	headers, err := d.openHeaders(hook, sealedHeaders)
	if err != nil {
		return model.WebhookDelivery{}, err
	}

	replayed := d.send(ctx, job{
		hook:    hook,
		headers: headers,
		eventID: delivery.EventID,
		event:   delivery.EventType,
		body:    []byte(delivery.RequestBody),
		attempt: delivery.Attempt + 1,
	})

	// Logged like any other attempt. A redelivery that is not in the log is a
	// redelivery nobody can point at afterwards, and the failure counter has to
	// move for it too or a manual retry against a still-broken endpoint would
	// never trip the auto-disable.
	if err := d.store.RecordWebhookDelivery(ctx, replayed, maxFailures, time.Now().UTC()); err != nil {
		return model.WebhookDelivery{}, err
	}
	return replayed, nil
}

// attempt performs one delivery and schedules a retry if it is owed one.
func (d *Dispatcher) attempt(ctx context.Context, j job) {
	delivery := d.send(ctx, j)

	if delivery.Outcome == model.DeliveryFailed && j.attempt < maxAttempts {
		delivery.NextRetryAt = pointer(delivery.CreatedAt.Add(d.backoff(j.attempt)))
	}
	if err := d.store.RecordWebhookDelivery(ctx, delivery, maxFailures, time.Now().UTC()); err != nil {
		d.log.Error("record webhook delivery", "error", err, "webhook", j.hook.ID.String())
	}
	if delivery.NextRetryAt == nil {
		return
	}

	// Scheduled in memory rather than swept from next_retry_at. The column is
	// written so an operator can see a retry is owed and so a future sweeper has
	// somewhere to look; the timer is what actually fires it in this build, and
	// a restart therefore loses in-flight retries. That is the honest trade for
	// not running a second background loop, and it is stated here rather than
	// discovered.
	go func() {
		timer := time.NewTimer(time.Until(*delivery.NextRetryAt))
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			next := j
			next.attempt++
			select {
			case d.queue <- next:
			default:
				d.dropped.Add(1)
			}
		}
	}()
}

// send performs the request and returns the delivery record, successful or not.
func (d *Dispatcher) send(ctx context.Context, j job) model.WebhookDelivery {
	now := time.Now().UTC()
	delivery := model.WebhookDelivery{
		ID:          model.NewID(),
		WebhookID:   j.hook.ID,
		OrgID:       j.hook.OrgID,
		EventID:     j.eventID,
		EventType:   j.event,
		Attempt:     j.attempt,
		RequestBody: string(j.body),
		Outcome:     model.DeliveryFailed,
		CreatedAt:   now,
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, j.hook.URL, bytes.NewReader(j.body))
	if err != nil {
		delivery.Error = err.Error()
		return delivery
	}

	// The operator's headers go on first and the reserved ones after, so a
	// configured header can add to the request but can never replace the
	// signature or the event identity. A receiver whose deduplication key can be
	// changed by a typo in a settings field is a receiver with no deduplication.
	for name, value := range j.headers {
		request.Header.Set(name, value)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "uptime-cairn/"+d.instance.Version)
	request.Header.Set(eventIDHeader, j.eventID.String())
	request.Header.Set(eventTypeHeader, j.event)
	request.Header.Set(deliveryHeader, delivery.ID.String())
	request.Header.Set(timestampHeader, strconv.FormatInt(now.Unix(), 10))

	if secret, err := d.openSecret(j.hook); err != nil {
		d.log.Error("open webhook secret", "error", err, "webhook", j.hook.ID.String())
	} else if len(secret) > 0 {
		request.Header.Set(signatureHeader, Sign(secret, j.body))
	}

	client := d.client
	if !j.hook.VerifyTLS {
		client = d.insecure
	}

	started := time.Now()
	response, err := client.Do(request)
	elapsed := float64(time.Since(started).Microseconds()) / 1000.0
	delivery.DurationMs = &elapsed

	if err != nil {
		delivery.Error = err.Error()
		return delivery
	}
	defer func() { _ = response.Body.Close() }()

	status := response.StatusCode
	delivery.ResponseStatus = &status
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
	delivery.ResponseBody = string(body)

	if status >= 200 && status < 300 {
		delivery.Outcome = model.DeliverySucceeded
		return delivery
	}
	delivery.Error = fmt.Sprintf("receiver returned %s", response.Status)
	return delivery
}

func (d *Dispatcher) openSecret(hook model.Webhook) ([]byte, error) {
	if len(hook.SecretEncrypted) == 0 {
		return nil, nil
	}
	return d.vault.Open(hook.OrgID[:], hook.ID[:], hook.SecretEncrypted)
}

func (d *Dispatcher) openHeaders(hook model.Webhook, sealed []byte) (map[string]string, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	plain, err := d.vault.Open(hook.OrgID[:], hook.ID[:], sealed)
	if err != nil {
		return nil, err
	}
	var headers map[string]string
	if err := json.Unmarshal(plain, &headers); err != nil {
		return nil, err
	}
	return headers, nil
}

// Sign computes the delivery signature: HMAC-SHA256 over the raw body, hex, with
// the algorithm named in the value so the scheme can change without the header
// name changing.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Envelope renders the EventEnvelope the spec documents.
//
// Hand-built rather than marshalled from a struct with omitempty, because the
// `data` object's shape follows the event type and a receiver branches on
// exactly that: a monitor.* event carries monitor and heartbeat, an incident.*
// event carries incident. A single struct would send both halves with one of
// them null, which is a contract that says less.
func Envelope(ev notify.Event) ([]byte, error) {
	data := map[string]any{}

	if ev.Monitor.ID != (model.ID{}) {
		data["monitor"] = map[string]any{
			"id":          ev.Monitor.ID.String(),
			"name":        ev.Monitor.Name,
			"description": ev.Monitor.Description,
			"type":        ev.Monitor.Type,
			"target":      ev.Monitor.Target,
			"status":      ev.Monitor.Status,
		}
	}
	if ev.Heartbeat != nil {
		data["heartbeat"] = map[string]any{
			"time":             ev.Heartbeat.Time,
			"status":           ev.Heartbeat.Status,
			"response_time_ms": ev.Heartbeat.ResponseTimeMs,
			"message":          ev.Heartbeat.Message,
			"code":             ev.Heartbeat.Code,
			"attempt":          ev.Heartbeat.Attempt,
			"important":        ev.Heartbeat.Important,
		}
	}
	if ev.PreviousStatus != "" {
		data["previous_status"] = ev.PreviousStatus
	}
	if ev.Incident != nil {
		data["incident"] = map[string]any{
			"id":          ev.Incident.ID.String(),
			"title":       ev.Incident.Title,
			"state":       ev.Incident.State,
			"impact":      ev.Incident.Impact,
			"started_at":  ev.Incident.StartedAt,
			"resolved_at": ev.Incident.ResolvedAt,
			"monitor_ids": ev.Incident.MonitorIDs,
		}
	}

	return json.Marshal(map[string]any{
		"id":          ev.ID.String(),
		"type":        ev.Type,
		"occurred_at": ev.OccurredAt,
		"instance": map[string]any{
			"name":     ev.Instance.Name,
			"base_url": ev.Instance.BaseURL,
			"version":  ev.Instance.Version,
		},
		"data": data,
	})
}

// retryAfter is exponential with a ceiling: 1s, 4s, 16s, 64s. A receiver that is
// briefly restarting is caught by the first two; one that is genuinely down is
// not hammered while it recovers.
func retryAfter(attempt int) time.Duration {
	backoff := time.Second
	for range attempt - 1 {
		backoff *= 4
	}
	return min(backoff, 2*time.Minute)
}

func pointer[T any](v T) *T { return &v }
