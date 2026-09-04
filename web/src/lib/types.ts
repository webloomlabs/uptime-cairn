/**
 * The wire shapes, mirroring internal/api/dto.go and docs/api/openapi.yaml.
 *
 * Hand-written for the same reason the Go side is: the spec is frozen, so these
 * change only when it does, and a small hand-written type is easier to read
 * against the spec than a generated one. Nothing here is invented — a field that
 * does not appear in the spec does not appear here.
 */

export type Page<T> = {
	data: T[];
	pagination: { next_cursor: string | null; has_more: boolean };
};

export type MonitorStatus = 'up' | 'down' | 'pending' | 'paused' | 'maintenance';

/**
 * A single check's outcome, which has two values a monitor's status never takes.
 *
 * `unknown` means the probe could not perform the check at all — a resolver it
 * cannot reach, a capability it does not have — and `skipped` means the check
 * never started. Both say something about the probe rather than about the
 * target, so neither moves the monitor's verdict or its uptime figure. A client
 * that types these as MonitorStatus renders them as "unknown" by accident and
 * never says why nothing is happening.
 */
export type CheckStatus = MonitorStatus | 'unknown' | 'skipped';

export type Heartbeat = {
	monitor_id: string;
	time: string;
	status: CheckStatus;
	response_time_ms: number | null;
	message: string | null;
	code: string | null;
	attempt: number;
	important: boolean;
	suppressed: boolean;
	suppression_reason: string | null;
	probe_id: string | null;
};

export type CertificateInfo = {
	subject?: string;
	issuer?: string;
	serial_number?: string;
	valid_from?: string;
	valid_to: string;
	days_remaining: number;
	fingerprint_sha256?: string;
	subject_alternative_names?: string[];
	chain_valid?: boolean;
	chain_error: string | null;
	observed_at: string;
};

export type Monitor = {
	id: string;
	name: string;
	description: string | null;
	type: string;
	config: Record<string, unknown>;
	enabled: boolean;
	interval_seconds: number;
	timeout_seconds: number;
	retries: number;
	retry_interval_seconds: number | null;
	resend_after: number;
	upside_down: boolean;
	group_id: string | null;
	parent_monitor_id: string | null;
	/**
	 * The probe this monitor is pinned to, or null for "run it anywhere".
	 *
	 * Only `docker` needs one: every other check answers the same question from
	 * anywhere with the right egress, while "is this container running" is a
	 * question about one host's daemon. The server fills it in when the install
	 * has exactly one probe, which is every solo install.
	 */
	probe_id: string | null;
	tag_ids: string[];
	notification_channel_ids: string[];
	notify_on_recovery: boolean;
	status: MonitorStatus;
	last_check_at: string | null;
	next_check_at: string | null;
	created_at: string;
	updated_at: string;

	// include= embeds, absent unless asked for.
	last_heartbeat?: Heartbeat;
	/** A bounded run of recent checks, newest first. `include=heartbeats`. */
	heartbeats?: Heartbeat[];
	uptime?: { '24h': number | null; '30d': number | null };
	group?: Group;
	tags?: Tag[];
	certificate?: CertificateInfo;
};

/** The membership signal ADR-004 reconciles filtered views against. */
export type MembershipSignal = {
	// An int64 on the wire, so a number here. Compared for inequality and never
	// ordered — it is an opaque version, not a timestamp to do arithmetic on.
	version: number;
	count: number;
	generated_at: string;
};

/**
 * An incident.
 *
 * State is not a field a client may PATCH. Advancing an incident goes through
 * the timeline, so that every state change carries the sentence explaining it —
 * an incident that moved from investigating to identified with nobody saying
 * what was identified is the thing a post-mortem cannot reconstruct.
 */
export type Incident = {
	id: string;
	title: string;
	state: IncidentState;
	impact: IncidentImpact;
	started_at: string;
	resolved_at: string | null;
	monitor_ids: string[];
	status_page_ids: string[];
	auto_opened: boolean;
	acknowledged_at: string | null;
	acknowledged_by: string | null;
	assigned_to: string | null;
	updates?: IncidentUpdate[];
	metrics: {
		time_to_detect_seconds: number | null;
		time_to_acknowledge_seconds: number | null;
		time_to_resolve_seconds: number | null;
	} | null;
	created_at: string;
	updated_at: string;
};

