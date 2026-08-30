-- 0008_reporting — brand profiles, report templates, schedules, runs,
-- artifacts, deliveries, and share links, plus the SLO target on monitors and
-- groups and the settings section the artifact mirror needs.
--
-- Derived from the frozen Phase 2 surface in docs/api/openapi.yaml (ReportTemplate,
-- ReportSchedule, ReportRun, ReportArtifact, BrandProfile, ReportShareLink*) and
-- from ADR-008, which fixes where artifact bytes live and what a row about them
-- has to carry. Conventions from data model §1, unchanged: UUIDv7 as BLOB(16);
-- time as INTEGER epoch ms; booleans as INTEGER 0/1; enums as TEXT + CHECK; JSON
-- as TEXT.
--
-- The data model lists four Phase 2 tables at §4.13 — report_templates,
-- report_schedules, report_runs, report_artifacts. This migration creates seven.
-- The three it does not name are consequences of decisions taken after that list
-- was written: brand_profiles (spec Q2, 2026-08-27), report_share_links
-- (ADR-008 items 14–15), and report_schedule_deliveries + report_deliveries,
-- which are the configured target and the attempt against it — the same split
-- notification_channels and notification_deliveries already make. **§4.13 needs
-- updating in the same pull request as this file.**
--
-- Every table carries org_id from creation. Phase 3 tenancy is then a permission
-- change rather than the retrofit ADR-003 exists to prevent.
--
-- PRE-RELEASE: no release implements the reporting surface, so this file may
-- still change. From Phase 2's first tagged release it is immutable per data
-- model §8.

-- ---------------------------------------------------------------------------
-- Brand profiles
--
-- Reports only. Status pages keep their inline theme/logo_url/primary_color/
-- footer_text columns from 0003 untouched, which is decision Q2 of 2026-08-27:
-- unifying them means turning four columns of a shipped Phase 1 schema into a
-- brand_profile_id, and that is a breaking change this phase declined to make.
-- The accepted cost is that branding lives in three places and can drift.
-- ---------------------------------------------------------------------------

