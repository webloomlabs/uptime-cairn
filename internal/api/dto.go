package api

import (
	"encoding/hex"
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
	TagIDs                 []string        `json:"tag_ids"`
	NotificationChannelIDs []string        `json:"notification_channel_ids"`
	NotifyOnRecovery       bool            `json:"notify_on_recovery"`
	Status                 string          `json:"status"`
	LastCheckAt            *time.Time      `json:"last_check_at"`
	NextCheckAt            *time.Time      `json:"next_check_at"`
	CreatedAt              time.Time       `json:"created_at"`
	UpdatedAt              time.Time       `json:"updated_at"`

	// The include= embeds. Every one is omitempty, so a client that did not ask
	// for them sees a response identical to the one it saw before they existed —
	// which is what makes adding an embed a non-breaking change.
	//
	// They are opt-in because they cost per-row work: last_heartbeat and uptime
	// are the two the dashboard's list view wants and an export does not, and at
	// 5,000 monitors that difference is the difference between a page that loads
	// and one that times out.
	LastHeartbeat *heartbeatJSON   `json:"last_heartbeat,omitempty"`
	Uptime        *uptimeEmbed     `json:"uptime,omitempty"`
	Group         *groupJSON       `json:"group,omitempty"`
	Tags          []tagJSON        `json:"tags,omitempty"`
	Certificate   *certificateJSON `json:"certificate,omitempty"`
}

// uptimeEmbed is the two windows a list view renders under a monitor's name.
// Both are pointers: null means "not computed yet", which is a different claim
// from zero, and zero would render as total downtime for a monitor created
// minutes ago.
type uptimeEmbed struct {
	Ratio24h *float64 `json:"24h"`
	Ratio30d *float64 `json:"30d"`
}

// certificateJSON is CertificateInfo in docs/api/openapi.yaml.
type certificateJSON struct {
	Subject           string     `json:"subject,omitempty"`
	Issuer            string     `json:"issuer,omitempty"`
	SerialNumber      string     `json:"serial_number,omitempty"`
	ValidFrom         *time.Time `json:"valid_from,omitempty"`
	ValidTo           time.Time  `json:"valid_to"`
	DaysRemaining     int        `json:"days_remaining"`
	FingerprintSHA256 string     `json:"fingerprint_sha256,omitempty"`
	SANs              []string   `json:"subject_alternative_names,omitempty"`
	ChainValid        *bool      `json:"chain_valid,omitempty"`
	ChainError        *string    `json:"chain_error"`
	ObservedAt        time.Time  `json:"observed_at"`
}

func toCertificateJSON(c model.Certificate, now time.Time) certificateJSON {
	out := certificateJSON{
		Subject:       c.Subject,
		Issuer:        c.Issuer,
		SerialNumber:  c.SerialNumber,
		ValidFrom:     c.ValidFrom,
		ValidTo:       c.ValidTo,
		DaysRemaining: c.DaysRemaining(now),
		SANs:          c.SANs,
		ChainValid:    c.ChainValid,
		ObservedAt:    c.ObservedAt,
	}
	if len(c.FingerprintSHA256) > 0 {
		out.FingerprintSHA256 = hex.EncodeToString(c.FingerprintSHA256)
	}
	if c.ChainError != "" {
		out.ChainError = &c.ChainError
	}
	return out
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

	// Absent or null leaves the monitor ungrouped and untagged. There is no
	// PATCH on monitors yet, so the "unset an existing value" case these fields
	// will eventually need does not arise.
	GroupID *string   `json:"group_id"`
	TagIDs  *[]string `json:"tag_ids"`

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
	out.TagIDs = []string{}
	return out
}

// withTags fills in the tag list. Separate from toMonitorJSON because the list
// path resolves every monitor's tags in one query rather than one per row.
func withTags(out monitorJSON, ids []model.ID) monitorJSON {
	out.TagIDs = idStrings(ids)
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

// groupJSON is Group in docs/api/openapi.yaml.
type groupJSON struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Description   *string `json:"description"`
	ParentGroupID *string `json:"parent_group_id"`
	MonitorCount  int     `json:"monitor_count"`

	// Status is the worst among the group's monitors, its children's included.
	// Null when it holds none — which is a different statement from "up", and
	// rendering it green would be the dashboard inventing health.
	Status *string `json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type groupWrite struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`

	// Raw, not **string, because encoding/json collapses an explicit null into
	// an absent field for any pointer depth — and here the two mean different
	// things: absent leaves the parent alone, null takes the group out to the
	// top level. Nothing else in this file needs the distinction, so nothing
	// else pays for it.
	ParentGroupID json.RawMessage `json:"parent_group_id"`
}

