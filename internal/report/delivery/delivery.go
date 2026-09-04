// Package delivery sends a finished report to the people it was made for.
//
// # A report is a payload, not a new transport
//
// That sentence is the whole design. Every target below reaches its destination
// through machinery that already exists and is already tested: the instance SMTP
// relay that alerts and status-page bulletins use, the Slack incoming webhook a
// notification channel already holds, and the HMAC signature `internal/outbound`
// already computes. Nothing here dials, retries or signs in a way the rest of the
// product does not.
//
// The consequence worth stating is what it buys: an operator who rotates a Slack
// token rotates it in one place. A delivery that named its own copy of the token
// would work perfectly and then, three months later, silently stop — with no
// error anybody would connect to the rotation.
//
// # Decoupled from generation
//
// A run produces artifacts and finishes. Delivery is a separate step over a
// finished run, which is what makes "the PDF failed but the HTML went out" and
// "re-send last month's" expressible at all. It also means a delivery that fails
// does not fail the run: the report exists, and what failed is one attempt to
// hand it over.
//
// # Every attempt is a row
//
// The `internal/notify` discipline, adopted rather than reinvented, and the
// reason is the same one that package gives: silence with no row behind it is
// indistinguishable from a system that is not running. A **skipped** delivery is
// recorded as loudly as a failed one — no relay configured, nothing rendered in a
// format this target takes — because those are the cases where an operator is
// most likely to believe something went out.
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// MaxAttempts is how many times one target is tried before the run is left with
// a failure recorded against it.
//
// Three, matching neither the notification dispatcher's nor the webhook
// dispatcher's count on purpose — a report is not an alert. An alert is worth
// retrying hard because it is time-critical and its value decays in minutes; a
// monthly report's value does not decay at all, and an operator who reads the
// delivery log tomorrow can re-send it with one request. Retrying a broken
// mailbox twenty times just fills the log.
const MaxAttempts = 3

// RetryDelay is the wait between attempts. Short, because the failures worth
// retrying at all are the transient ones — a relay restarting, a rate limit —
// and anything longer would hold a worker for the length of a monthly burst.
var RetryDelay = 15 * time.Second

// Store is what delivery needs from persistence, declared here by the consumer.
type Store interface {
	GetReportRun(ctx context.Context, id model.ID) (model.ReportRun, error)
	ReportTemplateForRun(ctx context.Context, id model.ID) (model.ReportTemplate, error)
	ArtifactsForRuns(ctx context.Context, ids []model.ID) (map[model.ID][]model.ReportArtifact, error)
	DeliveriesForSchedule(ctx context.Context, scheduleID model.ID) ([]model.ReportScheduleDelivery, error)
	RecordReportDelivery(ctx context.Context, d model.ReportDelivery) error

	// GetChannel is how a target that names a notification channel reads its
	// configuration rather than restating it. The count that comes back with it
	// is ignored — it is what the channel screen needs, not what this does.
	GetChannel(ctx context.Context, id model.ID) (store.ChannelWithCount, error)
}

// Files reads an artifact's bytes back off disk.
//
// Read rather than re-rendered, and it is the same rule a share link follows: the
// document a client receives is the one that was produced, so retention dropping
// a tier between generation and delivery cannot change the figures in it.
type Files interface {
	Open(rel string) (io.ReadCloser, error)
}

// Vault opens a notification channel's sealed secrets.
type Vault interface {
	Open(orgID, channelID model.ID, envelope []byte) (map[string]any, error)
}

// Secrets opens a delivery target's own sealed credential.
//
// A second interface rather than a method on Vault, because the two envelopes
// hold different shapes and are bound to different rows: a channel's secrets are
// a map under (notification_channels, secrets), and a drop's is one string under
// (report_schedule_deliveries, secrets). Conflating them would mean one AAD for
// two tables, which is the property that stops a ciphertext being relocated from
// a row somebody controls onto one they do not.
type Secrets interface {
	Open(orgID, rowID, envelope []byte) ([]byte, error)
}