export type IncidentState = 'investigating' | 'identified' | 'monitoring' | 'resolved';
export type IncidentImpact = 'none' | 'minor' | 'major' | 'critical';

export const INCIDENT_STATES: IncidentState[] = [
	'investigating',
	'identified',
	'monitoring',
	'resolved'
];
export const INCIDENT_IMPACTS: IncidentImpact[] = ['none', 'minor', 'major', 'critical'];

/** One entry on an incident's timeline. */
export type IncidentUpdate = {
	id: string;
	state?: IncidentState;
	body: string;
	author_id: string | null;
	notified_subscribers: boolean;
	created_at: string;
};

/** Instance settings, as read. Secrets are never in this shape. */
export type Settings = {
	general: {
		instance_name?: string;
		base_url?: string;
		timezone?: string;
		locale?: string;
	};
	appearance: { theme?: string; primary_color?: string | null };
	retention: {
		raw_days?: number;
		rollup_1m_days?: number;
		rollup_5m_days?: number;
		rollup_1h_days?: number;
		rollup_1d_days?: number;
		webhook_delivery_days?: number;
		/**
		 * How long a rendered report's bytes are kept, and **deliberately not a
		 * tier**: an artifact is expected to outlive the data it was computed
		 * from, so the coarser-outlives-finer rule does not apply to it. Zero
		 * keeps them indefinitely.
		 */
		report_artifact_days?: number;
	};
	smtp: {
		host: string | null;
		port: number | null;
		username: string | null;
		encryption: string;
		from_address: string | null;
		from_name: string | null;
	};
	monitoring: {
		default_interval_seconds?: number;
		default_timeout_seconds?: number;
		default_retries?: number;
		default_notification_channel_ids?: string[];
		max_concurrent_checks?: number;
	};
	security: {
		session_timeout_minutes?: number;
		login_rate_limit_per_minute?: number;
		api_rate_limit_per_minute?: number;
		require_totp?: boolean;
		trusted_proxies?: string[];
	};
	telemetry: { enabled: boolean; last_sent_at?: string | null };
	/**
	 * The optional offsite **mirror** for report artifacts. Local storage under
	 * the data directory is the source of truth and the only read path in every
	 * configuration; this is a durability copy, and a failed upload is recorded
	 * rather than fatal.
	 *
	 * Not to be confused with a report schedule's S3 *delivery*, which drops one
	 * run's files for a recipient. They share a client and nothing else.
	 */
	report_storage: ReportStorageSettings;
};

export type ReportStorageSettings = {
	mirror_enabled: boolean;
	bucket: string | null;
	prefix: string | null;
	/** Required for the request signature even where the provider ignores it. */
	region: string | null;
	endpoint: string | null;
	path_style: boolean;
	access_key_id: string | null;
	/** Whether a secret is stored. **The value itself is never read back.** */
	secret_access_key_set: boolean;
	server_side_encryption: string | null;
	max_artifact_bytes: number;
};

/** One check executor. Read-only in this build; enrolment is Phase 4. */
export type Probe = {
	id: string;
	name: string;
	region: string | null;
	mode: 'embedded' | 'remote';
	version: string | null;
	last_seen_at: string | null;
	enabled: boolean;
	created_at: string;
};

export type Group = {
	id: string;
	name: string;
	description: string | null;
	parent_group_id: string | null;
	/**
	 * The worst status among the group's monitors, its children's included, and
	 * absent when it holds none. "No monitors" is a different statement from
	 * "up", and rendering it green would be the dashboard inventing health
	 * (internal/api/dto.go).
	 */
	status?: MonitorStatus;
	monitor_count: number;
	created_at: string;
	updated_at: string;
};

export type Tag = {
	id: string;
	name: string;
	/** Derived from the name by the server and never supplied by a client. */
	slug: string;
	color: string | null;
	description: string | null;
	monitor_count: number;
	created_at: string;
	updated_at: string;
};