func toGroupJSON(g model.GroupSummary) groupJSON {
	out := groupJSON{
		ID:           g.Group.ID.String(),
		Name:         g.Group.Name,
		MonitorCount: g.MonitorCount,
		CreatedAt:    g.Group.CreatedAt,
		UpdatedAt:    g.Group.UpdatedAt,
	}
	if g.Group.Description != "" {
		out.Description = &g.Group.Description
	}
	if g.Group.ParentGroupID != nil {
		id := g.Group.ParentGroupID.String()
		out.ParentGroupID = &id
	}
	if g.Status != "" {
		status := g.Status
		out.Status = &status
	}
	return out
}

// tagJSON is Tag. slug is readOnly and derived from the name.
type tagJSON struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	Color        string    `json:"color"`
	Description  *string   `json:"description"`
	MonitorCount int       `json:"monitor_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type tagWrite struct {
	Name        *string `json:"name"`
	Color       *string `json:"color"`
	Description *string `json:"description"`
}

func toTagJSON(t model.TagSummary) tagJSON {
	out := tagJSON{
		ID:           t.Tag.ID.String(),
		Name:         t.Tag.Name,
		Slug:         t.Tag.Slug,
		Color:        t.Tag.Color,
		MonitorCount: t.MonitorCount,
		CreatedAt:    t.Tag.CreatedAt,
		UpdatedAt:    t.Tag.UpdatedAt,
	}
	if t.Tag.Description != "" {
		out.Description = &t.Tag.Description
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

// monitorUpdate is MonitorUpdate: a partial edit where every field is optional
// and `type` is absent because it is immutable.
//
// json.RawMessage rather than *string for the three nullable identifiers,
// because encoding/json collapses an explicit null into the same nil a missing
// field produces at any pointer depth — and here the two mean different things.
// Absent leaves the association alone; null clears it.
type monitorUpdate struct {
	Name                 *string         `json:"name"`
	Description          *string         `json:"description"`
	Enabled              *bool           `json:"enabled"`
	IntervalSeconds      *int            `json:"interval_seconds"`
	TimeoutSeconds       *int            `json:"timeout_seconds"`
	Retries              *int            `json:"retries"`
	RetryIntervalSeconds json.RawMessage `json:"retry_interval_seconds"`
	ResendAfter          *int            `json:"resend_after"`
	UpsideDown           *bool           `json:"upside_down"`
	NotifyOnRecovery     *bool           `json:"notify_on_recovery"`

	GroupID         json.RawMessage `json:"group_id"`
	ParentMonitorID json.RawMessage `json:"parent_monitor_id"`

	TagIDs                 *[]string `json:"tag_ids"`
	NotificationChannelIDs *[]string `json:"notification_channel_ids"`

	// Config is merged shallowly against what is stored, so a caller can change
	// one setting without restating the rest — and, crucially, without having to
	// resend a password it can no longer read.
	Config json.RawMessage `json:"config"`
}

// monitorBulkRequest is MonitorBulkRequest.
type monitorBulkRequest struct {
	MonitorIDs             []string        `json:"monitor_ids"`
	Operation              string          `json:"operation"`
	TagIDs                 []string        `json:"tag_ids"`
	GroupID                json.RawMessage `json:"group_id"`
	NotificationChannelIDs []string        `json:"notification_channel_ids"`
}

// bulkResult reports each identifier's outcome separately. Partial success is
// the normal case for a thousand-monitor operation, and failing the batch
// because one id was deleted five minutes ago would be useless.
type bulkResult struct {
	Succeeded []string      `json:"succeeded"`
	Failed    []bulkFailure `json:"failed"`
}

type bulkFailure struct {
	ID      string `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// membershipSignal is MembershipSignal — ADR-004's reconciliation half.