// Dispatcher delivers finished runs.
type Dispatcher struct {
	store Store
	files Files
	vault Vault
	log   *slog.Logger

	// drops opens an s3 delivery target's sealed secret access key. Nil in a
	// build or a test that is not exercising one, which sendS3 reports as a skip
	// naming the reason rather than panicking on — the same treatment a missing
	// SMTP relay gets, and for the same reason: an operator has to be told which
	// of the two it was.
	drops Secrets

	// instanceName goes on the covering message, so a client receiving reports
	// from two installs can tell which one sent this.
	instanceName string

	// sleep is injectable so retry can be tested without waiting.
	sleep func(context.Context, time.Duration)
}

// WithDrops attaches the opener for an s3 delivery target's credential.
//
// A setter rather than a sixth argument to New, following the convention the API
// server already uses: the drop is optional in a way the other five are not.
func (d *Dispatcher) WithDrops(s Secrets) *Dispatcher {
	d.drops = s
	return d
}

// New builds the dispatcher.
func New(s Store, files Files, vault Vault, instanceName string, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{
		store:        s,
		files:        files,
		vault:        vault,
		log:          log,
		instanceName: instanceName,
		sleep:        sleepFor,
	}
}

func sleepFor(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Deliver sends one finished run to every target its schedule configures.
//
// Returns nil when every target either succeeded or was legitimately skipped. A
// target that failed every attempt returns an error naming the first one, so the
// caller can log it — but the rows are already written by then, which is what
// makes the log the source of truth rather than the return value.
func (d *Dispatcher) Deliver(ctx context.Context, runID model.ID, now time.Time) error {
	run, err := d.store.GetReportRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if run.ReportScheduleID == nil {
		// An ad-hoc run: generated from the API rather than from a schedule, so
		// there is nobody configured to send it to. **Nil rather than an error,
		// and nothing recorded.** A "run now" is a report somebody is about to
		// download; returning an error would make the caller log a warning for
		// the most ordinary thing this subsystem does, and writing a row would
		// fill the delivery log with entries saying nobody asked for this to be
		// sent — burying the rows that matter.
		return nil
	}

	targets, err := d.store.DeliveriesForSchedule(ctx, *run.ReportScheduleID)
	if err != nil {
		return fmt.Errorf("load delivery targets: %w", err)
	}
	if len(targets) == 0 {
		return nil
	}

	artifacts, err := d.artifactsFor(ctx, run.ID)
	if err != nil {
		return err
	}
	template, err := d.store.ReportTemplateForRun(ctx, run.ReportTemplateID)
	if err != nil {
		return fmt.Errorf("load template: %w", err)
	}

	var firstFailure error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.deliverTo(ctx, run, template, target, artifacts, now); err != nil && firstFailure == nil {
			firstFailure = err
		}
	}
	return firstFailure
}

// artifactsFor is the rendered artifacts of a run, in a stable order.
//
// Only `rendered` ones. A failed artifact is a format that did not arrive and an
// expired one is a set of bytes that no longer exists, and attaching either would
// be attaching nothing under a filename that promises something.
func (d *Dispatcher) artifactsFor(ctx context.Context, runID model.ID) ([]model.ReportArtifact, error) {
	byRun, err := d.store.ArtifactsForRuns(ctx, []model.ID{runID})
	if err != nil {
		return nil, fmt.Errorf("load artifacts: %w", err)
	}

	var out []model.ReportArtifact
	for _, a := range byRun[runID] {
		if a.State == model.ArtifactRendered && a.Path != "" {
			out = append(out, a)
		}
	}
	// Sorted, because a message with two attachments should have them in the
	// same order every month — and because map iteration upstream is not
	// something this should depend on.
	sort.Slice(out, func(i, j int) bool { return out[i].Format < out[j].Format })
	return out, nil
}

