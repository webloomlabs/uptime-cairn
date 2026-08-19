-- 0003_alerting_and_pages — notification channels, maintenance windows,
-- incidents, status pages, outbound webhooks, the audit log, imports, and the
-- two operational tables the rollup and retention jobs need.
--
-- Derived from docs/data-model/README.md §4.4–§4.12, §5.5, and §9.3. Conventions
-- from §1: UUIDv7 as BLOB(16); time as INTEGER epoch ms; booleans as INTEGER 0/1;
-- enums as TEXT + CHECK; JSON as TEXT.
--
-- This is the rest of the model 0001 deferred. Nothing here has an API or a
-- worker behind it yet except monitor_uptime_cache and pending_purges, which the
-- rollup and retention jobs use from this migration onward. The tables land now
-- because the alternative is a schema change in the middle of building the
-- alerting engine, and because every one of them carries org_id from creation —
-- adding tenancy afterwards is the retrofit ADR-003 exists to prevent.
--
-- PRE-RELEASE: nothing has shipped, so this file may still change. From Phase 1's
-- first tagged release it is immutable per data model §8.

-- ---------------------------------------------------------------------------
-- Notification channels (§4.4, §4.5)
-- ---------------------------------------------------------------------------

CREATE TABLE notification_channels (
    id           BLOB    PRIMARY KEY,
    org_id       BLOB    NOT NULL REFERENCES organisations(id),
    name         TEXT    NOT NULL,
    type         TEXT    NOT NULL CHECK (type IN (
                     'email', 'webhook', 'slack', 'discord', 'telegram', 'matrix',
                     'gotify', 'ntfy', 'msteams', 'pagerduty', 'opsgenie',
                     'twilio', 'apprise')),
    -- Non-secret configuration only.
    config       TEXT    NOT NULL CHECK (json_valid(config)),
    -- The writeOnly fields — bot tokens, webhook URLs, SMTP passwords — in one
    -- encrypted blob, deliberately outside config so that a read path
    -- serialising the config can never accidentally serialise a secret (§12).
    secrets      BLOB,
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    -- Default channels are attached to newly created monitors automatically.
    is_default   INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    -- Array of EventType. Empty means all monitor state changes.
    events       TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(events)),
    last_used_at INTEGER,
    -- Surfaced in the UI, because a channel that silently stopped delivering is
    -- indistinguishable from one that has had nothing to say.
    last_error   TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_notification_channels_cursor  ON notification_channels (org_id, updated_at DESC, id DESC);
CREATE INDEX idx_notification_channels_default ON notification_channels (org_id) WHERE is_default = 1;

CREATE TABLE monitor_notification_channels (
    monitor_id BLOB NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    channel_id BLOB NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    org_id     BLOB NOT NULL REFERENCES organisations(id),
    PRIMARY KEY (monitor_id, channel_id)
) STRICT;

-- Reverse direction: the channel list's monitor_count reads this way.
CREATE INDEX idx_monitor_channels_rev ON monitor_notification_channels (org_id, channel_id, monitor_id);

-- "Did the alert actually go out?" — distinct from webhook_deliveries, and the
-- input to Phase 2's post-mortem "alerts fired" section (§4.5).
CREATE TABLE notification_deliveries (
    id               BLOB    PRIMARY KEY,
    org_id           BLOB    NOT NULL REFERENCES organisations(id),
    -- Nullable: some events are not monitor-scoped.
    monitor_id       BLOB    REFERENCES monitors(id) ON DELETE SET NULL,
    channel_id       BLOB    REFERENCES notification_channels(id) ON DELETE SET NULL,
    event_type       TEXT    NOT NULL,
    incident_id      BLOB,
    outcome          TEXT    NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'suppressed')),
    error            TEXT,
    duration_ms      REAL,
    attempt          INTEGER NOT NULL DEFAULT 1,
    -- Truncated on write. A rendered payload carries whatever the user's
    -- template put in it, which is why retention applies here at all.
    rendered_payload TEXT,
    created_at       INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_notification_deliveries_time    ON notification_deliveries (org_id, created_at DESC);
