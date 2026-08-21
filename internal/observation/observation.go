// Package observation renders what a check saw onto the probe protocol.
//
// One definition, and that is the entire reason it is a package rather than two
// functions. A check produces a certificate or a registration alongside its
// verdict, and two things put that on the wire: the probe session, for a
// scheduled check, and the API, for a manual one. Written twice, the two agree
// on the day they are written and disagree the first time a field is added —
// and the symptom is a certificate panel that is correct after a scheduled
// check and stale after a manual one, which is the harder of the two to notice.
//
// # Why it is not in internal/protocol
//
// internal/protocol is imported by the control plane, and this package imports
// internal/probe/check. Putting the mapping there would put the checkers in the
// control plane's build graph, and a control plane serving remote probes has no
// business linking checker binaries it does not run (ADR-001). The import
// restriction is the seam, and it is expressed here as a package boundary a
// reviewer can check mechanically: `go list -deps ./internal/controlplane` does
// not name this package, and a change that makes it do so is the change to
// argue about.
//
// # Why it is not in internal/probe/check
//
// Adding a monitor type never changes the probe protocol (ADR-005 decision 6),
// and the way that stays true is that the package owning monitor types does not
// know the protocol exists.
package observation

import (
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/protocol"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Check renders a whole observation — verdict and what was on the wire — into
// the shape the control plane's ingest takes.
func Check(obs check.Observation) protocol.Check {
	return protocol.Check{
		Status:       obs.Status,
		Code:         obs.Code,
		Message:      obs.Message,
		ResponseTime: obs.ResponseTime,
		Certificate:  Certificate(obs.Certificate),
		Domain:       Domain(obs.Domain),
	}
}

// Certificate renders the leaf certificate a handshake presented.
//
// nil in means nil out, and that is load-bearing rather than defensive: a check
// that did not complete a handshake observed nothing, and sending the previous
// certificate again would let the expiry page claim a certificate is being
// served when nothing has served it for a week.
func Certificate(c *check.Certificate) *probev1.CertificateObservation {
	if c == nil {
		return nil
	}
	out := &probev1.CertificateObservation{
		Subject:                 c.Subject,
		Issuer:                  c.Issuer,
		SerialNumber:            c.SerialNumber,
		ValidToUnixMicros:       c.NotAfter.UnixMicro(),
		FingerprintSha256:       c.FingerprintSHA256,
		SubjectAlternativeNames: c.SANs,
		ChainValid:              c.ChainValid,
		ChainError:              c.ChainError,
	}
	// A zero NotBefore is "not reported" rather than "the epoch", and the
	// distinction survives because the field is left at its zero value rather
	// than being filled with arithmetic on a zero time.
	if !c.NotBefore.IsZero() {
		out.ValidFromUnixMicros = c.NotBefore.UnixMicro()
	}
	if c.DaysRemainingThreshold != nil {
		threshold := int32(*c.DaysRemainingThreshold)
		out.DaysRemainingThreshold = &threshold
	}
	return out
}

// Domain renders the registration behind a name.
func Domain(d *check.Domain) *probev1.DomainObservation {
	if d == nil {
		return nil
	}
	out := &probev1.DomainObservation{
		Domain:              d.Domain,
		ExpiresAtUnixMicros: d.ExpiresAt.UnixMicro(),
		Registrar:           d.Registrar,
		Source:              d.Source,
	}
	if d.DaysRemainingThreshold != nil {
		threshold := int32(*d.DaysRemainingThreshold)
		out.DaysRemainingThreshold = &threshold
	}
	return out
}