// deliverTo attempts one target, with retry, recording every attempt.
func (d *Dispatcher) deliverTo(
	ctx context.Context,
	run model.ReportRun,
	template model.ReportTemplate,
	target model.ReportScheduleDelivery,
	artifacts []model.ReportArtifact,
	now time.Time,
) error {
	wanted := filterFormats(artifacts, target.Formats)
	if len(wanted) == 0 {
		// Recorded rather than passed over. "The auditor's PDF did not go out
		// because the PDF did not render" is precisely the sentence somebody
		// needs, and it is invisible without a row.
		return d.record(ctx, run, target, model.DeliverySkipped, 1,
			d.describeTarget(ctx, target), reasonNoFormats(target, artifacts), now)
	}

	var lastErr error
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		description, err := d.send(ctx, run, template, target, wanted)

		switch {
		case err == nil:
			return d.record(ctx, run, target, model.DeliverySucceeded, attempt, description, "", now)

		case errors.Is(err, errSkip):
			// A skip is a decision, not a transient fault: retrying a missing
			// relay twice more produces two more identical skips and three rows
			// that say the same thing.
			return d.record(ctx, run, target, model.DeliverySkipped, attempt, description,
				strings.TrimPrefix(err.Error(), errSkip.Error()+": "), now)

		case errors.Is(err, errPermanent):
			// A refused address or an unbuilt transport does not become
			// deliverable by being tried again.
			return d.record(ctx, run, target, model.DeliveryFailed, attempt, description,
				strings.TrimPrefix(err.Error(), errPermanent.Error()+": "), now)
		}

		lastErr = err
		// Every attempt is a row, including the ones that will be retried.
		// Recording only the last one would make "it took three goes tonight and
		// two last month" invisible, which is the shape of a problem that is
		// about to become an outage.
		if recordErr := d.record(ctx, run, target, model.DeliveryFailed, attempt, description, err.Error(), now); recordErr != nil {
			return recordErr
		}
		if attempt < MaxAttempts {
			d.sleep(ctx, RetryDelay)
		}
	}
	return lastErr
}

// errSkip and errPermanent classify a send's failure.
//
// The distinction is what stops the retry loop from being noise. A missing SMTP
// relay is not a transient fault and never becomes one inside forty-five
// seconds; neither is an s3 target on a build with no S3 client. Retrying either
// produces identical rows and delays the moment an operator is told which it was
// — the same reasoning ProviderError.Permanent already applies in
// internal/notify.
var (
	errSkip      = errors.New("skipped")
	errPermanent = errors.New("permanent")
)

func skip(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errSkip, fmt.Sprintf(format, args...))
}

func permanent(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errPermanent, fmt.Sprintf(format, args...))
}

// send dispatches to the right transport and reports what it aimed at.
func (d *Dispatcher) send(
	ctx context.Context,
	run model.ReportRun,
	template model.ReportTemplate,
	target model.ReportScheduleDelivery,
	artifacts []model.ReportArtifact,
) (description string, err error) {
	config, err := decodeConfig(target.Config)
	if err != nil {
		return "", permanent("delivery target configuration is not readable: %s", err)
	}

	channel, haveChannel, err := d.channelFor(ctx, target)
	if err != nil {
		return "", err
	}

	switch target.Type {
	case model.ReportDeliveryEmail:
		return d.sendEmail(ctx, run, template, config, channel, haveChannel, artifacts)
	case model.ReportDeliverySlack:
		return d.sendSlack(ctx, run, template, config, channel, haveChannel, artifacts)
	case model.ReportDeliveryWebhook:
		return d.sendWebhook(ctx, run, template, target, config, channel, haveChannel, artifacts)
	case model.ReportDeliveryS3:
		return d.sendS3(ctx, run, template, target, config, artifacts)
	}
	return "", permanent("unknown delivery type %q", target.Type)
}

// channelFor resolves the notification channel a target delegates to, with its
// secrets opened.
//
// **The channel's configuration is read, never copied.** That is the whole point
// of the reference: a rotated Slack token is rotated once, and a delivery holding
// its own copy would keep working until the old token was revoked and then fail
// in a way nobody would connect to the rotation.
func (d *Dispatcher) channelFor(ctx context.Context, target model.ReportScheduleDelivery) (map[string]any, bool, error) {
	if target.NotificationChannelID == nil {
		return nil, false, nil
	}

	withCount, err := d.store.GetChannel(ctx, *target.NotificationChannelID)
	if err != nil {
		// The foreign key is ON DELETE SET NULL, so a missing channel here means
		// the row was read between the delete and the null — or the id is from a
		// hand-written row. Either way it is not transient.
		return nil, false, permanent("the notification channel this target delivers "+
			"through is no longer available: %s", err)
	}
	channel := withCount.Channel
	if !channel.Enabled {
		return nil, false, skip("the notification channel %q is disabled", channel.Name)
	}

	config, err := decodeConfig(channel.Config)
	if err != nil {
		return nil, false, permanent("channel configuration is not readable: %s", err)
	}

	if len(channel.Secrets) > 0 && d.vault != nil {
		secret, err := d.vault.Open(channel.OrgID, channel.ID, channel.Secrets)
		if err != nil {
			return nil, false, permanent("the channel's stored credentials could not be opened: %s", err)
		}
		config = notify.Merge(config, secret)
	}
	return config, true, nil
}