type membershipSignal struct {
	Version     int64     `json:"version"`
	Count       int       `json:"count"`
	GeneratedAt time.Time `json:"generated_at"`
}

// incidentJSON is Incident.
type incidentJSON struct {
	ID             string               `json:"id"`
	Title          string               `json:"title"`
	State          string               `json:"state"`
	Impact         string               `json:"impact"`
	StartedAt      time.Time            `json:"started_at"`
	ResolvedAt     *time.Time           `json:"resolved_at"`
	MonitorIDs     []string             `json:"monitor_ids"`
	StatusPageIDs  []string             `json:"status_page_ids"`
	AutoOpened     bool                 `json:"auto_opened"`
	AcknowledgedAt *time.Time           `json:"acknowledged_at"`
	AcknowledgedBy *string              `json:"acknowledged_by"`
	AssignedTo     *string              `json:"assigned_to"`
	Updates        []incidentUpdateJSON `json:"updates,omitempty"`
	Metrics        *incidentMetricsJSON `json:"metrics"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

type incidentMetricsJSON struct {
	TimeToDetect      *int64 `json:"time_to_detect_seconds"`
	TimeToAcknowledge *int64 `json:"time_to_acknowledge_seconds"`
	TimeToResolve     *int64 `json:"time_to_resolve_seconds"`
}

type incidentUpdateJSON struct {
	ID                  string    `json:"id"`
	State               *string   `json:"state,omitempty"`
	Body                string    `json:"body"`
	AuthorID            *string   `json:"author_id"`
	NotifiedSubscribers bool      `json:"notified_subscribers"`
	CreatedAt           time.Time `json:"created_at"`
}

// incidentWrite is IncidentWrite: the opening call, which may carry the first
// timeline entry inline.
type incidentWrite struct {
	Title             *string    `json:"title"`
	State             *string    `json:"state"`
	Impact            *string    `json:"impact"`
	StartedAt         *time.Time `json:"started_at"`
	MonitorIDs        []string   `json:"monitor_ids"`
	StatusPageIDs     []string   `json:"status_page_ids"`
	Body              *string    `json:"body"`
	NotifySubscribers *bool      `json:"notify_subscribers"`
}

// incidentPatch is IncidentUpdateRequest: metadata only. State is absent by
// design — advancing an incident goes through the timeline, so that every state
// change carries the note explaining it.
type incidentPatch struct {
	Title         *string         `json:"title"`
	Impact        *string         `json:"impact"`
	StartedAt     *time.Time      `json:"started_at"`
	MonitorIDs    *[]string       `json:"monitor_ids"`
	StatusPageIDs *[]string       `json:"status_page_ids"`
	AssignedTo    json.RawMessage `json:"assigned_to"`
}

// timelineWrite is IncidentTimelineEntryWrite.
type timelineWrite struct {
	State             *string `json:"state"`
	Body              *string `json:"body"`
	NotifySubscribers *bool   `json:"notify_subscribers"`
}

func toIncidentJSON(in model.Incident, withTimeline bool) incidentJSON {
	metrics := in.Metrics()
	out := incidentJSON{
		ID:             in.ID.String(),
		Title:          in.Title,
		State:          in.State,
		Impact:         in.Impact,
		StartedAt:      in.StartedAt,
		ResolvedAt:     in.ResolvedAt,
		MonitorIDs:     idStrings(in.MonitorIDs),
		StatusPageIDs:  idStrings(in.StatusPageIDs),
		AutoOpened:     in.AutoOpened,
		AcknowledgedAt: in.AcknowledgedAt,
		AcknowledgedBy: idString(in.AcknowledgedBy),
		AssignedTo:     idString(in.AssignedTo),
		Metrics: &incidentMetricsJSON{
			TimeToDetect:      metrics.TimeToDetect,
			TimeToAcknowledge: metrics.TimeToAcknowledge,
			TimeToResolve:     metrics.TimeToResolve,
		},
		CreatedAt: in.CreatedAt,
		UpdatedAt: in.UpdatedAt,
	}
	if withTimeline {
		out.Updates = make([]incidentUpdateJSON, 0, len(in.Updates))
		for _, u := range in.Updates {
			out.Updates = append(out.Updates, toIncidentUpdateJSON(u))
		}
	}
	return out
}

func toIncidentUpdateJSON(u model.IncidentUpdate) incidentUpdateJSON {
	out := incidentUpdateJSON{
		ID:                  u.ID.String(),
		Body:                u.Body,
		AuthorID:            idString(u.AuthorID),
		NotifiedSubscribers: u.NotifiedSubscribers,
		CreatedAt:           u.CreatedAt,
	}
	if u.State != "" {
		state := u.State
		out.State = &state
	}
	return out
}

func idString(id *model.ID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// statusPageJSON is StatusPage. The password is absent: it is writeOnly in the
// spec and hashed in the column, so there is nothing here that could return it.
type statusPageJSON struct {
	ID                    string                  `json:"id"`
	Slug                  string                  `json:"slug"`
	Title                 string                  `json:"title"`
	Description           *string                 `json:"description"`
	Published             bool                    `json:"published"`
	CustomDomain          *string                 `json:"custom_domain"`
	Visibility            string                  `json:"visibility"`
	Theme                 string                  `json:"theme"`
	LogoURL               *string                 `json:"logo_url"`
	FaviconURL            *string                 `json:"favicon_url"`
	PrimaryColor          *string                 `json:"primary_color"`
	FooterText            *string                 `json:"footer_text"`
	CustomCSS             *string                 `json:"custom_css"`
	Timezone              *string                 `json:"timezone"`
	ShowUptimePercentage  bool                    `json:"show_uptime_percentage"`
	ShowResponseTimeChart bool                    `json:"show_response_time_chart"`
	UptimeBarDays         int                     `json:"uptime_bar_days"`
	ShowPoweredBy         bool                    `json:"show_powered_by"`
	SubscriptionsEnabled  bool                    `json:"subscriptions_enabled"`
	GoogleAnalyticsID     *string                 `json:"google_analytics_id"`
	Sections              []statusPageSectionJSON `json:"sections"`
	CreatedAt             time.Time               `json:"created_at"`
	UpdatedAt             time.Time               `json:"updated_at"`
}

type statusPageSectionJSON struct {
	Name        string   `json:"name"`
	Description *string  `json:"description"`
	MonitorIDs  []string `json:"monitor_ids"`
}

// statusPageWrite is StatusPageWrite. Sections are replaced wholesale, matching
// the store: the request's ordering is the ordering.
type statusPageWrite struct {
	Slug                  *string                  `json:"slug"`
	Title                 *string                  `json:"title"`
	Description           *string                  `json:"description"`
	Published             *bool                    `json:"published"`
	CustomDomain          json.RawMessage          `json:"custom_domain"`
	Visibility            *string                  `json:"visibility"`
	Password              json.RawMessage          `json:"password"`
	Theme                 *string                  `json:"theme"`
	LogoURL               *string                  `json:"logo_url"`
	FaviconURL            *string                  `json:"favicon_url"`
	PrimaryColor          *string                  `json:"primary_color"`
	FooterText            *string                  `json:"footer_text"`
	CustomCSS             *string                  `json:"custom_css"`
	Timezone              *string                  `json:"timezone"`
	ShowUptimePercentage  *bool                    `json:"show_uptime_percentage"`
	ShowResponseTimeChart *bool                    `json:"show_response_time_chart"`
	UptimeBarDays         *int                     `json:"uptime_bar_days"`
	ShowPoweredBy         *bool                    `json:"show_powered_by"`
	SubscriptionsEnabled  *bool                    `json:"subscriptions_enabled"`
	GoogleAnalyticsID     *string                  `json:"google_analytics_id"`
	Sections              *[]statusPageSectionJSON `json:"sections"`
}

func toStatusPageJSON(p model.StatusPage) statusPageJSON {
	out := statusPageJSON{
		ID:                    p.ID.String(),
		Slug:                  p.Slug,
		Title:                 p.Title,
		Description:           optional(p.Description),
		Published:             p.Published,
		CustomDomain:          optional(p.CustomDomain),
		Visibility:            p.Visibility,
		Theme:                 p.Theme,
		LogoURL:               optional(p.LogoURL),
		FaviconURL:            optional(p.FaviconURL),
		PrimaryColor:          optional(p.PrimaryColor),
		FooterText:            optional(p.FooterText),
		CustomCSS:             optional(p.CustomCSS),
		Timezone:              optional(p.Timezone),
		ShowUptimePercentage:  p.ShowUptimePercentage,
		ShowResponseTimeChart: p.ShowResponseTimeChart,
		UptimeBarDays:         p.UptimeBarDays,
		ShowPoweredBy:         p.ShowPoweredBy,
		SubscriptionsEnabled:  p.SubscriptionsEnabled,
		GoogleAnalyticsID:     optional(p.GoogleAnalyticsID),
		Sections:              make([]statusPageSectionJSON, 0, len(p.Sections)),
		CreatedAt:             p.CreatedAt,
		UpdatedAt:             p.UpdatedAt,
	}
	if out.Theme == "" {
		out.Theme = "auto"
	}
	for _, section := range p.Sections {
		out.Sections = append(out.Sections, statusPageSectionJSON{
			Name:        section.Name,
			Description: optional(section.Description),
			MonitorIDs:  idStrings(section.MonitorIDs),
		})
	}
	return out
}

// optional renders an empty string as JSON null. The spec types these fields as
// nullable, and "" and null are the same absence here — a status page with an
// empty-string footer is a status page with no footer.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// publicStatusPage is PublicStatusPage: the visitor-facing projection, built
// from PublicMonitor rather than from a monitor read, so nothing about
// configuration can reach it.
type publicStatusPage struct {
	Slug                 string                    `json:"slug"`
	Title                string                    `json:"title"`
	Description          *string                   `json:"description"`
	Theme                string                    `json:"theme"`
	LogoURL              *string                   `json:"logo_url"`
	FaviconURL           *string                   `json:"favicon_url"`
	PrimaryColor         *string                   `json:"primary_color"`
	FooterText           *string                   `json:"footer_text"`
	CustomCSS            *string                   `json:"custom_css"`
	ShowPoweredBy        bool                      `json:"show_powered_by"`
	SubscriptionsEnabled bool                      `json:"subscriptions_enabled"`
	OverallStatus        string                    `json:"overall_status"`
	Sections             []publicSection           `json:"sections"`
	ActiveIncidents      []publicIncident          `json:"active_incidents"`
	RecentIncidents      []publicIncident          `json:"recent_incidents"`
	ScheduledMaintenance []publicMaintenanceWindow `json:"scheduled_maintenance"`
	GeneratedAt          time.Time                 `json:"generated_at"`
}

type publicSection struct {
	Name        string                `json:"name"`
	Description *string               `json:"description"`
	Monitors    []publicMonitorRecord `json:"monitors"`
}

type publicMonitorRecord struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Description      *string          `json:"description"`
	Status           string           `json:"status"`
	UptimePercentage *float64         `json:"uptime_percentage"`
	UptimeBar        []publicBarEntry `json:"uptime_bar,omitempty"`
	ResponseTimeMs   *float64         `json:"response_time_ms"`
}

type publicBarEntry struct {
	Date string `json:"date"`

	// UptimeRatio is null for a day with no data. Rendering that as downtime is
	// the single most common way a status page lies.
	UptimeRatio *float64 `json:"uptime_ratio"`
	Status      *string  `json:"status"`
}

type publicIncident struct {
	ID                 string                 `json:"id"`
	Title              string                 `json:"title"`
	State              string                 `json:"state"`
	Impact             string                 `json:"impact"`
	StartedAt          time.Time              `json:"started_at"`
	ResolvedAt         *time.Time             `json:"resolved_at"`
	AffectedMonitorIDs []string               `json:"affected_monitor_ids"`
	Updates            []publicIncidentUpdate `json:"updates"`
}

type publicIncidentUpdate struct {
	State     *string   `json:"state,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type publicMaintenanceWindow struct {
	Title              string     `json:"title"`
	Description        *string    `json:"description"`
	StartsAt           time.Time  `json:"starts_at"`
	EndsAt             *time.Time `json:"ends_at"`
	AffectedMonitorIDs []string   `json:"affected_monitor_ids"`
}

