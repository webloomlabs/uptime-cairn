package model

import (
	"encoding/json"
	"time"
)

// The thirteen channel types, matching notification_channels.type in the schema
// and NotificationChannelType in the OpenAPI spec.
//
// Twelve are native. apprise is the meta-provider: one dependency the operator
// opts into, buying roughly ninety more destinations for the cost of shelling
// out to a binary (PHASE-1-PLAN.md §3.3).
const (
	ChannelEmail     = "email"
	ChannelWebhook   = "webhook"
	ChannelSlack     = "slack"
	ChannelDiscord   = "discord"
	ChannelTelegram  = "telegram"
	ChannelMatrix    = "matrix"
	ChannelGotify    = "gotify"
	ChannelNtfy      = "ntfy"
	ChannelMSTeams   = "msteams"
	ChannelPagerDuty = "pagerduty"
	ChannelOpsgenie  = "opsgenie"
	ChannelTwilio    = "twilio"
	ChannelApprise   = "apprise"
)

// EventType values, matching EventType in the spec. Only the monitor lifecycle
// subset is emitted by this build; the rest are declared because a channel may
// subscribe to them and the stored value has to round-trip.
const (
	EventMonitorUp                  = "monitor.up"
	EventMonitorDown                = "monitor.down"
	EventMonitorPending             = "monitor.pending"
	EventMonitorCreated             = "monitor.created"
	EventMonitorUpdated             = "monitor.updated"
	EventMonitorDeleted             = "monitor.deleted"
	EventMonitorPaused              = "monitor.paused"
	EventMonitorResumed             = "monitor.resumed"
	EventMonitorCertificateExpiring = "monitor.certificate_expiring"
	EventMonitorDomainExpiring      = "monitor.domain_expiring"
	EventIncidentOpened             = "incident.opened"
	EventIncidentUpdated            = "incident.updated"
	EventIncidentResolved           = "incident.resolved"
	EventMaintenanceStarted         = "maintenance.started"
	EventMaintenanceEnded           = "maintenance.ended"
	EventReportGenerated            = "report.generated"
)

// Delivery outcomes, matching notification_deliveries.outcome.
//
// suppressed is a first-class outcome rather than an absence of a row, because
// "we decided not to tell you" and "we tried and failed" are different answers
// to the only question that matters after an outage.
const (
	DeliverySucceeded  = "succeeded"
	DeliveryFailed     = "failed"
	DeliverySuppressed = "suppressed"
)

// NotificationChannel is an alert destination.
//
// Config and Secrets are split at the storage boundary rather than at the API
// boundary, and that is the point: a read path serialising Config cannot leak a
// bot token by accident, because the token is not in Config to leak (data model
// §4.4, §12.1).
type NotificationChannel struct {
	ID    ID
	OrgID ID
	Name  string

	// Type is one of the constants above and is immutable after creation:
	// changing it would reinterpret Config against a different schema.
	Type string

	// Config is the non-secret type-specific JSON, stored verbatim.
	Config json.RawMessage

	// Secrets is the AES-256-GCM envelope holding the writeOnly fields. Nil when
	// the type has none set. Opened only at delivery.
	Secrets []byte

	Enabled   bool
	IsDefault bool

	// Events this channel receives. Empty means every monitor state change,
	// which is the default a user who never opens the events control gets.
	Events []string

	LastUsedAt *time.Time

	// LastError is the most recent delivery failure, kept so a channel that has
	// silently stopped working is visible without reading the delivery log.
	LastError string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// WantsEvent reports whether this channel should receive an event type.
//
// An empty Events list means up and down, and deliberately not pending. Pending
// precedes every down transition, so including it by default would send two
// messages for one outage — which is how people learn to filter the alerts.
// A channel that wants it can subscribe to it explicitly.
func (c NotificationChannel) WantsEvent(eventType string) bool {
	if len(c.Events) == 0 {
		switch eventType {
		case EventMonitorUp, EventMonitorDown:
			return true
		}
		return false
	}
	for _, e := range c.Events {
		if e == eventType {
			return true
		}
	}
	return false
}

// NotificationDelivery is one attempt to reach one channel, recorded whether it
// worked or not.
type NotificationDelivery struct {
	ID              ID
	OrgID           ID
	MonitorID       *ID
	ChannelID       *ID
	EventType       string
	Outcome         string
	Error           string
	DurationMs      *float64
	Attempt         int
	RenderedPayload string
	CreatedAt       time.Time
}
