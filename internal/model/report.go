package model

import (
	"encoding/json"
	"time"
)

// The reporting domain (Phase 2), matching migration 0008.
//
// Three things kept separate — a template, a schedule, and a run — because that
// separation is what makes "re-send last month's", "regenerate it after we
// corrected the incident record" and "the PDF failed but the HTML went out" all
// expressible. Collapsing any pair of them makes one of those three impossible
// to say, and each is a request an agency actually makes.
//
// Everything here is storage-shaped rather than wire-shaped: the API's DTOs are
// hand-written against the frozen spec in internal/api/dto.go, and these types
// carry no tags, in keeping with the note at the top of this package.

// Report template types.
const (
	ReportTypeUptime      = "uptime"
	ReportTypeSLA         = "sla"
	ReportTypePostMortem  = "post_mortem"
	ReportTypeComparative = "comparative"
	ReportTypeCustom      = "custom"
)

// Report periods and how their boundaries are cut. Calendar means the month a
// human means by "March"; rolling means the last thirty days.
const (
	ReportPeriodDay     = "day"
	ReportPeriodWeek    = "week"
	ReportPeriodMonth   = "month"
	ReportPeriodQuarter = "quarter"
	ReportPeriodYear    = "year"
	ReportPeriodCustom  = "custom"

	ReportStyleCalendar = "calendar"
	ReportStyleRolling  = "rolling"
)

// Output formats. Four, and the set is closed by a CHECK constraint as well as
// by this list — 'docx' is refused by the database, which is where a typo in a
// request body would otherwise become a row.
const (
	FormatPDF  = "pdf"
	FormatHTML = "html"
	FormatCSV  = "csv"
	FormatJSON = "json"
)

// Schedule frequencies. cron is the escape hatch, the same one maintenance
// windows carry and for the same reason: no enumeration covers "the first
// Monday of the quarter".
const (
	ReportFrequencyDaily     = "daily"
	ReportFrequencyWeekly    = "weekly"
	ReportFrequencyMonthly   = "monthly"
	ReportFrequencyQuarterly = "quarterly"
	ReportFrequencyCron      = "cron"
)

// Run states.
//
// partial is a real state and not a rounding of the other two. One format
// produced and another not is the common failure — a PDF that could not render
// while the HTML and the CSV went out — and collapsing it into succeeded is how
// somebody concludes a delivery went out whole when it did not.
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunPartial   = "partial"
	RunFailed    = "failed"
)

// Artifact states. expired is a tombstone: retention reclaims the bytes and
// keeps the row, so a bookmarked link answers "this existed and is gone" with a
// 410 rather than "no such thing" with a 404.
const (
	ArtifactRendered = "rendered"
	ArtifactExpired  = "expired"
	ArtifactFailed   = "failed"
)

// Delivery target types and attempt outcomes.
const (
	ReportDeliveryEmail   = "email"
	ReportDeliverySlack   = "slack"
	ReportDeliveryWebhook = "webhook"
	ReportDeliveryS3      = "s3"

	// DeliverySkipped is not a failure and must not read as one: no relay
	// configured, or nothing rendered in a format this target takes. It is
	// recorded for the same reason a suppressed notification is — silence with
	// no row behind it is indistinguishable from a system that is not running.
	//
	// The succeeded and failed outcomes are the constants notification deliveries
	// already declare, reused rather than restated: one string with two names is
	// how a comparison ends up against the wrong one. The third differs on
	// purpose — a notification is *suppressed* by a rule somebody configured,
	// while a report delivery is *skipped* because there was nothing to send it.
	DeliverySkipped = "skipped"
)

// Logo content types a brand profile accepts.
//
// PNG and JPEG only, refused at upload with the reason rather than dropped at
// render time. ADR-007 is explicit that SVG is the expected case rather than the
// exotic one — status pages take an arbitrary LogoURL and the project's own mark
// is an SVG — so the refusal has to be legible to somebody holding one.
const (
	LogoPNG  = "image/png"
	LogoJPEG = "image/jpeg"
)