CREATE INDEX idx_notification_deliveries_monitor ON notification_deliveries (org_id, monitor_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- Certificate and domain observations (§4.6)
-- ---------------------------------------------------------------------------

-- One row per monitor, replaced on each observation. History is not required in
-- Phase 1, and keeping one row makes /certificate a primary-key read.
CREATE TABLE monitor_certificates (
    monitor_id         BLOB    PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
    org_id             BLOB    NOT NULL REFERENCES organisations(id),
    subject            TEXT,
    issuer             TEXT,
    serial_number      TEXT,
    valid_from         INTEGER,
    valid_to           INTEGER NOT NULL,
    fingerprint_sha256 BLOB,
    sans               TEXT    CHECK (sans IS NULL OR json_valid(sans)),
    chain_valid        INTEGER CHECK (chain_valid IN (0, 1)),
    chain_error        TEXT,
    observed_at        INTEGER NOT NULL
) STRICT;

-- "Certificates expiring soon" on the overview is a range scan, not a table
-- scan, and Phase 2's expiry calendar reads the same index.
CREATE INDEX idx_monitor_certificates_expiry ON monitor_certificates (org_id, valid_to);

-- Domain expiry gets its own shape rather than being forced into a
-- certificate-shaped row: a registration has a registrar and a source, and no
-- issuer, subject, or chain (§4.6).
CREATE TABLE monitor_domain_expiry (
    monitor_id  BLOB    PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
    org_id      BLOB    NOT NULL REFERENCES organisations(id),
    domain      TEXT    NOT NULL,
    expires_at  INTEGER NOT NULL,
    registrar   TEXT,
    source      TEXT    CHECK (source IS NULL OR source IN ('rdap', 'whois')),
    observed_at INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_monitor_domain_expiry ON monitor_domain_expiry (org_id, expires_at);

-- ---------------------------------------------------------------------------
-- Maintenance windows (§4.7)
-- ---------------------------------------------------------------------------

CREATE TABLE maintenance_windows (
    id                     BLOB    PRIMARY KEY,
    org_id                 BLOB    NOT NULL REFERENCES organisations(id),
    title                  TEXT    NOT NULL,
    description            TEXT,
    strategy               TEXT    NOT NULL CHECK (strategy IN (
                               'single', 'recurring_daily', 'recurring_weekly',
                               'recurring_monthly', 'cron')),
    -- An IANA zone name, not an offset: "02:00 every Sunday" has to survive a
    -- daylight-saving transition meaning what the operator meant.
    timezone               TEXT    NOT NULL DEFAULT 'UTC',
    starts_at              INTEGER,
    -- Null for a recurring window, which has no end.
    ends_at                INTEGER,
    duration_minutes       INTEGER,
    -- Weekdays, days_of_month, cron, until.
    recurrence             TEXT    CHECK (recurrence IS NULL OR json_valid(recurrence)),
    suppress_notifications INTEGER NOT NULL DEFAULT 1 CHECK (suppress_notifications IN (0, 1)),
    show_on_status_pages   INTEGER NOT NULL DEFAULT 1 CHECK (show_on_status_pages IN (0, 1)),
    cancelled_at           INTEGER,
    -- Materialised so the scheduler finds due windows with an index seek rather
    -- than evaluating every recurrence rule on every tick. Recomputed whenever a
    -- window is written and when an occurrence ends.
    next_occurrence_at     INTEGER,
    created_at             INTEGER NOT NULL,
    updated_at             INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_maintenance_next   ON maintenance_windows (org_id, next_occurrence_at)
    WHERE cancelled_at IS NULL;
CREATE INDEX idx_maintenance_cursor ON maintenance_windows (org_id, updated_at DESC, id DESC);

-- Polymorphic by design. Targeting by tag is what lets a window keep covering
-- monitors added after it was created, so resolution is a query at evaluation
-- time and never a snapshot of monitor ids.
CREATE TABLE maintenance_targets (
    id          BLOB PRIMARY KEY,
    window_id   BLOB NOT NULL REFERENCES maintenance_windows(id) ON DELETE CASCADE,
    org_id      BLOB NOT NULL REFERENCES organisations(id),
    target_type TEXT NOT NULL CHECK (target_type IN ('monitor', 'group', 'tag')),
    -- No foreign key: the referent is one of three tables. Resolution filters
    -- through the live table, so a dangling target resolves to nothing rather
    -- than to the wrong monitor.
    target_id   BLOB NOT NULL
) STRICT;

CREATE UNIQUE INDEX idx_maintenance_targets ON maintenance_targets (window_id, target_type, target_id);
CREATE INDEX idx_maintenance_targets_rev    ON maintenance_targets (org_id, target_type, target_id);

-- ---------------------------------------------------------------------------
-- Incidents (§4.8)
-- ---------------------------------------------------------------------------

CREATE TABLE incidents (
    id              BLOB    PRIMARY KEY,
    org_id          BLOB    NOT NULL REFERENCES organisations(id),
    title           TEXT    NOT NULL,
    state           TEXT    NOT NULL CHECK (state IN (
                        'investigating', 'identified', 'monitoring', 'resolved')),
    impact          TEXT    NOT NULL DEFAULT 'none' CHECK (impact IN (
                        'none', 'minor', 'major', 'critical')),
    started_at      INTEGER NOT NULL,
    resolved_at     INTEGER,
    auto_opened     INTEGER NOT NULL DEFAULT 0 CHECK (auto_opened IN (0, 1)),
    acknowledged_at INTEGER,
    acknowledged_by BLOB    REFERENCES users(id) ON DELETE SET NULL,
    assigned_to     BLOB    REFERENCES users(id) ON DELETE SET NULL,
    -- MTTD/MTTA/MTTR are DERIVED from these four timestamps at read time and are
    -- deliberately not stored: a stored metric drifts from the timeline it was
    -- computed from, and the timeline is the thing anyone will argue about.
    detected_at     INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_incidents_cursor ON incidents (org_id, updated_at DESC, id DESC);
CREATE INDEX idx_incidents_open   ON incidents (org_id, started_at DESC) WHERE resolved_at IS NULL;

CREATE TABLE incident_updates (
    id                   BLOB    PRIMARY KEY,
    incident_id          BLOB    NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    org_id               BLOB    NOT NULL REFERENCES organisations(id),
    -- Nullable: an update need not change state.
    state                TEXT    CHECK (state IS NULL OR state IN (
                             'investigating', 'identified', 'monitoring', 'resolved')),
    body                 TEXT    NOT NULL,
    author_id            BLOB    REFERENCES users(id) ON DELETE SET NULL,
    notified_subscribers INTEGER NOT NULL DEFAULT 0 CHECK (notified_subscribers IN (0, 1)),
    created_at           INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_incident_updates ON incident_updates (incident_id, created_at, id);

CREATE TABLE incident_monitors (
    incident_id BLOB NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    monitor_id  BLOB NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    org_id      BLOB NOT NULL REFERENCES organisations(id),
    PRIMARY KEY (incident_id, monitor_id)
) STRICT;

CREATE INDEX idx_incident_monitors_rev ON incident_monitors (org_id, monitor_id, incident_id);

-- ---------------------------------------------------------------------------
-- Status pages (§4.9)
-- ---------------------------------------------------------------------------

CREATE TABLE status_pages (
    id                       BLOB    PRIMARY KEY,
    org_id                   BLOB    NOT NULL REFERENCES organisations(id),
    slug                     TEXT    NOT NULL,
    title                    TEXT    NOT NULL,
    description              TEXT,
    published                INTEGER NOT NULL DEFAULT 0 CHECK (published IN (0, 1)),
    custom_domain            TEXT,
    visibility               TEXT    NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'password')),
    -- Hashed, not encrypted: it is verified against, never replayed (§12.1).
    password_hash            TEXT,
    theme                    TEXT,
    logo_url                 TEXT,
    favicon_url              TEXT,
    primary_color            TEXT,
    footer_text              TEXT,
    custom_css               TEXT,
    timezone                 TEXT    NOT NULL DEFAULT 'UTC',
    show_uptime_percentage   INTEGER NOT NULL DEFAULT 1 CHECK (show_uptime_percentage IN (0, 1)),
    show_response_time_chart INTEGER NOT NULL DEFAULT 1 CHECK (show_response_time_chart IN (0, 1)),
    uptime_bar_days          INTEGER NOT NULL DEFAULT 90,
    show_powered_by          INTEGER NOT NULL DEFAULT 1 CHECK (show_powered_by IN (0, 1)),
    subscriptions_enabled    INTEGER NOT NULL DEFAULT 0 CHECK (subscriptions_enabled IN (0, 1)),
    google_analytics_id      TEXT,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX idx_status_pages_slug ON status_pages (org_id, slug);
-- A hostname can serve exactly one page, across every org: the request arrives
-- with nothing but the Host header to route on.
CREATE UNIQUE INDEX idx_status_pages_domain ON status_pages (custom_domain) WHERE custom_domain IS NOT NULL;
CREATE INDEX idx_status_pages_cursor ON status_pages (org_id, updated_at DESC, id DESC);

CREATE TABLE status_page_sections (
    id             BLOB    PRIMARY KEY,
    status_page_id BLOB    NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
    org_id         BLOB    NOT NULL REFERENCES organisations(id),
    name           TEXT    NOT NULL,
    description    TEXT,
    position       INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX idx_status_page_sections ON status_page_sections (status_page_id, position, id);

CREATE TABLE status_page_section_monitors (
    section_id     BLOB    NOT NULL REFERENCES status_page_sections(id) ON DELETE CASCADE,
    monitor_id     BLOB    NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    -- Denormalised from the section purely to carry the uniqueness constraint
    -- below, which the API states and which cannot be expressed across the join.
    status_page_id BLOB    NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
    org_id         BLOB    NOT NULL REFERENCES organisations(id),
    position       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (section_id, monitor_id)
) STRICT;

-- A monitor appears in at most one section per page.
CREATE UNIQUE INDEX idx_status_page_monitor ON status_page_section_monitors (status_page_id, monitor_id);

CREATE TABLE subscribers (
    id                     BLOB    PRIMARY KEY,
    status_page_id         BLOB    NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
    org_id                 BLOB    NOT NULL REFERENCES organisations(id),
    channel                TEXT    NOT NULL CHECK (channel IN ('email', 'webhook')),
    -- Encrypted: it is replayed on every notification, so it cannot be hashed.
    target                 BLOB    NOT NULL,
    -- The uniqueness check without a plaintext index over every subscriber
    -- address on the instance (§12.5).
    target_hash            BLOB    NOT NULL,
    confirm_token_hash     BLOB,
    confirmed_at           INTEGER,
    unsubscribe_token_hash BLOB,
    created_at             INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX idx_subscribers_target      ON subscribers (status_page_id, target_hash);
CREATE UNIQUE INDEX idx_subscribers_unsubscribe ON subscribers (unsubscribe_token_hash)
    WHERE unsubscribe_token_hash IS NOT NULL;

CREATE TABLE maintenance_status_pages (
    window_id      BLOB NOT NULL REFERENCES maintenance_windows(id) ON DELETE CASCADE,
    status_page_id BLOB NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
    org_id         BLOB NOT NULL REFERENCES organisations(id),
    PRIMARY KEY (window_id, status_page_id)
) STRICT;

CREATE TABLE incident_status_pages (
    incident_id    BLOB NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    status_page_id BLOB NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
    org_id         BLOB NOT NULL REFERENCES organisations(id),
    PRIMARY KEY (incident_id, status_page_id)
) STRICT;

CREATE INDEX idx_incident_status_pages_rev ON incident_status_pages (status_page_id, incident_id);

-- ---------------------------------------------------------------------------
-- Outbound webhooks (§4.10)
-- ---------------------------------------------------------------------------

CREATE TABLE webhooks (
    id                   BLOB    PRIMARY KEY,
    org_id               BLOB    NOT NULL REFERENCES organisations(id),
    name                 TEXT    NOT NULL,
    url                  TEXT    NOT NULL,
    events               TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(events)),
    enabled              INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    headers              BLOB,
    -- ENCRYPTED, not hashed. Every delivery recomputes an HMAC over the body
    -- with this secret, so it has to be recoverable — the distinction in §12.1,
    -- and the one that only surfaces when the first delivery goes out.
    secret_encrypted     BLOB,
    -- Enough to recognise which secret a receiver was given, and not enough to
    -- sign with.
    secret_prefix        TEXT,
    verify_tls           INTEGER NOT NULL DEFAULT 1 CHECK (verify_tls IN (0, 1)),
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    -- Set when a webhook is auto-disabled after repeated failure, so "why did
    -- this stop" has an answer in the row rather than in the logs.
    disabled_at          INTEGER,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_webhooks_cursor ON webhooks (org_id, updated_at DESC, id DESC);

-- Append-only and high volume: it belongs with the time-series tables for
-- retention purposes even though it is not a hypertable.
CREATE TABLE webhook_deliveries (
    id              BLOB    PRIMARY KEY,
    webhook_id      BLOB    NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    org_id          BLOB    NOT NULL REFERENCES organisations(id),
    -- Stable across retries, so a receiver can deduplicate.
    event_id        BLOB    NOT NULL,
    event_type      TEXT    NOT NULL,
    outcome         TEXT    NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'pending')),
    attempt         INTEGER NOT NULL DEFAULT 1,
    request_body    TEXT,
    response_status INTEGER,
    -- Truncated on write: a receiver returning a megabyte of HTML on error must
    -- not turn one failing webhook into a disk-space incident.
    response_body   TEXT,
    error           TEXT,
    duration_ms     REAL,
    next_retry_at   INTEGER,
    created_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_webhook_deliveries_time    ON webhook_deliveries (org_id, created_at DESC);
CREATE INDEX idx_webhook_deliveries_webhook ON webhook_deliveries (webhook_id, created_at DESC);
CREATE INDEX idx_webhook_deliveries_retry   ON webhook_deliveries (next_retry_at) WHERE next_retry_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Audit log and imports (§4.12)
-- ---------------------------------------------------------------------------

-- Append-only, never updated, never deleted by the application. Retention does
-- not touch it: deleting an audit log defeats its purpose (§9.1).
CREATE TABLE audit_log (
    id          BLOB    PRIMARY KEY,
    org_id      BLOB    NOT NULL REFERENCES organisations(id),
    actor_type  TEXT    NOT NULL CHECK (actor_type IN ('user', 'api_key', 'system')),
    -- No foreign key: the actor may have been deleted, and an audit entry that
    -- vanishes when its subject does is not an audit entry.
    actor_id    BLOB,
    -- The EventType vocabulary extended with configuration verbs
    -- ("monitor.created" and so on). Not a CHECK: the vocabulary grows with
    -- every feature, and a migration per verb is a tax with no payer.
    action      TEXT    NOT NULL,
    entity_type TEXT    NOT NULL,
    entity_id   BLOB,
    -- Before/after, with secret fields elided at write time.
    changes     TEXT    CHECK (changes IS NULL OR json_valid(changes)),
    ip          TEXT,
    user_agent  TEXT,
    request_id  TEXT,
    created_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_audit_log_time   ON audit_log (org_id, created_at DESC);
CREATE INDEX idx_audit_log_entity ON audit_log (org_id, entity_type, entity_id, created_at DESC);

CREATE TABLE import_jobs (
    id          BLOB    PRIMARY KEY,
    org_id      BLOB    NOT NULL REFERENCES organisations(id),
    source      TEXT    NOT NULL,
    state       TEXT    NOT NULL CHECK (state IN (
                    'pending', 'analysing', 'importing', 'completed', 'failed', 'cancelled')),
    options     TEXT    CHECK (options IS NULL OR json_valid(options)),
    source_meta TEXT    CHECK (source_meta IS NULL OR json_valid(source_meta)),
    error       TEXT,
    started_at  INTEGER,
    finished_at INTEGER,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_import_jobs_cursor ON import_jobs (org_id, updated_at DESC, id DESC);

-- One row per source entity: the guarantee that nothing was silently dropped.
-- An import that maps 900 of 1,000 monitors and says so is trustworthy; one that
-- reports success is not.
CREATE TABLE import_entries (
    id            BLOB    PRIMARY KEY,
    job_id        BLOB    NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    org_id        BLOB    NOT NULL REFERENCES organisations(id),
    source_type   TEXT    NOT NULL,
    source_id     TEXT,
    source_name   TEXT,
    outcome       TEXT    NOT NULL CHECK (outcome IN ('imported', 'skipped', 'merged', 'failed')),
    reason        TEXT,
    entity_type   TEXT,
    entity_id     BLOB,
    created_at    INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_import_entries_job ON import_entries (job_id, outcome, id);

-- ---------------------------------------------------------------------------
-- Operational tables the rollup and retention jobs use (§5.5, §9.3)
-- ---------------------------------------------------------------------------

-- A performance structure, not a source of truth: always reconstructible from
-- the rollups, and safe to drop. It exists because computing 24h and 30d uptime
-- per row across a page of monitors is a fan-out of range scans, and at 5,000
-- monitors that is exactly the kind of convenience that fails the load gate.
CREATE TABLE monitor_uptime_cache (
    monitor_id       BLOB    NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    org_id           BLOB    NOT NULL REFERENCES organisations(id),
    window           TEXT    NOT NULL CHECK (window IN ('24h', '7d', '30d', '90d', '365d')),
    -- Null when up_count + down_count is zero for the window. A gap is not
    -- downtime, and the null has to survive all the way to the API (§5.3).
    uptime_ratio     REAL,
    total_checks     INTEGER NOT NULL DEFAULT 0,
    down_checks      INTEGER NOT NULL DEFAULT 0,
    downtime_seconds INTEGER NOT NULL DEFAULT 0,
    -- Staleness is bounded by the refresh interval and reported through this.
    computed_at      INTEGER NOT NULL,
    PRIMARY KEY (monitor_id, window)
) STRICT;

CREATE INDEX idx_monitor_uptime_cache_org ON monitor_uptime_cache (org_id, window);

-- Deleting a monitor removes its configuration row synchronously so the API can
-- return 204 honestly, and enqueues its history here for a background worker to
-- delete in bounded batches. A cascade over millions of heartbeat rows cannot
-- run inside a request (§9.3).
--
-- Orphaned heartbeats are invisible to every API query, because those all filter
-- through a live monitor. A purge that lags is a disk-space concern, never a
-- correctness one.
CREATE TABLE pending_purges (
    entity_type  TEXT    NOT NULL CHECK (entity_type IN ('monitor')),
    entity_id    BLOB    NOT NULL,
    org_id       BLOB    NOT NULL REFERENCES organisations(id),
    requested_at INTEGER NOT NULL,
    PRIMARY KEY (entity_type, entity_id)
) STRICT;

CREATE INDEX idx_pending_purges_order ON pending_purges (requested_at);
