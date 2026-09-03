package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

var (
	now          = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	march        = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	aprilOnFirst = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

// fakeStore is the row half; fakeFiles is the disk half. Both in memory, because
// every property here is about which rows get written and what goes on the wire.
type fakeStore struct {
	mu sync.Mutex

	run       model.ReportRun
	template  model.ReportTemplate
	targets   []model.ReportScheduleDelivery
	artifacts []model.ReportArtifact
	channel   model.NotificationChannel
	channelOK bool

	recorded []model.ReportDelivery
}

func (f *fakeStore) GetReportRun(context.Context, model.ID) (model.ReportRun, error) {
	return f.run, nil
}

func (f *fakeStore) ReportTemplateForRun(context.Context, model.ID) (model.ReportTemplate, error) {
	return f.template, nil
}

func (f *fakeStore) ArtifactsForRuns(_ context.Context, ids []model.ID) (map[model.ID][]model.ReportArtifact, error) {
	return map[model.ID][]model.ReportArtifact{ids[0]: f.artifacts}, nil
}

func (f *fakeStore) DeliveriesForSchedule(context.Context, model.ID) ([]model.ReportScheduleDelivery, error) {
	return f.targets, nil
}

func (f *fakeStore) RecordReportDelivery(_ context.Context, d model.ReportDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, d)
	return nil
}

func (f *fakeStore) GetChannel(context.Context, model.ID) (store.ChannelWithCount, error) {
	if !f.channelOK {
		return store.ChannelWithCount{}, errors.New("no such channel")
	}
	return store.ChannelWithCount{Channel: f.channel}, nil
}

func (f *fakeStore) log() []model.ReportDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.ReportDelivery(nil), f.recorded...)
}

type fakeFiles struct {
	data map[string][]byte
	err  error
}