export type Overview = {
	monitors: {
		total: number;
		up: number;
		down: number;
		pending: number;
		paused: number;
		maintenance: number;
	};
	active_incidents: number;
	active_maintenance_windows: number;
	certificates_expiring_soon: number;
	domains_expiring_soon: number;
	generated_at: string;
};

export type SystemInfo = {
	version: string;
	mode: string;
	storage_engine: string;
	api_version: string;
	monitor_types: string[];
	notification_channel_types: string[];
	capabilities: Record<string, boolean>;
};

export type User = {
	id: string;
	email: string;
	name: string | null;
	role: string;
	active: boolean;
	totp_enabled: boolean;
	timezone: string | null;
	locale: string | null;
	last_login_at: string | null;
	team_ids: string[];
	created_at: string;
	updated_at: string;
};

export type Session = {
	principal_type: string;
	user?: User;
	api_key_id: string | null;
	scopes: string[];
	csrf_token: string | null;
	expires_at: string | null;
};

export type HistoryBucket = {
	bucket_start: string;
	up_count: number;
	down_count: number;
	maintenance_count: number;
	pending_count: number;
	uptime_ratio: number | null;
	response_time_avg_ms: number | null;
	response_time_min_ms: number | null;
	response_time_max_ms: number | null;
	response_time_p95_ms: number | null;
};

export type History = {
	monitor_id: string;
	resolution: string;
	from: string;
	to: string;
	data: HistoryBucket[];
};

export type NotificationChannel = {
	id: string;
	name: string;
	type: string;
	enabled: boolean;
	is_default: boolean;
	config: Record<string, unknown>;
	last_error: string | null;
	last_success_at: string | null;
	created_at: string;
	updated_at: string;
};

/* ---- Status pages: the authenticated management shape ---- */

export type StatusPageSection = {
	name: string;
	description: string | null;
	monitor_ids: string[];
};

/**
 * A status page as its operator configures it.
 *
 * `password` is absent on purpose. The spec marks it writeOnly and the column
 * holds an argon2id hash, so there is nothing the read path could return — which
 * is why the form treats an empty password box as "leave it alone" rather than
 * as "clear it".
 */
export type StatusPage = {
	id: string;
	slug: string;
	title: string;
	description: string | null;
	published: boolean;
	custom_domain: string | null;
	visibility: 'public' | 'password';
	theme: 'light' | 'dark' | 'auto';
	logo_url: string | null;
	favicon_url: string | null;
	primary_color: string | null;
	footer_text: string | null;
	custom_css: string | null;
	timezone: string | null;
	show_uptime_percentage: boolean;
	show_response_time_chart: boolean;
	uptime_bar_days: number;
	show_powered_by: boolean;
	subscriptions_enabled: boolean;
	google_analytics_id: string | null;
	sections: StatusPageSection[];
	created_at: string;
	updated_at: string;
};

/**
 * Somebody who asked a status page to tell them about outages.
 *
 * `target` arrives masked from the server — a page's subscriber list is an
 * export of somebody else's customers, and the dashboard has no business
 * needing the full address to show that a row exists.
 */
export type Subscriber = {
	id: string;
	channel: string;
	target: string;
	confirmed: boolean;
	confirmed_at: string | null;
	created_at: string;
};

/* ---- The public status page projection ---- */

export type PublicBarEntry = {
	date: string;
	uptime_ratio: number | null;
	status: MonitorStatus | null;
};

export type PublicMonitorRecord = {
	id: string;
	name: string;
	description: string | null;
	status: MonitorStatus;
	uptime_percentage: number | null;
	uptime_bar?: PublicBarEntry[];
	response_time_ms: number | null;
};

export type PublicSection = {
	name: string;
	description: string | null;
	monitors: PublicMonitorRecord[];
};

export type PublicIncidentUpdate = {
	state?: string;
	body: string;
	created_at: string;
};

export type PublicIncident = {
	id: string;
	title: string;
	state: string;
	impact: string;
	started_at: string;
	resolved_at: string | null;
	affected_monitor_ids: string[];
	updates: PublicIncidentUpdate[];
};