// subscriberJSON is Subscriber. The target is masked even here: a page's
// subscriber list is an export of somebody else's customers.
type subscriberJSON struct {
	ID          string     `json:"id"`
	Channel     string     `json:"channel"`
	Target      string     `json:"target"`
	Confirmed   bool       `json:"confirmed"`
	ConfirmedAt *time.Time `json:"confirmed_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

type subscribeRequest struct {
	Channel *string `json:"channel"`
	Target  *string `json:"target"`
}

type pageAuthRequest struct {
	Password *string `json:"password"`
}

// webhookJSON is Webhook. Headers are absent rather than redacted: they are
// stored encrypted and the spec marks them write-only, so the read shape has
// nowhere to put them.
type webhookJSON struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	Events              []string   `json:"events"`
	Enabled             bool       `json:"enabled"`
	VerifyTLS           bool       `json:"verify_tls"`
	SecretPrefix        string     `json:"secret_prefix,omitempty"`
	LastDeliveryAt      *time.Time `json:"last_delivery_at"`
	LastDeliveryOutcome *string    `json:"last_delivery_outcome"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	DisabledAt          *time.Time `json:"disabled_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`

	// Secret appears exactly once, in the creation response. It is encrypted at
	// rest and never returned again, which is why this is the only opportunity
	// to copy it.
	Secret string `json:"secret,omitempty"`
}

