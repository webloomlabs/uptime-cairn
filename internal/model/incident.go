package model

import "time"

// Incidents: the human narrative laid over the machine's observations.
//
// A monitor going down is a fact; an incident is what somebody decided that
// fact meant, and it is the thing customers read. The two are deliberately
// separate records — an outage with no incident is common and fine, and an
// incident with no outage (a partner's API degrading, a planned rollback going
// wrong) is exactly as real as one with.

// Incident states, matching IncidentState in docs/api/openapi.yaml.
const (
	IncidentInvestigating = "investigating"
	IncidentIdentified    = "identified"
	IncidentMonitoring    = "monitoring"
	IncidentResolved      = "resolved"
)

// Incident impact levels.
const (
	ImpactNone     = "none"
	ImpactMinor    = "minor"
	ImpactMajor    = "major"
	ImpactCritical = "critical"
)

// ValidIncidentState reports whether s is one of the four states.
func ValidIncidentState(s string) bool {
	switch s {
	case IncidentInvestigating, IncidentIdentified, IncidentMonitoring, IncidentResolved:
		return true
	}
	return false
}

// ValidIncidentImpact reports whether s is one of the four impact levels.
func ValidIncidentImpact(s string) bool {
	switch s {
	case ImpactNone, ImpactMinor, ImpactMajor, ImpactCritical:
		return true
	}
	return false
}

// Incident is one declared event, with the monitors and pages it touches.
type Incident struct {
	ID     ID
	OrgID  ID
	Title  string
	State  string
	Impact string

	// StartedAt may be in the past: an incident recorded after the fact is the
	// normal case, because the first half-hour goes on fixing it.
	StartedAt  time.Time
	ResolvedAt *time.Time

	// AutoOpened marks an incident raised from a failing check rather than by a
	// person. Phase 3 sets it; Phase 1 never does, and the field exists now so
	// that a consumer can already tell the two apart.
	AutoOpened bool

	AcknowledgedAt *time.Time
	AcknowledgedBy *ID
	AssignedTo     *ID

	// DetectedAt is when the system first saw the underlying failure, as
	// distinct from when a human declared the incident. The gap between the two
	// is time-to-detect, and it is the only one of the three MTT* figures that
	// cannot be reconstructed from the timeline afterwards.
	DetectedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time

	// MonitorIDs and StatusPageIDs are the join rows, carried inline because
	// every read of an incident wants them and a second round trip per incident
	// is a list page of round trips.
	MonitorIDs    []ID
	StatusPageIDs []ID

	// Updates is the timeline, oldest first. Populated on a single-incident
	// read; empty on a list, where rendering fifty timelines would be a page
	// nobody reads.
	Updates []IncidentUpdate
}

// IncidentUpdate is one entry on the timeline.
//
// State changes travel through here rather than through PATCH, so that every
// change of state carries the sentence explaining it. An incident that moved
// from investigating to identified with no note is an incident whose history
// answers no question anyone will actually ask.
type IncidentUpdate struct {
	ID         ID
	IncidentID ID
	OrgID      ID

	// State is empty when the update does not advance the incident.
	State string

	Body     string
	AuthorID *ID

	NotifiedSubscribers bool
	CreatedAt           time.Time
}

// IncidentMetrics are the three response-time figures, derived at read time.
//
// Deliberately not stored: a stored metric drifts from the timeline it was
// computed from, and the timeline is the thing anyone will argue about
// afterwards (data model §4.8).
type IncidentMetrics struct {
	TimeToDetect      *int64
	TimeToAcknowledge *int64
	TimeToResolve     *int64
}

// Metrics derives the three figures this incident's timestamps support. Each is
// nil until the timestamp it needs exists, rather than zero — zero seconds to
// resolve is a claim, and an unresolved incident is not making it.
func (i Incident) Metrics() IncidentMetrics {
	var m IncidentMetrics
	if i.DetectedAt != nil {
		seconds := int64(i.DetectedAt.Sub(i.StartedAt).Seconds())
		m.TimeToDetect = &seconds
	}
	if i.AcknowledgedAt != nil {
		seconds := int64(i.AcknowledgedAt.Sub(i.StartedAt).Seconds())
		m.TimeToAcknowledge = &seconds
	}
	if i.ResolvedAt != nil {
		seconds := int64(i.ResolvedAt.Sub(i.StartedAt).Seconds())
		m.TimeToResolve = &seconds
	}
	return m
}
