package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Certificate and domain observations: the half of a check that is not a
// verdict.
//
// A check produces two independent facts. Whether the target answered, which is
// the heartbeat — and what was on the wire while it did: the certificate, or the
// registration behind the name. The second one outlives the first. An http
// monitor is up every minute for ninety days and the certificate underneath it
// expires exactly once, and no run of up-heartbeats records when.
//
// So observations are stored beside heartbeats rather than inside them, one row
// per monitor replaced in place, and they are what /monitors/{id}/certificate,
// `include=certificate`, and the overview's expiring-soon counts read.
//
// Three rules govern this file:
//
//   - Nothing here fails ingest. By the time it runs the heartbeats are durable
//     and the acknowledgement is earned; failing now would resend a batch that
//     is already written, to chase a row that changes twice a year.
//   - The verdict is not recomputed. The probe decided up or down. This decides
//     only what is worth recording, and what is worth saying about expiry.
//   - Expiry alerting is deduplicated against the stored row rather than against
//     process memory, so a restart cannot re-page for something already sent.
type observed struct {
	monitor model.Monitor
	beat    model.Heartbeat

	// status is the monitor's status after this result. Carried because an
	// expiry event is not a transition — the monitor is whatever it already was,
	// and an http monitor serving an expiring certificate is emphatically up.
	status string

	result *probev1.Result
}

// record writes the observations a batch carried and returns the alerts they
// raised, for the caller to publish alongside the transition alerts.
func (s *Server) record(ctx context.Context, batch []observed) []pendingAlert {
	var alerts []pendingAlert

	for _, o := range batch {
		if c := o.result.GetCertificate(); c != nil {
			if raised, ok := s.recordCertificate(ctx, o, c); ok {
				alerts = append(alerts, raised)
			}
		}
		if d := o.result.GetDomain(); d != nil {
			if raised, ok := s.recordDomain(ctx, o, d); ok {
				alerts = append(alerts, raised)
			}
		}
	}
	return alerts
}

func (s *Server) recordCertificate(ctx context.Context, o observed, seen *probev1.CertificateObservation) (pendingAlert, bool) {
	validTo := seen.GetValidToUnixMicros()
	if validTo == 0 {
		// valid_to is the one field the schema will not take a null for, and a
		// certificate without one is far more likely to be a probe bug than a
		// real observation. Logged rather than stored as something invented.
		s.log.Warn("certificate observation carries no expiry date", "monitor", o.monitor.ID)
		return pendingAlert{}, false
	}

	current := model.Certificate{
		MonitorID:         o.monitor.ID,
		OrgID:             o.monitor.OrgID,
		Subject:           seen.GetSubject(),
		Issuer:            seen.GetIssuer(),
		SerialNumber:      seen.GetSerialNumber(),
		ValidTo:           time.UnixMicro(validTo).UTC(),
		FingerprintSHA256: seen.GetFingerprintSha256(),
		SANs:              seen.GetSubjectAlternativeNames(),
		ChainError:        seen.GetChainError(),
		ObservedAt:        o.beat.Time,
	}
	if from := seen.GetValidFromUnixMicros(); from != 0 {
		at := time.UnixMicro(from).UTC()
		current.ValidFrom = &at
	}
	if seen.ChainValid != nil {
		valid := seen.GetChainValid()
		current.ChainValid = &valid
	}

	// Read before write: the alert decision needs the row this one replaces, and
	// after the upsert it is gone. Affordable here precisely because the probe
	// resends an unchanged observation about once an hour per monitor rather
	// than once per check.
	previous, err := s.store.GetCertificate(ctx, o.monitor.ID)
	prior := &previous
	if errors.Is(err, store.ErrNotFound) {
		prior = nil
	} else if err != nil {
		s.log.Error("read certificate before replacing it", "monitor", o.monitor.ID, "error", err)
		return pendingAlert{}, false
	}

	if err := s.store.SaveCertificate(ctx, current); err != nil {
		s.log.Error("save certificate", "monitor", o.monitor.ID, "error", err)
		return pendingAlert{}, false
	}

	threshold, drawn := thresholdOf(seen.DaysRemainingThreshold)
	if !drawn || underMaintenance(o.beat) {
		// No threshold means the operator drew no line on this monitor — an http
		// monitor was not created to watch a certificate — so the observation is
		// recorded and nobody is told. A maintenance window is the other silence:
		// planned downtime that pages somebody is not planned downtime.
		return pendingAlert{}, false
	}

	days := current.DaysRemaining(o.beat.Time)
	if !certificateWorthSaying(prior, current, days, threshold) {
		return pendingAlert{}, false
	}

	// The monitor's target leads, because it is what the operator recognises;
	// the subject is the fallback for a monitor that never promoted one.
	subject := o.monitor.Target
	if subject == "" {
		subject = current.Subject
	}
	return s.expiryAlert(o, model.EventMonitorCertificateExpiring, days,
		fmt.Sprintf("certificate for %s %s, on %s",
			subject, expiryPhrase(days), current.ValidTo.Format(time.RFC3339))), true
}