type webhookWrite struct {
	Name      *string           `json:"name"`
	URL       *string           `json:"url"`
	Events    *[]string         `json:"events"`
	Enabled   *bool             `json:"enabled"`
	Headers   map[string]string `json:"headers"`
	VerifyTLS *bool             `json:"verify_tls"`
}

func toWebhookJSON(h model.Webhook) webhookJSON {
	out := webhookJSON{
		ID:                  h.ID.String(),
		Name:                h.Name,
		URL:                 h.URL,
		Events:              h.Events,
		Enabled:             h.Enabled,
		VerifyTLS:           h.VerifyTLS,
		SecretPrefix:        h.SecretPrefix,
		LastDeliveryAt:      h.LastDeliveryAt,
		ConsecutiveFailures: h.ConsecutiveFailures,
		DisabledAt:          h.DisabledAt,
		CreatedAt:           h.CreatedAt,
		UpdatedAt:           h.UpdatedAt,
	}
	if out.Events == nil {
		out.Events = []string{}
	}
	if h.LastDeliveryOutcome != "" {
		outcome := h.LastDeliveryOutcome
		out.LastDeliveryOutcome = &outcome
	}
	return out
}

// webhookDeliveryJSON is WebhookDelivery.
type webhookDeliveryJSON struct {
	ID             string     `json:"id"`
	Event          string     `json:"event"`
	Outcome        string     `json:"outcome"`
	Attempt        int        `json:"attempt"`
	RequestBody    *string    `json:"request_body"`
	ResponseStatus *int       `json:"response_status"`
	ResponseBody   *string    `json:"response_body"`
	Error          *string    `json:"error"`
	DurationMs     *float64   `json:"duration_ms"`
	CreatedAt      time.Time  `json:"created_at"`
	NextRetryAt    *time.Time `json:"next_retry_at"`
}

