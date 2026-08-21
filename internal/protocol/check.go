package protocol

import (
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Check is one completed check in the vocabulary the control plane ingests.
//
// It exists so that a check run inside the API — "check now" — reaches ingest
// carrying everything a probe's check would have carried, rather than the
// subset that happened to fit in a parameter list. The two facts a check
// produces are a verdict and an observation, and the verdict alone is what the
// manual path used to deliver: pressing "check now" after installing a
// certificate refreshed the up/down state and left the expiry row showing the
// old one until the next scheduled check.
//
// The observation fields are the protocol's types rather than the checker's on
// purpose. This struct is named by the control plane's ingest signature, and
// the control plane must not import internal/probe/check (ADR-001): a control
// plane serving remote probes has no business linking the checker binaries it
// does not run. So the mapping happens on the caller's side of the seam, in
// internal/observation, which is imported by the probe and by the API and by
// neither the control plane nor this package.
type Check struct {
	// Status is the verdict: up, down, unknown, or skipped. Never pending or
	// maintenance, which require control-plane knowledge.
	Status model.Status

	// Code is the protocol-level result — HTTP status, DNS rcode, gRPC health.
	Code string

	// Message is user-facing and has every credential redacted already.
	Message string

	// ResponseTime is nil when nothing was measured.
	ResponseTime *time.Duration

	// Certificate and Domain are what was on the wire alongside the verdict.
	// Both nil for a check that observed neither, which is most checks.
	Certificate *probev1.CertificateObservation
	Domain      *probev1.DomainObservation
}