func (f *fakeFiles) Open(rel string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	data, ok := f.data[rel]
	if !ok {
		return nil, errors.New("no such file")
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fixture(targets ...model.ReportScheduleDelivery) (*fakeStore, *fakeFiles) {
	scheduleID := model.NewID()
	runID := model.NewID()

	return &fakeStore{
			run: model.ReportRun{
				ID: runID, OrgID: model.SentinelOrgID,
				ReportTemplateID: model.NewID(),
				ReportScheduleID: &scheduleID,
				State:            model.RunSucceeded,
				PeriodStart:      march, PeriodEnd: aprilOnFirst,
				Timezone:  "Australia/Sydney",
				CreatedAt: now,
			},
			template: model.ReportTemplate{Name: "Acme monthly"},
			targets:  targets,
			artifacts: []model.ReportArtifact{
				{ID: model.NewID(), Format: model.FormatPDF, State: model.ArtifactRendered,
					Path: "2026/03/a.pdf", SizeBytes: 6, SHA256: "digest-pdf"},
				{ID: model.NewID(), Format: model.FormatCSV, State: model.ArtifactRendered,
					Path: "2026/03/a.csv", SizeBytes: 6, SHA256: "digest-csv"},
			},
		}, &fakeFiles{data: map[string][]byte{
			"2026/03/a.pdf": []byte("%PDF-1"),
			"2026/03/a.csv": []byte("a,b,c\n"),
		}}
}

func target(kind string, config map[string]any, formats ...string) model.ReportScheduleDelivery {
	raw, _ := json.Marshal(config)
	return model.ReportScheduleDelivery{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Type: kind, Config: raw, Formats: formats,
	}
}

func dispatcher(t *testing.T, s *fakeStore, f *fakeFiles) *Dispatcher {
	t.Helper()
	d := New(s, f, nil, "Acme Ops", quiet())
	// No real waiting: retry is a rule about how many rows appear, not about how
	// long a test takes.
	d.sleep = func(context.Context, time.Duration) {}
	return d
}

// A webhook target receives a description of the run, and every artifact carries
// its digest.
//
// The digest is what makes the delivery evidence rather than a notification: a
// receiver that fetches the file can assert it is the one this message described.
// The payload is deliberately not the document — a program that gets this has an
// API key and can fetch what it wants, and posting a base64 PDF would make every
// delivery cost the size of the report.
func TestAWebhookTargetGetsTheRunAndTheDigests(t *testing.T) {
	t.Parallel()

	var body []byte
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		headers = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s, f := fixture(target(model.ReportDeliveryWebhook, map[string]any{"url": server.URL}))
	if err := dispatcher(t, s, f).Deliver(context.Background(), s.run.ID, now); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	var payload struct {
		Event     string `json:"event"`
		ReportRun struct {
			ID       string `json:"id"`
			State    string `json:"state"`
			Timezone string `json:"timezone"`
		} `json:"report_run"`
		Artifacts []struct {
			Format string `json:"format"`
			SHA256 string `json:"sha256"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v (%s)", err, body)
	}
	if payload.Event != "report.delivered" {
		t.Errorf("event = %q", payload.Event)
	}
	if payload.ReportRun.ID != s.run.ID.String() {
		t.Errorf("run id = %q", payload.ReportRun.ID)
	}
	// The zone travels, because "March" means different instants in different
	// ones and a receiver reconciling the period needs to know which.
	if payload.ReportRun.Timezone != "Australia/Sydney" {
		t.Errorf("timezone = %q, want the zone the period was cut in", payload.ReportRun.Timezone)
	}
	if len(payload.Artifacts) != 2 {
		t.Fatalf("%d artifacts, want 2", len(payload.Artifacts))
	}
	for _, a := range payload.Artifacts {
		if a.SHA256 == "" {
			t.Errorf("the %s artifact has no digest, so a receiver cannot assert "+
				"the file it downloads is the one this message described", a.Format)
		}
	}
	if headers.Get("X-Cairn-Event") != "report.delivered" {
		t.Error("no event header")
	}
	// **Not signed, and that is a recorded gap rather than a slip.** An HMAC
	// needs a shared secret, and neither the spec's delivery target nor a
	// webhook notification channel has a field for one — the signing key lives
	// on the outbound-webhook resource, which a delivery target cannot
	// reference. Asserting the absence keeps the gap visible: whoever adds the
	// field will find this line and know to change it.
	if headers.Get("X-Cairn-Signature") != "" {
		t.Error("a signature appeared, so a key was found somewhere — update the " +
			"note in transports.go, because the recorded spec gap has been closed")
	}

	logged := s.log()
	if len(logged) != 1 || logged[0].Outcome != model.DeliverySucceeded {
		t.Fatalf("delivery log = %+v, want one success", logged)
	}
	if logged[0].DeliveredAt == nil {
		t.Error("a successful delivery has no delivered_at")
	}
}

// A target that names a notification channel sends **that channel's** URL and
// secret headers, read at delivery rather than copied at configuration.
//
// This is the whole reason the reference exists. A delivery holding its own copy
// of the credential would keep working after a rotation and then, when the old
// one was revoked, fail in a way nobody would connect to the rotation.
//
// It also pins the reserved-header rule: a configured header can add to the
// request and can never replace the run's identity, because a receiver whose
// deduplication key can be changed by a typo in a settings field has no
// deduplication.
func TestANamedChannelSuppliesTheCredential(t *testing.T) {
	t.Parallel()

	var headers http.Header
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		headers = r.Header.Clone()
	}))
	defer server.Close()

	channelID := model.NewID()
	tgt := target(model.ReportDeliveryWebhook, map[string]any{})
	tgt.NotificationChannelID = &channelID

	s, f := fixture(tgt)
	s.channelOK = true
	s.channel = model.NotificationChannel{
		ID: channelID, OrgID: model.SentinelOrgID, Name: "Ops webhook",
		Type: model.ChannelWebhook, Enabled: true,
		Config: json.RawMessage(`{"url":"` + server.URL + `"}`),
		// Non-empty, because that is what makes the dispatcher open the vault —
		// a channel with no sealed blob has no secrets to read.
		Secrets: []byte("sealed"),
	}

	d := dispatcher(t, s, f)
	d.vault = staticVault{"headers": map[string]any{
		"Authorization": "Bearer s3cr3t",
		// An attempt to overwrite a reserved header, which must not win.
		"X-Cairn-Event": "spoofed",
	}}

	if err := d.Deliver(context.Background(), s.run.ID, now); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if got := headers.Get("Authorization"); got != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, want the channel's own credential", got)
	}
	if got := headers.Get("X-Cairn-Event"); got != "report.delivered" {
		t.Errorf("X-Cairn-Event = %q — a configured header replaced the run's "+
			"identity, so a receiver's deduplication key is settable by a typo", got)
	}
	if len(body) == 0 {
		t.Error("no body reached the endpoint")
	}
}

// A skipped delivery is a row, and it is not a failure.
//
// Both halves matter. The row, because silence with nothing behind it is
// indistinguishable from a system that is not running — and "the auditor's PDF
// did not go out because the PDF did not render" is exactly the sentence somebody
// needs. Not a failure, because nothing broke: this target asked for a format
// this run did not produce.
func TestATargetWithNoMatchingFormatIsSkippedWithAReason(t *testing.T) {
	t.Parallel()

	s, f := fixture(target(model.ReportDeliveryWebhook,
		map[string]any{"url": "http://127.0.0.1:1"}, model.FormatJSON))

	if err := dispatcher(t, s, f).Deliver(context.Background(), s.run.ID, now); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	logged := s.log()
	if len(logged) != 1 {
		t.Fatalf("%d rows, want 1", len(logged))
	}
	if logged[0].Outcome != model.DeliverySkipped {
		t.Errorf("outcome = %q, want %q — nothing broke, there was simply nothing "+
			"in this target's format", logged[0].Outcome, model.DeliverySkipped)
	}
	if !strings.Contains(logged[0].Error, "json") || !strings.Contains(logged[0].Error, "pdf") {
		t.Errorf("reason = %q, want it to name both what was asked for and what "+
			"the run produced", logged[0].Error)
	}
	if logged[0].DeliveredAt != nil {
		t.Error("a skipped delivery has a delivered_at, which claims it was sent")
	}
}

// A transient failure is retried, and **every attempt is a row**.
//
// Recording only the final one would make "it took three goes tonight and two
// last month" invisible, which is the shape of a problem that is about to become
// an outage.
func TestATransientFailureIsRetriedAndEveryAttemptIsRecorded(t *testing.T) {
	t.Parallel()

	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		attempt := calls
		mu.Unlock()
		if attempt < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s, f := fixture(target(model.ReportDeliveryWebhook, map[string]any{"url": server.URL}))
	if err := dispatcher(t, s, f).Deliver(context.Background(), s.run.ID, now); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	logged := s.log()
	if len(logged) != 3 {
		t.Fatalf("%d rows, want 3 — one per attempt", len(logged))
	}
	if logged[0].Outcome != model.DeliveryFailed || logged[0].Attempt != 1 {
		t.Errorf("first row = %+v, want a failed attempt 1", logged[0])
	}
	if logged[2].Outcome != model.DeliverySucceeded || logged[2].Attempt != 3 {
		t.Errorf("last row = %+v, want a successful attempt 3", logged[2])
	}
}

// A 4xx is permanent and is tried once.
//
// Retrying a refused request twice more produces two identical failures and
// delays the moment the operator is told which kind of failure it was — the
// classification internal/notify already makes, applied here for the same reason.
func TestAPermanentFailureIsNotRetried(t *testing.T) {
	t.Parallel()

	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	s, f := fixture(target(model.ReportDeliveryWebhook, map[string]any{"url": server.URL}))
	_ = dispatcher(t, s, f).Deliver(context.Background(), s.run.ID, now)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("the endpoint was called %d times for a 403; it will answer 403 again", got)
	}
	if logged := s.log(); len(logged) != 1 || logged[0].Outcome != model.DeliveryFailed {
		t.Errorf("delivery log = %+v, want one failure", logged)
	}
}

// An s3 target is refused with the reason rather than recorded as sent.
//
// The failure a mirror is bought to prevent is believing you have a durability
// copy that does not exist. Recording a success here would produce exactly that
// belief, and it would be discovered on the day the local copy was needed.
func TestAnS3TargetIsRefusedWithTheReason(t *testing.T) {
	t.Parallel()

	s, f := fixture(target(model.ReportDeliveryS3, map[string]any{"bucket": "reports"}))
	_ = dispatcher(t, s, f).Deliver(context.Background(), s.run.ID, now)

	logged := s.log()
	if len(logged) != 1 {
		t.Fatalf("%d rows, want 1", len(logged))
	}
	if logged[0].Outcome != model.DeliveryFailed {
		t.Errorf("outcome = %q, want failed — a copy that does not exist must not "+
			"be recorded as one that does", logged[0].Outcome)
	}
	if !strings.Contains(logged[0].Error, "S3 client is not built") {
		t.Errorf("reason = %q, want it to name what is missing", logged[0].Error)
	}
}

// An ad-hoc run has no schedule, so it has no recipients — and that produces no
// rows at all.
//
// A "run now" from the UI is a report somebody is about to download. A delivery
// log full of entries saying nobody asked for this to be sent would bury the rows
// that matter, which is the failure mode the log exists to avoid.
func TestAnAdHocRunProducesNoDeliveryRows(t *testing.T) {
	t.Parallel()

	s, f := fixture()
	s.run.ReportScheduleID = nil

	if err := dispatcher(t, s, f).Deliver(context.Background(), s.run.ID, now); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if logged := s.log(); len(logged) != 0 {
		t.Errorf("%d delivery rows for an ad-hoc run, want none", len(logged))
	}
}

// A failed or expired artifact is never handed over.
//
// A failed row is a format that did not arrive; an expired one is bytes that no
// longer exist. Delivering either would attach nothing under a filename that
// promises something.
func TestOnlyRenderedArtifactsAreDelivered(t *testing.T) {
	t.Parallel()

	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
	}))
	defer server.Close()

	s, f := fixture(target(model.ReportDeliveryWebhook, map[string]any{"url": server.URL}))
	s.artifacts = append(s.artifacts,
		model.ReportArtifact{ID: model.NewID(), Format: model.FormatHTML, State: model.ArtifactFailed},
		model.ReportArtifact{ID: model.NewID(), Format: model.FormatJSON, State: model.ArtifactExpired},
	)

	if err := dispatcher(t, s, f).Deliver(context.Background(), s.run.ID, now); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if strings.Contains(string(body), `"html"`) || strings.Contains(string(body), `"json"`) {
		t.Errorf("a failed or expired artifact was offered to a receiver: %s", body)
	}
}

// The recorded target never carries a credential.
//
// A Slack incoming webhook URL **is** the credential — the path is the secret —
// and the delivery log is read on screen and pasted into support conversations.
// Recording the whole URL would put a working token wherever that conversation
// went.
func TestTheDeliveryLogNeverRecordsACredential(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()

	secretURL := server.URL + "/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
	s, f := fixture(target(model.ReportDeliverySlack, map[string]any{"url": secretURL}))
	_ = dispatcher(t, s, f).Deliver(context.Background(), s.run.ID, now)

	logged := s.log()
	if len(logged) != 1 {
		t.Fatalf("%d rows, want 1", len(logged))
	}
	if strings.Contains(logged[0].Target, "XXXXXXXXXXXXXXXXXXXXXXXX") {
		t.Errorf("target = %q — the path of a Slack webhook URL is the credential", logged[0].Target)
	}
	if !strings.Contains(logged[0].Target, "127.0.0.1") {
		t.Errorf("target = %q, want the host kept so the row is still useful", logged[0].Target)
	}
}

// A disabled channel is a skip, not a failure. Somebody turned it off on purpose
// and marking the delivery failed would put a red mark against a deliberate act.
func TestADisabledChannelIsSkipped(t *testing.T) {
	t.Parallel()

	channelID := model.NewID()
	tgt := target(model.ReportDeliverySlack, map[string]any{})
	tgt.NotificationChannelID = &channelID

	s, f := fixture(tgt)
	s.channelOK = true
	s.channel = model.NotificationChannel{
		ID: channelID, Name: "Client Slack", Type: model.ChannelSlack, Enabled: false,
		Config: json.RawMessage(`{"webhook_url":"https://example.invalid/x"}`),
	}

	_ = dispatcher(t, s, f).Deliver(context.Background(), s.run.ID, now)

	logged := s.log()
	if len(logged) != 1 || logged[0].Outcome != model.DeliverySkipped {
		t.Fatalf("delivery log = %+v, want one skip", logged)
	}
	if !strings.Contains(logged[0].Error, "disabled") {
		t.Errorf("reason = %q, want it to say the channel is disabled", logged[0].Error)
	}
}

type staticVault map[string]any

func (v staticVault) Open(model.ID, model.ID, []byte) (map[string]any, error) {
	return map[string]any(v), nil
}
