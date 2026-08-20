package controlplane

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Certificate and domain observations, and the alert that comes off them.
//
// Two things are worth testing here and neither is visible in a heartbeat. That
// the observation is stored at all — every expiry surface in the product reads
// the row and nothing else writes it — and the cadence of
// monitor.certificate_expiring, which is the alert people actually want from a
// TLS monitor and the one they turn off if it arrives every twenty seconds.

func certificateResult(id model.ID, at time.Time, notAfter time.Time, fingerprint byte, threshold *int32) *probev1.Result {
	r := result(id, probev1.Outcome_OUTCOME_UP, at)
	r.Certificate = &probev1.CertificateObservation{
		Subject:                 "CN=api.example.com",
		Issuer:                  "CN=Example CA R3",
		SerialNumber:            "04a1b2c3",
		ValidFromUnixMicros:     notAfter.Add(-90 * 24 * time.Hour).UnixMicro(),
		ValidToUnixMicros:       notAfter.UnixMicro(),
		FingerprintSha256:       []byte{fingerprint},
		SubjectAlternativeNames: []string{"api.example.com"},
		DaysRemainingThreshold:  threshold,
	}
	return r
}

func days(n int) *int32 {
	v := int32(n)
	return &v
}

func TestCertificateObservationIsStored(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	server, store, _ := newAlertingServer(t, monitor)

	at := time.Now().UTC().Truncate(time.Millisecond)
	notAfter := at.Add(60 * 24 * time.Hour).Truncate(time.Microsecond)
	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{certificateResult(monitor.ID, at, notAfter, 1, days(14))}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	stored := store.certificate
	if stored == nil {
		t.Fatal("nothing was stored; every expiry surface in the product reads this row")
	}
	if !stored.ValidTo.Equal(notAfter) {
		t.Errorf("valid_to = %s, want %s", stored.ValidTo, notAfter)
	}
	if stored.MonitorID != monitor.ID || stored.OrgID != monitor.OrgID {
		t.Error("the row was filed against the wrong monitor or organisation")
	}
	if stored.Subject != "CN=api.example.com" || len(stored.SANs) != 1 {
		t.Errorf("certificate detail lost in transit: %+v", stored)
	}
	if !stored.ObservedAt.Equal(at) {
		t.Errorf("observed_at = %s, want the check's own time %s", stored.ObservedAt, at)
	}
	// The chain was not evaluated by this observation, and nil is the only
	// honest answer — false would report the monitor as having a broken chain.
	if stored.ChainValid != nil {
		t.Errorf("chain_valid = %v, want unset", *stored.ChainValid)
	}
}

