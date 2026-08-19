package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
	"github.com/webloomlabs/uptime-cairn/internal/telemetry"
)

// The delivery pipeline.
//
// Everything here exists to serve one rule from this package's doc comment:
// delivery failures are recorded, retried, and visible. An alerting system that
// silently fails to alert is the worst failure mode this product has, and it is
// invisible by construction unless somebody builds the visibility — so every
// path below that gives up writes a row saying so, and every path that gives up
// permanently also writes last_error onto the channel, where the UI shows it
// without anybody having to go and read a log.

// ChannelStore is what delivery needs from persistence. Declared here, by the
// consumer, so this package never names a backend (ADR-002).
type ChannelStore interface {
	// ChannelsForMonitor returns the enabled channels attached to a monitor.
	ChannelsForMonitor(ctx context.Context, monitorID model.ID) ([]model.NotificationChannel, error)

	// RecordDelivery appends one attempt, successful or not.
	RecordDelivery(ctx context.Context, d model.NotificationDelivery) error

	// MarkChannelResult updates last_used_at and last_error. An empty
	// deliveryError clears the error, which is what makes a channel that has
	// recovered stop looking broken.
	MarkChannelResult(ctx context.Context, id model.ID, at time.Time, deliveryError string) error
}

// Vault seals and opens a channel's secrets. The AAD binding each blob to its
// row lives in secrets.Vault, which the monitor-credential path uses too — one
// implementation, so the two cannot drift into binding different things (data
// model §12.2).
type Vault struct{ inner *secrets.Vault }

// NewVault wraps a keeper.
func NewVault(keeper *secrets.Keeper) *Vault {
	return &Vault{inner: secrets.NewVault(keeper, "notification_channels", "secrets")}
}

// Seal encrypts the secret half of a channel's configuration. A channel with no
// secrets stores nil rather than an envelope around an empty object, so "has
// secrets" is answerable without decrypting anything.
func (v *Vault) Seal(orgID, channelID model.ID, secret map[string]any) ([]byte, error) {
	if len(secret) == 0 {
		return nil, nil
	}
	plaintext, err := json.Marshal(secret)
	if err != nil {
		return nil, fmt.Errorf("encode channel secrets: %w", err)
	}
	return v.inner.Seal(orgID[:], channelID[:], plaintext)
}

// Open decrypts it.
func (v *Vault) Open(orgID, channelID model.ID, envelope []byte) (map[string]any, error) {
	plaintext, err := v.inner.Open(orgID[:], channelID[:], envelope)
	if err != nil {
		return nil, err
	}
	if len(plaintext) == 0 {
		return map[string]any{}, nil
	}

	var out map[string]any
	if err := json.Unmarshal(plaintext, &out); err != nil {
		return nil, fmt.Errorf("decode channel secrets: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// Delivery policy.
const (
	// queueDepth is how many events can be waiting. Sized against the worst
	// realistic burst — a network partition marking several thousand monitors
	// down within one scheduler tick — because that is exactly when alerting
	// must not be the thing that fails.
	queueDepth = 4096

	// workers send concurrently. Bounded rather than one goroutine per delivery:
	// an SMTP conversation can take thirty seconds, and an unbounded fan-out at
	// 5,000 monitors would open 5,000 sockets.
	workers = 8

	// maxAttempts includes the first. Three is enough to ride out a restarted
	// receiver and few enough that a genuinely broken channel is reported as
	// broken within a couple of minutes rather than retried all afternoon.
	maxAttempts = 3

	// maxLastError bounds what the channel row keeps. A misconfigured endpoint
	// often answers with a full HTML error page, and last_error is read at a
	// glance in a table cell — the delivery log keeps the long version.
	maxLastError = 300
)

// retryAfter is the backoff before attempt n+1.
func retryAfter(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 5 * time.Second
	default:
		return 30 * time.Second
	}
}

type job struct {
	event   Event
	channel *model.NotificationChannel // nil means "resolve the channel list first"
	attempt int
}

// Dispatcher turns events into deliveries.
type Dispatcher struct {
	store    ChannelStore
	vault    *Vault
	sender   *Sender
	instance Instance
	log      *slog.Logger

	queue chan job
	wg    sync.WaitGroup

	// timers tracks scheduled retries so shutdown does not leave one pending.
	mu     sync.Mutex
	timers map[*time.Timer]struct{}

	// dropped counts events the queue could not hold. Surfaced in the log and
	// asserted in tests; a silently dropped alert is the failure this whole
	// package is written to prevent.
	dropped uint64

	// backoff is retryAfter, indirected so a test can exercise the retry path
	// without waiting out a real one.
	backoff func(attempt int) time.Duration
}

// NewDispatcher builds one. Start must be called before Publish does anything.
func NewDispatcher(store ChannelStore, vault *Vault, sender *Sender, instance Instance, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:    store,
		vault:    vault,
		sender:   sender,
		instance: instance,
		log:      log,
		queue:    make(chan job, queueDepth),
		timers:   map[*time.Timer]struct{}{},
		backoff:  retryAfter,
	}
}

// Instance returns the identity stamped on every event this dispatcher sends.
func (d *Dispatcher) Instance() Instance { return d.instance }

// AppriseAvailable reports whether the meta-provider can run here.
func (d *Dispatcher) AppriseAvailable() bool { return d.sender.AppriseAvailable() }

// Start launches the workers and returns immediately.
func (d *Dispatcher) Start(ctx context.Context) {
	for i := 0; i < workers; i++ {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-d.queue:
					d.run(ctx, j)
				}
			}
		}()
	}
}