func (s *Server) recordDomain(ctx context.Context, o observed, seen *probev1.DomainObservation) (pendingAlert, bool) {
	expiresAt := seen.GetExpiresAtUnixMicros()
	if expiresAt == 0 || seen.GetDomain() == "" {
		s.log.Warn("domain observation carries no domain or no expiry date", "monitor", o.monitor.ID)
		return pendingAlert{}, false
	}

	current := model.DomainExpiry{
		MonitorID:  o.monitor.ID,
		OrgID:      o.monitor.OrgID,
		Domain:     seen.GetDomain(),
		ExpiresAt:  time.UnixMicro(expiresAt).UTC(),
		Registrar:  seen.GetRegistrar(),
		Source:     seen.GetSource(),
		ObservedAt: o.beat.Time,
	}

	previous, err := s.store.GetDomainExpiry(ctx, o.monitor.ID)
	prior := &previous
	if errors.Is(err, store.ErrNotFound) {
		prior = nil
	} else if err != nil {
		s.log.Error("read domain expiry before replacing it", "monitor", o.monitor.ID, "error", err)
		return pendingAlert{}, false
	}

	if err := s.store.SaveDomainExpiry(ctx, current); err != nil {
		s.log.Error("save domain expiry", "monitor", o.monitor.ID, "error", err)
		return pendingAlert{}, false
	}

	threshold, drawn := thresholdOf(seen.DaysRemainingThreshold)
	if !drawn || underMaintenance(o.beat) {
		return pendingAlert{}, false
	}

	days := current.DaysRemaining(o.beat.Time)
	if !domainWorthSaying(prior, current, days, threshold) {
		return pendingAlert{}, false
	}

	message := fmt.Sprintf("the registration for %s %s, on %s", current.Domain,
		expiryPhrase(days), current.ExpiresAt.Format(time.RFC3339))
	if current.Registrar != "" {
		message += ", registered with " + current.Registrar
	}
	return s.expiryAlert(o, model.EventMonitorDomainExpiring, days, message), true
}

// expiryAlert builds the pending alert for an expiry event.
//
// The heartbeat it carries is a copy with the expiry sentence written into it,
// and that is deliberate rather than incidental. The envelope has exactly one
// place for the words describing what happened, and for an http monitor that is
// up the check's own message is empty — an alert saying "certificate expiring"
// and nothing else is an alert the operator has to go and research at the moment
// they are least able to. Nothing stored changes: the heartbeat row went to disk
// before this ran, carrying the check's own words.
func (s *Server) expiryAlert(o observed, eventType string, days int, message string) pendingAlert {
	beat := o.beat
	beat.Message = message
	beat.Code = strconv.Itoa(days)

	return pendingAlert{
		monitor: o.monitor,
		beat:    beat,
		alert:   alert{fire: true, eventType: eventType},
		status:  o.status,
	}
}

// certificateWorthSaying decides whether this observation is worth an event.
//
// The shape of the problem: a check runs every twenty seconds and an expiry date
// moves once every ninety days, so "tell me when it is close" has to mean
// something other than "tell me on every check". It fires when the countdown
// first crosses the operator's line, when the certificate is replaced by another
// one that is also inside the line, and once a day after that — the cadence
// somebody acting on it needs, and quiet enough that it is not the alert people
// switch off.
//
// The comparison is against the stored row, so it survives a restart. A control
// plane that kept this in memory would re-page for every expiring certificate
// on the install every time it was upgraded.
func certificateWorthSaying(previous *model.Certificate, current model.Certificate, days, threshold int) bool {
	if days > threshold {
		return false
	}
	if previous == nil {
		return true
	}
	if !bytes.Equal(previous.FingerprintSHA256, current.FingerprintSHA256) {
		// A different certificate, and still inside the threshold: either a
		// renewal that did not buy enough time or a swap to something worse.
		// Both are worth one line.
		return true
	}
	// Same certificate as last time. Say it again only when the countdown has
	// ticked over, which is once a day.
	return previous.DaysRemaining(previous.ObservedAt) != days
}

// domainWorthSaying is certificateWorthSaying for a registration, which has no
// fingerprint: its identity is the date, so a renewal is a date that moved.
func domainWorthSaying(previous *model.DomainExpiry, current model.DomainExpiry, days, threshold int) bool {
	if days > threshold {
		return false
	}
	if previous == nil {
		return true
	}
	if !previous.ExpiresAt.Equal(current.ExpiresAt) {
		return true
	}
	return previous.DaysRemaining(previous.ObservedAt) != days
}

// underMaintenance is the only suppression that silences an expiry event, and
// the exclusion of the other one is the point.
//
// Dependency suppression withholds a page because the child's failure is already
// explained by the parent's — and an expiring certificate is not explained by
// anything upstream being down. It is a fact about this monitor's own target,
// true whether or not the router in front of it is answering, and the operator
// who is mid-incident is exactly the one who does not want to discover it next
// week instead.
func underMaintenance(beat model.Heartbeat) bool {
	return beat.Suppressed && beat.SuppressionReason == model.SuppressionMaintenance
}

// thresholdOf reads the operator's line off the wire. Absent means they drew
// none, which is different from zero — zero is "only once it has actually
// expired", and it is a real choice a tls_expiry monitor can make.
func thresholdOf(threshold *int32) (int, bool) {
	if threshold == nil {
		return 0, false
	}
	return int(*threshold), true
}

// expiryPhrase reads correctly at both ends of the range, including past it. A
// lapsed certificate is a fact worth stating plainly rather than clamping to
// "expires in 0 days", which reads as a bug in the alert rather than a problem
// with the target.
func expiryPhrase(days int) string {
	switch {
	case days < 0:
		return "expired " + humaniseDays(-days) + " ago"
	case days == 0:
		return "expires today"
	default:
		return "expires in " + humaniseDays(days)
	}
}

func humaniseDays(days int) string {
	if days == 1 {
		return "1 day"
	}
	return strconv.Itoa(days) + " days"
}
