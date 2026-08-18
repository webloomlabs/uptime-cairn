package model

import "time"

// Monitor statuses as stored in monitor_state.status and rendered by the API.
// Distinct from Status, which is the per-heartbeat outcome: a monitor is paused,
// a heartbeat never is.
const (
	MonitorStatusUp          = "up"
	MonitorStatusDown        = "down"
	MonitorStatusPending     = "pending"
	MonitorStatusPaused      = "paused"
	MonitorStatusMaintenance = "maintenance"
)

// MonitorState is what a monitor is currently doing, kept in its own table so a
// status update every 20 seconds does not rewrite the row holding the user's
// configuration (data model §4.2).
type MonitorState struct {
	MonitorID           ID
	OrgID               ID
	Status              string
	LastCheckAt         *time.Time
	NextCheckAt         *time.Time
	LastStatusChangeAt  *time.Time
	ConsecutiveFailures int
	LastResponseTimeMs  *float64
	LastMessage         string
	SuppressedBy        string

	// StateVersion increments on every state change and feeds ADR-004's
	// membership signal, which is how a browser learns its filtered view moved
	// without being sent the view.
	StateVersion int64
}