export type PublicMaintenanceWindow = {
	title: string;
	description: string | null;
	starts_at: string;
	ends_at: string | null;
	affected_monitor_ids: string[];
};

export type PublicStatusPage = {
	slug: string;
	title: string;
	description: string | null;
	theme: string;
	logo_url: string | null;
	favicon_url: string | null;
	primary_color: string | null;
	footer_text: string | null;
	custom_css: string | null;
	show_powered_by: boolean;
	subscriptions_enabled: boolean;
	overall_status: string;
	sections: PublicSection[];
	active_incidents: PublicIncident[];
	recent_incidents: PublicIncident[];
	scheduled_maintenance: PublicMaintenanceWindow[];
	generated_at: string;
};

/**
 * Reporting.
 *
 * Three things kept apart, and the separation is what makes "re-send last
 * month's", "regenerate it after we corrected the incident record" and "the PDF
 * failed but the HTML went out" all expressible: a **template** is the
 * definition, a **schedule** is when it runs and who receives it, and a **run**
 * is one execution with the artifacts it produced.
 */

export type ReportType = 'uptime' | 'sla' | 'post_mortem' | 'comparative' | 'custom';
export type ReportPeriod = 'day' | 'week' | 'month' | 'quarter' | 'year' | 'custom';
export type ReportPeriodStyle = 'calendar' | 'rolling';
export type ReportFormat = 'pdf' | 'html' | 'csv' | 'json';
export type MaintenanceHandling = 'exclude' | 'count_as_up' | 'count_as_down';

/**
 * `partial` is a real state and is not collapsible into either neighbour: one
 * format produced and another not. Rendering it as "succeeded" is how somebody
 * concludes a delivery went out whole.
 */
export type ReportRunState = 'queued' | 'running' | 'succeeded' | 'partial' | 'failed';

export type ReportScope = {
	monitor_ids?: string[];
	group_ids?: string[];
	tag_ids?: string[];
	incident_id?: string | null;
};

export type ReportComparison = {
	mode: 'previous_period' | 'monitors' | 'groups';
	monitor_ids?: string[];
	group_ids?: string[];
};

export type ReportTemplate = {
	id: string;
	name: string;
	description: string | null;
	type: ReportType;
	scope: ReportScope;
	period: ReportPeriod;
	period_style: ReportPeriodStyle;
	sla_target: number | null;
	response_time_target_ms: number | null;
	maintenance_handling: MaintenanceHandling;
	brand_profile_id: string | null;
	comparison: ReportComparison | null;
	sections: string[];
	formats: ReportFormat[];
	created_at: string;
	updated_at: string;
};

/**
 * `expired` is a tombstone rather than a deletion: the bytes are reclaimed and
 * the row stays, so a bookmarked link answers "this existed and is gone" instead
 * of "no such thing". `failed` is one format that did not render while others
 * did, which is what makes a run `partial`.
 */
export type ReportArtifactState = 'rendered' | 'expired' | 'failed';

export type ReportArtifact = {
	id: string;
	format: ReportFormat;
	state: ReportArtifactState;
	size_bytes: number | null;
	sha256: string | null;
	error: string | null;
	/**
	 * Null when there is nothing to fetch — and that covers three different
	 * facts, so read it with `state`. `expired` is retention doing its job;
	 * `failed` never produced a file; and **`rendered` with a null URL means the
	 * bytes are not on disk**, which is what a database restored without
	 * `<data-dir>/reports/` leaves behind. The server does the disk check, because
	 * a row saying `rendered` is not the same claim as a readable file.
	 */
	download_url: string | null;
	expires_at: string | null;
	/**
	 * The offsite copy's state, and null when no mirror was configured — which is
	 * deliberately not the same as `pending`. The mirror is a durability copy and
	 * never a read path, so a failure here does not affect `state` and must not be
	 * rendered as though the report were damaged.
	 */
	mirror: ReportArtifactMirror | null;
	created_at: string;
};