CREATE TABLE brand_profiles (
    id              BLOB    PRIMARY KEY,
    org_id          BLOB    NOT NULL REFERENCES organisations(id),
    name            TEXT    NOT NULL,
    company_name    TEXT,
    -- Six-digit hex including the leading '#', validated at the API against the
    -- spec's pattern. Stored as written so a colour round-trips exactly.
    primary_color   TEXT,
    accent_color    TEXT,
    -- Plain text, both of them, and that is a rendering constraint rather than a
    -- storage one: ADR-007's PDF writer has no rich-text pipeline, and a field
    -- that renders in HTML and not in PDF is worse than one that renders nowhere.
    footer_text     TEXT,
    cover_text      TEXT,
    hide_powered_by INTEGER NOT NULL DEFAULT 0 CHECK (hide_powered_by IN (0, 1)),

    -- The logo lives in the database, unlike a report artifact.
    --
    -- ADR-008 sends artifacts to the filesystem on three specifics: every
    -- VACUUM INTO backup growing in proportion, fifty writes contending with
    -- heartbeat ingest during the monthly burst, and no incremental blob access
    -- for a hundred-megabyte CSV. A logo shares none of them. It is written once
    -- when somebody sets up a client, it is bounded below a megabyte by the API,
    -- and keeping it here means branding survives the backup procedure that this
    -- release documents rather than needing a second directory beside it.
    --
    -- PNG or JPEG only, refused at upload with the reason. The PDF writer embeds
    -- rasters and has no SVG path translator, and an SVG dropped at render time
    -- is discovered by a client rather than by the operator.
    logo             BLOB,
    logo_content_type TEXT CHECK (logo_content_type IN ('image/png', 'image/jpeg')),
    logo_bytes       INTEGER,
    logo_updated_at  INTEGER,

    is_default      INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_brand_profiles_cursor ON brand_profiles (org_id, updated_at DESC, id DESC);

-- "Exactly one profile may hold this" is the spec's wording, so it is a
-- constraint rather than a convention. Partial, because the alternative is a
-- unique index over every profile that is not the default.
CREATE UNIQUE INDEX idx_brand_profiles_default ON brand_profiles (org_id) WHERE is_default = 1;

-- ---------------------------------------------------------------------------
-- Report templates — the saved definition
--
-- A template is the saved thing, a run is one execution of it, an artifact is
-- one rendered file. Keeping the three apart is what makes "re-send last
-- month's", "regenerate it now the incident record is corrected", and "the PDF
-- failed but the HTML went out" each expressible rather than approximated.
-- ---------------------------------------------------------------------------

CREATE TABLE report_templates (
    id          BLOB    PRIMARY KEY,
    org_id      BLOB    NOT NULL REFERENCES organisations(id),
    name        TEXT    NOT NULL,
    description TEXT,
    type        TEXT    NOT NULL CHECK (type IN (
                    'uptime', 'sla', 'post_mortem', 'comparative', 'custom')),

    -- Selection by rule, resolved at run time and never flattened to a list of
    -- ids at save time: {monitor_ids, group_ids, tag_ids, incident_id}, combined
    -- as a union. An agency that adds a monitor to a client's tag expects it in
    -- that client's next report without editing the report, and a saved list
    -- cannot do that.
    scope       TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(scope)),

    period       TEXT   NOT NULL DEFAULT 'month'
                     CHECK (period IN ('day', 'week', 'month', 'quarter', 'year', 'custom')),
    period_style TEXT   NOT NULL DEFAULT 'calendar'
                     CHECK (period_style IN ('calendar', 'rolling')),

    -- Null means "use the monitors' own targets", which is not the same as no
    -- target: resolution is this field, then the monitor, then its group, then
    -- none — and the report states which of the three answered, because a
    -- monitor silently inheriting a group's number is otherwise invisible to
    -- whoever reads it.
    --
    -- Exactly 100 is refused: its error budget is zero seconds, which makes burn
    -- rate undefined and every report a breach report.
    sla_target   REAL   CHECK (sla_target IS NULL OR (sla_target >= 0 AND sla_target < 100)),

    -- The threshold behind days_over_target. Deliberately not the same thing as
    -- a monitor's response_time_threshold_ms, which marks a check DOWN when
    -- breached; this one classifies days after the fact and changes no monitor's
    -- status.
    response_time_target_ms INTEGER CHECK (response_time_target_ms IS NULL OR response_time_target_ms > 0),

    -- Stated on the report face whichever way it is set. The default excludes
    -- declared maintenance from the denominator, and an SLA figure that silently
    -- counted it either way would be the wrong number in a contract review.
    maintenance_handling TEXT NOT NULL DEFAULT 'exclude'
                     CHECK (maintenance_handling IN ('exclude', 'count_as_up', 'count_as_down')),

    -- {mode, monitor_ids, group_ids} for comparative reports, null otherwise.
    -- Region-against-region is absent rather than stubbed: the shape accepts it,
    -- and the data does not exist until Phase 4 ships multi-region probes.
    comparison  TEXT    CHECK (comparison IS NULL OR json_valid(comparison)),

    -- Null uses the default profile, which derives from settings.appearance, so
    -- an install that never opens the branding screen still produces a report
    -- that does not look unbranded. SET NULL rather than RESTRICT: deleting a
    -- profile should not be blocked by a template that will fall back correctly.
    brand_profile_id BLOB REFERENCES brand_profiles(id) ON DELETE SET NULL,

    -- Ordered content blocks; empty means the defaults for the type.
    sections    TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(sections)),
    -- Array of pdf|html|csv|json, at least one, enforced at the API where the
    -- error can name the field.
    formats     TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(formats)),

    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_report_templates_cursor ON report_templates (org_id, updated_at DESC, id DESC);
CREATE INDEX idx_report_templates_brand  ON report_templates (org_id, brand_profile_id)
    WHERE brand_profile_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Schedules
-- ---------------------------------------------------------------------------

