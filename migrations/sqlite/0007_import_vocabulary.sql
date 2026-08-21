-- Align the import tables with the frozen OpenAPI contract.
--
-- migration 0003 shipped `import_jobs.state` as
-- (pending, analysing, importing, completed, failed, cancelled) and
-- `import_entries.outcome` as (imported, skipped, merged, failed), written
-- before the importer existed. The spec — frozen first, and therefore the
-- contract — says states are (queued, running, succeeded, partial, failed) and
-- results are (imported, renamed, skipped, failed, unsupported).
--
-- Three of the differences matter rather than being cosmetic:
--
--   * `partial`. An import of a thousand monitors where thirty were a Kuma type
--     this build has no equivalent for has not failed and has not succeeded.
--     With only the two, it would report `completed`, and a user who reads that
--     and stops looking is missing thirty monitors.
--   * `unsupported` against `skipped`. "This build cannot represent it" and "you
--     asked me not to" are different sentences, and the first is the one an
--     evaluating user needs before committing to the migration.
--   * `renamed`. A collision resolved by suffixing is not the same event as an
--     import, and somebody looking for "API gateway" and finding "API gateway
--     (2)" has to be able to find out why from the report rather than by guessing.
--
-- SQLite cannot alter a CHECK constraint, so the tables are rebuilt. Both are
-- empty on every install in existence — there has never been an importer to
-- write to them — but the copy is done anyway rather than assumed, because a
-- migration that silently discards rows when its assumption is wrong is the
-- worst kind of migration to have written.

CREATE TABLE import_jobs_new (
    id          BLOB    PRIMARY KEY,
    org_id      BLOB    NOT NULL REFERENCES organisations(id),
    source      TEXT    NOT NULL,
    state       TEXT    NOT NULL CHECK (state IN (
                    'queued', 'running', 'succeeded', 'partial', 'failed')),
    dry_run     INTEGER NOT NULL DEFAULT 0 CHECK (dry_run IN (0, 1)),
    options     TEXT    CHECK (options IS NULL OR json_valid(options)),
    source_meta TEXT    CHECK (source_meta IS NULL OR json_valid(source_meta)),
    error       TEXT,
    started_at  INTEGER,
    finished_at INTEGER,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

INSERT INTO import_jobs_new (id, org_id, source, state, dry_run, options, source_meta,
                             error, started_at, finished_at, created_at, updated_at)
SELECT id, org_id, source,
       CASE state
           WHEN 'pending'   THEN 'queued'
           WHEN 'analysing' THEN 'running'
           WHEN 'importing' THEN 'running'
           WHEN 'completed' THEN 'succeeded'
           -- A cancelled job did not finish, and `failed` is the only honest
           -- state left in the contract's vocabulary for one that did not.
           WHEN 'cancelled' THEN 'failed'
           ELSE 'failed'
       END,
       0, options, source_meta, error, started_at, finished_at, created_at, updated_at
FROM import_jobs;

DROP TABLE import_jobs;
ALTER TABLE import_jobs_new RENAME TO import_jobs;

CREATE INDEX idx_import_jobs_cursor ON import_jobs (org_id, updated_at DESC, id DESC);

-- One row per source entity: the guarantee that nothing was silently dropped.
CREATE TABLE import_entries_new (
    id            BLOB    PRIMARY KEY,
    job_id        BLOB    NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
    org_id        BLOB    NOT NULL REFERENCES organisations(id),
    source_file   TEXT    NOT NULL DEFAULT '',
    source_type   TEXT    NOT NULL,
    source_id     TEXT,
    source_name   TEXT,
    outcome       TEXT    NOT NULL CHECK (outcome IN (
                      'imported', 'renamed', 'skipped', 'failed', 'unsupported')),
    reason        TEXT,
    entity_type   TEXT,
    entity_id     BLOB,
    created_at    INTEGER NOT NULL
) STRICT;

INSERT INTO import_entries_new (id, job_id, org_id, source_file, source_type, source_id,
                                source_name, outcome, reason, entity_type, entity_id, created_at)
SELECT id, job_id, org_id, '', source_type, source_id, source_name,
       -- `merged` had no equivalent and no writer; it becomes `imported`,
       -- which is what it meant.
       CASE outcome WHEN 'merged' THEN 'imported' ELSE outcome END,
       reason, entity_type, entity_id, created_at
FROM import_entries;

DROP TABLE import_entries;
ALTER TABLE import_entries_new RENAME TO import_entries;

-- Ordered by id, which is UUIDv7 and therefore insertion order: the report is
-- read in the order the user's own install had the entities, which is the order
-- that makes a list of what did not come across workable.
CREATE INDEX idx_import_entries_job ON import_entries (job_id, id);
