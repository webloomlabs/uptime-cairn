package sqlite

import (
	"errors"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The observation tables are STRICT, their JSON column is checked, and their
// source column has a CHECK constraint — so a mismatch between what the control
// plane writes and what the schema accepts is an error at write time, on the
// ingest path, once a certificate somewhere in the world happens to have the
// shape that trips it. Worth a round trip rather than a review.

func TestCertificateRoundTrip(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("Gateway")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	if _, err := s.GetCertificate(t.Context(), monitor.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unobserved certificate = %v, want ErrNotFound", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	validFrom := now.Add(-30 * 24 * time.Hour)
	valid := true
	certificate := model.Certificate{
		MonitorID:         monitor.ID,
		OrgID:             model.SentinelOrgID,
		Subject:           "CN=api.example.com",
		Issuer:            "CN=Example CA R3,O=Example CA,C=US",
		SerialNumber:      "04a1b2c3d4e5f6",
		ValidFrom:         &validFrom,
		ValidTo:           now.Add(60 * 24 * time.Hour),
		FingerprintSHA256: make([]byte, 32),
		SANs:              []string{"api.example.com", "www.example.com"},
		ChainValid:        &valid,
		ObservedAt:        now,
	}
	if err := s.SaveCertificate(t.Context(), certificate); err != nil {
		t.Fatalf("save certificate: %v", err)
	}

	got, err := s.GetCertificate(t.Context(), monitor.ID)
	if err != nil {
		t.Fatalf("get certificate: %v", err)
	}
	if got.Subject != certificate.Subject || got.Issuer != certificate.Issuer ||
		got.SerialNumber != certificate.SerialNumber {
		t.Errorf("identity fields did not survive the round trip: %+v", got)
	}
	if len(got.SANs) != 2 || got.SANs[1] != "www.example.com" {
		t.Errorf("sans = %v, want both names in order", got.SANs)
	}
	if got.ValidFrom == nil || !got.ValidFrom.Equal(validFrom) {
		t.Errorf("valid_from = %v, want %s", got.ValidFrom, validFrom)
	}
	if got.ChainValid == nil || !*got.ChainValid {
		t.Errorf("chain_valid = %v, want true", got.ChainValid)
	}
	if len(got.FingerprintSHA256) != 32 {
		t.Errorf("fingerprint is %d bytes, want 32", len(got.FingerprintSHA256))
	}

	// Replaced in place, not appended to: one row per monitor is what keeps the
	// read a primary-key lookup.
	renewed := certificate
	renewed.SerialNumber = "05f6e5d4c3b2a1"
	renewed.ValidTo = now.Add(120 * 24 * time.Hour)
	renewed.SANs = nil
	renewed.ChainValid = nil
	renewed.ChainError = "unable to get local issuer certificate"
	renewed.ObservedAt = now.Add(time.Hour)
	if err := s.SaveCertificate(t.Context(), renewed); err != nil {
		t.Fatalf("replace certificate: %v", err)
	}

	got, err = s.GetCertificate(t.Context(), monitor.ID)
	if err != nil {
		t.Fatalf("get certificate after replacing: %v", err)
	}
	if got.SerialNumber != renewed.SerialNumber || !got.ValidTo.Equal(renewed.ValidTo) {
		t.Errorf("the replacement did not take: %+v", got)
	}
	if got.SANs != nil {
		t.Errorf("sans = %v, want nil once the new certificate had none", got.SANs)
	}
	// nil is a state the column has to be able to return to. A certificate that
	// stopped being verified must not keep reporting last week's verdict.
	if got.ChainValid != nil {
		t.Errorf("chain_valid = %v, want unset", *got.ChainValid)
	}
	if got.ChainError != renewed.ChainError {
		t.Errorf("chain_error = %q, want %q", got.ChainError, renewed.ChainError)
	}

	var rows int
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM monitor_certificates WHERE monitor_id = ?`, monitor.ID[:]).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("monitor_certificates holds %d rows for one monitor, want 1", rows)
	}
}

func TestDomainExpiryRoundTrip(t *testing.T) {
	t.Parallel()

	s := open(t)
	monitor := testMonitor("example.com")
	if err := s.CreateMonitor(t.Context(), monitor); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	if _, err := s.GetDomainExpiry(t.Context(), monitor.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unobserved registration = %v, want ErrNotFound", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	registration := model.DomainExpiry{
		MonitorID:  monitor.ID,
		OrgID:      model.SentinelOrgID,
		Domain:     "example.com",
		ExpiresAt:  now.Add(300 * 24 * time.Hour),
		Registrar:  "Example Registrar, Inc.",
		Source:     "rdap",
		ObservedAt: now,
	}
	if err := s.SaveDomainExpiry(t.Context(), registration); err != nil {
		t.Fatalf("save registration: %v", err)
	}

	got, err := s.GetDomainExpiry(t.Context(), monitor.ID)
	if err != nil {
		t.Fatalf("get registration: %v", err)
	}
	if got.Domain != "example.com" || got.Registrar != registration.Registrar || got.Source != "rdap" {
		t.Errorf("registration did not survive the round trip: %+v", got)
	}
	if !got.ExpiresAt.Equal(registration.ExpiresAt) {
		t.Errorf("expires_at = %s, want %s", got.ExpiresAt, registration.ExpiresAt)
	}

	// The column takes exactly two spellings, both lowercase. A checker that
	// wrote "RDAP" — which is how the message next to it reads — would fail here
	// rather than in production.
	registration.Source = "whois"
	if err := s.SaveDomainExpiry(t.Context(), registration); err != nil {
		t.Fatalf("save with the other source: %v", err)
	}
	registration.Source = "RDAP"
	if err := s.SaveDomainExpiry(t.Context(), registration); err == nil {
		t.Error("an uppercase source was accepted; the schema's CHECK constraint says otherwise")
	}
}

// The overview's headline figures, which are a range scan over the index the
// migration created rather than a scan of every monitor.
func TestExpiringSoonCounts(t *testing.T) {
	t.Parallel()

	s := open(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	horizon := now.Add(30 * 24 * time.Hour)

	soon := testMonitor("Expiring")
	later := testMonitor("Comfortable")
	domain := testMonitor("example.com")
	for _, m := range []model.Monitor{soon, later, domain} {
		if err := s.CreateMonitor(t.Context(), m); err != nil {
			t.Fatalf("create monitor: %v", err)
		}
	}

	certificate := func(id model.ID, validTo time.Time) model.Certificate {
		return model.Certificate{
			MonitorID: id, OrgID: model.SentinelOrgID,
			ValidTo: validTo, ObservedAt: now,
		}
	}
	if err := s.SaveCertificate(t.Context(), certificate(soon.ID, now.Add(9*24*time.Hour))); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SaveCertificate(t.Context(), certificate(later.ID, now.Add(300*24*time.Hour))); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SaveDomainExpiry(t.Context(), model.DomainExpiry{
		MonitorID: domain.ID, OrgID: model.SentinelOrgID, Domain: "example.com",
		ExpiresAt: now.Add(20 * 24 * time.Hour), Source: "rdap", ObservedAt: now,
	}); err != nil {
		t.Fatalf("save registration: %v", err)
	}

	certificates, domains, err := s.ExpiringSoon(t.Context(), horizon)
	if err != nil {
		t.Fatalf("expiring soon: %v", err)
	}
	if certificates != 1 {
		t.Errorf("certificates expiring soon = %d, want 1", certificates)
	}
	if domains != 1 {
		t.Errorf("domains expiring soon = %d, want 1", domains)
	}

	// An expired certificate is still expiring-soon, and emphatically so: the
	// count exists to be acted on, and dropping the ones already past would
	// empty it at the worst moment.
	if err := s.SaveCertificate(t.Context(), certificate(later.ID, now.Add(-24*time.Hour))); err != nil {
		t.Fatalf("save: %v", err)
	}
	if certificates, _, err = s.ExpiringSoon(t.Context(), horizon); err != nil {
		t.Fatalf("expiring soon: %v", err)
	}
	if certificates != 2 {
		t.Errorf("certificates expiring soon = %d, want 2 with one already expired", certificates)
	}
}
