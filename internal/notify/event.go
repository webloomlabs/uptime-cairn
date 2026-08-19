package notify

import (
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Event is one thing that happened, in the shape the spec's EventEnvelope
// documents. It is the unit the dispatcher queues, the templater renders, and
// the outbound webhook will eventually post verbatim.
//
// It carries flattened copies rather than pointers into the store, because it
// outlives the request that produced it: a delivery retried ninety seconds later
// must describe the world as it was when the event happened, not as it is when
// the retry lands. An alert saying "down" that arrives after recovery is
// confusing; an alert that silently rewrites itself to "up" is worse.
type Event struct {
	ID         model.ID
	Type       string
	OccurredAt time.Time

	Instance Instance
	Monitor  Monitor

	// Heartbeat is the check that caused the event, where there was one.
	// Absent for lifecycle events like monitor.created.
	Heartbeat *Heartbeat

	// PreviousStatus is what the monitor was before this event moved it. Empty
	// when the event is not a transition.
	PreviousStatus string

	// Incident is the subject of an incident.* event, and nil for every other
	// kind. The envelope's `data` object is shaped by the event type, which is
	// what the spec's EventEnvelope says and what a receiver branches on.
	Incident *Incident
}

// Instance identifies the sending install, so an operator running three of them
// can tell which one is shouting.
type Instance struct {
	Name    string
	BaseURL string
	Version string
}

// Monitor is the subject, flattened.
type Monitor struct {
	ID          model.ID
	Name        string
	Description string
	Type        string
	Target      string
	Status      string
}

// Heartbeat is the observation, flattened.
type Heartbeat struct {
	Time           time.Time
	Status         string
	ResponseTimeMs *float64
	Message        string
	Code           string
	Attempt        int
	Important      bool
}

// NewEvent builds an event from a monitor, its new state, and the heartbeat that
// moved it.
func NewEvent(eventType string, inst Instance, m model.Monitor, previous string, beat *model.Heartbeat, status string, at time.Time) Event {
	ev := Event{
		ID:         model.NewID(),
		Type:       eventType,
		OccurredAt: at.UTC(),
		Instance:   inst,
		Monitor: Monitor{
			ID:          m.ID,
			Name:        m.Name,
			Description: m.Description,
			Type:        m.Type,
			Target:      m.Target,
			Status:      status,
		},
		PreviousStatus: previous,
	}
	if beat != nil {
		hb := Heartbeat{
			Time:      beat.Time,
			Status:    beat.Status.String(),
			Message:   beat.Message,
			Code:      beat.Code,
			Attempt:   beat.Attempt,
			Important: beat.Important,
		}
		if beat.ResponseTime != nil {
			ms := float64(beat.ResponseTime.Microseconds()) / 1000.0
			hb.ResponseTimeMs = &ms
		}
		ev.Heartbeat = &hb
	}
	return ev
}

// Severity says how loud a channel should be about this event. Only the on-call
// providers act on it, and they are the ones where getting it wrong wakes
// somebody.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityCritical
)

// Severity of the event.
func (e Event) Severity() Severity {
	switch e.Type {
	case model.EventMonitorDown:
		return SeverityCritical
	case model.EventMonitorPending, model.EventMonitorCertificateExpiring, model.EventMonitorDomainExpiring:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

// Resolves reports whether this event closes something an earlier one opened.
// PagerDuty and Opsgenie both need it: an outage and its recovery are one
// incident with two edges, not two incidents.
func (e Event) Resolves() bool { return e.Type == model.EventMonitorUp }

// DedupKey is the stable identity of the thing being alerted about, so a
// provider that models incidents can resolve the same one it opened. Keyed by
// monitor rather than by event, which is the whole point.
func (e Event) DedupKey() string { return "cairn-monitor-" + e.Monitor.ID.String() }

// Incident is the subject of an incident.* event, flattened for the same reason
// Monitor is: a delivery retried ninety seconds later must describe the incident
// as it was when the event happened, not as it is when the retry lands.
type Incident struct {
	ID         model.ID
	Title      string
	State      string
	Impact     string
	StartedAt  time.Time
	ResolvedAt *time.Time

	// MonitorIDs are the monitors the incident names, rendered as strings
	// because a template variable is text and a receiver reading the envelope
	// wants the same form the REST API gave it.
	MonitorIDs []string
}

// NewIncidentEvent builds an incident lifecycle event.
//
// The monitor half of the envelope stays empty rather than being filled with the
// first affected monitor. An incident is not about one monitor — that is the
// whole reason it is a separate record — and picking one would make a template
// like "{{monitor.name}} is down" render something arbitrary.
func NewIncidentEvent(eventType string, inst Instance, in Incident, at time.Time) Event {
	return Event{
		ID:         model.NewID(),
		Type:       eventType,
		OccurredAt: at.UTC(),
		Instance:   inst,
		Incident:   &in,
	}
}