// The cadence table. A check runs every twenty seconds and an expiry date moves
// once every ninety days, so the interesting question is not whether the event
// fires but how often.
func TestCertificateExpiringCadence(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	server, _, alerts := newAlertingServer(t, monitor)

	at := time.Now().UTC().Truncate(time.Millisecond)
	// Nine days left, against a fourteen-day threshold: inside the line from the
	// first observation.
	notAfter := at.Add(9*24*time.Hour + time.Hour)

	feedResult := func(offset time.Duration, fingerprint byte, expiry time.Time) {
		t.Helper()
		if _, err := server.ingest(context.Background(),
			[]*probev1.Result{certificateResult(monitor.ID, at.Add(offset), expiry, fingerprint, days(14))}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}

	feedResult(0, 1, notAfter)
	if got := len(expiringEvents(alerts)); got != 1 {
		t.Fatalf("crossing the threshold raised %d events, want 1", got)
	}

	// Same certificate, an hour later: the countdown has not ticked over and
	// there is nothing new to say.
	feedResult(time.Hour, 1, notAfter)
	if got := len(expiringEvents(alerts)); got != 1 {
		t.Errorf("an unchanged certificate raised %d events, want 1", got)
	}

	// A day later the figure has moved from 9 to 8, which is the cadence
	// somebody acting on it needs.
	feedResult(25*time.Hour, 1, notAfter)
	if got := len(expiringEvents(alerts)); got != 2 {
		t.Errorf("a day later raised %d events in total, want 2", got)
	}

	// Renewed, and well clear: silence, and specifically not a "certificate
	// expiring" event about a certificate that is not.
	feedResult(26*time.Hour, 2, at.Add(90*24*time.Hour))
	if got := len(expiringEvents(alerts)); got != 2 {
		t.Errorf("a renewal raised an expiry event: %d in total, want 2", got)
	}

	// Renewed into something that is still inside the threshold. A different
	// certificate is worth one line even though the day count barely moved.
	feedResult(27*time.Hour, 3, at.Add(10*24*time.Hour))
	if got := len(expiringEvents(alerts)); got != 3 {
		t.Errorf("a renewal that did not buy enough time raised %d events in total, want 3", got)
	}
}

// An http monitor was not created to watch a certificate. Its observation is
// recorded so the expiry page can show it, and nobody is paged about it — the
// operator who wants to be told adds a tls_expiry monitor, which is the type
// that asks for the threshold.
func TestCertificateWithNoThresholdIsRecordedAndSilent(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	server, store, alerts := newAlertingServer(t, monitor)

	at := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{certificateResult(monitor.ID, at, at.Add(24*time.Hour), 1, nil)}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if store.certificate == nil {
		t.Error("an observation with no threshold was not recorded")
	}
	if got := expiringEvents(alerts); len(got) != 0 {
		t.Errorf("a monitor that drew no expiry line was alerted about: %v", got)
	}
}

// An expiry event is not a transition. Reporting the monitor as pending would be
// wrong in the one field a receiver branches on: an http monitor serving an
// expiring certificate is up.
func TestExpiryEventCarriesTheMonitorsActualStatus(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	server, _, alerts := newAlertingServer(t, monitor)

	at := time.Now().UTC().Truncate(time.Millisecond)
	// Up first, so the monitor is not sitting in pending when the observation
	// lands.
	feed(t, server, monitor.ID, probev1.Outcome_OUTCOME_UP)
	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{certificateResult(monitor.ID, at.Add(time.Minute), at.Add(3*24*time.Hour), 1, days(14))}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	events := expiringEvents(alerts)
	if len(events) != 1 {
		t.Fatalf("raised %d expiry events, want 1", len(events))
	}
	if events[0].Monitor.Status != model.MonitorStatusUp {
		t.Errorf("status = %q, want %q", events[0].Monitor.Status, model.MonitorStatusUp)
	}
	if events[0].PreviousStatus != "" {
		t.Errorf("previous_status = %q, want empty: an expiry is not a transition", events[0].PreviousStatus)
	}
	// The words are the whole value of the alert. An http monitor that is up has
	// no message of its own, so an envelope carrying the check's empty one would
	// say "certificate expiring" and nothing else.
	if events[0].Heartbeat == nil || events[0].Heartbeat.Message == "" {
		t.Fatal("the expiry event carried no message")
	}
	if got := events[0].Heartbeat.Message; !strings.Contains(got, "expires in 2 days") || !strings.Contains(got, monitor.Target) {
		t.Errorf("message = %q, want the target and the days remaining", got)
	}
}

// Planned downtime that pages somebody is not planned downtime, and a
// maintenance window is the most known problem there is.
func TestExpiryIsSilentUnderMaintenance(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	server, store, alerts := newAlertingServer(t, monitor)
	store.state.SuppressedBy = model.SuppressedByMaintenance

	at := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{certificateResult(monitor.ID, at, at.Add(24*time.Hour), 1, days(14))}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if store.certificate == nil {
		t.Error("maintenance suppressed the observation as well as the alert; the check still ran and still saw it")
	}
	if got := expiringEvents(alerts); len(got) != 0 {
		t.Errorf("a monitor under maintenance was paged about expiry: %v", got)
	}
}

// A certificate with no expiry date is not a certificate this schema can store,
// and inventing one would put a wrong date in front of an operator. The result
// is still a heartbeat.
func TestCertificateWithoutAnExpiryIsNotStored(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	server, store, _ := newAlertingServer(t, monitor)

	at := time.Now().UTC().Truncate(time.Millisecond)
	r := result(monitor.ID, probev1.Outcome_OUTCOME_UP, at)
	r.Certificate = &probev1.CertificateObservation{Subject: "CN=api.example.com"}
	if _, err := server.ingest(context.Background(), []*probev1.Result{r}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if store.certificate != nil {
		t.Error("a certificate with no expiry date was stored")
	}
	if len(store.written) != 1 {
		t.Errorf("heartbeats written = %d, want 1: a bad observation must not cost the check", len(store.written))
	}
}

func TestDomainObservationIsStoredAndAlerted(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	monitor.Type = model.TypeDomainExpiry
	monitor.Target = "example.com"
	server, store, alerts := newAlertingServer(t, monitor)

	at := time.Now().UTC().Truncate(time.Millisecond)
	// Twenty days and a few hours, so the second observation an hour later is
	// still twenty days out: the cadence rule is "once the countdown ticks
	// over", and a fixture sitting exactly on the boundary would test the
	// boundary rather than the rule.
	expiresAt := at.Add(20*24*time.Hour + 3*time.Hour).Truncate(time.Microsecond)

	domainResult := func(offset time.Duration, expiry time.Time) *probev1.Result {
		r := result(monitor.ID, probev1.Outcome_OUTCOME_UP, at.Add(offset))
		r.Domain = &probev1.DomainObservation{
			Domain:                 "example.com",
			ExpiresAtUnixMicros:    expiry.UnixMicro(),
			Registrar:              "Example Registrar, Inc.",
			Source:                 "rdap",
			DaysRemainingThreshold: days(30),
		}
		return r
	}

	if _, err := server.ingest(context.Background(), []*probev1.Result{domainResult(0, expiresAt)}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	stored := store.domain
	if stored == nil {
		t.Fatal("nothing was stored; the overview's expiring-soon count reads this row")
	}
	if stored.Domain != "example.com" || stored.Registrar != "Example Registrar, Inc." || stored.Source != "rdap" {
		t.Errorf("registration detail lost in transit: %+v", stored)
	}
	if !stored.ExpiresAt.Equal(expiresAt) {
		t.Errorf("expires_at = %s, want %s", stored.ExpiresAt, expiresAt)
	}

	events := expiringEvents(alerts)
	if len(events) != 1 {
		t.Fatalf("raised %d expiry events, want 1", len(events))
	}
	if events[0].Type != model.EventMonitorDomainExpiring {
		t.Errorf("event type = %q, want %q", events[0].Type, model.EventMonitorDomainExpiring)
	}
	if msg := events[0].Heartbeat.Message; !strings.Contains(msg, "example.com") || !strings.Contains(msg, "Example Registrar") {
		t.Errorf("message = %q, want the domain and its registrar", msg)
	}

	// Unchanged an hour later: the same registration, and nothing new to say.
	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{domainResult(time.Hour, expiresAt)}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := len(expiringEvents(alerts)); got != 1 {
		t.Errorf("an unchanged registration raised %d events, want 1", got)
	}

	// Renewed for another year: silence, and the stored row moves on.
	renewed := expiresAt.Add(365 * 24 * time.Hour)
	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{domainResult(2*time.Hour, renewed)}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := len(expiringEvents(alerts)); got != 1 {
		t.Errorf("a renewal raised an expiry event: %d in total, want 1", got)
	}
	if !store.domain.ExpiresAt.Equal(renewed) {
		t.Errorf("expires_at = %s, want the renewed date %s", store.domain.ExpiresAt, renewed)
	}
}

// A restart must not re-page for something already sent, which is why the
// deduplication is against the stored row rather than against memory.
func TestExpiryDeduplicationSurvivesARestart(t *testing.T) {
	t.Parallel()

	monitor := monitorFor(t)
	server, store, alerts := newAlertingServer(t, monitor)

	at := time.Now().UTC().Truncate(time.Millisecond)
	notAfter := at.Add(5*24*time.Hour + time.Hour)
	if _, err := server.ingest(context.Background(),
		[]*probev1.Result{certificateResult(monitor.ID, at, notAfter, 1, days(14))}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := len(expiringEvents(alerts)); got != 1 {
		t.Fatalf("first observation raised %d events, want 1", got)
	}

	// A second control plane over the same store, which is what a restart looks
	// like from here: no memory of the first, and the row is still there.
	restarted, restartedAlerts := serverOver(t, store)
	if _, err := restarted.ingest(context.Background(),
		[]*probev1.Result{certificateResult(monitor.ID, at.Add(time.Minute), notAfter, 1, days(14))}); err != nil {
		t.Fatalf("ingest after restart: %v", err)
	}
	if got := expiringEvents(restartedAlerts); len(got) != 0 {
		t.Errorf("a restart re-paged for a certificate already alerted on: %v", got)
	}
}

func TestExpiryPhrasing(t *testing.T) {
	t.Parallel()

	// "expires in 0 days" and "expires in 1 days" both read as a bug in the
	// alert rather than a problem with the target, and "-3 days" reads as one
	// too.
	for value, want := range map[int]string{
		30: "expires in 30 days",
		1:  "expires in 1 day",
		0:  "expires today",
		-1: "expired 1 day ago",
		-3: "expired 3 days ago",
	} {
		if got := expiryPhrase(value); got != want {
			t.Errorf("expiryPhrase(%d) = %q, want %q", value, got, want)
		}
	}
}

func expiringEvents(a *recordingAlerter) []notify.Event {
	a.mu.Lock()
	defer a.mu.Unlock()

	var out []notify.Event
	for _, e := range a.events {
		if e.Type == model.EventMonitorCertificateExpiring || e.Type == model.EventMonitorDomainExpiring {
			out = append(out, e)
		}
	}
	return out
}

// serverOver is a second control plane over the same store, which is what a
// restart looks like from in here: no memory of what the first one sent, and the
// stored rows still where it left them.
func serverOver(t *testing.T, store *fakeStore) (*Server, *recordingAlerter) {
	t.Helper()

	alerts := &recordingAlerter{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, NewPublisher(), alerts, nil, log, model.EmbeddedProbeID, model.SentinelOrgID), alerts
}

// Which suppression silences an expiry event, and which deliberately does not.
//
// Dependency suppression withholds a page because the child's failure is already
// explained by the parent's. An expiring certificate is not explained by
// anything upstream being down — it is a fact about this monitor's own target,
// and the operator mid-incident is exactly the one who should not find out about
// it next week instead.
func TestOnlyMaintenanceSilencesExpiry(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		beat model.Heartbeat
		want bool
	}{
		"not suppressed": {
			beat: model.Heartbeat{},
		},
		"maintenance": {
			beat: model.Heartbeat{Suppressed: true, SuppressionReason: model.SuppressionMaintenance},
			want: true,
		},
		"dependency": {
			beat: model.Heartbeat{Suppressed: true, SuppressionReason: model.SuppressionDependency},
		},
	}
	for name, tc := range cases {
		if got := underMaintenance(tc.beat); got != tc.want {
			t.Errorf("%s: underMaintenance = %v, want %v", name, got, tc.want)
		}
	}
}
