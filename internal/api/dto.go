package api

import (
	"encoding/json"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The wire shapes, matching docs/api/openapi.yaml. Hand-written rather than
// generated: generating from the spec is the right answer once contract tests
// exist to prove these agree, and until then a small hand-written struct is
// easier to read against the spec than a generated one.

type monitorJSON struct {
	ID                     string          `json:"id"`
	Name                   string          `json:"name"`
	Description            *string         `json:"description"`
	Type                   string          `json:"type"`
	Config                 json.RawMessage `json:"config"`
	Enabled                bool            `json:"enabled"`
	IntervalSeconds        int             `json:"interval_seconds"`
	TimeoutSeconds         int             `json:"timeout_seconds"`
	Retries                int             `json:"retries"`
	RetryIntervalSecond    *int            `json:"retry_interval_seconds"`
	ResendAfter            int             `json:"resend_after"`
	UpsideDown             bool            `json:"upside_down"`
	GroupID                *string         `json:"group_id"`
	ParentMonitorID        *string         `json:"parent_monitor_id"`
	NotificationChannelIDs []string        `json:"notification_channel_ids"`
	NotifyOnRecovery       bool            `json:"notify_on_recovery"`
	Status                 string          `json:"status"`
	LastCheckAt            *time.Time      `json:"last_check_at"`
	NextCheckAt            *time.Time      `json:"next_check_at"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`
}

// withPushToken folds the one-time push credentials into the config object the
// spec puts them in. They live under `config` because PushConfig declares them
// there as readOnly, and they are omitted everywhere else — a token that appears
// on every read is a token that ends up in a log.
func withPushToken(out monitorJSON, token, url string) monitorJSON {
	if token == "" {
		return out
	}
	var config map[string]any
	if len(out.Config) > 0 {
		_ = json.Unmarshal(out.Config, &config)
	}
	if config == nil {
		config = map[string]any{}
	}
	config["push_token"] = token
	config["push_url"] = url

	if encoded, err := json.Marshal(config); err == nil {
		out.Config = encoded
	}
	return out
}

// monitorWrite is the request body. Pointers everywhere a default exists, so the
// server can tell "unset, use the default" from "explicitly set to the zero
// value" — the difference between an unspecified interval and a nonsensical one.
type monitorWrite struct {
	Name                 *string         `json:"name"`
	Description          *string         `json:"description"`
	Type                 *string         `json:"type"`
	Config               json.RawMessage `json:"config"`
	Enabled              *bool           `json:"enabled"`
	IntervalSeconds      *int            `json:"interval_seconds"`
	TimeoutSeconds       *int            `json:"timeout_seconds"`
	Retries              *int            `json:"retries"`
	RetryIntervalSeconds *int            `json:"retry_interval_seconds"`
	ResendAfter          *int            `json:"resend_after"`
	UpsideDown           *bool           `json:"upside_down"`
	NotifyOnRecovery     *bool           `json:"notify_on_recovery"`

	ParentMonitorID *string `json:"parent_monitor_id"`

	// NotificationChannelIDs is a pointer to a slice so the three cases stay
	// distinguishable: absent means "attach the default channels", an empty
	// array means "no alerts for this monitor", and a populated one means
	// exactly those. Collapsing the first two would make it impossible to create
	// a deliberately silent monitor.
	NotificationChannelIDs *[]string `json:"notification_channel_ids"`
}

type heartbeatJSON struct {
	MonitorID         string    `json:"monitor_id"`
	Time              time.Time `json:"time"`
	Status            string    `json:"status"`
	ResponseTimeMs    *float64  `json:"response_time_ms"`
	Message           *string   `json:"message"`
	Code              *string   `json:"code"`
	Attempt           int       `json:"attempt"`
	Important         bool      `json:"important"`
	Suppressed        bool      `json:"suppressed"`
	SuppressionReason *string   `json:"suppression_reason"`
	ProbeID           *string   `json:"probe_id"`
}

type pagination struct {
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

type page[T any] struct {
	Data       []T        `json:"data"`
	Pagination pagination `json:"pagination"`
}

func toMonitorJSON(m store.MonitorWithState) monitorJSON {
	out := monitorJSON{
		ID:               m.Monitor.ID.String(),
		Name:             m.Monitor.Name,
		Type:             m.Monitor.Type,
		Config:           json.RawMessage(m.Monitor.Config),
		Enabled:          m.Monitor.Enabled,
		IntervalSeconds:  int(m.Monitor.Interval.Seconds()),
		TimeoutSeconds:   int(m.Monitor.Timeout.Seconds()),
		Retries:          m.Monitor.Retries,
		ResendAfter:      m.Monitor.ResendAfter,
		UpsideDown:       m.Monitor.UpsideDown,
		NotifyOnRecovery: m.Monitor.NotifyOnRecovery,
		Status:           m.State.Status,
		LastCheckAt:      m.State.LastCheckAt,
		NextCheckAt:      m.State.NextCheckAt,
		CreatedAt:        m.Monitor.CreatedAt,
		UpdatedAt:        m.Monitor.UpdatedAt,
	}
	if m.Monitor.Description != "" {
		out.Description = &m.Monitor.Description
	}
	if m.Monitor.RetryInterval > 0 {
		seconds := int(m.Monitor.RetryInterval.Seconds())
		out.RetryIntervalSecond = &seconds
	}
	if m.Monitor.GroupID != nil {
		id := m.Monitor.GroupID.String()
		out.GroupID = &id
	}
	if m.Monitor.ParentMonitorID != nil {
		id := m.Monitor.ParentMonitorID.String()
		out.ParentMonitorID = &id
	}
	out.NotificationChannelIDs = []string{}
	return out
}

// withChannels fills in the assignment list. Separate from toMonitorJSON because
// the list path resolves every monitor's channels in one query rather than one
// per row.
func withChannels(out monitorJSON, ids []model.ID) monitorJSON {
	out.NotificationChannelIDs = make([]string, 0, len(ids))
	for _, id := range ids {
		out.NotificationChannelIDs = append(out.NotificationChannelIDs, id.String())
	}
	return out
}

func toHeartbeatJSON(b model.Heartbeat) heartbeatJSON {
	out := heartbeatJSON{
		MonitorID:  b.MonitorID.String(),
		Time:       b.Time,
		Status:     b.Status.String(),
		Attempt:    b.Attempt,
		Important:  b.Important,
		Suppressed: b.Suppressed,
	}
	if b.ResponseTime != nil {
		ms := float64(b.ResponseTime.Microseconds()) / 1000.0
		out.ResponseTimeMs = &ms
	}
	if b.Message != "" {
		out.Message = &b.Message
	}
	if b.Code != "" {
		out.Code = &b.Code
	}
	if reason := model.SuppressionReasonName(b.SuppressionReason); reason != "" {
		out.SuppressionReason = &reason
	}
	if !b.ProbeID.IsZero() {
		id := b.ProbeID.String()
		out.ProbeID = &id
	}
	return out
}

// historyResponse is HistoryResponse in docs/api/openapi.yaml.
type historyResponse struct {
	MonitorID  string              `json:"monitor_id"`
	Resolution string              `json:"resolution"`
	From       time.Time           `json:"from"`
	To         time.Time           `json:"to"`
	Data       []historyBucketJSON `json:"data"`
}

// historyBucketJSON is HistoryBucket. Every response-time field is nullable
// because a bucket can hold checks that measured nothing.
type historyBucketJSON struct {
	BucketStart      time.Time `json:"bucket_start"`
	UpCount          int       `json:"up_count"`
	DownCount        int       `json:"down_count"`
	MaintenanceCount int       `json:"maintenance_count"`
	PendingCount     int       `json:"pending_count"`
	UptimeRatio      *float64  `json:"uptime_ratio"`
	ResponseTimeAvg  *float64  `json:"response_time_avg_ms"`
	ResponseTimeMin  *float64  `json:"response_time_min_ms"`
	ResponseTimeMax  *float64  `json:"response_time_max_ms"`
	ResponseTimeP95  *float64  `json:"response_time_p95_ms"`
}

func toHistoryBucketJSON(b store.HistoryBucket) historyBucketJSON {
	out := historyBucketJSON{
		BucketStart:      b.Start,
		UpCount:          b.Up,
		DownCount:        b.Down,
		MaintenanceCount: b.Maintenance,
		PendingCount:     b.Pending,
		ResponseTimeMin:  b.ResponseTimeMin,
		ResponseTimeMax:  b.ResponseTimeMax,
		ResponseTimeP95:  b.ResponseTimeP95,
	}

	// Null, not zero, when nothing was observed. A bucket whose checks were all
	// unknown or skipped is a gap, and a chart that draws a gap at 0% invents an
	// outage that never happened.
	if observed := b.Observed(); observed > 0 {
		ratio := float64(b.Up) / float64(observed)
		out.UptimeRatio = &ratio
	}
	if b.ResponseTimeCount > 0 {
		avg := b.ResponseTimeSum / float64(b.ResponseTimeCount)
		out.ResponseTimeAvg = &avg
	}
	return out
}

// uptimeSummary is UptimeSummary.
type uptimeSummary struct {
	// Reported back so an SLA figure carries its own method. A ratio quoted
	// without saying what it did with maintenance is not a defensible number.
	MaintenanceHandling string                      `json:"maintenance_handling"`
	Windows             map[string]uptimeWindowJSON `json:"windows"`
}

type uptimeWindowJSON struct {
	UptimeRatio        *float64 `json:"uptime_ratio"`
	TotalChecks        int      `json:"total_checks"`
	DownChecks         int      `json:"down_checks"`
	DowntimeSeconds    int64    `json:"downtime_seconds"`
	MaintenanceSeconds int64    `json:"maintenance_seconds"`
	ResponseTimeAvg    *float64 `json:"response_time_avg_ms"`
	ResponseTimeP95    *float64 `json:"response_time_p95_ms"`

	// IncidentCount is omitted rather than reported as zero: incidents are not
	// implemented in this build, and "no incidents" and "we do not track
	// incidents" are different claims. The schema does not require the field.
	IncidentCount *int `json:"incident_count,omitempty"`
}

// toUptimeWindowJSON applies the caller's maintenance policy.
//
// This is the reason uptime_ratio is computed at read time and never stored
// (data model §5.3): the same buckets produce three different defensible
// numbers, and storing one would make the other two unimplementable.
//
// unknown and skipped enter none of the three, in either the numerator or the
// denominator, in any mode.
func toUptimeWindowJSON(b store.HistoryBucket, handling string, interval time.Duration) uptimeWindowJSON {
	out := uptimeWindowJSON{
		TotalChecks:     b.Observed(),
		DownChecks:      b.Down,
		ResponseTimeP95: b.ResponseTimeP95,
	}

	numerator, denominator := b.Up, b.Observed()
	switch handling {
	case "count_as_up":
		numerator += b.Maintenance
		denominator += b.Maintenance
	case "count_as_down":
		denominator += b.Maintenance
	}
	if denominator > 0 {
		ratio := float64(numerator) / float64(denominator)
		out.UptimeRatio = &ratio
	}

	// A failing check stands for one interval of unavailability. Deriving it
	// from the check count rather than from the window means a monitor that was
	// only checked for half the window does not have the other half attributed
	// to it either way.
	seconds := int64(interval.Seconds())
	out.DowntimeSeconds = int64(b.Down) * seconds
	out.MaintenanceSeconds = int64(b.Maintenance) * seconds

	if b.ResponseTimeCount > 0 {
		avg := b.ResponseTimeSum / float64(b.ResponseTimeCount)
		out.ResponseTimeAvg = &avg
	}
	return out
}

// maintenanceWindowJSON is MaintenanceWindow in docs/api/openapi.yaml.
type maintenanceWindowJSON struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	Strategy    string  `json:"strategy"`

	// State is derived from the schedule and the clock on every read. Storing it
	// would make it wrong between the moment a window starts and the moment
	// something notices, which is exactly the interval anyone asks about.
	State string `json:"state"`

	Timezone              string          `json:"timezone"`
	StartsAt              time.Time       `json:"starts_at"`
	EndsAt                *time.Time      `json:"ends_at"`
	DurationMinutes       *int            `json:"duration_minutes"`
	Recurrence            *recurrenceJSON `json:"recurrence"`
	Targets               targetsJSON     `json:"targets"`
	SuppressNotifications bool            `json:"suppress_notifications"`
	ShowOnStatusPages     bool            `json:"show_on_status_pages"`
	StatusPageIDs         []string        `json:"status_page_ids"`
	NextOccurrenceAt      *time.Time      `json:"next_occurrence_at"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
}

type recurrenceJSON struct {
	Weekdays    []int      `json:"weekdays,omitempty"`
	DaysOfMonth []int      `json:"days_of_month,omitempty"`
	Cron        *string    `json:"cron,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
}

type targetsJSON struct {
	MonitorIDs []string `json:"monitor_ids"`
	GroupIDs   []string `json:"group_ids"`
	TagIDs     []string `json:"tag_ids"`
}

// maintenanceWindowWrite is the request body for both create and update. The
// spec makes them the same shape, so an update is a replacement rather than a
// patch — and a replacement is what a target set wants, because "these two
// monitors" has to be able to mean exactly two.
type maintenanceWindowWrite struct {
	Title                 *string         `json:"title"`
	Description           *string         `json:"description"`
	Strategy              *string         `json:"strategy"`
	Timezone              *string         `json:"timezone"`
	StartsAt              *time.Time      `json:"starts_at"`
	EndsAt                *time.Time      `json:"ends_at"`
	DurationMinutes       *int            `json:"duration_minutes"`
	Recurrence            *recurrenceJSON `json:"recurrence"`
	Targets               *targetsJSON    `json:"targets"`
	SuppressNotifications *bool           `json:"suppress_notifications"`
	ShowOnStatusPages     *bool           `json:"show_on_status_pages"`
	StatusPageIDs         *[]string       `json:"status_page_ids"`

	// Cancelled is not in the spec's write shape; a window is cancelled by
	// deleting it. Declared here only so a client echoing a read back is met
	// with a clear rejection rather than "unknown field".
	State *string `json:"state"`
}

func toMaintenanceWindowJSON(w model.MaintenanceWindow, state string, statusPageIDs []model.ID) maintenanceWindowJSON {
	out := maintenanceWindowJSON{
		ID:                    w.ID.String(),
		Title:                 w.Title,
		Strategy:              w.Strategy,
		State:                 state,
		Timezone:              w.Timezone,
		StartsAt:              w.StartsAt,
		EndsAt:                w.EndsAt,
		SuppressNotifications: w.SuppressNotifications,
		ShowOnStatusPages:     w.ShowOnStatusPages,
		NextOccurrenceAt:      w.NextOccurrenceAt,
		CreatedAt:             w.CreatedAt,
		UpdatedAt:             w.UpdatedAt,
		Targets: targetsJSON{
			MonitorIDs: idStrings(w.Targets.MonitorIDs),
			GroupIDs:   idStrings(w.Targets.GroupIDs),
			TagIDs:     idStrings(w.Targets.TagIDs),
		},
		StatusPageIDs: idStrings(statusPageIDs),
	}
	if w.Description != "" {
		out.Description = &w.Description
	}
	if w.Duration > 0 {
		minutes := int(w.Duration.Minutes())
		out.DurationMinutes = &minutes
	}

	// Omitted rather than returned as an empty object when the strategy has no
	// recurrence rule: a single window with `"recurrence": {}` invites the reader
	// to wonder what it recurs on.
	if r := w.Recurrence; len(r.Weekdays) > 0 || len(r.DaysOfMonth) > 0 || r.Cron != "" || r.Until != nil {
		rendered := recurrenceJSON{Weekdays: r.Weekdays, DaysOfMonth: r.DaysOfMonth, Until: r.Until}
		if r.Cron != "" {
			cron := r.Cron
			rendered.Cron = &cron
		}
		out.Recurrence = &rendered
	}
	return out
}

func idStrings(ids []model.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// channelJSON is NotificationChannel in docs/api/openapi.yaml.
//
// Config is a map rather than json.RawMessage because it is assembled, not
// echoed: the stored non-secret half plus a redaction marker for each secret
// that is set. A channel read has to say "a bot token is configured" without
// saying what it is.
type channelJSON struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Config       map[string]any `json:"config"`
	Enabled      bool           `json:"enabled"`
	IsDefault    bool           `json:"is_default"`
	Events       []string       `json:"events"`
	LastUsedAt   *time.Time     `json:"last_used_at"`
	LastError    *string        `json:"last_error"`
	MonitorCount int            `json:"monitor_count"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// channelWrite is both NotificationChannelWrite and NotificationChannelUpdate.
// Pointers everywhere, so a PATCH can tell "leave this alone" from "set this to
// false" — the difference between not touching a channel's enabled flag and
// silently disabling it.
type channelWrite struct {
	Name      *string        `json:"name"`
	Type      *string        `json:"type"`
	Config    map[string]any `json:"config"`
	Enabled   *bool          `json:"enabled"`
	IsDefault *bool          `json:"is_default"`
	Events    *[]string      `json:"events"`
}

func toChannelJSON(c store.ChannelWithCount, config map[string]any) channelJSON {
	out := channelJSON{
		ID:           c.Channel.ID.String(),
		Name:         c.Channel.Name,
		Type:         c.Channel.Type,
		Config:       config,
		Enabled:      c.Channel.Enabled,
		IsDefault:    c.Channel.IsDefault,
		Events:       c.Channel.Events,
		LastUsedAt:   c.Channel.LastUsedAt,
		MonitorCount: c.MonitorCount,
		CreatedAt:    c.Channel.CreatedAt,
		UpdatedAt:    c.Channel.UpdatedAt,
	}
	if out.Events == nil {
		out.Events = []string{}
	}
	if c.Channel.LastError != "" {
		out.LastError = &c.Channel.LastError
	}
	return out
}

// notificationTestResult is NotificationTestResult. The provider's error is
// passed through rather than summarised: "delivery failed" is not something an
// operator can act on, and "invalid_auth" is.
type notificationTestResult struct {
	Delivered       bool    `json:"delivered"`
	StatusCode      *int    `json:"status_code"`
	Error           *string `json:"error"`
	DurationMs      float64 `json:"duration_ms"`
	RenderedPayload *string `json:"rendered_payload"`
}

// templatePreviewRequest is TemplatePreviewRequest.
type templatePreviewRequest struct {
	Template  string            `json:"template"`
	Headers   map[string]string `json:"headers"`
	Event     string            `json:"event"`
	MonitorID *string           `json:"monitor_id"`
	Context   map[string]any    `json:"context"`
}

// templatePreviewResult is TemplatePreviewResult. A template that fails to
// render is a 200 with ok:false — a broken template is the user's typo, shown
// inline, not a server fault.
type templatePreviewResult struct {
	OK              bool              `json:"ok"`
	RenderedBody    *string           `json:"rendered_body"`
	RenderedHeaders map[string]string `json:"rendered_headers"`
	Error           *templateError    `json:"error"`
	ContextUsed     map[string]any    `json:"context_used"`
}

type templateError struct {
	Message string `json:"message"`
	Line    *int   `json:"line"`
	Column  *int   `json:"column"`
}

// templateVariableJSON is TemplateVariable. Published as an endpoint so the
// UI's autocomplete and the renderer cannot drift apart.
type templateVariableJSON struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Example     any    `json:"example"`
}
