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
