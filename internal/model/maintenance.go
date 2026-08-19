package model

import (
	"encoding/json"
	"time"
)

// Maintenance strategies, matching maintenance_windows.strategy and
// MaintenanceStrategy in the spec. cron is the escape hatch for schedules the
// named ones cannot express — "the first Monday of the quarter" is a real
// request and no enumeration of strategies will ever cover all of them.
const (
	StrategySingle           = "single"
	StrategyRecurringDaily   = "recurring_daily"
	StrategyRecurringWeekly  = "recurring_weekly"
	StrategyRecurringMonthly = "recurring_monthly"
	StrategyCron             = "cron"
)

// Maintenance states, derived at read time rather than stored. A stored state
// would be wrong between the moment a window starts and the moment something
// notices, which is exactly the interval a status page is asked about.
const (
	MaintenanceScheduled = "scheduled"
	MaintenanceActive    = "active"
	MaintenanceEnded     = "ended"
	MaintenanceCancelled = "cancelled"
)

// Maintenance target types.
const (
	TargetMonitor = "monitor"
	TargetGroup   = "group"
	TargetTag     = "tag"
)

// Suppression reasons. Stored as integers on heartbeats (data model §5.2) and as
// text on monitor_state, which is not an inconsistency: one is written 21 million
// times a day and the other 5,000 times.
const (
	SuppressionNone        = 0
	SuppressionMaintenance = 1
	SuppressionDependency  = 2

	SuppressedByMaintenance = "maintenance"
	SuppressedByDependency  = "dependency"
)

// SuppressionReasonName maps the stored integer to the API's string.
func SuppressionReasonName(reason int) string {
	switch reason {
	case SuppressionMaintenance:
		return SuppressedByMaintenance
	case SuppressionDependency:
		return SuppressedByDependency
	default:
		return ""
	}
}

// Recurrence is the JSON blob a window carries for the strategies that need one.
type Recurrence struct {
	// Weekdays for recurring_weekly. 0 is Sunday, matching the spec and
	// time.Weekday.
	Weekdays []int `json:"weekdays,omitempty"`

	// DaysOfMonth for recurring_monthly. A day past the end of a short month is
	// skipped rather than clamped: "the 31st" meaning the 28th of February is a
	// guess about intent, and a maintenance window is not a good place to guess.
	DaysOfMonth []int `json:"days_of_month,omitempty"`

	// Cron is a five-field expression for the cron strategy.
	Cron string `json:"cron,omitempty"`

	// Until stops the recurrence. Nil recurs indefinitely.
	Until *time.Time `json:"until,omitempty"`
}

// MaintenanceWindow is a planned interruption: when it happens, what it covers,
// and whether it silences alerts while it does.
type MaintenanceWindow struct {
	ID          ID
	OrgID       ID
	Title       string
	Description string
	Strategy    string

	// Timezone is an IANA zone name, never an offset. "02:00 every Sunday" has
	// to survive a daylight-saving transition still meaning 02:00 local, and an
	// offset cannot express that.
	Timezone string

	// StartsAt is the first occurrence, and for a recurring strategy the anchor
	// its time-of-day is taken from.
	StartsAt time.Time

	// EndsAt ends a single window. Recurring windows use Duration instead.
	EndsAt *time.Time

	// Duration is the length of each occurrence of a recurring window.
	Duration time.Duration

	Recurrence Recurrence

	SuppressNotifications bool
	ShowOnStatusPages     bool

	CancelledAt *time.Time

	// NextOccurrenceAt is materialised so the sweep finds due windows with an
	// index seek rather than evaluating every recurrence rule on every tick.
	// Recomputed on every write and whenever an occurrence ends.
	NextOccurrenceAt *time.Time

	Targets MaintenanceTargets

	CreatedAt time.Time
	UpdatedAt time.Time
}

// MaintenanceTargets is what a window covers. A monitor matched by any of the
// three is under maintenance.
//
// Resolution is a query at evaluation time and never a snapshot of monitor ids,
// which is the whole reason tag targeting exists: a window covering "everything
// tagged production" has to keep covering monitors created after it.
type MaintenanceTargets struct {
	MonitorIDs []ID
	GroupIDs   []ID
	TagIDs     []ID
}

// Empty reports whether the window covers nothing. A window with no targets
// suppresses nothing and is almost always a mistake in the making.
func (t MaintenanceTargets) Empty() bool {
	return len(t.MonitorIDs)+len(t.GroupIDs)+len(t.TagIDs) == 0
}

// EncodeRecurrence renders the JSON column.
func (w MaintenanceWindow) EncodeRecurrence() (string, error) {
	raw, err := json.Marshal(w.Recurrence)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