// Wait blocks until the workers have stopped. Used by tests and shutdown.
func (d *Dispatcher) Wait() {
	d.mu.Lock()
	for timer := range d.timers {
		timer.Stop()
		delete(d.timers, timer)
	}
	d.mu.Unlock()
	d.wg.Wait()
}

// Publish queues an event. Never blocks: it is called from the result-ingest
// path, and an alerting backlog must not become heartbeat backpressure.
//
// A full queue is logged rather than swallowed. It is not recorded as a failed
// delivery, because at that point the recording path would be writing thousands
// of rows through the same single writer the backlog is already waiting on —
// the log line names the count instead.
func (d *Dispatcher) Publish(ev Event) {
	telemetry.Engine.AlertsPublished.Add(1)

	select {
	case d.queue <- job{event: ev, attempt: 1}:
	default:
		telemetry.Engine.AlertsDropped.Add(1)
		d.mu.Lock()
		d.dropped++
		total := d.dropped
		d.mu.Unlock()
		d.log.Error("notification queue full, event dropped",
			"event", ev.Type, "monitor", ev.Monitor.Name, "dropped_total", total)
	}
}

// Dropped is how many events the queue could not hold since start.
func (d *Dispatcher) Dropped() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped
}

// run handles one queue item: either fanning an event out to its channels, or
// performing one attempt against one channel.
func (d *Dispatcher) run(ctx context.Context, j job) {
	if j.channel != nil {
		d.attempt(ctx, j)
		return
	}

	channels, err := d.store.ChannelsForMonitor(ctx, j.event.Monitor.ID)
	if err != nil {
		d.log.Error("load notification channels", "error", err, "monitor", j.event.Monitor.ID.String())
		return
	}
	for i := range channels {
		channel := channels[i]
		if !channel.Enabled || !channel.WantsEvent(j.event.Type) {
			continue
		}
		d.attempt(ctx, job{event: j.event, channel: &channel, attempt: 1})
	}
}

// attempt performs one delivery and decides what happens next.
func (d *Dispatcher) attempt(ctx context.Context, j job) {
	channel := *j.channel
	started := time.Now()

	receipt, err := d.deliver(ctx, channel, j.event)
	elapsed := float64(time.Since(started).Microseconds()) / 1000.0

	outcome := model.DeliverySucceeded
	failure := ""
	switch {
	case errors.Is(err, ErrNotApplicable):
		outcome = model.DeliverySuppressed
		failure = err.Error()
	case err != nil:
		outcome = model.DeliveryFailed
		failure = err.Error()
	}

	d.record(ctx, model.NotificationDelivery{
		ID:              model.NewID(),
		OrgID:           channel.OrgID,
		MonitorID:       monitorIDOf(j.event),
		ChannelID:       &channel.ID,
		EventType:       j.event.Type,
		Outcome:         outcome,
		Error:           failure,
		DurationMs:      &elapsed,
		Attempt:         j.attempt,
		RenderedPayload: receipt.Payload,
		CreatedAt:       time.Now().UTC(),
	})

	if outcome != model.DeliveryFailed {
		// A suppressed delivery is not a failure, so it neither clears nor sets
		// last_error; only a real attempt says anything about the channel's
		// health.
		if outcome == model.DeliverySucceeded {
			d.mark(ctx, channel.ID, "")
		}
		return
	}

	if j.attempt < maxAttempts && retryable(err) {
		d.log.Warn("notification delivery failed, retrying",
			"channel", channel.Name, "type", channel.Type, "attempt", j.attempt, "error", err)
		d.scheduleRetry(job{event: j.event, channel: j.channel, attempt: j.attempt + 1})
		return
	}

	// Out of attempts. This is the moment the channel becomes visibly broken.
	d.log.Error("notification delivery failed",
		"channel", channel.Name, "type", channel.Type, "attempts", j.attempt, "error", err)
	d.mark(ctx, channel.ID, err.Error())
}

// deliver opens the channel's secrets and sends. The two halves of the
// configuration are whole only inside this call.
func (d *Dispatcher) deliver(ctx context.Context, channel model.NotificationChannel, ev Event) (Receipt, error) {
	public, err := DecodeConfig(channel.Config)
	if err != nil {
		return Receipt{}, err
	}
	secret, err := d.vault.Open(channel.OrgID, channel.ID, channel.Secrets)
	if err != nil {
		// Almost always a restored database without its key file. Said plainly,
		// because the operator's next move depends on knowing which of the two
		// they are missing.
		return Receipt{}, fmt.Errorf("cannot read this channel's stored credentials: %w", err)
	}
	return d.sender.Send(ctx, channel.Type, Merge(public, secret), ev)
}