// describeTarget is what the delivery row records as the destination, for a
// target that was skipped before any send was attempted.
func (d *Dispatcher) describeTarget(ctx context.Context, target model.ReportScheduleDelivery) string {
	config, err := decodeConfig(target.Config)
	if err != nil {
		return target.Type
	}
	if recipients := stringList(config["recipients"]); len(recipients) > 0 {
		return strings.Join(recipients, ", ")
	}
	if url := stringValue(config["url"]); url != "" {
		return redactURL(url)
	}
	if target.NotificationChannelID != nil {
		if withCount, err := d.store.GetChannel(ctx, *target.NotificationChannelID); err == nil {
			return withCount.Channel.Name
		}
	}
	return target.Type
}

// record appends one attempt to the log.
func (d *Dispatcher) record(
	ctx context.Context,
	run model.ReportRun,
	target model.ReportScheduleDelivery,
	outcome string,
	attempt int,
	description, failure string,
	now time.Time,
) error {
	row := model.ReportDelivery{
		ID:                       model.NewID(),
		OrgID:                    run.OrgID,
		ReportRunID:              run.ID,
		ReportScheduleDeliveryID: &target.ID,
		Type:                     target.Type,
		Outcome:                  outcome,
		Error:                    failure,
		Attempt:                  attempt,
		Target:                   description,
		CreatedAt:                now,
	}
	if outcome == model.DeliverySucceeded {
		at := now
		row.DeliveredAt = &at
	}

	// Detached from the caller's context, for the same reason finishing a run
	// is: a shutdown arriving here must not lose the record of a message that
	// has already left the building.
	if err := d.store.RecordReportDelivery(context.WithoutCancel(ctx), row); err != nil {
		d.log.Error("record report delivery", "run_id", run.ID.String(), "error", err)
		return err
	}
	if outcome == model.DeliveryFailed {
		d.log.Warn("report delivery failed", "run_id", run.ID.String(),
			"type", target.Type, "target", description, "attempt", attempt, "error", failure)
	}
	return nil
}

// filterFormats narrows the run's artifacts to the ones this target takes.
//
// An empty list means every format the run produced, which is the schema's own
// documented meaning and the right default: a schedule that names no formats per
// target is one somebody set up without thinking about the distinction, and
// sending everything is the answer that surprises nobody.
func filterFormats(artifacts []model.ReportArtifact, formats []string) []model.ReportArtifact {
	if len(formats) == 0 {
		return artifacts
	}
	wanted := make(map[string]bool, len(formats))
	for _, f := range formats {
		wanted[f] = true
	}

	var out []model.ReportArtifact
	for _, a := range artifacts {
		if wanted[a.Format] {
			out = append(out, a)
		}
	}
	return out
}

// reasonNoFormats explains an empty selection in the terms the reader needs,
// which are different depending on whether anything rendered at all.
func reasonNoFormats(target model.ReportScheduleDelivery, artifacts []model.ReportArtifact) string {
	if len(artifacts) == 0 {
		return "the run produced no artifact that could be delivered"
	}
	var have []string
	for _, a := range artifacts {
		have = append(have, a.Format)
	}
	return fmt.Sprintf("this target takes %s and the run produced %s",
		strings.Join(target.Formats, ", "), strings.Join(have, ", "))
}

func decodeConfig(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	return notify.DecodeConfig(raw)
}

func stringValue(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func stringList(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range raw {
		if s := stringValue(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// redactURL keeps the host and drops everything that identifies the credential.
//
// A Slack incoming webhook URL **is** the credential: the path is the secret.
// The delivery log is read on screen and copied into support conversations, so
// recording the whole URL would put a working token wherever that conversation
// went — the same reasoning internal/notify applies when it keeps bot tokens out
// of recorded payloads.
func redactURL(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "webhook"
	}
	host, _, _ := strings.Cut(rest, "/")
	if host == "" {
		return "webhook"
	}
	return scheme + "://" + host + "/…"
}
