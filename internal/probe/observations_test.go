package probe

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// The cadence is the whole design of the observation path, and getting it wrong
// is silent in both directions: send on every check and the buffer covers a
// fifth of the outage it promises, send only on change and a renewal that is
// shed during a control-plane outage is never mentioned again.
func TestObservationCadence(t *testing.T) {
	t.Parallel()

	s := &Session{lastSeen: make(map[string]observationMark)}

	first := check.Observation{Certificate: &check.Certificate{
		FingerprintSHA256: []byte{1, 2, 3},
		NotAfter:          time.Now().Add(90 * 24 * time.Hour),
	}}
	renewed := check.Observation{Certificate: &check.Certificate{
		FingerprintSHA256: []byte{4, 5, 6},
		NotAfter:          time.Now().Add(180 * 24 * time.Hour),
	}}

	at := time.Now()
	if !s.shouldReport("monitor", first, at) {
		t.Fatal("the first observation was not reported; nothing else would ever write the row")
	}
	if s.shouldReport("monitor", first, at.Add(time.Minute)) {
		t.Error("an unchanged certificate was reported a minute later")
	}
	if s.shouldReport("monitor", first, at.Add(observationInterval-time.Second)) {
		t.Error("an unchanged certificate was reported inside the resend interval")
	}
	if !s.shouldReport("monitor", first, at.Add(observationInterval)) {
		t.Error("an unchanged certificate was never re-reported; observed_at would go stale forever")
	}

	// A renewal is the event an operator is actually waiting for, so it goes on
	// the next check rather than on the next hour.
	if !s.shouldReport("monitor", renewed, at.Add(observationInterval+time.Second)) {
		t.Error("a renewed certificate waited for the resend interval")
	}

	// Separate assignments do not share a mark.
	if !s.shouldReport("other", first, at.Add(observationInterval+time.Second)) {
		t.Error("a second monitor was suppressed by the first one's observation")
	}

	// A check that saw neither carries neither, and must not reset the mark for
	// one that did: an unknown outcome between two checks is not evidence the
	// certificate changed.
	if s.shouldReport("monitor", check.Observation{}, at.Add(2*observationInterval)) {
		t.Error("a result with no observation was reported as one")
	}
	if s.shouldReport("monitor", renewed, at.Add(observationInterval+2*time.Second)) {
		t.Error("the mark was lost across a check that observed nothing")
	}
}

// A registration has no fingerprint, so its identity is the date it carries.
func TestObservationCadenceForRegistrations(t *testing.T) {
	t.Parallel()

	s := &Session{lastSeen: make(map[string]observationMark)}
	at := time.Now()
	expiry := at.Add(400 * 24 * time.Hour)

	same := check.Observation{Domain: &check.Domain{Domain: "example.com", ExpiresAt: expiry}}
	// Same date, different registrar record: nothing downstream reacts to the
	// registrar, and re-reporting on it would defeat the interval.
	relabelled := check.Observation{Domain: &check.Domain{
		Domain: "example.com", ExpiresAt: expiry, Registrar: "New Registrar",
	}}
	renewed := check.Observation{Domain: &check.Domain{
		Domain: "example.com", ExpiresAt: expiry.Add(365 * 24 * time.Hour),
	}}

	if !s.shouldReport("domain", same, at) {
		t.Fatal("the first registration was not reported")
	}
	if s.shouldReport("domain", relabelled, at.Add(time.Minute)) {
		t.Error("an unchanged expiry date was reported again")
	}
	if !s.shouldReport("domain", renewed, at.Add(time.Minute)) {
		t.Error("a renewal was not reported")
	}
}

// The buffer bound is a promise in megabytes, and an observation is several
// times the size of the result carrying it. An undercount only shows up during a
// long outage, which is the worst moment to discover the buffer is holding
// multiples of what it claims.
func TestBufferCountsObservations(t *testing.T) {
	t.Parallel()

	id := model.NewID()
	bare := &probev1.Result{ResultId: id[:], MonitorId: id[:], Code: "200"}

	withCert := &probev1.Result{ResultId: id[:], MonitorId: id[:], Code: "200"}
	withCert.Certificate = &probev1.CertificateObservation{
		Subject:                 "CN=api.example.com",
		Issuer:                  "CN=Example CA R3,O=Example CA,C=US",
		SerialNumber:            "04f1e2d3c4b5a697887766554433221100",
		FingerprintSha256:       make([]byte, 32),
		SubjectAlternativeNames: []string{"api.example.com", "www.example.com"},
	}

	if approxSize(withCert) <= approxSize(bare)+100 {
		t.Errorf("approxSize counted an observation as almost free: %d vs %d",
			approxSize(withCert), approxSize(bare))
	}
}