// Test delivers a sample event through one channel, synchronously, and records
// it like any other delivery. The button exists on every channel because a
// channel that fails silently at 3am is worse than no channel at all.
func (d *Dispatcher) Test(ctx context.Context, channel model.NotificationChannel, eventType string) (Receipt, error) {
	ev := d.SampleEvent(eventType)
	started := time.Now()

	receipt, err := d.deliver(ctx, channel, ev)
	elapsed := float64(time.Since(started).Microseconds()) / 1000.0

	outcome := model.DeliverySucceeded
	failure := ""
	switch {
	case errors.Is(err, ErrNotApplicable):
		outcome = model.DeliverySuppressed
		failure = err.Error()
	case err != nil:
		outcome = model.DeliveryFailed
		failure = err.Error()
	}

	d.record(ctx, model.NotificationDelivery{
		ID:              model.NewID(),
		OrgID:           channel.OrgID,
		ChannelID:       &channel.ID,
		EventType:       eventType,
		Outcome:         outcome,
		Error:           failure,
		DurationMs:      &elapsed,
		Attempt:         1,
		RenderedPayload: receipt.Payload,
		CreatedAt:       time.Now().UTC(),
	})

	// A test writes last_error too. Testing a channel and then leaving the row
	// claiming health would make the button a lie.
	if outcome == model.DeliveryFailed {
		d.mark(ctx, channel.ID, err.Error())
	} else if outcome == model.DeliverySucceeded {
		d.mark(ctx, channel.ID, "")
	}
	return receipt, err
}

// SampleEvent is the synthetic event a test fire and a template preview render
// against. Built from the same catalogue examples the preview endpoint publishes,
// so what the user previewed is what the test sends.
func (d *Dispatcher) SampleEvent(eventType string) Event {
	if eventType == "" {
		eventType = model.EventMonitorDown
	}
	status := "down"
	previous := "up"
	switch eventType {
	case model.EventMonitorUp:
		status, previous = "up", "down"
	case model.EventMonitorPending:
		status, previous = "pending", "up"
	}

	responseTime := 412.7
	now := time.Now().UTC()
	return Event{
		ID:         model.NewID(),
		Type:       eventType,
		OccurredAt: now,
		Instance:   d.instance,
		Monitor: Monitor{
			// A real identifier rather than the zero UUID: a template that
			// interpolates monitor.id should preview something that looks like
			// what it will actually send.
			ID:          model.NewID(),
			Name:        "Sample monitor",
			Description: "A test notification from " + d.instance.Name,
			Type:        "http",
			Target:      "https://example.com/health",
			Status:      status,
		},
		PreviousStatus: previous,
		Heartbeat: &Heartbeat{
			Time:           now,
			Status:         status,
			ResponseTimeMs: &responseTime,
			Message:        "This is a test. No monitor is actually in this state.",
			Code:           "200",
			Attempt:        1,
			Important:      true,
		},
	}
}

func (d *Dispatcher) scheduleRetry(j job) {
	delay := d.backoff(j.attempt - 1)

	d.mu.Lock()
	defer d.mu.Unlock()

	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		d.mu.Lock()
		delete(d.timers, timer)
		d.mu.Unlock()

		select {
		case d.queue <- j:
		default:
			d.log.Error("notification queue full, retry dropped",
				"event", j.event.Type, "attempt", j.attempt)
		}
	})
	d.timers[timer] = struct{}{}
}

// record writes the delivery row. Its own context, because the row explaining
// why an alert did not arrive must still be written when the reason is that the
// request was cancelled.
func (d *Dispatcher) record(ctx context.Context, delivery model.NotificationDelivery) {
	delivery.RenderedPayload = truncate(delivery.RenderedPayload, maxRecordedPayload)

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := d.store.RecordDelivery(writeCtx, delivery); err != nil {
		d.log.Error("record notification delivery", "error", err, "outcome", delivery.Outcome)
	}
}

func (d *Dispatcher) mark(ctx context.Context, channelID model.ID, failure string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := d.store.MarkChannelResult(writeCtx, channelID, time.Now().UTC(), truncate(failure, maxLastError)); err != nil {
		d.log.Error("update notification channel result", "error", err)
	}
}

// retryable decides whether another attempt could plausibly work.
//
// A misconfigured channel is not retried: sending the same invalid bot token
// three times produces three identical failures and delays the moment the
// operator is told. Transport failures are, because a receiver that was
// restarting thirty seconds ago usually is not now.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var render *RenderError
	if errors.As(err, &render) {
		return false
	}
	var provider *ProviderError
	if errors.As(err, &provider) && provider.Permanent() {
		return false
	}
	return !errors.Is(err, ErrNotApplicable)
}

func monitorIDOf(ev Event) *model.ID {
	if ev.Monitor.ID.IsZero() {
		return nil
	}
	id := ev.Monitor.ID
	return &id
}
