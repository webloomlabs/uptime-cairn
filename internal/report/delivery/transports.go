package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/s3"
)

// The three transports, each of which is somebody else's machinery with a report
// on it.

// httpTimeout bounds one Slack or webhook POST. Generous relative to an alert,
// because the receiver of a monthly report is not being paged.
const httpTimeout = 30 * time.Second

// --- email -----------------------------------------------------------------

// sendEmail attaches the artifacts and sends through the instance relay.
//
// **Attached, not linked**, and the reason is stated where somebody will look for
// it: notify/mail.go. A link needs a share link, share links are human-led work,
// and a link to an authenticated endpoint is a link the client it was sent to
// cannot open.
func (d *Dispatcher) sendEmail(
	ctx context.Context,
	run model.ReportRun,
	template model.ReportTemplate,
	config map[string]any,
	channel map[string]any,
	haveChannel bool,
	artifacts []model.ReportArtifact,
) (string, error) {
	recipients := stringList(config["recipients"])
	if haveChannel {
		// The channel's recipients when the target delegates to one, so an
		// address list is maintained in one place.
		if fromChannel := stringList(channel["to"]); len(fromChannel) > 0 {
			recipients = fromChannel
		}
	}
	if len(recipients) == 0 {
		return "email", permanent("no recipients are configured for this target")
	}
	if !notify.InstanceSMTPConfigured() {
		// A skip rather than a failure. An install that has not configured mail
		// has not failed to send mail, and a red mark against an operator who
		// never asked for email would be the wrong story on the screen.
		return strings.Join(recipients, ", "),
			skip("no SMTP relay is configured, so the report could not be emailed")
	}

	attachments, err := d.attachments(artifacts, template, run)
	if err != nil {
		return strings.Join(recipients, ", "), err
	}

	subject := reportTitle(template, run)
	err = notify.SendMail(ctx, notify.Mail{
		To:      recipients,
		Subject: subject,
		Body:    d.coveringNote(run, template, artifacts),
		// Derived from the run rather than from the clock, so a re-send threads
		// with the original in any client that honours it instead of arriving as
		// an unrelated second message.
		MessageID:   "report-" + run.ID.String(),
		Sent:        run.CreatedAt,
		Attachments: attachments,
	})

	description := strings.Join(recipients, ", ")
	switch {
	case err == nil:
		return description, nil
	case isSkippable(err):
		return description, skip("%s", err)
	}
	return description, err
}

func isSkippable(err error) bool {
	// A relay that vanished between the check above and the send, and a message
	// too large to be worth three attempts at. Both are decisions rather than
	// transient faults.
	return errors.Is(err, notify.ErrNoRelay) || errors.Is(err, notify.ErrMailTooLarge)
}

// attachments reads each artifact back off disk.
//
// **Read, never re-rendered.** The same rule a share link follows: the document a
// client receives is the one that was produced and digested, so retention
// dropping a tier between generation and delivery cannot silently change the
// figures in the file that arrives.
func (d *Dispatcher) attachments(
	artifacts []model.ReportArtifact,
	template model.ReportTemplate,
	run model.ReportRun,
) ([]notify.Attachment, error) {
	// The size is checked from the row **before** anything is read, which is the
	// difference between refusing a hundred-megabyte CSV and allocating it first
	// to find out it will not fit. The row carries the size precisely so this
	// question can be answered without opening the file.
	var total int64
	for _, a := range artifacts {
		total += a.SizeBytes
	}
	if total > notify.MaxMailBytes {
		return nil, skip("the artifacts total %d bytes, over the %d-byte message limit; "+
			"deliver this schedule by webhook, or narrow it to one format",
			total, notify.MaxMailBytes)
	}

	out := make([]notify.Attachment, 0, len(artifacts))
	for _, a := range artifacts {
		data, err := readAll(d.files, a.Path)
		if err != nil {
			// The bytes are gone while the row says otherwise, which the orphan
			// sweeper cannot produce and a restore from an older backup can.
			// Permanent, because reading the same missing file twice more is not
			// a strategy.
			return nil, permanent("the %s artifact could not be read from disk: %s", a.Format, err)
		}
		out = append(out, notify.Attachment{
			Filename:    attachmentName(template, run, a.Format),
			ContentType: contentTypeFor(a.Format),
			Data:        data,
		})
	}
	return out, nil
}