CREATE TABLE report_schedules (
    id                 BLOB    PRIMARY KEY,
    org_id             BLOB    NOT NULL REFERENCES organisations(id),
    -- Deleting a template takes its schedules with it. A schedule with no
    -- template to render is not a thing an operator can act on.
    report_template_id BLOB    NOT NULL REFERENCES report_templates(id) ON DELETE CASCADE,
    name               TEXT,
    enabled            INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    frequency          TEXT    NOT NULL
                           CHECK (frequency IN ('daily', 'weekly', 'monthly', 'quarterly', 'cron')),
    -- Five-field expression, required when frequency is 'cron' and otherwise
    -- null. Parsed by the shared parser lifted out of internal/maintenance —
    -- one implementation of the day-of-month/day-of-week union rule, not two.
    cron               TEXT,
    -- An IANA zone, defaulted from general.timezone at write time rather than
    -- resolved at run time, so that changing the instance zone does not silently
    -- move the boundaries of a report somebody has been receiving for a year.
    -- A monthly report cut at midnight UTC for an Australian agency is wrong by
    -- a working day.
    timezone           TEXT    NOT NULL,
    send_at            TEXT    NOT NULL DEFAULT '09:00',
    last_run_at        INTEGER,
    -- Computed on write and after every run. Stored rather than derived because
    -- the scheduler seeks on it, and because "when does this next fire" is a
    -- question the UI asks for every schedule on the page.
    next_run_at        INTEGER,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_report_schedules_cursor   ON report_schedules (org_id, updated_at DESC, id DESC);
CREATE INDEX idx_report_schedules_template ON report_schedules (org_id, report_template_id);

-- The due query, and the only index the scheduler uses. Partial on enabled,
-- because a disabled schedule is never due and an install that has paused half
-- its reports should not pay for them on every tick.
CREATE INDEX idx_report_schedules_due ON report_schedules (next_run_at)
    WHERE enabled = 1 AND next_run_at IS NOT NULL;

-- One row per configured delivery target, rather than a JSON array on the
-- schedule.
--
-- The reason is the secret. An s3 delivery carries an access key that is sealed
-- with AAD binding it to its row, which needs a row and an id to bind to — the
-- same shape notification_channels already uses, and for the same reason. It
-- also makes "the Slack delivery failed and the email went out" a per-row fact
-- rather than an index into an array.
CREATE TABLE report_schedule_deliveries (
    id                 BLOB    PRIMARY KEY,
    org_id             BLOB    NOT NULL REFERENCES organisations(id),
    report_schedule_id BLOB    NOT NULL REFERENCES report_schedules(id) ON DELETE CASCADE,
    type               TEXT    NOT NULL CHECK (type IN ('email', 'slack', 'webhook', 's3')),

    -- Non-secret configuration only: recipients, url, bucket, prefix, region,
    -- endpoint, path_style. Deliberately outside the sealed blob so that a read
    -- path serialising this can never serialise a credential (data model §12).
    config             TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(config)),

    -- The s3 secret access key, AES-256-GCM, AAD-bound to
    -- (org_id, 'report_schedule_deliveries', 'secrets', id). Encrypted rather
    -- than hashed because SigV4 replays it on every request — data model §12.1,
    -- where this row is already listed as "report_schedules S3 credentials,
    -- Phase 2".
    secrets            BLOB,

    -- Deliver through an existing notification channel rather than restating its
    -- configuration, so a rotated Slack token is rotated once. SET NULL rather
    -- than CASCADE: losing the channel should disable the delivery and say so,
    -- not silently delete a recipient from a schedule.
    notification_channel_id BLOB REFERENCES notification_channels(id) ON DELETE SET NULL,

    -- Which formats this target receives; empty means the run's formats. An
    -- auditor gets the PDF and a BI pipeline gets the CSV from one schedule.
    formats            TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(formats)),
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_report_schedule_deliveries_schedule
    ON report_schedule_deliveries (report_schedule_id);
CREATE INDEX idx_report_schedule_deliveries_channel
    ON report_schedule_deliveries (org_id, notification_channel_id)
    WHERE notification_channel_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Runs
-- ---------------------------------------------------------------------------