// BrandProfile is the white-label identity a report is rendered under.
//
// The logo lives in the database rather than beside the artifacts on disk, which
// is a deliberate departure from ADR-008. That ADR sends artifacts to the
// filesystem on three specifics — every VACUUM INTO backup growing in
// proportion, fifty writes contending with heartbeat ingest during the monthly
// burst, and no incremental blob access for a hundred-megabyte CSV — and a logo
// shares none of them. It is written once when somebody sets up a client and is
// bounded below a megabyte, so keeping it here means branding survives the
// documented backup procedure instead of needing a second directory beside it.
type BrandProfile struct {
	ID    ID
	OrgID ID

	Name        string
	CompanyName string

	// Six-digit hex including the leading '#', stored as written so a colour
	// round-trips exactly rather than through a normalisation nobody asked for.
	PrimaryColor string
	AccentColor  string

	// Plain text, both of them. That is a rendering constraint rather than a
	// storage one: the PDF writer has no rich-text pipeline, and a field that
	// renders in HTML and not in PDF is worse than one that renders nowhere.
	FooterText string
	CoverText  string

	HidePoweredBy bool

	Logo            []byte
	LogoContentType string
	LogoBytes       int64
	LogoUpdatedAt   *time.Time

	// IsDefault marks the one profile a template with no explicit choice falls
	// back to. At most one per organisation, enforced by a unique partial index
	// rather than by whichever handler last touched it.
	IsDefault bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReportScope is selection by rule, resolved at run time and never flattened to
// a list of ids at save time.
//
// An agency that adds a monitor to a client's tag expects it in that client's
// next report without editing the report, and a saved list cannot do that. The
// three collections are a union, so a monitor selected three ways appears once.
type ReportScope struct {
	MonitorIDs []ID
	GroupIDs   []ID
	TagIDs     []ID

	// IncidentID narrows a post-mortem to one declared incident.
	IncidentID *ID
}

// IsEmpty reports whether nothing was selected. An empty scope is not an error:
// a client whose monitors were all deleted still gets a report saying so, which
// beats a failed run nobody looks at until the invoice goes out.
func (s ReportScope) IsEmpty() bool {
	return len(s.MonitorIDs) == 0 && len(s.GroupIDs) == 0 && len(s.TagIDs) == 0
}

// ReportComparison configures a comparative report. Region-against-region is
// absent rather than stubbed: the shape accepts it and the data does not exist
// until multi-region probes ship.
type ReportComparison struct {
	Mode       string
	MonitorIDs []ID
	GroupIDs   []ID
}

// ReportTemplate is a saved definition: what to report on, over what window,
// under whose branding, in which formats.
type ReportTemplate struct {
	ID    ID
	OrgID ID

	Name        string
	Description string
	Type        string

	Scope ReportScope

	Period      string
	PeriodStyle string

	// SLATarget nil means "use the monitors' own targets", which is not the same
	// as no target: resolution is this field, then the monitor, then its group,
	// then none — and the report states which of the three answered, because a
	// monitor silently inheriting a group's number is otherwise invisible to
	// whoever reads it.
	//
	// Exactly 100 is refused, at the API and again by a CHECK: its error budget
	// is zero seconds, which makes burn rate undefined and every report a breach
	// report.
	SLATarget *float64

	// ResponseTimeTargetMS is the threshold behind days_over_target, and is
	// deliberately not a monitor's response_time_threshold_ms. That one marks a
	// check down when breached; this one classifies days after the fact and
	// changes no monitor's status.
	ResponseTimeTargetMS *int

	// MaintenanceHandling is stated on the report face whichever way it is set.
	// The same window yields three different lawful percentages, so a figure
	// without its policy cannot be checked by the person it is handed to.
	MaintenanceHandling string

	Comparison *ReportComparison

	// BrandProfileID nil uses the default profile, which derives from
	// settings.appearance — so an install that never opens the branding screen
	// still produces a report that does not look unbranded.
	BrandProfileID *ID

	// Sections are ordered content blocks; empty means the defaults for the type.
	Sections []string

	// Formats is at least one of pdf|html|csv|json, enforced at the API where the
	// error can name the field.
	Formats []string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReportSchedule is when a template fires.
type ReportSchedule struct {
	ID               ID
	OrgID            ID
	ReportTemplateID ID

	Name    string
	Enabled bool

	Frequency string

	// Cron is a five-field expression, required when Frequency is cron and
	// otherwise empty. Parsed by the one shared parser rather than a second copy
	// that would agree with the first only on the day it was written.
	Cron string

	// Timezone is an IANA zone, defaulted from general.timezone at write time
	// rather than resolved at run time — so changing the instance zone does not
	// silently move the boundaries of a report somebody has been receiving for a
	// year. A monthly report cut at midnight UTC for an Australian agency is
	// wrong by a working day.
	Timezone string

	// SendAt is "HH:MM" in that zone.
	SendAt string

	LastRunAt *time.Time

	// NextRunAt is computed on write and after every run. Stored rather than
	// derived because the scheduler seeks on it, and because "when does this next
	// fire" is a question the UI asks for every schedule on the page.
	NextRunAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReportScheduleDelivery is one configured target on a schedule.
//
// A row rather than a JSON array on the schedule, mirroring the split between
// notification_channels and notification_deliveries: one target's failure is
// then a fact about that target rather than an edit to the schedule.
type ReportScheduleDelivery struct {
	ID               ID
	OrgID            ID
	ReportScheduleID ID

	Type string

	// Config is non-secret configuration only — recipients, url, bucket, prefix,
	// region, endpoint, path_style. Deliberately outside the sealed blob so that
	// a read path serialising this can never serialise a credential.
	//
	// Raw JSON, the same shape NotificationChannel.Config takes and for the same
	// reason: the set of keys differs per target type, and a map[string]any would
	// round-trip a number as a float and a large integer as scientific notation.
	Config json.RawMessage

	// SecretsSealed is the S3 secret access key, AES-256-GCM, AAD-bound to
	// (org_id, table, column, id). Encrypted rather than hashed because SigV4
	// replays it on every request.
	SecretsSealed []byte

	// NotificationChannelID delivers through an existing channel rather than
	// restating its configuration, so a rotated Slack token is rotated once.
	NotificationChannelID *ID

	// Formats is which formats this target receives; empty means the run's
	// formats. An auditor gets the PDF and a BI pipeline gets the CSV, from one
	// schedule.
	Formats []string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReportRun is one generation: a template, a window, and what came of it.
type ReportRun struct {
	ID               ID
	OrgID            ID
	ReportTemplateID ID

	// ReportScheduleID is nil for an ad-hoc "run now", and is cleared rather than
	// cascaded when a schedule is deleted: the artifact is a record of what a
	// client was sent, and it outlives the arrangement that sent it.
	ReportScheduleID *ID

	State string

	PeriodStart time.Time
	PeriodEnd   time.Time

	// Timezone is the zone the boundaries were actually cut in, recorded rather
	// than assumed — it is the difference between a month and a month minus a
	// working day, and there is no way to recover it afterwards.
	Timezone string

	// Late means the run started materially after it was due, because the
	// instance was down when it should have fired. A missed schedule is late, not
	// lost, and the UI has to be able to say which.
	Late bool

	// Error is why the run is partial or failed. Per-artifact detail lives on the
	// artifact; this is the sentence shown against the run.
	Error string

	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
}

// ReportArtifact is one rendered file.
type ReportArtifact struct {
	ID          ID
	OrgID       ID
	ReportRunID ID

	Format string
	State  string

	// Path is relative to <data-dir>/reports, e.g. '2026/09/0192f0e3….pdf'.
	// Written from the artifact id and the format and never from the template
	// title, so a definition called '../../etc' has nowhere to go. Empty while
	// the state is failed, because nothing was written.
	Path string

	SizeBytes int64

	// SHA256 is hex of the bytes as written. The point is not corruption
	// detection for its own sake: it is what lets somebody assert that the file
	// restored from a backup is the file that was sent to the client.
	SHA256 string

	Error string

	// ExpiresAt nil means kept indefinitely, which is what report_artifact_days
	// of 0 selects. Otherwise the instant the sweeper may reclaim the bytes.
	ExpiresAt *time.Time

	CreatedAt time.Time
}

// ReportShareLink is an unauthenticated URL onto one run.
type ReportShareLink struct {
	ID          ID
	OrgID       ID
	ReportRunID ID

	// TokenHash is what a lookup compares against; TokenSealed is what lets the
	// UI show the link again to somebody who has already created it. Both, for
	// the reason the data model gives: the hash is the credential check and the
	// sealed copy is the convenience, and conflating them would either make the
	// link unshowable or make the store a place where a live credential is
	// readable in the clear.
	TokenHash   []byte
	TokenSealed []byte

	// ExpiresAt nil never expires. RevokedAt is a column rather than a delete, so
	// that a revoked link answers "this was withdrawn" instead of looking like a
	// typo.
	ExpiresAt *time.Time
	RevokedAt *time.Time

	// LastAccessedAt answers "has the client opened it yet", which is the first
	// thing anybody asks after sending one.
	LastAccessedAt *time.Time

	CreatedAt time.Time
}

// ReportDelivery is one attempt against one target.
type ReportDelivery struct {
	ID          ID
	OrgID       ID
	ReportRunID ID

	// ReportScheduleDeliveryID is cleared rather than cascaded, because the
	// delivery log outlives the schedule: "we sent it to them in March" has to
	// survive somebody removing the recipient in April.
	ReportScheduleDeliveryID *ID

	Type    string
	Outcome string
	Error   string
	Attempt int

	// Target is the address, channel or bucket this attempt went to, recorded
	// rather than joined — the configuration can change and the log is a
	// statement about what happened.
	Target string

	DeliveredAt *time.Time
	CreatedAt   time.Time
}

// UpcomingExpiry is one certificate or domain registration approaching its end,
// as the expiry calendar reports it.
//
// A view rather than a table. The rows live in `monitor_certificates` and
// `monitor_domain_expiry`, which are shaped differently on purpose — a
// registration has a registrar and a source, and no subject, chain or serial —
// and this is the one shape they have in common: something expires, on a date,
// belonging to a monitor.
//
// The two are unified here rather than at the schema, because the calendar is
// the only reader that wants them together. Every other consumer wants one or
// the other and would have to filter a merged table on every read.
type UpcomingExpiry struct {
	// Kind is ExpiryCertificate or ExpiryDomain.
	Kind string

	MonitorID   ID
	MonitorName string

	// Subject is the certificate's subject, or the registered domain. Issuer is
	// the certificate's issuer, or the registrar — the spec's own wording, and
	// the reason the two columns are shared rather than split: a calendar row
	// reads "expires on this date, issued by this party", whichever kind it is.
	Subject string
	Issuer  string

	ExpiresAt time.Time

	// DaysRemaining is computed against the instant of the request rather than
	// stored, and it is signed: an expiry that has already passed reports a
	// negative number rather than zero. "Expired eleven days ago" is the row
	// somebody most needs to see, and flooring it at zero would file it beside
	// "expires today".
	DaysRemaining int

	// ObservedAt is when the probe last confirmed this. A calendar built from a
	// stale observation is a calendar that can be confidently wrong, so the date
	// travels with every row.
	ObservedAt time.Time
}

// The two kinds of expiry the calendar reports.
const (
	ExpiryCertificate = "certificate"
	ExpiryDomain      = "domain"
)