// attachmentName is what the file is called in the client's mailbox.
//
// Built from the template name and the period rather than from the artifact id,
// because this one **is** read by a person: "Acme-uptime-2026-03.pdf" filed in a
// folder beats a UUID. The name is sanitised to a conservative set rather than
// escaped, and that matters — it is the one place in this subsystem where a
// user-supplied string reaches a filename, and the artifact's own on-disk path
// deliberately avoids the problem by not using one at all.
func attachmentName(template model.ReportTemplate, run model.ReportRun, format string) string {
	stem := slug(template.Name)
	if stem == "" {
		stem = "report"
	}
	return fmt.Sprintf("%s-%s%s", stem, run.PeriodStart.Format("2006-01"), extensionFor(format))
}

func slug(name string) string {
	var out strings.Builder
	lastDash := true
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out.WriteRune(r)
			lastDash = false
		case !lastDash && out.Len() < 48:
			out.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func extensionFor(format string) string {
	switch format {
	case model.FormatPDF:
		return ".pdf"
	case model.FormatHTML:
		return ".html"
	case model.FormatCSV:
		return ".csv"
	case model.FormatJSON:
		return ".json"
	}
	return ".bin"
}

func contentTypeFor(format string) string {
	switch format {
	case model.FormatPDF:
		return "application/pdf"
	case model.FormatHTML:
		return "text/html; charset=utf-8"
	case model.FormatCSV:
		return "text/csv; charset=utf-8"
	case model.FormatJSON:
		return "application/json"
	}
	return "application/octet-stream"
}

// coveringNote is the sentence above the attachments.
//
// Short on purpose, and it says three things a recipient actually needs: what
// period it covers, which install sent it, and — when the run was partial —
// that a format is missing. The last is the one worth the line: a client who
// expected a PDF and got only a CSV should be told why by the message rather
// than left to notice.
func (d *Dispatcher) coveringNote(run model.ReportRun, template model.ReportTemplate, artifacts []model.ReportArtifact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", reportTitle(template, run))
	fmt.Fprintf(&b, "Period: %s to %s (%s)\n",
		run.PeriodStart.Format("2 January 2006"),
		run.PeriodEnd.Format("2 January 2006"),
		run.Timezone)

	if len(artifacts) > 0 {
		formats := make([]string, 0, len(artifacts))
		for _, a := range artifacts {
			formats = append(formats, strings.ToUpper(a.Format))
		}
		fmt.Fprintf(&b, "Attached: %s\n", strings.Join(formats, ", "))
	}

	if run.State == model.RunPartial {
		fmt.Fprintf(&b, "\nNot every requested format was produced for this period. %s\n", run.Error)
	}
	if d.instanceName != "" {
		fmt.Fprintf(&b, "\nSent by %s.\n", d.instanceName)
	}
	return b.String()
}

func readAll(files Files, rel string) ([]byte, error) {
	r, err := files.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func reportTitle(template model.ReportTemplate, run model.ReportRun) string {
	name := strings.TrimSpace(template.Name)
	if name == "" {
		name = "Uptime report"
	}
	return fmt.Sprintf("%s — %s", name, run.PeriodStart.Format("January 2006"))
}

// --- slack -----------------------------------------------------------------

// sendSlack posts a message announcing the report.
//
// **A message, not the file.** A Slack incoming webhook cannot carry an upload —
// that needs the files API and a bot token, which is a different credential from
// the one a notification channel holds — so what goes to Slack is the fact that
// the report exists and what it covers. The obvious improvement is a link, and
// the obvious link is a share link, which is human-led work under AGENTS.md
// rule 8. Until that lands this message is honest about being an announcement:
// it does not claim a file is attached and does not offer a URL that would ask
// the reader to authenticate.
func (d *Dispatcher) sendSlack(
	ctx context.Context,
	run model.ReportRun,
	template model.ReportTemplate,
	config map[string]any,
	channel map[string]any,
	haveChannel bool,
	artifacts []model.ReportArtifact,
) (string, error) {
	url := stringValue(config["url"])
	if haveChannel {
		// The channel's own incoming-webhook URL, read rather than copied, which
		// is what makes a rotated token a one-place change.
		if fromChannel := stringValue(channel["webhook_url"]); fromChannel != "" {
			url = fromChannel
		}
	}
	if url == "" {
		return "slack", permanent("no Slack webhook URL is configured for this target, " +
			"and no notification channel was named to take one from")
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("*%s*", reportTitle(template, run)))
	lines = append(lines, fmt.Sprintf("Period %s – %s (%s)",
		run.PeriodStart.Format("2 Jan 2006"), run.PeriodEnd.Format("2 Jan 2006"), run.Timezone))

	if len(artifacts) > 0 {
		formats := make([]string, 0, len(artifacts))
		for _, a := range artifacts {
			formats = append(formats, strings.ToUpper(a.Format))
		}
		lines = append(lines, "Available: "+strings.Join(formats, ", "))
	}
	if run.State == model.RunPartial {
		lines = append(lines, "Not every requested format was produced. "+run.Error)
	}

	payload := map[string]any{"text": strings.Join(lines, "\n")}
	if v := stringValue(channel["channel"]); haveChannel && v != "" {
		payload["channel"] = v
	}
	if v := stringValue(channel["username"]); haveChannel && v != "" {
		payload["username"] = v
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return redactURL(url), permanent("could not encode the Slack message: %s", err)
	}
	return redactURL(url), d.post(ctx, url, "application/json", body, nil)
}

// --- webhook ---------------------------------------------------------------

// sendWebhook posts a machine-readable description of the run.
//
// # These deliveries are authenticated by header, not signed — and that is a gap
//
// The plan asks for report webhooks "signed exactly as `internal/outbound` signs
// them", and they are not, because **there is nowhere to put the key.** An HMAC
// needs a shared secret; the frozen spec's schedule delivery target carries
// `type`, `recipients`, `notification_channel_id`, `url`, an `s3` block and
// `formats`, and none of those is a signing secret. A notification channel of
// type `webhook` has none either — it has secret *headers*, which is how this
// product already authenticates an outgoing webhook channel. The HMAC key lives
// on `outbound.Webhook`, which is a separate resource that a delivery target has
// no field to reference.
//
// So the choice was between inventing a field — an API change on a frozen spec,
// which AGENTS.md rule 4 does not allow an agent to make — and using the
// authentication the configuration actually offers. This does the second and
// says so, and the gap is recorded for the maintainer rather than papered over
// with a signature computed from something that was not a signing key.
//
// A target that names a webhook channel therefore sends **that channel's secret
// headers**, read at delivery rather than copied — which is the property that
// matters either way: a rotated credential is rotated in one place.
//
// The payload is a description rather than the document. A program that receives
// this has an API key and can fetch the artifact it wants; posting a base64 PDF
// to an endpoint that may not want one would make every delivery cost the size of
// the report.
func (d *Dispatcher) sendWebhook(
	ctx context.Context,
	run model.ReportRun,
	template model.ReportTemplate,
	target model.ReportScheduleDelivery,
	config map[string]any,
	channel map[string]any,
	haveChannel bool,
	artifacts []model.ReportArtifact,
) (string, error) {
	url := stringValue(config["url"])
	if haveChannel {
		if fromChannel := stringValue(channel["url"]); fromChannel != "" {
			url = fromChannel
		}
	}
	if url == "" {
		return "webhook", permanent("no URL is configured for this webhook target")
	}

	body, err := json.Marshal(webhookPayload(run, template, artifacts))
	if err != nil {
		return redactURL(url), permanent("could not encode the webhook payload: %s", err)
	}

	headers := map[string]string{}
	if haveChannel {
		// The channel's configured headers, which is where an operator puts an
		// Authorization or an API key today. Read from the channel rather than
		// restated on the target, so rotating one rotates it everywhere.
		for name, value := range stringMap(channel["headers"]) {
			headers[name] = value
		}
	}
	// The reserved headers go on last, so a configured header can add to the
	// request and can never replace the run's identity — the rule
	// internal/outbound already states, for the reason it gives: a receiver whose
	// deduplication key can be changed by a typo in a settings field has no
	// deduplication.
	headers["X-Cairn-Event"] = "report.delivered"
	headers["X-Cairn-Event-Id"] = run.ID.String()
	headers["X-Cairn-Delivery-Id"] = target.ID.String()
	headers["X-Cairn-Timestamp"] = fmt.Sprintf("%d", run.CreatedAt.Unix())

	return redactURL(url), d.post(ctx, url, "application/json", body, headers)
}

// webhookPayload is what a program receives.
//
// The digest is on every artifact, which is what makes the delivery evidence
// rather than a notification: a receiver that downloads the file can assert it is
// the one this message described.
func webhookPayload(run model.ReportRun, template model.ReportTemplate, artifacts []model.ReportArtifact) map[string]any {
	files := make([]map[string]any, 0, len(artifacts))
	for _, a := range artifacts {
		files = append(files, map[string]any{
			"artifact_id": a.ID.String(),
			"format":      a.Format,
			"size_bytes":  a.SizeBytes,
			"sha256":      a.SHA256,
		})
	}
	return map[string]any{
		"event": "report.delivered",
		"report_run": map[string]any{
			"id":                 run.ID.String(),
			"report_template_id": run.ReportTemplateID.String(),
			"template_name":      template.Name,
			"state":              run.State,
			"period_start":       run.PeriodStart.UTC().Format(time.RFC3339),
			"period_end":         run.PeriodEnd.UTC().Format(time.RFC3339),
			"timezone":           run.Timezone,
			"late":               run.Late,
		},
		"artifacts": files,
	}
}

// post is the one HTTP call both chat and webhook make.
func (d *Dispatcher) post(ctx context.Context, url, contentType string, body []byte, headers map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return permanent("the configured URL is not usable: %s", err)
	}
	request.Header.Set("Content-Type", contentType)
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		// A network error is the case retry exists for.
		return fmt.Errorf("post to the configured endpoint: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	// 408, 425 and 429 are the three 4xx codes that mean "not now" rather than
	// "not ever" — the same classification internal/notify's ProviderError makes,
	// restated here rather than imported because that type is unexported.
	switch response.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return fmt.Errorf("the endpoint answered %s", response.Status)
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 {
		return permanent("the endpoint answered %s, which will not change on a retry", response.Status)
	}
	return fmt.Errorf("the endpoint answered %s", response.Status)
}

// stringMap reads a map of string headers out of decoded configuration.
func stringMap(v any) map[string]string {
	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		if s, ok := value.(string); ok {
			out[key] = s
		}
	}
	return out
}

// --- s3 (the drop) ----------------------------------------------------------

// sendS3 drops one run's files into a bucket.
//
// # The drop is not the mirror
//
// They share the client in internal/s3 and nothing else, and the distinction is
// worth holding onto because the two look identical from the outside and answer
// opposite questions. The **mirror** is a durability copy of every artifact this
// install produces, configured once in settings, and it exists so that a disk
// failure does not take the evidence with it. The **drop** is a delivery: one
// schedule's output, in the formats that target takes, put where a recipient's
// pipeline will find it. An operator who configures a drop and believes they have
// durability has bought nothing against the failure the mirror exists for, which
// is why the two are named apart everywhere they appear.
//
// # Why the key layout is what it is
//
// `<prefix>/<template-slug>/<period>/<name>.<ext>` — a human-readable path, and
// deliberately not the artifact-id layout the mirror uses. The mirror's consumer
// is a restore, which wants the on-disk tree reproduced exactly; a drop's
// consumer is somebody's data pipeline or somebody's colleague, and a bucket full
// of UUIDs serves neither. This is the same reasoning attachmentName follows, and
// it uses the same sanitiser: it is one of the two places in this subsystem where
// a user-supplied string reaches a path, and the sanitising is what makes that
// safe.
//
// # Failure is a delivery failure, not a run failure
//
// A drop that cannot reach its bucket records a failed delivery and is retried
// like any other target. The run is untouched: the report exists, and what failed
// is one attempt to hand it over.
func (d *Dispatcher) sendS3(
	ctx context.Context,
	run model.ReportRun,
	template model.ReportTemplate,
	target model.ReportScheduleDelivery,
	config map[string]any,
	artifacts []model.ReportArtifact,
) (string, error) {
	bucket := stringValue(config["bucket"])
	description := "s3://" + bucket
	if prefix := stringValue(config["prefix"]); prefix != "" {
		description += "/" + strings.Trim(prefix, "/")
	}

	if d.drops == nil {
		// A build with no opener cannot read the credential, so the upload would
		// authenticate as nobody.
		//
		// **Failed rather than skipped**, and the difference is the point. A skip
		// reads as "there was nothing to do"; an operator who configured a drop
		// and reads that will believe their files went out. The failure a mirror
		// is bought to prevent is believing in a copy that does not exist, and
		// recording this as anything softer produces exactly that belief — to be
		// discovered on the day the copy is needed. Permanent, because a build
		// does not grow the opener between attempts.
		return description, permanent("this build cannot open the delivery credential, " +
			"so nothing was uploaded to the s3 target")
	}
	if len(target.SecretsSealed) == 0 {
		return description, permanent("no secret access key is stored for this s3 target; re-save the schedule with one")
	}

	secret, err := d.drops.Open(target.OrgID[:], target.ID[:], target.SecretsSealed)
	if err != nil {
		// The envelope is bound by AAD to this row, so a failure here is either
		// a key problem or a relocated ciphertext. Neither is fixed by retrying.
		return description, permanent("the s3 credential could not be opened: %s", err)
	}

	cfg := s3.Config{
		Bucket:          bucket,
		Prefix:          stringValue(config["prefix"]),
		Region:          stringValue(config["region"]),
		Endpoint:        stringValue(config["endpoint"]),
		PathStyle:       boolValue(config["path_style"]),
		AccessKeyID:     stringValue(config["access_key_id"]),
		SecretAccessKey: string(secret),
	}
	if err := cfg.Validate(); err != nil {
		return description, permanent("the s3 target is incompletely configured: %s", err)
	}
	client := s3.New(cfg, nil)

	// Sorted, so a bucket listing after a run reads in a stable order rather than
	// in whatever order the map came back in.
	sorted := append([]model.ReportArtifact(nil), artifacts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Format < sorted[j].Format })

	for _, a := range sorted {
		data, err := readAll(d.files, a.Path)
		if err != nil {
			// The row says the bytes are there and they are not. The orphan
			// sweeper cannot produce this; a restore from an older backup can.
			// Permanent, because reading the same missing file twice more is not
			// a strategy.
			return description, permanent("the %s artifact could not be read from disk: %s", a.Format, err)
		}
		key := dropKey(template, run, a.Format)
		if err := client.Put(ctx, key, data, contentTypeFor(a.Format)); err != nil {
			// Transient by default: a bucket that is unreachable now may not be
			// in fifteen seconds, and the retry loop is what that is for. The
			// provider's own message travels with it, because 'NoSuchBucket' and
			// 'SignatureDoesNotMatch' send an operator to two different screens.
			return description, fmt.Errorf("uploading %s to %s/%s: %w", a.Format, description, key, err)
		}
	}
	return description, nil
}

// dropKey is where one artifact lands in the bucket.
//
// Read by a person or by somebody's pipeline, so it is named rather than
// identified — the opposite choice from the mirror, which reproduces the on-disk
// path because its reader is a restore. Sanitised by the same slug the mail
// attachment uses, which is what keeps a template called `../../etc` from being
// a path here.
func dropKey(template model.ReportTemplate, run model.ReportRun, format string) string {
	stem := slug(template.Name)
	if stem == "" {
		stem = "report"
	}
	period := run.PeriodStart.Format("2006-01")
	return fmt.Sprintf("%s/%s/%s-%s%s", stem, period, stem, period, extensionFor(format))
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}