export type ReportArtifactMirror = {
	state: 'pending' | 'uploaded' | 'failed';
	uploaded_at: string | null;
	error: string | null;
};

/**
 * One entry per configured target rather than per attempt: "we tried three times
 * and the third worked" is one delivery with three attempts, not three
 * deliveries.
 *
 * `skipped` is not a failure and must not be rendered as one — no relay
 * configured, or nothing rendered in a format this target takes.
 */
export type ReportDelivery = {
	type: 'email' | 'slack' | 'webhook' | 's3';
	outcome: 'succeeded' | 'failed' | 'skipped';
	error: string | null;
	attempts: number;
	delivered_at: string | null;
};

export type ReportRun = {
	id: string;
	report_template_id: string;
	report_schedule_id: string | null;
	state: ReportRunState;
	period_start: string;
	period_end: string;
	/** The zone the boundaries were cut in. A month means different instants in different ones. */
	timezone: string;
	artifacts: ReportArtifact[];
	deliveries: ReportDelivery[];
	/**
	 * The active share link, and **never the token**. It is shown once when the
	 * link is created and is not readable back: a screen that could re-display it
	 * is a screen that leaks it the first time it is screenshotted.
	 */
	share: ReportShare | null;
	late: boolean;
	error: string | null;
	started_at: string | null;
	finished_at: string | null;
	created_at: string;
};

export type ReportShare = {
	expires_at: string | null;
	created_at: string;
	/** Answers "has the client opened it yet", which is the first thing anybody asks. */
	last_accessed_at?: string | null;
};

/** The create response. **The only place the URL ever appears.** */
export type ReportShareCreated = {
	url: string;
	expires_at: string | null;
	created_at: string;
};

export type ReportScheduleDelivery = {
	type: 'email' | 'slack' | 'webhook' | 's3';
	recipients?: string[];
	notification_channel_id?: string | null;
	url?: string | null;
	/**
	 * The **drop**: a delivery target for one run's files. Not the mirror, which
	 * is a durability copy of every artifact configured once in settings. They
	 * share a client and nothing else, and an operator who configures this
	 * believing they have durability has bought nothing.
	 */
	s3?: ReportDeliveryS3 | null;
	formats?: ReportFormat[];
};

export type ReportDeliveryS3 = {
	bucket: string;
	prefix?: string | null;
	region?: string | null;
	endpoint?: string | null;
	path_style?: boolean;
	/** Write-only. A read never returns either of these. */
	access_key_id?: string;
	secret_access_key?: string;
};

export type ReportSchedule = {
	id: string;
	report_template_id: string;
	frequency: 'daily' | 'weekly' | 'monthly' | 'quarterly' | 'cron';
	cron: string | null;
	timezone: string;
	send_at: string | null;
	enabled: boolean;
	deliveries: ReportScheduleDelivery[];
	last_run_at: string | null;
	next_run_at: string | null;
	created_at: string;
	updated_at: string;
};

/**
 * A brand profile.
 *
 * `logo_url` is always null: the field is defined and no operation serves the
 * bytes, so a URL here would name an endpoint answering 405. The rendered report
 * embeds the logo directly, which is what makes the file standalone.
 */
export type BrandProfile = {
	id: string;
	name: string;
	company_name: string | null;
	primary_color: string | null;
	accent_color: string | null;
	footer_text: string | null;
	cover_text: string | null;
	hide_powered_by: boolean;
	logo_url: string | null;
	/** Non-null once a logo has been uploaded; the bytes themselves are not served. */
	logo_content_type: string | null;
	is_default: boolean;
	created_at: string;
	updated_at: string;
};

/**
 * One row of the expiry calendar.
 *
 * `days_remaining` is **signed**: something that expired eleven days ago reports
 * −11, because that is the row somebody opened the page to find. Flooring it at
 * zero would file it beside "expires today".
 */
export type UpcomingExpiry = {
	kind: 'certificate' | 'domain';
	monitor_id: string;
	monitor_name: string;
	subject: string | null;
	issuer: string | null;
	expires_at: string;
	days_remaining: number;
	observed_at: string;
};
