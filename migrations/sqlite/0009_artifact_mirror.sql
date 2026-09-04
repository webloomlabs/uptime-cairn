-- 0009_artifact_mirror — the offsite copy's state, per artifact.
--
-- Required by the frozen surface rather than invented here: ReportArtifact in
-- docs/api/openapi.yaml carries a `mirror` object with `state`, `uploaded_at`
-- and `error`, and 0008 created report_artifacts without anywhere to put it. A
-- separate migration rather than an edit to 0008 because 0008 has shipped —
-- schema_migrations records its checksum, and changing a file that installs have
-- already applied is how a migration runner starts refusing to start.
--
-- ADR-008 item 9 is the whole design in one sentence: **the mirror is a
-- durability copy and never a read path.** So these columns describe the copy and
-- nothing reads them to find bytes. A failed upload leaves the artifact
-- perfectly readable from local disk, which is why mirror_state is beside
-- `state` rather than inside it — folding a mirror failure into the artifact's
-- own state would make an offsite problem look like a rendering problem, and
-- would take a downloadable report out of the UI over a bucket that was briefly
-- unreachable.
--
-- 'pending' is the state an artifact is created in when a mirror is configured,
-- and it is deliberately not the default for the column: an install with no
-- mirror has nothing pending, and a column that said otherwise would put every
-- artifact on every install into a queue that does not exist. NULL means "no
-- mirror was configured when this was written", which is a different fact from
-- "an upload has not happened yet" and is the one the API renders as `null`.

ALTER TABLE report_artifacts ADD COLUMN mirror_state TEXT
    CHECK (mirror_state IS NULL OR mirror_state IN ('pending', 'uploaded', 'failed'));

ALTER TABLE report_artifacts ADD COLUMN mirror_uploaded_at INTEGER;

-- The provider's own message, kept rather than reduced to a boolean: 'NoSuchBucket'
-- and 'SignatureDoesNotMatch' send an operator to two different screens, and a
-- mirror that says only "failed" sends them to neither.
ALTER TABLE report_artifacts ADD COLUMN mirror_error TEXT;

-- No index, and no reconciliation pass to need one.
--
-- ADR-008's Consequences say it plainly: "The mirror can drift. An upload that
-- fails and is retried later, or a bucket someone empties, leaves local and
-- remote disagreeing. The artifact row records mirror state; nothing reconciles
-- it automatically in Phase 2."
--
-- So these columns are a record, read on the artifact and shown to an operator,
-- and the retry that would justify a partial index on them is a later phase's
-- work. An index built for a query nobody makes is a write cost on every
-- artifact in exchange for nothing.