CREATE TABLE report_runs (
    id                 BLOB    PRIMARY KEY,
    org_id             BLOB    NOT NULL REFERENCES organisations(id),
    report_template_id BLOB    NOT NULL REFERENCES report_templates(id) ON DELETE CASCADE,
    -- Null for an ad-hoc "run now". SET NULL because deleting a schedule must
    -- not delete the reports it produced: the artifact is a record of what a
    -- client was sent, and it outlives the arrangement that sent it.
    report_schedule_id BLOB    REFERENCES report_schedules(id) ON DELETE SET NULL,

    state              TEXT    NOT NULL DEFAULT 'queued'
                           CHECK (state IN ('queued', 'running', 'succeeded', 'partial', 'failed')),

    period_start       INTEGER NOT NULL,
    period_end         INTEGER NOT NULL,
    -- The zone the boundaries were actually cut in, recorded rather than
    -- assumed, because it is the difference between a month and a month minus a
    -- working day and there is no way to recover it afterwards.
    timezone           TEXT    NOT NULL,

    -- The run started materially after it was due — the instance was down when
    -- it should have fired. A missed schedule is late, not lost, and the UI has
    -- to be able to say which.
    late               INTEGER NOT NULL DEFAULT 0 CHECK (late IN (0, 1)),

    -- Why the run is 'partial' or 'failed'. Per-artifact detail lives on the
    -- artifact; this is the sentence shown against the run.
    error              TEXT,
    started_at         INTEGER,
    finished_at        INTEGER,
    created_at         INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_report_runs_cursor   ON report_runs (org_id, created_at DESC, id DESC);
CREATE INDEX idx_report_runs_template ON report_runs (org_id, report_template_id, created_at DESC);
CREATE INDEX idx_report_runs_schedule ON report_runs (org_id, report_schedule_id, created_at DESC)
    WHERE report_schedule_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Artifacts — the index. The bytes are on disk (ADR-008).
-- ---------------------------------------------------------------------------

CREATE TABLE report_artifacts (
    id            BLOB    PRIMARY KEY,
    org_id        BLOB    NOT NULL REFERENCES organisations(id),
    report_run_id BLOB    NOT NULL REFERENCES report_runs(id) ON DELETE CASCADE,
    format        TEXT    NOT NULL CHECK (format IN ('pdf', 'html', 'csv', 'json')),

    -- 'expired' is a tombstone: retention reclaims the bytes and keeps the row,
    -- so a bookmarked share link answers "this existed and is gone" rather than
    -- "no such thing". 'failed' is one format that did not render while others
    -- did, which is what makes a run 'partial' rather than 'failed'.
    state         TEXT    NOT NULL DEFAULT 'rendered'
                      CHECK (state IN ('rendered', 'expired', 'failed')),

    -- Relative to <data-dir>/reports, e.g. '2026/09/0192f0e3….pdf'. Written by
    -- us from the artifact id and the format and never from the template title,
    -- so a definition called '../../etc' has nowhere to go.
    --
    -- Stored rather than recomputed on read: retention and the orphan sweeper
    -- both need to go from a row to a file without reconstructing whatever
    -- dating rule was in force when it was written, and a rule that changes
    -- later would otherwise strand every artifact created before it. Null while
    -- state is 'failed', because nothing was written.
    path          TEXT,

    size_bytes    INTEGER,
    -- Hex SHA-256 of the bytes as written. The point is not corruption
    -- detection for its own sake: it is what lets somebody assert that the file
    -- restored from a backup is the file that was sent to the client.
    sha256        TEXT,

    error         TEXT,
    -- Null means kept indefinitely, which is what report_artifact_days = 0
    -- selects. Otherwise the instant the sweeper may reclaim the bytes.
    expires_at    INTEGER,
    created_at    INTEGER NOT NULL
) STRICT;

-- One artifact per format per run. A re-render replaces the row and the file
-- rather than accumulating a second PDF nobody can choose between.
CREATE UNIQUE INDEX idx_report_artifacts_run_format ON report_artifacts (report_run_id, format);

-- The retention sweep. Partial, because artifacts kept indefinitely are not
-- candidates and an install that keeps everything should not pay to be asked.
CREATE INDEX idx_report_artifacts_expiry ON report_artifacts (expires_at)
    WHERE expires_at IS NOT NULL AND state = 'rendered';

-- ---------------------------------------------------------------------------
-- Share links
-- ---------------------------------------------------------------------------

-- The token is stored twice, following subscribers.unsubscribe_token: hashed for
-- the lookup index, sealed for replay. "Hash what you verify, encrypt what you
-- replay" (data model §12.1) — and this token is both, because the link is
-- verified when a client follows it and rendered again whenever somebody asks
-- what the link was.
--
-- One live link per run: creating a second revokes the first rather than leaving
-- two credentials to the same document in circulation.
CREATE TABLE report_share_links (
    id              BLOB    PRIMARY KEY,
    org_id          BLOB    NOT NULL REFERENCES organisations(id),
    report_run_id   BLOB    NOT NULL REFERENCES report_runs(id) ON DELETE CASCADE,

    token_hash      BLOB    NOT NULL,
    token_encrypted BLOB    NOT NULL,

    -- Null never expires. Revocation is available regardless, and is a column
    -- rather than a delete so that a revoked link answers "this was withdrawn"
    -- instead of looking like a typo.
    expires_at      INTEGER,
    revoked_at      INTEGER,
    -- Answers "has the client opened it yet", which is the first thing anybody
    -- asks after sending one.
    last_accessed_at INTEGER,
    created_at      INTEGER NOT NULL
) STRICT;

-- The unauthenticated lookup, and the reason it is unique: the token in the path
-- is the entire credential, so guessing has to cost a probe against a unique
-- index rather than a walk of every share link on the instance.
CREATE UNIQUE INDEX idx_report_share_links_token ON report_share_links (token_hash);

-- One live link per run, enforced rather than conventional.
CREATE UNIQUE INDEX idx_report_share_links_live ON report_share_links (report_run_id)
    WHERE revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- Deliveries — one row per attempt, mirroring notification_deliveries
-- ---------------------------------------------------------------------------

CREATE TABLE report_deliveries (
    id            BLOB    PRIMARY KEY,
    org_id        BLOB    NOT NULL REFERENCES organisations(id),
    report_run_id BLOB    NOT NULL REFERENCES report_runs(id) ON DELETE CASCADE,

    -- Which configured target this attempt was against. SET NULL because the
    -- delivery log outlives the schedule: "we sent it to them in March" has to
    -- survive somebody removing the recipient in April.
    report_schedule_delivery_id BLOB REFERENCES report_schedule_deliveries(id) ON DELETE SET NULL,

    type          TEXT    NOT NULL CHECK (type IN ('email', 'slack', 'webhook', 's3')),
    -- 'skipped' is not a failure and must not read as one: no relay configured,
    -- nothing rendered in a format this target takes. It is recorded for the
    -- same reason a suppressed notification is — silence with no row behind it
    -- is indistinguishable from a system that is not running.
    outcome       TEXT    NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'skipped')),
    error         TEXT,
    attempt       INTEGER NOT NULL DEFAULT 1,
    -- The address, channel or bucket this attempt went to, for the delivery log.
    -- Recorded rather than joined, because the configuration can change and the
    -- log is a statement about what happened.
    target        TEXT,
    delivered_at  INTEGER,
    created_at    INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_report_deliveries_run  ON report_deliveries (report_run_id, created_at DESC);