func toWebhookDeliveryJSON(d model.WebhookDelivery) webhookDeliveryJSON {
	return webhookDeliveryJSON{
		ID:             d.ID.String(),
		Event:          d.EventType,
		Outcome:        d.Outcome,
		Attempt:        d.Attempt,
		RequestBody:    optional(d.RequestBody),
		ResponseStatus: d.ResponseStatus,
		ResponseBody:   optional(d.ResponseBody),
		Error:          optional(d.Error),
		DurationMs:     d.DurationMs,
		CreatedAt:      d.CreatedAt,
		NextRetryAt:    d.NextRetryAt,
	}
}

// settingsJSON is Settings. The SMTP password is absent from the read shape
// entirely rather than redacted, because it is writeOnly in the spec and the
// struct that carries it out of the database tags it json:"-".
type settingsJSON struct {
	General    model.GeneralSettings    `json:"general"`
	Appearance model.AppearanceSettings `json:"appearance"`
	Retention  model.RetentionSettings  `json:"retention"`
	SMTP       smtpSettingsJSON         `json:"smtp"`
	Monitoring model.MonitoringSettings `json:"monitoring"`
	Security   model.SecuritySettings   `json:"security"`
	Telemetry  model.TelemetrySettings  `json:"telemetry"`
}

// smtpSettingsJSON is the SMTP section as it is read. password_configured is not
// in the spec's schema and is not sent; the field exists so the marshalled shape
// is explicit about carrying no secret.
type smtpSettingsJSON struct {
	Host        *string `json:"host"`
	Port        *int    `json:"port"`
	Username    *string `json:"username"`
	Encryption  string  `json:"encryption"`
	FromAddress *string `json:"from_address"`
	FromName    *string `json:"from_name"`
}

