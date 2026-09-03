package model

import "time"

// Instance settings.
//
// One row per organisation, seven JSON sections, each of which is optional on
// update — the shape migration 0002 created and the OpenAPI Settings schema
// documents. JSON rather than a column per field because these are settings: a
// new one every release would otherwise be a migration every release, and the
// read pattern is "load all of them once", never "query by one".
//
// Two of the sections are load-bearing rather than decorative. `smtp` is what
// makes an email channel's `use_instance_smtp` mean something — until it
// existed, that flag had nowhere to read from and the channel was refused at
// save time. `retention` is what stops a Raspberry Pi filling its disk, and it
// is read by the rollup runner on every pass rather than at startup, so a change
// takes effect on the next sweep instead of on the next restart.

// Settings is the whole instance configuration.
type Settings struct {
	OrgID      ID
	General    GeneralSettings
	Appearance AppearanceSettings
	Retention  RetentionSettings
	SMTP       SMTPSettings
	Monitoring MonitoringSettings
	Security   SecuritySettings
	Telemetry  TelemetrySettings
	UpdatedAt  time.Time
}

// GeneralSettings names the install and says where it lives.
type GeneralSettings struct {
	InstanceName string `json:"instance_name,omitempty"`

	// BaseURL is the public URL of this install. It is what a push URL, a status
	// page link, and an alert's "view monitor" link are built from — every one
	// of which is wrong in a way nobody notices until it is clicked from outside
	// the network.
	BaseURL  string `json:"base_url,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Locale   string `json:"locale,omitempty"`
}

// AppearanceSettings is dashboard chrome. Stored for the UI, which does not
// exist in this build; nothing server-side reads it.
type AppearanceSettings struct {
	Theme        string  `json:"theme,omitempty"`
	PrimaryColor *string `json:"primary_color,omitempty"`
}

// RetentionSettings is days per tier. Zero means keep indefinitely.
type RetentionSettings struct {
	RawDays             *int `json:"raw_days,omitempty"`
	Rollup1mDays        *int `json:"rollup_1m_days,omitempty"`
	Rollup5mDays        *int `json:"rollup_5m_days,omitempty"`
	Rollup1hDays        *int `json:"rollup_1h_days,omitempty"`
	Rollup1dDays        *int `json:"rollup_1d_days,omitempty"`
	WebhookDeliveryDays *int `json:"webhook_delivery_days,omitempty"`

	// ReportArtifactDays is how long a rendered report's bytes are kept, and it
	// is **deliberately not a tier**. ADR-008 item 6: an artifact is expected to
	// outlive the data it was computed from — that is the whole point of keeping
	// one — so the coarser-outlives-finer rule the tiers answer to does not
	// apply and must not be extended to it. Zero keeps artifacts indefinitely,
	// the same convention as every field above.
	//
	// Retention reclaims the bytes and keeps the row as a tombstone, so an
	// expired artifact answers 410 rather than 404.
	ReportArtifactDays *int `json:"report_artifact_days,omitempty"`
}

// DefaultReportArtifactDays is a year: long enough that last year's SLA report
// is still downloadable during this year's contract review, which is when
// somebody actually goes looking for one.
const DefaultReportArtifactDays = 365

// SMTPSettings is the instance-wide mail relay.
type SMTPSettings struct {
	Host        string `json:"host,omitempty"`
	Port        int    `json:"port,omitempty"`
	Username    string `json:"username,omitempty"`
	Encryption  string `json:"encryption,omitempty"`
	FromAddress string `json:"from_address,omitempty"`
	FromName    string `json:"from_name,omitempty"`

	// PasswordSealed is the AES-GCM envelope, bound by AAD to this row. It is
	// encrypted rather than hashed because SMTP replays it on every connection
	// (data model §12.1), and it lives inside the section's JSON rather than in
	// a column of its own so that adding it needed no migration.
	//
	// A []byte marshals to base64, so the column holds no plaintext at any
	// point.
	PasswordSealed []byte `json:"password_sealed,omitempty"`

	// Password is the plaintext, in memory only, on its way in from a request or
	// out to the mail sender. It is never marshalled: the `-` tag is what keeps
	// the read path from being one careless struct literal away from writing a
	// password into the settings column.
	Password string `json:"-"`
}

// Configured reports whether the instance relay is usable — which is what an
// email channel's use_instance_smtp needs to know before it is accepted.
func (s SMTPSettings) Configured() bool {
	return s.Host != "" && s.FromAddress != ""
}

// MonitoringSettings are the defaults a newly created monitor inherits.
type MonitoringSettings struct {
	DefaultIntervalSeconds        *int     `json:"default_interval_seconds,omitempty"`
	DefaultTimeoutSeconds         *int     `json:"default_timeout_seconds,omitempty"`
	DefaultRetries                *int     `json:"default_retries,omitempty"`
	DefaultNotificationChannelIDs []string `json:"default_notification_channel_ids,omitempty"`
	MaxConcurrentChecks           *int     `json:"max_concurrent_checks,omitempty"`
}

// SecuritySettings govern sessions and rate limits.
type SecuritySettings struct {
	SessionTimeoutMinutes   *int     `json:"session_timeout_minutes,omitempty"`
	LoginRateLimitPerMinute *int     `json:"login_rate_limit_per_minute,omitempty"`
	APIRateLimitPerMinute   *int     `json:"api_rate_limit_per_minute,omitempty"`
	RequireTOTP             *bool    `json:"require_totp,omitempty"`
	TrustedProxies          []string `json:"trusted_proxies,omitempty"`
}

// TelemetrySettings is opt-in and off by default. Nothing leaves the install
// unless somebody turns this on, which is principle 7 and not a preference.
type TelemetrySettings struct {
	Enabled    bool       `json:"enabled"`
	LastSentAt *time.Time `json:"last_sent_at,omitempty"`
}

// SMTP encryption modes.
const (
	SMTPNone     = "none"
	SMTPStartTLS = "starttls"
	SMTPTLS      = "tls"
)