CREATE INDEX idx_report_deliveries_time ON report_deliveries (org_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- SLO targets on monitors and groups
--
-- On the monitor rather than on the report, so that alerting can act on the same
-- number later without a second place to configure it. **Phase 2 only reads
-- it**: nothing alerts on it, and no monitor's status is affected by it.
--
-- Only the uptime target moves here. response_time_target_ms stays on the
-- template — a monitor already carries response_time_threshold_ms in its HTTP
-- config, which marks a check DOWN when breached, and a second per-monitor
-- latency number that merely classifies days would be two settings a name apart
-- doing different things.
--
-- Null is the default and means no target, which is what a solo user sees: the
-- field is inert until somebody sets it, and the SLA block is then absent rather
-- than defaulted to a number nobody chose.
-- ---------------------------------------------------------------------------

ALTER TABLE monitors ADD COLUMN slo_target_percent REAL
    CHECK (slo_target_percent IS NULL OR (slo_target_percent >= 0 AND slo_target_percent < 100));

ALTER TABLE groups ADD COLUMN slo_target_percent REAL
    CHECK (slo_target_percent IS NULL OR (slo_target_percent >= 0 AND slo_target_percent < 100));

-- No index on either. A target is read when a report resolves one for a monitor
-- already in hand, never searched on, and an index over 5,000 mostly-null
-- columns would earn nothing on the busiest table in the schema.

-- ---------------------------------------------------------------------------
-- Settings — the artifact mirror
--
-- An eighth section beside general/appearance/retention/smtp/monitoring/
-- security/telemetry. retention.report_artifact_days needs no schema change:
-- the retention section is already JSON, and 365 is its default.
--
-- The mirror's secret access key is sealed inside this JSON as
-- secret_access_key_sealed, exactly as smtp.password_sealed is — a []byte
-- marshals to base64, so the column holds no plaintext at any point, and no
-- migration is needed to add a secret to a section that already exists.
-- ---------------------------------------------------------------------------

ALTER TABLE settings ADD COLUMN report_storage TEXT NOT NULL DEFAULT '{}'
    CHECK (json_valid(report_storage));
