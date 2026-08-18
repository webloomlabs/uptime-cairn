package model

import "time"

// Status is the stored heartbeat outcome. The integer values are the schema's
// (heartbeats.status) and must not be renumbered — history is written in them.
//
// Note that they do not match the probe protocol's Outcome enum, which is
// proto3 and must start at 0 with UNSPECIFIED. The mapping between the two lives
// in one place, documented in docs/probe/protocol.md §6.2, and casting one to
// the other silently inverts up and down.
type Status uint8

const (
	StatusDown        Status = 0
	StatusUp          Status = 1
	StatusPending     Status = 2
	StatusMaintenance Status = 3

	// StatusUnknown means the probe could not perform the check: capability
	// missing, config unparseable, its own DNS or egress broken. Not a failure
	// of the target, and never a page (ADR-005 decision 16).
	StatusUnknown Status = 4

	// StatusSkipped means the check never started — shed under overload or past
	// its lateness budget. A probe capacity signal.
	StatusSkipped Status = 5
)

// CountsTowardUptime reports whether this status belongs in an uptime ratio.
//
// Only up and down do. unknown and skipped are gaps in observation, not
// observations of failure, and a status page that renders "our probe fell over"
// as customer downtime is lying — Phase 2's SLA reports would inherit the lie.
// pending and maintenance are excluded for their own reasons (data model §5.3).
func (s Status) CountsTowardUptime() bool {
	return s == StatusUp || s == StatusDown
}

// String is for logs and errors, never for storage.
func (s Status) String() string {
	switch s {
	case StatusDown:
		return "down"
	case StatusUp:
		return "up"
	case StatusPending:
		return "pending"
	case StatusMaintenance:
		return "maintenance"
	case StatusUnknown:
		return "unknown"
	case StatusSkipped:
		return "skipped"
	default:
		return "invalid"
	}
}

// Heartbeat is one check result as stored. The hottest row in the system: 5,000
// monitors on the 20-second floor is ~21.6 million of these a day.
type Heartbeat struct {
	// Time is when the check ran, on the probe's clock, at microsecond
	// resolution. Never rewritten by the control plane: a corrected timestamp
	// changes the natural key of a row that may already exist, turning a
	// deduplicated replay into a second row (docs/probe/protocol.md §8).
	Time time.Time

	MonitorID ID
	OrgID     ID

	// ProbeID is recorded from the first commit, even in solo mode where the
	// probe is compiled in. Nullable-and-backfill-later is the retrofit ADR-001
	// exists to prevent.
	ProbeID ID

	Status Status

	// ResponseTime is nil when nothing was measured. Zero is a measurement of
	// zero, which is a different claim.
	ResponseTime *time.Duration

	// Code is the protocol-level result as text, because every protocol spells
	// it differently: HTTP status, DNS rcode, gRPC health status.
	Code string

	// Message is set on failures and state changes only. At 250 rows a second a
	// message on every row is gigabytes a year of "OK", and it must never carry
	// a secret.
	Message string

	Attempt int

	// Important marks a state transition. The control plane owns this: it
	// requires consecutive_failures and the current state, neither of which a
	// probe has (ADR-005 decision 1).
	Important bool

	Suppressed        bool
	SuppressionReason int
}
