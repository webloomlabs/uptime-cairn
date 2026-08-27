# ADR-008: Report Artifact Storage — Local Files as the Source of Truth, an Optional S3 Mirror, and Share Links

- **Status:** Accepted
- **Date:** 2026-08-27
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** Independent. Consumes what [ADR-007](007-report-rendering.md) produces and stores what [ADR-006](006-report-latency-statistics.md) computes; supersedes neither.

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

**Recorded for the repository's own history:** [AGENTS.md](../../AGENTS.md) §3 places
ADRs with a human and describes that restriction as absolute. It was explicitly
waived for this document by the project maintainer on 2026-08-27, on the same
terms as [ADR-006](006-report-latency-statistics.md) and
[ADR-007](007-report-rendering.md), following the precedent at
[data model §11.6](../data-model/README.md#116-secrets-at-rest). The decision below
is the maintainer's; the drafting is not.

## Context

Phase 2 generates files. Nothing in this product has ever stored one — the
database and the root key are the entire on-disk footprint, and
[README.md](../../README.md) makes a promise out of that: back up `cairn.db` and
`cairn.key`, and you have backed up the install.

A report artifact needs a lifecycle — generated, delivered, retained, expired —
and a public share link that is unguessable, revocable, and optionally expiring.
The question is where the bytes live and what happens to them.

### The question that decides it: cache or record?

The Phase 2 plan offered an escape hatch — artifacts could be "explicitly
documented as reproducible-on-demand" and left out of the backup story. **That
escape hatch is not available, because the claim behind it is false.** Three
reasons, each concrete:

- **Retention erases the inputs.** [`DefaultRetention`](../../internal/rollup/rollup.go)
  keeps the 1h tier for a year. A report over last March, regenerated in 2028,
  reads whatever tier still exists and returns daily figures where it once
  returned hourly ones. The document does not reproduce.
- **The narrative changes underneath it.** An incident whose timeline or impact
  was corrected after the fact yields a different post-mortem on regeneration.
- **It is what was sent to a client.** An agency made an uptime claim in a
  document. If that claim is ever disputed, the artifact is the evidence, and
  evidence that regenerates differently is not evidence.

**An artifact is a record, not a cache.** Everything below follows from that: it
must be durable, it must be inside a backup story, and it must be verifiable
after the fact.

### What the build allows

- **`/data` is the only writable path in the container.** Stated in
  [backup-restore.md](../operations/backup-restore.md) while explaining why the
  online backup lands there first. The process runs as `USER 10001` with `/data`
  as the volume.
- **The backup promise is two files, deliberately stored apart.** The same
  document instructs storing them in different places, "because a backup that puts
  the key beside the database it protects has encrypted nothing against the threat
  that actually happens, which is somebody walking off with the backup."
- **`cp -r` of the data directory is already documented as wrong**, because the
  three SQLite files "are read at three different instants and need not agree."
- **`VACUUM INTO` is proportional to data, not uptime.** Anything added to
  `cairn.db` enlarges and slows every backup from then on.
- **SQLite has a single writer.** At 5,000 monitors the heartbeat ingest is the
  busiest writer in the system, and fifty artifacts written at 09:00 on the first
  of the month would contend with it.

## Decision

**Artifacts are files under the data directory. The database holds the index, not
the bytes. An S3-compatible bucket may additionally be configured as an offsite
mirror; local storage remains the source of truth in every configuration.**

### Local storage

1. **Layout.** `<data-dir>/reports/<yyyy>/<mm>/<artifact-id>.<ext>`, dated by run.
   Sharding by year and month keeps any one directory small on an install
   producing thousands of artifacts a year.

2. **The on-disk name derives from the artifact id and the format, never from the
   report title.** Report names are free text; a definition titled `../../etc` must
   not be able to reach outside the reports directory. There is no sanitisation
   step to get wrong because there is no user input on the path.

3. **Permissions match what the codebase already chooses:** directories `0750`,
   files `0600`, consistent with `os.MkdirAll(cfg.DataDir, 0o750)` in
   [`internal/app`](../../internal/app) and the `0600` the root key is written with.

4. **Write the file, fsync, then commit the row.** In that order, and it is
   load-bearing. A crash between the two leaves an **orphan file**, which is inert
   and reclaimable by a sweeper; the reverse order would leave a **dangling row**,
   which is an artifact the UI offers and the disk cannot supply. A periodic
   sweeper reclaims orphans, in the same manner as the existing retention passes.

5. **Every artifact row carries a SHA-256 of its bytes and its size.** Given that
   an artifact is evidence, "is this the document we sent?" has to be answerable,
   and truncation from a full disk has to be detectable rather than silent.

6. **Retention: `ReportArtifactDays` joins
   [`RetentionSettings`](../../internal/model/settings.go)**, defaulting to 365,
   with zero meaning indefinitely — the convention that section already uses. It
   is deliberately independent of the rollup tiers: an artifact is expected to
   **outlive the data it was computed from**, which is the whole point of keeping
   it. `Retention.Validate`'s coarser-outlives-finer rule does not apply, because
   an artifact is not a tier.

   **Retention reclaims the bytes and keeps the row.** An expired artifact remains
   listed, with its digest and size intact, so a bookmarked share link answers
   `410 Gone` rather than `404` — "this existed and is gone" and "no such thing" are
   different answers to somebody holding a link, and only one of them is true. The
   tombstone is a row, so the cost is bounded by artifact count rather than by bytes.
   This was left open in the first draft of this ADR and settled on 2026-08-27.

7. **A per-artifact size cap, enforced with a clear error.** The case that hits it
   is not the PDF: a CSV over 5,000 monitors for a year is roughly 1.8 million
   daily rows, on the order of a hundred megabytes. A cap that reports what was
   exceeded and by how much is the difference between a refused report and a
   filled disk.

8. **Disk-full and write failure follow the ADR-007 discipline** — the run degrades,
   the reason is recorded against it, what succeeded is delivered, and the failure
   surfaces where an operator sees it.

### The optional S3 mirror

9. **S3 is a mirror, not a replacement.** Local remains the source of truth and the
   only read path. The mirror is an offsite durability copy, uploaded after the
   local write succeeds, with its own success or failure recorded on the artifact
   row. **One read path is the property being protected**; pruning local files after
   upload would introduce a second and is explicitly deferred.

10. **The client is written against the standard library** — SigV4 with
    `crypto/hmac`, `crypto/sha256` and `net/http` — rather than a vendor SDK. This
    is the trade [AGENTS.md](../../AGENTS.md) §5 asks for by name: a few hundred
    lines of our own code against a dependency tree measured in hundreds of
    packages. Three compatibility details are part of the decision rather than
    discoveries: **path-style addressing must be selectable**, because MinIO,
    Garage and Ceph commonly need it while AWS prefers virtual-host style; **a
    region is required for the signature** even where the provider ignores it; and
    **the endpoint is overridable**, which is what makes "S3-compatible" mean
    anything.

11. **Static credentials only.** No instance profiles, no STS, no credential
    refresh — those are AWS-specific paths with their own failure modes, and they
    can be added later if asked for.

12. **The secret key is sealed, following the SMTP precedent exactly.** A new
    settings section carries `SecretKeySealed []byte` alongside a
    `SecretKey string \`json:"-"\`` for the in-memory plaintext, mirroring
    [`SMTPSettings`](../../internal/model/settings.go) — encrypted rather than
    hashed because it is replayed on every request, inside the section's JSON so
    that adding it needs no migration, and with the `-` tag that keeps the write
    path one careless struct literal away from nothing.

13. **The bucket must not be public, and server-side encryption headers are passed
    through when configured.** Client-side encryption of artifacts is not
    implemented; see item 16.

### Share links

14. **The token is stored twice, following
    [`Subscriber`](../../internal/model/statuspage.go)'s unsubscribe token.** That
    comment states the rule this case also satisfies twice over: *hash what you
    verify, encrypt what you replay*. A share token is verified when the client
    follows the link and replayed when the operator returns to copy it — so a hash
    carries the unique lookup index and a sealed envelope carries the value.
    Tokens are generated from `crypto/rand` with at least 128 bits of entropy, are
    revocable, and may carry an expiry.

15. **A share link serves the stored artifact, never a re-render.** This follows
    directly from the cache-or-record finding: a client who bookmarks a link must
    not find the figures changed underneath them when retention drops a tier. It is
    also what makes the public path cheap — an artifact is immutable, so it caches
    hard. The path is unauthenticated and therefore follows the status-page
    discipline in [`internal/model/statuspage.go`](../../internal/model/statuspage.go):
    a **separate public projection**, not a filter over the private shape. It
    carries `X-Robots-Tag: noindex` and is rate limited, because a share link
    publishes a client's uptime data to anyone holding the URL.

### What is not encrypted, and why

16. **Artifacts are not encrypted at rest.** [`internal/secrets`](../../internal/secrets)
    encrypts credentials; monitor names, uptime figures and incident narratives are
    already plaintext in `cairn.db`, so encrypting the report that renders them
    would be inconsistent without being protective. Stated explicitly so that it is
    a decision on the record rather than an omission somebody later reads as one.

### The backup documentation changes in the same pull request

17. This is the price of storing files, and it is paid openly. Backup becomes
    `VACUUM INTO` for the database, a copy or sync of `<data-dir>/reports/`, and the
    key separately as before. Where the S3 mirror is enabled, the reports directory
    is already offsite and the local step may be skipped — which is a genuine
    reason to enable it beyond durability.

18. **The root key must not be written to the same bucket as a database backup.**
    Recorded here because this ADR introduces the bucket that makes the mistake
    convenient. The existing guidance is unambiguous — a backup that puts the key
    beside the database it protects has encrypted nothing against the threat that
    actually happens. Artifacts and a database backup may share a bucket; the key
    requires a different trust boundary. Remote backup of the database and key is a
    **Phase 4** roadmap item ("Backup/restore and disaster recovery") and is not in
    scope here; this decision makes it cheap to build when its phase arrives,
    subject to that constraint.

## Consequences

**What this makes easy.**

No contention with the heartbeat writer, no growth in `cairn.db`, and no change to
what `VACUUM INTO` costs. The database keeps doing what it is good at — a small
indexed row per artifact — and the filesystem does what it is good at.

Large artifacts stop being a database problem. A hundred-megabyte CSV is a file,
streamable on read and write; as a blob it would have been read whole into memory,
because `database/sql` exposes no incremental blob interface.

The S3 mirror is genuinely additive. It changes no read path, so a bug in it
cannot make an artifact unreadable, and its failure is recorded rather than fatal.
The same client and configuration make the Phase 4 remote-backup work small.

Share links inherit an existing, reviewed pattern rather than inventing one, and
the immutability of an artifact makes the public path cacheable in a way the
status page's live data can never be.

**What this makes hard, or forecloses.**

*The backup story is now two commands, and an operator can get half of it right.*
This is the real cost of choosing files over blobs, and the failure mode is silent:
you discover the reports directory was never in the backup on the day a client
disputes an SLA figure. Mitigations are documentation, the S3 mirror, and a
startup or settings-page warning where artifacts exist and no mirror is configured
— but none of them makes the risk zero, and it should not be described as though
they do.

*Two stores must be kept consistent.* The write ordering in item 4 makes the
failure direction safe rather than absent, and the orphan sweeper is a moving part
that did not exist before. A restore of the database against a stale reports
directory yields rows whose files are missing; the artifact list must render that
as a missing file rather than an error page.

*Backup of a live reports directory has its own consistency question*, milder than
the database's: a file being written during the copy may be caught partial. The
write-then-commit ordering means such a file has no row yet, so it is an orphan on
restore rather than a corrupt artifact — but the documentation should say so
rather than leave it to be worked out.

*The mirror can drift.* An upload that fails and is retried later, or a bucket
someone empties, leaves local and remote disagreeing. The artifact row records
mirror state; nothing reconciles it automatically in Phase 2.

*Object-storage misconfiguration is a new exposure class.* A public bucket
containing client reports is a data breach with no code defect behind it. Item 13
and the documentation are the whole of the defence, which is worth being honest
about.

*Artifacts on disk are readable by anyone with the volume*, exactly as the database
is. Item 16 accepts this as consistent rather than incidental.

**What becomes expensive to reverse later.**

The path layout in item 1 is effectively permanent once installs hold artifacts:
changing it means a migration that moves files, which is a migration that can fail
halfway with no transaction around it. It is worth getting the sharding right at
the first release rather than the second.

Moving to blobs later would be a genuine data migration; moving from blobs to files
would have been the same in reverse. This is the one part of the decision that is
costly to revisit, and it is why the cache-or-record question was settled first.

Making S3 the primary rather than a mirror later is additive but introduces the
second read path item 9 exists to avoid — it should be a superseding ADR, not a
setting that quietly appears.

## Alternatives considered

**Blobs in `cairn.db`.** The strongest alternative, and the one with the best
durability story: the backup promise stays two files and one command, a run and its
artifacts commit atomically, and there is no orphan sweeper because there are no
orphans. It lost on three specifics. Every backup grows and slows in proportion,
against a `VACUUM INTO` path the documentation already describes as proportional to
data. Fifty artifact writes in the 09:00 burst contend with heartbeat ingest on
SQLite's single writer, at the exact scale the project's headline claim is measured
at. And `database/sql` offers no incremental blob access, so every artifact is read
and written whole in memory — tolerable at a few hundred kilobytes, not at the
hundred-megabyte CSV item 7 anticipates. The decisive consideration against it was
that its advantage is a documentation convenience, while its disadvantages are
measured in contention on the busiest writer in the system.

**Files, declared reproducible, excluded from backups.** The original escape hatch.
It lost outright: the reproducibility claim is false for the three reasons in
*Context*, and a durability story resting on a false premise is worse than none,
because it is believed.

**Object storage as the primary, with no local copy.** It lost on the principle
that solo mode keeps zero required external dependencies — a solo user would need
MinIO running before they could generate a PDF. As an optional primary it means two
read paths, a backup story that varies by install, and an artifact whose
readability depends on a network.

**S3 as a mirror with local pruning after upload.** Attractive for disk reclamation
and rejected for Phase 2 because it reintroduces the second read path: every read
must then ask where the bytes are, and an artifact becomes unreadable when the
bucket is unreachable. Artifact retention already reclaims disk. Worth revisiting
if a real install reports disk pressure that retention does not solve.

**A vendor S3 SDK.** Rejected on the dependency policy. SigV4 for `PUT`, `GET`,
`HEAD` and `DELETE` is a canonical request, a string to sign, a four-step HMAC key
derivation and one header — a few hundred lines against a dependency tree that
would dwarf the rest of `go.mod` combined, for a client that touches four verbs.

**A separate SQLite database for artifacts.** Considered as a way to keep blob
convenience while escaping writer contention. It lost because it delivers the worst
of both: a third file to back up, exactly as files-on-disk require, *and* the memory
and backup-size costs of blobs, *and* cross-database writes that are not atomic in
the way the single-database argument depended on.

**Re-rendering on every share-link view instead of serving the artifact.** It would
have avoided storing anything at all. It lost on the cache-or-record finding — the
figures would change under a bookmarked link as retention advances — and on cost,
since a public URL that triggers a full report computation is a denial-of-service
primitive pointed at the instance.

## Compliance with the product principles

- [x] **Sixty seconds to first monitor is preserved.** Artifact storage needs no
      configuration; the directory is created on demand under the existing data
      directory. S3 is opt-in and absent by default.
- [x] **Nothing is paywalled in the open source build.** Artifacts, share links and
      the S3 mirror all ship to every user.
- [x] **API-first.** Artifacts, share links and their revocation are API resources;
      the dashboard uses the same endpoints. The additions belong in the spec PR that
      precedes implementation, per the Phase 0 freeze rule.
- [x] **Progressive disclosure.** A solo user downloads a PDF and never encounters
      buckets, mirrors, checksums or retention. Each is a surface that appears when
      asked for.
- [x] **The client is never sent full state; the UI stays fast at 5,000 monitors.**
      Artifact listings paginate like every other listing, and the bytes are streamed
      rather than embedded in a JSON response.
- [x] **Solo mode keeps zero required external dependencies.** The reason S3 is a
      mirror rather than a primary.
- [x] **Dependency surface stays minimal.** Nothing added to `go.mod`; the S3 client
      is standard library.

## References

- [ADR-006](006-report-latency-statistics.md), [ADR-007](007-report-rendering.md) —
  what is computed and what renders it; and the precedent for the waiver recorded
  above.
- [ADR-002](002-storage-engine.md) — solo mode as one binary with no external
  services, which is why S3 is optional.
- [docs/operations/backup-restore.md](../operations/backup-restore.md) — the
  two-file promise, the `VACUUM INTO` path and its cost, the `cp -r` hazard, `/data`
  as the only writable path, and the key-beside-the-database warning item 18
  restates.
- [`internal/model/settings.go`](../../internal/model/settings.go) —
  `RetentionSettings` for item 6, and `SMTPSettings.PasswordSealed` as the precedent
  item 12 follows exactly.
- [`internal/model/statuspage.go`](../../internal/model/statuspage.go) — the
  unsubscribe token stored twice, and the public-projection discipline items 14 and
  15 adopt.
- [`internal/secrets`](../../internal/secrets) — "hash what you verify, encrypt what
  you replay", the rule item 14 applies.
- [`internal/rollup/rollup.go`](../../internal/rollup/rollup.go) — retention
  defaults, and the reason an artifact outlives the data behind it.
- [AGENTS.md](../../AGENTS.md) §5 — the dependency trade item 10 invokes.
- [ROADMAP.md](../../ROADMAP.md) — "Backup/restore and disaster recovery" as Phase 4,
  the scope boundary item 18 holds.
- [PHASE-2-PLAN.md §3.4, §4.7](../plans/PHASE-2-PLAN.md) — the problem statement this
  ADR closes.
- Open follow-up: Phase 4 remote backup of the database and key, reusing this
  client, subject to the trust-boundary constraint in item 18.