// settingsWrite is the PATCH body: every section optional, and every section's
// fields optional within it.
type settingsWrite struct {
	General    *model.GeneralSettings    `json:"general"`
	Appearance *model.AppearanceSettings `json:"appearance"`
	Retention  *model.RetentionSettings  `json:"retention"`
	SMTP       *smtpWrite                `json:"smtp"`
	Monitoring *model.MonitoringSettings `json:"monitoring"`
	Security   *model.SecuritySettings   `json:"security"`
	Telemetry  *telemetryWrite           `json:"telemetry"`
}

type smtpWrite struct {
	Host        *string         `json:"host"`
	Port        *int            `json:"port"`
	Username    *string         `json:"username"`
	Password    json.RawMessage `json:"password"`
	Encryption  *string         `json:"encryption"`
	FromAddress *string         `json:"from_address"`
	FromName    *string         `json:"from_name"`
}

// telemetryWrite excludes last_sent_at, which is readOnly: the exporter stamps
// it, and a client that could set it could claim to have reported when it had
// not.
type telemetryWrite struct {
	Enabled *bool `json:"enabled"`
}

func toSettingsJSON(set model.Settings) settingsJSON {
	out := settingsJSON{
		General:    set.General,
		Appearance: set.Appearance,
		Retention:  set.Retention,
		Monitoring: set.Monitoring,
		Security:   set.Security,
		Telemetry:  set.Telemetry,
		SMTP: smtpSettingsJSON{
			Host:        optional(set.SMTP.Host),
			Username:    optional(set.SMTP.Username),
			Encryption:  set.SMTP.Encryption,
			FromAddress: optional(set.SMTP.FromAddress),
			FromName:    optional(set.SMTP.FromName),
		},
	}
	if set.SMTP.Port > 0 {
		port := set.SMTP.Port
		out.SMTP.Port = &port
	}
	return out
}

// currentUserUpdate is CurrentUserUpdate. current_password is required for a
// change of email or password, and it is verified rather than merely present.
type currentUserUpdate struct {
	Email           *string `json:"email"`
	Name            *string `json:"name"`
	Timezone        *string `json:"timezone"`
	Locale          *string `json:"locale"`
	CurrentPassword *string `json:"current_password"`
	NewPassword     *string `json:"new_password"`
}

// systemInfoJSON is SystemInfo — the progressive-disclosure feed. A dashboard
// hides surfaces the instance does not have rather than showing dead controls,
// and this is where it learns which those are.
type systemInfoJSON struct {
	Version                  string          `json:"version"`
	Commit                   string          `json:"commit,omitempty"`
	BuiltAt                  *time.Time      `json:"built_at,omitempty"`
	Mode                     string          `json:"mode"`
	StorageEngine            string          `json:"storage_engine"`
	APIVersion               string          `json:"api_version"`
	MonitorTypes             []string        `json:"monitor_types"`
	NotificationChannelTypes []string        `json:"notification_channel_types"`
	Capabilities             map[string]bool `json:"capabilities"`
}

// overviewJSON is Overview.
type overviewJSON struct {
	Monitors                 overviewCounts `json:"monitors"`
	ActiveIncidents          int            `json:"active_incidents"`
	ActiveMaintenanceWindows int            `json:"active_maintenance_windows"`
	CertificatesExpiringSoon int            `json:"certificates_expiring_soon"`
	DomainsExpiringSoon      int            `json:"domains_expiring_soon"`
	GeneratedAt              time.Time      `json:"generated_at"`
}

type overviewCounts struct {
	Total       int `json:"total"`
	Up          int `json:"up"`
	Down        int `json:"down"`
	Pending     int `json:"pending"`
	Paused      int `json:"paused"`
	Maintenance int `json:"maintenance"`
}
