-- 0002_identity — users, sessions, API keys, and the encryption key ledger.
--
-- Derived from docs/data-model/README.md §3.2–§3.4 and §12.3. Conventions from
-- §1: UUIDv7 as BLOB(16); time as INTEGER epoch ms; booleans as INTEGER 0/1;
-- enums as TEXT + CHECK; JSON as TEXT.
--
-- PRE-RELEASE: nothing has shipped, so this file may still change. From Phase 1's
-- first tagged release it is immutable per data model §8.

-- Phase 1 has exactly one account, and it is the owner. The other roles are in
-- the CHECK from the start so that Phase 3's RBAC is a feature rather than a
-- migration of every existing row (ADR-003).
CREATE TABLE users (
    id              BLOB    PRIMARY KEY,
    org_id          BLOB    NOT NULL REFERENCES organisations(id),
    -- Stored lowercased, so uniqueness means what a user expects it to mean.
    email           TEXT    NOT NULL,
    name            TEXT,
    -- argon2id in its encoded string form, parameters included, so the cost can
    -- be raised later without a schema change and old hashes still verify.
    password_hash   TEXT    NOT NULL,
    role            TEXT    NOT NULL CHECK (role IN (
                        'owner', 'admin', 'editor', 'responder', 'viewer', 'billing')),
    active          INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    -- Encrypted at rest (§12): it is replayed on every login, so it cannot be
    -- hashed. The envelope binds it to this row, so relocating the blob onto
    -- another user fails to open.
    totp_secret     BLOB,
    -- Null means enrolment was started and never confirmed, which is not the
    -- same as TOTP being off — an unconfirmed secret must never gate a login.
    totp_enabled_at INTEGER,
    timezone        TEXT,
    locale          TEXT,
    last_login_at   INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX idx_users_email ON users (org_id, email);

-- A child table rather than an array column: each code is single-use and needs
-- its own consumption timestamp (§3.2).
CREATE TABLE user_recovery_codes (
    id         BLOB    PRIMARY KEY,
    user_id    BLOB    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  BLOB    NOT NULL,
    used_at    INTEGER,
    created_at INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_recovery_codes_user ON user_recovery_codes (user_id) WHERE used_at IS NULL;

-- The cookie carries the token; only its hash is stored, so a database leak does
-- not hand over live sessions (§3.3).
CREATE TABLE sessions (
    id              BLOB    PRIMARY KEY,
    user_id         BLOB    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      BLOB    NOT NULL UNIQUE,
    csrf_token_hash BLOB    NOT NULL,
    expires_at      INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    last_seen_at    INTEGER,
    ip              TEXT,
    user_agent      TEXT
) STRICT;

CREATE INDEX idx_sessions_user    ON sessions (user_id);
CREATE INDEX idx_sessions_expires ON sessions (expires_at);

CREATE TABLE api_keys (
    id           BLOB    PRIMARY KEY,
    org_id       BLOB    NOT NULL REFERENCES organisations(id),
    name         TEXT    NOT NULL,
    -- Non-secret leading characters, so a key can be identified in a listing
    -- without the listing being able to authenticate as it.
    prefix       TEXT    NOT NULL,
    key_hash     BLOB    NOT NULL UNIQUE,
    -- A join table buys nothing: scopes are always read as a whole set and never
    -- queried across keys (§3.4).
    scopes       TEXT    NOT NULL CHECK (json_valid(scopes)),
    expires_at   INTEGER,
    -- Written at most once per minute per key, not per request; otherwise every
    -- authenticated call becomes a write.
    last_used_at INTEGER,
    -- Soft, so a revoked key's audit entries stay resolvable.
    revoked_at   INTEGER,
    created_by   BLOB    REFERENCES users(id),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_api_keys_cursor ON api_keys (org_id, updated_at DESC, id DESC);

-- §12.3: envelope encryption, two levels. The root key never appears here — it
-- comes from a file the operator controls — and this table holds only data keys
-- sealed with it, so rotating the root key re-wraps a handful of rows instead of
-- rewriting every encrypted record.
CREATE TABLE encryption_keys (
    version     INTEGER PRIMARY KEY,
    wrapped_dek BLOB    NOT NULL,
    algorithm   TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    retired_at  INTEGER
) STRICT;

-- One row per org, a JSON document per section, rather than an EAV key-value
-- table (data model §4.12): settings are read as a whole, written rarely, and
-- validated as a unit, and EAV would lose type safety while buying nothing.
--
-- Only `general` carries anything today. The other sections exist so that
-- adding, say, SMTP configuration is a write rather than a migration.
CREATE TABLE settings (
    org_id      BLOB    PRIMARY KEY REFERENCES organisations(id),
    general     TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(general)),
    appearance  TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(appearance)),
    retention   TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(retention)),
    smtp        TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(smtp)),
    monitoring  TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(monitoring)),
    security    TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(security)),
    telemetry   TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(telemetry)),
    updated_at  INTEGER NOT NULL
) STRICT;

INSERT INTO settings (org_id, updated_at) VALUES
    (x'00000000000070008000000000000001', 1767225600000);
