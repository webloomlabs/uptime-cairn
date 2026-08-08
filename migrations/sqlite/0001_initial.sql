-- 0001_initial — core monitoring schema (SQLite)
--
-- Derived from docs/data-model/README.md. Conventions from §1:
--   UUIDv7 as BLOB(16); time as INTEGER epoch ms (microseconds on heartbeats);
--   booleans as INTEGER 0/1; enums as TEXT + CHECK; JSON as TEXT.
--
-- SCOPE: this covers the tables the load-test harness exercises — the monitoring
-- core and the time series. Notification channels, status pages, incidents,
-- maintenance windows, webhooks, imports, audit log, and the Phase 2/3 tables are
-- specified in the data model and land in later migrations.
--
-- PRE-RELEASE: nothing has shipped, so this file may still change. From Phase 1's
-- first tagged release it is immutable per data model §8 — corrections become new
-- numbered migrations, never edits to this one.

-- auto_vacuum MUST be set before the first write. Changing it on an existing
-- database needs a full VACUUM that rewrites the whole file and requires free
-- space equal to its size — on a Pi with a 32GB card that is the difference
-- between working and not (data model §9.2). Hence: here, in the first migration.
PRAGMA auto_vacuum = INCREMENTAL;

-- The other pragmas in the schema contract (§7) — journal_mode=WAL,
-- foreign_keys=ON, busy_timeout, synchronous — are per-connection and belong in
-- the connection initialiser, not here. journal_mode in particular returns a row
-- rather than acting as a plain setter, which makes it awkward to run through a
-- migration runner.

