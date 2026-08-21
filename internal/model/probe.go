package model

import "time"

// Probe modes, matching probes.mode in the schema.
//
// The distinction is identity: an embedded probe's row *is* its identity, seeded
// by migration 0001 and carrying no token, because the process it belongs to is
// the same process as the control plane. A remote probe enrols and is issued a
// credential (ADR-005 decision 8).
const (
	ProbeModeEmbedded = "embedded"
	ProbeModeRemote   = "remote"
)

// Probe is one check executor.
//
// Solo mode has exactly one, embedded and unnamed on the wire, and it still has
// a row — heartbeats reference a probe from the first commit, because
// nullable-and-backfill-later is the retrofit ADR-001 exists to avoid.
//
// It is read-only in this build. Enrolment, revocation, and the operator screens
// that go with them are Phase 4; what Phase 1 needs is the ability to *name* one,
// so a monitor that can only be answered from a particular host can say so.
type Probe struct {
	ID    ID
	OrgID ID
	Name  string

	// Region is free text and advisory. It groups probes for the multi-region
	// selection Phase 4 adds; it is never how a host-local monitor is placed,
	// because a region can hold more than one host and `docker` needs the host
	// (protocol §6.4).
	Region string

	// Mode is ProbeModeEmbedded or ProbeModeRemote.
	Mode string

	// Version is the agent version last reported. Empty for a probe that has
	// never connected.
	Version string

	// LastSeenAt is nil for a probe that has never connected.
	LastSeenAt *time.Time

	Enabled   bool
	CreatedAt time.Time
}

// TokenHash is deliberately absent from this struct. It is a credential, it is
// never read by anything that renders a probe, and a field that does not exist
// cannot be serialised into a response by a future handler that forgot.