CREATE TABLE organisations (
    id         BLOB    PRIMARY KEY,
    name       TEXT    NOT NULL,
    slug       TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- ADR-001: heartbeats reference a probe from the first commit, even in solo mode
-- where the probe is compiled in. Nullable-and-backfill-later is the retrofit that
-- ADR exists to prevent.
CREATE TABLE probes (
    id           BLOB    PRIMARY KEY,
    org_id       BLOB    NOT NULL REFERENCES organisations(id),
    name         TEXT    NOT NULL,
    region       TEXT,
    mode         TEXT    NOT NULL CHECK (mode IN ('embedded', 'remote')),
    token_hash   BLOB,
    version      TEXT,
    last_seen_at INTEGER,
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE groups (
    id              BLOB    PRIMARY KEY,
    org_id          BLOB    NOT NULL REFERENCES organisations(id),
    name            TEXT    NOT NULL,
    description     TEXT,
    parent_group_id BLOB    REFERENCES groups(id) ON DELETE SET NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT;

CREATE TABLE tags (
    id          BLOB    PRIMARY KEY,
    org_id      BLOB    NOT NULL REFERENCES organisations(id),
    name        TEXT    NOT NULL,
    slug        TEXT    NOT NULL,
    color       TEXT    NOT NULL DEFAULT '#6b7280',
    description TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (org_id, slug)
) STRICT;

CREATE TABLE monitors (
    id                     BLOB    PRIMARY KEY,
    org_id                 BLOB    NOT NULL REFERENCES organisations(id),
    name                   TEXT    NOT NULL,
    description            TEXT,
    type                   TEXT    NOT NULL CHECK (type IN (
                               'http', 'tcp', 'icmp', 'dns', 'tls_expiry',
                               'domain_expiry', 'push', 'docker', 'grpc')),
    config                 TEXT    NOT NULL CHECK (json_valid(config)),
    -- Promoted out of config so "what else points at this host?" is an indexed
    -- query rather than a JSON scan across 5,000 rows (§4.1).
    target                 TEXT,
    -- Push ingest is unauthenticated and hot; the token itself is encrypted for
    -- display, so the hash is what gets indexed (§12.5).
    push_token_hash        BLOB,
    enabled                INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    interval_seconds       INTEGER NOT NULL CHECK (interval_seconds >= 20),
    timeout_seconds        INTEGER NOT NULL CHECK (timeout_seconds < interval_seconds),
    retries                INTEGER NOT NULL DEFAULT 0,
    retry_interval_seconds INTEGER,
    resend_after           INTEGER NOT NULL DEFAULT 0,
    upside_down            INTEGER NOT NULL DEFAULT 0 CHECK (upside_down IN (0, 1)),
    notify_on_recovery     INTEGER NOT NULL DEFAULT 1 CHECK (notify_on_recovery IN (0, 1)),
    group_id               BLOB    REFERENCES groups(id) ON DELETE SET NULL,
    parent_monitor_id      BLOB    REFERENCES monitors(id) ON DELETE RESTRICT,
    created_at             INTEGER NOT NULL,
    updated_at             INTEGER NOT NULL
) STRICT;

-- ADR-004's cursor. Every list view pages on (updated_at, id) (§6.1).
CREATE INDEX idx_monitors_cursor  ON monitors (org_id, updated_at DESC, id DESC);
CREATE INDEX idx_monitors_group   ON monitors (org_id, group_id, updated_at DESC, id DESC);
CREATE INDEX idx_monitors_type    ON monitors (org_id, type, updated_at DESC, id DESC);
CREATE INDEX idx_monitors_enabled ON monitors (org_id, enabled, updated_at DESC, id DESC);
CREATE INDEX idx_monitors_target  ON monitors (org_id, target);
CREATE UNIQUE INDEX idx_monitors_push_token
    ON monitors (org_id, push_token_hash) WHERE push_token_hash IS NOT NULL;

CREATE TABLE monitor_tags (
    monitor_id BLOB NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    tag_id     BLOB NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    org_id     BLOB NOT NULL REFERENCES organisations(id),
    PRIMARY KEY (monitor_id, tag_id)
) STRICT;

-- Reverse direction: tag filtering on the list view reads this way, and without
-- the index it is a full scan (§4.3).
CREATE INDEX idx_monitor_tags_rev ON monitor_tags (org_id, tag_id, monitor_id);

-- Status deliberately does NOT live on monitors (§4.2). At 5,000 monitors on a
-- 20-second floor that is ~250 status writes/second; putting them on the wide
-- monitors row would rewrite it and every index entry covering it — including the
-- cursor index — on every heartbeat, and would churn updated_at so the list view
-- reorders under a paginating user.
CREATE TABLE monitor_state (
    monitor_id            BLOB    PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
    org_id                BLOB    NOT NULL REFERENCES organisations(id),
    status                TEXT    NOT NULL CHECK (status IN (
                              'up', 'down', 'pending', 'paused', 'maintenance')),
    last_check_at         INTEGER,
    next_check_at         INTEGER,
    last_status_change_at INTEGER,
    consecutive_failures  INTEGER NOT NULL DEFAULT 0,
    last_response_time_ms REAL,
    last_message          TEXT,
    suppressed_by         TEXT CHECK (suppressed_by IN ('maintenance', 'dependency')),
    -- Feeds ADR-004's membership signal (§6.5 option 3).
    state_version         INTEGER NOT NULL DEFAULT 0
) STRICT;

-- Trailing state_version is not padding: it makes this index COVERING for the
-- membership signal (§6.5), which is polled every ~5s per active filtered view.
-- Measured at 5,000 monitors: filtered membership 25.4us -> 12.1us, unfiltered
-- 166us -> 134us, both moving from index-then-table-lookup to a covering scan.
-- Trailing monitor_id keeps it covering for the status-filter join as well.
CREATE INDEX idx_monitor_state_status ON monitor_state (org_id, status, monitor_id, state_version);
-- The scheduler's due-work query, run every tick: arguably the hottest read here.
CREATE INDEX idx_monitor_state_due    ON monitor_state (next_check_at);

-- The only table within three orders of magnitude of the others: 5,000 monitors
-- at the 20s floor is ~250 writes/second, 21.6M rows/day (§5.1).
--
-- No surrogate primary key and no stored result_id: the natural key is
-- (monitor_id, probe_id, time), and a UUID column on a table this size costs 16
-- bytes plus an index for something nothing queries by (§11.8).
-- Status is an integer, not text — TEXT would cost several GB/year for nothing.
--
-- ADR-005: 4=unknown (the probe could not perform the check) and 5=skipped (shed
-- under overload) are NOT failures. Collapsing them into down would mean a probe
-- losing its network reports every monitor assigned to it as failing. Both are
-- excluded from uptime ratios, exactly as an absent bucket is.
CREATE TABLE heartbeats (
    time               INTEGER NOT NULL,  -- microseconds since epoch
    monitor_id         BLOB    NOT NULL,
    org_id             BLOB    NOT NULL,
    probe_id           BLOB    NOT NULL,
    -- 0=down 1=up 2=pending 3=maintenance 4=unknown 5=skipped
    status             INTEGER NOT NULL CHECK (status BETWEEN 0 AND 5),
    response_time_ms   REAL,
    code               TEXT,
    message            TEXT,              -- only on failures and state changes
    attempt            INTEGER NOT NULL DEFAULT 1,
    important          INTEGER NOT NULL DEFAULT 0 CHECK (important IN (0, 1)),
    suppressed         INTEGER NOT NULL DEFAULT 0 CHECK (suppressed IN (0, 1)),
    suppression_reason INTEGER            -- 1=maintenance 2=dependency
) STRICT;

-- UNIQUE per ADR-005: probes deliver at-least-once, so an unacknowledged batch is
-- resent, and under multi-region probing several probes check one monitor by
-- design. Ingest is INSERT ... ON CONFLICT DO NOTHING.
--
-- probe_id goes LAST on purpose. Monitor history filters org_id and monitor_id
-- and ranges over time, so it uses the leading three columns exactly as before;
-- a trailing column costs that query nothing. Ordering it
-- (org_id, monitor_id, probe_id, time) would force one range scan per probe.
-- Timescale also requires a hypertable's unique index to include the
-- partitioning column, which this satisfies by construction rather than by luck.
CREATE UNIQUE INDEX idx_heartbeats_monitor_time
    ON heartbeats (org_id, monitor_id, time DESC, probe_id);
-- Events only. Keeps the activity feed cheap without indexing 21M uneventful
-- rows a day.
CREATE INDEX idx_heartbeats_important ON heartbeats (org_id, time DESC) WHERE important = 1;

-- Rollup tiers per ADR-002: raw -> 1m -> 5m -> 1h -> 1d. One shape each, in its
-- own table so every tier's index stays dense and any tier can be dropped alone.
--
-- Sum and count, never a stored average: an average cannot be re-weighted into a
-- coarser tier, a sum and a count can. That is what lets 1h roll up from 5m.
-- uptime_ratio is NOT stored — it is computed at read time so the API's three-way
-- maintenance choice stays implementable.
-- A bucket with no checks has NO ROW: absence means "no data", which is not the
-- same as downtime.
--
-- ADR-005 adds a second route to "no data": a bucket whose checks were all
-- unknown or skipped DOES have a row, with up_count + down_count = 0. So the
-- null rule is stated on the denominator, not on the row's existence —
-- uptime_ratio is null whenever up_count + down_count = 0, row or no row. The
-- row is worth keeping because it carries WHY the observation is missing, which
-- an absent bucket cannot.
CREATE TABLE heartbeat_1m (
    bucket_start         INTEGER NOT NULL,
    monitor_id           BLOB    NOT NULL,
    org_id               BLOB    NOT NULL,
    up_count             INTEGER NOT NULL DEFAULT 0,
    down_count           INTEGER NOT NULL DEFAULT 0,
    pending_count        INTEGER NOT NULL DEFAULT 0,
    maintenance_count    INTEGER NOT NULL DEFAULT 0,
    -- Counted, never in the uptime denominator (ADR-005).
    unknown_count        INTEGER NOT NULL DEFAULT 0,
    skipped_count        INTEGER NOT NULL DEFAULT 0,
    response_time_sum    REAL,
    response_time_count  INTEGER,
    response_time_min    REAL,
    response_time_max    REAL,
    -- Populated only at this tier, computed from raw (§11.5). Coarser tiers carry
    -- an approximation and must label it as such.
    response_time_p95    REAL,
    PRIMARY KEY (monitor_id, bucket_start)
) STRICT;

CREATE INDEX idx_heartbeat_1m_lookup ON heartbeat_1m (org_id, monitor_id, bucket_start DESC);

CREATE TABLE heartbeat_5m (
    bucket_start        INTEGER NOT NULL,
    monitor_id          BLOB    NOT NULL,
    org_id              BLOB    NOT NULL,
    up_count            INTEGER NOT NULL DEFAULT 0,
    down_count          INTEGER NOT NULL DEFAULT 0,
    pending_count       INTEGER NOT NULL DEFAULT 0,
    maintenance_count   INTEGER NOT NULL DEFAULT 0,
    unknown_count       INTEGER NOT NULL DEFAULT 0,
    skipped_count       INTEGER NOT NULL DEFAULT 0,
    response_time_sum   REAL,
    response_time_count INTEGER,
    response_time_min   REAL,
    response_time_max   REAL,
    response_time_p95   REAL,
    PRIMARY KEY (monitor_id, bucket_start)
) STRICT;

CREATE INDEX idx_heartbeat_5m_lookup ON heartbeat_5m (org_id, monitor_id, bucket_start DESC);

CREATE TABLE heartbeat_1h (
    bucket_start        INTEGER NOT NULL,
    monitor_id          BLOB    NOT NULL,
    org_id              BLOB    NOT NULL,
    up_count            INTEGER NOT NULL DEFAULT 0,
    down_count          INTEGER NOT NULL DEFAULT 0,
    pending_count       INTEGER NOT NULL DEFAULT 0,
    maintenance_count   INTEGER NOT NULL DEFAULT 0,
    unknown_count       INTEGER NOT NULL DEFAULT 0,
    skipped_count       INTEGER NOT NULL DEFAULT 0,
    response_time_sum   REAL,
    response_time_count INTEGER,
    response_time_min   REAL,
    response_time_max   REAL,
    response_time_p95   REAL,
    PRIMARY KEY (monitor_id, bucket_start)
) STRICT;

CREATE INDEX idx_heartbeat_1h_lookup ON heartbeat_1h (org_id, monitor_id, bucket_start DESC);

CREATE TABLE heartbeat_1d (
    bucket_start        INTEGER NOT NULL,
    monitor_id          BLOB    NOT NULL,
    org_id              BLOB    NOT NULL,
    up_count            INTEGER NOT NULL DEFAULT 0,
    down_count          INTEGER NOT NULL DEFAULT 0,
    pending_count       INTEGER NOT NULL DEFAULT 0,
    maintenance_count   INTEGER NOT NULL DEFAULT 0,
    unknown_count       INTEGER NOT NULL DEFAULT 0,
    skipped_count       INTEGER NOT NULL DEFAULT 0,
    response_time_sum   REAL,
    response_time_count INTEGER,
    response_time_min   REAL,
    response_time_max   REAL,
    response_time_p95   REAL,
    PRIMARY KEY (monitor_id, bucket_start)
) STRICT;

CREATE INDEX idx_heartbeat_1d_lookup ON heartbeat_1d (org_id, monitor_id, bucket_start DESC);
