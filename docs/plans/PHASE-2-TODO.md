# Phase 2 — working checklist

Every deliverable in [PHASE-2-PLAN.md](PHASE-2-PLAN.md), as a list that can be
ticked. The plan is the contract and does not change; this is the tracker. The
same rule the Phase 1 list ran under applies here and is the reason that list
stayed honest: **a box needs a demonstrated run behind it.** "The code is
written" is not one, and neither is "the configuration was reviewed".

**Status: 2026-08-31. The report computes; nothing renders it.** All three ADRs
are accepted, the API surface they implied is merged into the frozen spec, and
the process for changing that spec is written down. Migration `0008` landed the
schema, `report.Store` is satisfied, and `Build` now produces a whole
`Document` — window cut in a stated zone, scope resolved at run time, uptime
with its denominator, SLA with its error budget and breach log, ADR-006's
latency figures, and `meta.resolution` labelling what actually answered. 52
tests in the package, `-race` clean.

**The Month 4 checkpoint is one handler away.** The plan asks to `curl` a
computed SLA report as JSON for a monitor over an arbitrary past month, with a
stated denominator, honest nulls, and a latency block that omits what it cannot
compute. All of that exists as a Go value; what is missing is the endpoint that
serves it, which is `POST /report-templates/{id}/generate` and therefore needs
the template CRUD underneath it.

**The percentile is wired, and it cost one spec change.** `unavailable_reason`
gained `scope_too_large` on explicit instruction (2026-08-31), classified under
[COMPATIBILITY.md](../api/COMPATIBILITY.md) §2 as **breaking-but-unshipped** —
free, because the operation is `x-cairn-phase: 2` and no release implements it,
and stated rather than assumed so a reviewer can check the claim. The enum also
gained the §3 tolerance sentence, which is the part that had to happen *now*:
adding it after clients exist does not help.

**The backup guide is written ahead of the code, deliberately.**
[ADR-008](../adr/008-report-artifact-storage.md) says in its own text that the
backup documentation changes *in the same pull request*, because choosing files
over blobs moves a directory into the operator's backup procedure and the failure
mode is silent until a client disputes a figure. The ADR merged and
[backup-restore.md](../operations/backup-restore.md) did not, so it has now been
revised (2026-08-30) — the reports directory, the snapshot-then-directory
ordering, the mirror's two constraints, the retention floor, and the restore
step. It is marked in the guide as the one section not backed by a drill, because
the directory it describes does not exist yet. **The box below stays unticked**:
the exit criterion is the procedure *tested end to end*, which is Month 7 work
and needs artifacts to test against. What is closed is the gap where an operator
could write a backup script today that would silently need changing later.

**What Phase 1 hands over is real and is listed in the plan's §2, so it is not
re-listed here.** The one operational consequence for this checklist: the rollup
tiers store a sum and a count rather than an average, the 1d tier is kept
indefinitely, and `HistoryBucket` keeps `unknown` and `skipped` apart from
`down`. Every computation box below is a query against data that already exists,
and with `0008` applied, no box in this file is blocked on a schema change at
all.

---

## Decisions and specification

- [x] [ADR-006](../adr/006-report-latency-statistics.md) — latency statistics. Exact aggregates only; the monthly percentile is not provided, not approximated, and not implied. The mergeable histogram is deferred with named triggers rather than rejected
- [x] [ADR-007](../adr/007-report-rendering.md) — rendering. A pure-Go PDF writer in-tree over the report model, one primitive set with two backends, no subprocess and no cgo. The AGENTS.md stack line that named Typst is corrected and says which ADR superseded it
- [x] [ADR-008](../adr/008-report-artifact-storage.md) — artifact storage. Files under `<data-dir>/reports/<yyyy>/<mm>/` with the database holding the index, an optional S3 mirror, and the escape hatch closed: an artifact is a record, not a cache, because retention erases the inputs it was computed from
- [x] OpenAPI extension merged (2026-08-27) — the reconciliation the plan §6 called for rather than an invention. `ReportDocument` typed, artifacts given identity and a digest, `ReportRunState.partial`, share links creatable and revocable, branding moved to `brand_profile_id`, `slo_target_percent` on monitors and groups, the expiry calendar, and the two settings blocks. [The draft](../api/drafts/phase-2-reports.yaml) is retained as the record of what changed and why — in particular the six decisions at its foot, which are recorded nowhere else
- [x] [COMPATIBILITY.md](../api/COMPATIBILITY.md) — the change process the freeze always implied and no document actually was. It is what made the merge above legal: the freeze binds shipped surface, and every operation it reshaped is `x-cairn-phase: 2` with no release behind it
- [ ] **The backup guide, revised in ADR-008's own terms** — written 2026-08-30 and marked as the one section on the page with no drill behind it: `VACUUM INTO` for the database, then the reports directory, then the key separately as before, plus the mirror's two constraints and the retention floor. **Unticked until the revised procedure is run end to end against real artifacts** (Month 7), which is what the exit criterion actually asks for
- [ ] Contract tests select `x-cairn-phase: 2` as they already select `1`, and the selection is switched on per operation as it lands rather than in one move at the end

## Schema and storage

- [x] Migration [`0008_reporting.sql`](../../migrations/sqlite/0008_reporting.sql) — **seven tables, not the four the data model named**: brand profiles, templates, schedules, schedule deliveries, runs, artifacts, share links, delivery attempts. `org_id` on every one from this first migration, so Phase 3 tenancy is a permission change and not a re-architecture. Applied over `0001`–`0007` against a real database with `integrity_check` and `foreign_key_check` clean, and each constraint exercised until it refused something: `sla_target = 100`, a second default brand profile, a second live share link on one run, a repeated `token_hash`, a second PDF for one run, `format = 'docx'`. Deleting a template cascades runs → artifacts → share links to zero. The three scheduled queries — due schedules, the expiry sweep, the share-token lookup — each plan onto their index as a `SEARCH`, with no scan
- [x] `slo_target_percent` on monitors and groups — one nullable column each, `NULL` meaning no target, with the `< 100` bound enforced by a `CHECK` rather than only at the API: 100 refused and 99.95 accepted, tested against a real monitor row and a real group row. The resolution order now reads it as far as monitor-then-group; the template's override and the API field are still unwritten
- [ ] Four things whose **column landed in `0008` and whose Go half is not written**: `report_artifact_days` inside the existing `retention` JSON (365 default, and exempt from the coarser-outlives-finer rule, because an artifact is expected to outlive the data behind it); `report_storage` as an eighth settings section with `secret_access_key_sealed` following `SMTPSettings.PasswordSealed`; the artifact row's `sha256`, `size_bytes` and `state` with `expired` as the tombstone behind a `410`; and `path`, stored rather than derived. No further migration is needed for any of them
- [x] The SQLite half of [`report.Store`](../../internal/report/report.go) — [`reports.go`](../../internal/store/sqlite/reports.go): `MonitorsInScope` unions ids, groups (reaching child groups) and tags in one predicate so a monitor selected three ways appears once, and includes paused monitors, which still have history in the window; `WindowTotals` and `DailySeries` are batched, seek per monitor by the `(monitor_id, bucket_start)` primary key, and return `NULL` for p95 at every tier including `1m`. Absent monitors stay absent rather than arriving zero-valued, because zero up and zero down is a real state meaning "observed nothing" and a report has to tell the two apart. Nine tests, `-race` clean
- [ ] **Data model §4.13 updated.** It lists four Phase 2 tables and `0008` creates seven. The three it does not name are each a consequence of a decision taken after that list was written — brand profiles (spec Q2), share links (ADR-008), and the configured-target/attempt split that mirrors `notification_channels` and `notification_deliveries`. The migration header says so; the data model does not yet

## The report model (`internal/report`)

- [x] The read-side contract, [`report.Store`](../../internal/report/report.go), named by the consumer rather than declared centrally — which is what `internal/store/store.go` asks for in as many words: the non-heartbeat interfaces are "deliberately not declared yet" because "each one's method set follows from the OpenAPI operations that use it". **There is no single repository interface in this codebase and this does not add one.** The file also records what is deliberately absent, so the gaps read as decisions: no window percentile, no window min/max, no expiries until `/api/v1/expiries` has a type to return, and nothing that writes
- [x] Computation produces a backend-independent `Document` from the repository interface — [`document.go`](../../internal/report/document.go). **Four reads regardless of scope size**, held by a test that runs 200 monitors through a counting fake: the fan-out fifty concurrent runs cannot afford is the kind that would be invisible in every result and fatal in the gate. `now` and the run id are parameters rather than read from the clock, because ADR-007 requires the same model rendered twice to be identical, and the estate series is sorted for the same reason — map iteration is randomised and would otherwise be the one place that quietly is not
- [x] Window resolution in a stated timezone — [`window.go`](../../internal/report/window.go). Day, week, month, quarter and year, calendar or rolling. A monthly report for a Sydney agency starts at 2026-02-28T13:00Z, and the test says so in both zones. Weeks start Monday (Sunday is weekday 0 in Go and the *last* day of an ISO week). `Duration` is the difference between the instants rather than 30 × 24 hours, so April in Sydney is 30 days and one hour and the error budget follows the clocks back — otherwise the budget is an hour wrong twice a year in a number people check. An unknown zone is refused by name rather than falling back to UTC
- [x] Scope resolves **at run time, not at save time** — monitor ids, groups and tags as a union, resolved in one predicate so a monitor selected three ways appears once. An empty scope produces a document rather than an error: a client whose monitors were all deleted still gets a report saying so, which beats a failed run nobody looks at until the invoice goes out
- [x] `meta.resolution` is filled from what actually answered — tier, requested tier, `downgraded`, and `covered_from` where retention truncated the window. **The figures are read over the covered window rather than the requested one**, so a truncated range cannot return the same rows under a period the data does not reach
- [x] Reads go through the rollup tiers, never raw. `ResolveTier` does not offer raw as a candidate at all, so the failure ADR-004 exists to prevent cannot be reached from a report by configuration
- [x] The denominator rules of §4.3 — [`uptime.go`](../../internal/report/uptime.go), nine tests, and the tests carry the reasoning rather than the code alone. `down` counts and nothing else does; `unknown` and `skipped` leave the denominator entirely (90 up, 10 down and 100 unmade checks is 90% over half the window — never 45% and never 95%); `pending` is a third thing again and is never counted; `maintenance` is excluded by default, and the policy is carried **on the figure** rather than only on the template, because one bucket yields 80%, 90% or 40% under the three settings. Excluding maintenance deliberately does not improve `unobserved_share`, which is taken over everything scheduled — otherwise the exclusion would flatter the quality of the observation as well as the uptime
- [x] `maintenance_handling` honoured as `exclude`, `count_as_up`, or `count_as_down`, with an empty value defaulting to `exclude` and *saying* that it did. Whichever was used appears on the figure, not only in the template
- [x] Target resolution reaches the computation, by the smaller of the two routes: [`SLOTargets`](../../internal/store/sqlite/reports.go) resolves monitor-then-group in one `COALESCE` and returns **which level answered**, so §4.3's requirement to print the source is satisfied by the same call that finds the number. `model.Monitor` gains no field, so nothing outside reporting can act on a target this phase says nothing may act on yet. Resolution deliberately stops at the monitor's own group: groups nest, the spec's order has no fourth step, and climbing would print "inherited from group" for a number set two levels up
- [x] Error budgets from `up_count` and `down_count` — [`sla.go`](../../internal/report/sla.go), eight tests. Target versus actual, budget consumed and remaining, burn rate. **No figure derives from a percentile.** Remaining goes negative rather than flooring at zero, because "41 minutes past" is the number somebody wants; a window that observed nothing consumes *nothing* rather than everything, since a probe that never looked has not spent budget; and a 100% target omits the ratio and burn rate rather than returning an infinity that would render as `+Inf` on a client's PDF. A target met exactly is met — 99.9% arrives as 8991/9000, which is not exactly 99.9 in binary, and a naive comparison turns a met SLA into a breach
- [x] Target resolution — template, then monitor, then group, then none — with the source carried onto every figure. All four levels resolve, and a monitor with no target at any of them gets **no SLA block** rather than one computed against a number nobody chose. What remains is the source reaching the rendered face, which needs a renderer

## The latency block (ADR-006)

- [x] Average over the window as `SUM(response_time_sum) / SUM(response_time_count)`, exact at any tier — [`latency.go`](../../internal/report/latency.go), nine tests. Never the mean of the daily averages: 1,000 checks at 100 ms and 10 at 1,000 ms is 108.9, not 550
- [x] Daily average series from the 1d tier — the report's primary latency exhibit. A day with no successful checks stays on the series with a null average, so the chart breaks rather than dipping to zero
- [x] Best and worst day, taken from that series, over observed days only — a day the probe could not run is not the fastest day the service ever had
- [x] Days over target, with their dates, present only when `response_time_target_ms` is set. No target yields null rather than zero: an absence of a rule and a clean sheet are different claims, and only one of them is a compliment. A day exactly at the target is not over it
- [x] A real nearest-rank p95 over the **trailing seven days only**, wired into `Build` and gated three ways. Two gates cost no query at all and are checked first: a scope over `P95MaxMonitors` (25) reports `scope_too_large`, and `raw_days` under seven reports `insufficient_raw_retention`. The third is `RawCovers` per monitor, compared against the daily tier rather than asked in the absolute — false exactly when retention pruned raw rows the tier summarised, which is the case ADR-006 gates against, while a monitor created three days ago passes correctly. The window is the last seven days **of the reported period**, not of the present moment: a March report describing April would be a figure about a month the document is not about
- [x] **No window-level minimum or maximum anywhere**, held by two tests rather than by discipline: `Sum` refuses to carry them across monitors, and the latency block has no field for them even when the input bucket does. Adding one later is a deliberate act against a failing test
- [ ] A test asserts `HistoryFromTier` still substitutes `NULL` for the coarse-tier percentile, and a second asserts the weekly p95 is absent when coverage is short. ADR-006 removes the mechanism rather than policing it, and these two tests are what keep it removed
- [ ] The response-time SLI is described as *"met the response-time target"* and never as *"was slow"*. A threshold breach already marks a check down, but `Class` is not persisted, so stored history cannot distinguish "too slow" from "did not answer"

## Rendering

- [ ] The drawing primitive set — text run, rectangle, line, path, image — and nothing else. Every primitive added is one both backends implement forever, which is why the set stays small
- [ ] SVG backend, and the HTML renderer as the canonical output
- [ ] CSV and JSON from the same model: one row per bucket per monitor plus a machine-readable summary. These are the data, not the layout, and they are what a client's BI tool consumes
- [ ] The JSON artifact is `ReportDocument` verbatim, no envelope, carrying `meta.schema_version` — a BI tool parsing a stored report years from now needs to know which shape it is holding
- [ ] PDF backend over the same primitives. It follows the SVG one deliberately: if it runs long, HTML plus print CSS ships and **PDF slips without disturbing anything else**
- [ ] One embedded TrueType family, regular and bold, subset into the file
- [ ] Deterministic output — creation date and `/ID` derived from the run rather than the clock, which is what makes re-runnable generation a property rather than an aspiration
- [ ] **The golden-report visual regression test stands up with the second renderer, not after it.** Two renderers over one model is two layouts that can drift, and the drift is invisible until a client sees a PDF that disagrees with the page they were shown
- [ ] Table page-breaking, budgeted as the known time-sink it is
- [ ] Print CSS on the HTML report as a complement, never as the substitute
- [ ] Render failure degrades the run to `partial` with the reason recorded per artifact, and the formats that succeeded are still delivered

## Branding

- [ ] Brand profiles: logo, primary and accent colours, footer text, cover text, "prepared for" client name
- [ ] Referenced by report templates only. **Status pages keep their inline branding**, and the resulting three-way duplication — profile, status page, `settings.appearance` — is documented rather than discovered
- [ ] Logo upload accepts PNG and JPEG and **refuses SVG at upload time with the reason**, not at render time. SVG is the expected case rather than the exotic one — status pages accept an arbitrary `LogoURL` and the project's own mark is `logo.svg` — so the refusal has to be legible to somebody who has one in their hand
- [ ] Branded text fields are plain text. A field that renders in HTML and not in PDF is worse than one that renders nowhere
- [ ] The default profile derives from `settings.appearance`, so an install that never opens this screen still produces a report that does not look unbranded
- [ ] Brand resolved at generation time and denormalised into `meta.brand`, so a stored document renders standalone after the profile has been edited or deleted

## Definitions, schedules, and runs

- [ ] Templates, schedules and runs kept as three things. That separation is what makes "re-send last month's", "regenerate it after we corrected the incident record" and "the PDF failed but the HTML went out" all expressible
- [ ] The five-field cron parser moves out of `internal/maintenance` into a shared package and is used by both. It is written once, with the day-of-month/day-of-week union rule already correct and tested; a second copy would agree on the day it was written
- [ ] Daily, weekly, monthly, quarterly, and cron, each at `send_at` in the schedule's own timezone
- [ ] A bounded worker pool with an explicit concurrency limit, off the check path. **Fifty PDFs at 09:00 on the 1st must not delay a single check, and the load test says so rather than the author**
- [ ] Generation is idempotent and re-runnable: a template plus a window plus a data snapshot yields the same artifact, and a re-run after a correction is a first-class action with both artifacts kept
- [ ] `late` is set and shown. If the instance was down at 09:00 on the 1st, the run is late, not lost, and the UI says which
- [ ] `partial` is a real state end to end — one format produced, another not, both halves visible. Collapsing it into `succeeded` or `failed` is how somebody concludes a delivery went out whole
- [ ] A run cancelled mid-flight leaves no half-written artifact

## Artifact storage (ADR-008)

- [ ] `<data-dir>/reports/<yyyy>/<mm>/<artifact-id>.<ext>`, directories `0750` and files `0640`, matching what the codebase already chooses elsewhere
- [ ] **The on-disk name derives from the artifact id and format, never the template title**, so a template called `../../etc` has nowhere to go
- [ ] **Write the file, fsync, then commit the row** — in that order, so a crash leaves an inert orphan rather than an artifact the UI offers and the disk cannot supply
- [ ] SHA-256 and size on every row
- [ ] The orphan sweeper, for the files the ordering above deliberately leaves behind
- [ ] Retention on `report_artifact_days`: bytes reclaimed, row kept as an `expired` tombstone, `410` on the download path
- [ ] A per-artifact size cap enforced with an error naming the limit and the size reached. The case that hits it is a CSV over 5,000 monitors for a year — roughly 1.8 million daily rows — not a PDF
- [ ] Disk-full and write failure degrade the run and record the reason rather than aborting the schedule

## The S3 client, the mirror, and the drop

- [ ] SigV4 over the standard library, justified in its PR under the dependency policy. Static credentials only — no instance profiles, no STS, no credential chain
- [ ] Selectable path-style addressing and an overridable endpoint. "S3-compatible" means nothing without them: MinIO, Garage and Ceph commonly need path-style, AWS prefers the alternative
- [ ] Server-side encryption headers passed through; the bucket is required to be non-public and the documentation says so, because a misconfigured bucket is a breach with no code defect behind it
- [ ] The **mirror** is a durability copy of every artifact and its failure is recorded rather than fatal. Local stays the source of truth and the only read path in every configuration
- [ ] The **drop** is a delivery target for one run's files. It shares a client with the mirror and nothing else, and the UI and docs keep them apart by name
- [ ] The guard that this phase makes convenient to violate: **the root key must not be written to the same bucket as a database backup.** A backup that puts the key beside the database it protects has encrypted nothing against the threat that actually happens. Remote backup of database and key is Phase 4 and is not built here

## Delivery

- [ ] Email through the configured relay, Slack through the existing provider, webhooks signed exactly as `internal/outbound` signs them, and the S3 drop. **A report is a payload, not a new transport**
- [ ] Delivery decoupled from generation: a run produces artifacts, then attempts are recorded per recipient with retry, exactly as notification deliveries are
- [ ] Every attempt recorded with its outcome, count and error, and the last error surfaced where somebody will see it — the `internal/notify` discipline, adopted rather than reinvented
- [ ] A delivery that names a notification channel uses the channel's configuration rather than restating it, so a rotated Slack token is rotated once

## Share links

- [ ] Unguessable token on an unauthenticated read path, stored twice — hashed for lookup, sealed for replay — following the subscriber unsubscribe-token precedent
- [ ] A **separate public projection**, not a filter over the private shape. A field cannot leak through a projection that has no place to put it, and this is the path where a leak reaches strangers
- [ ] Revocable, optionally expiring, `noindex`, and rate limited
- [ ] **A share link serves the stored artifact, never a re-render**, so the figures a client bookmarked do not change when retention drops a tier
- [ ] The golden-path assertion the status page taught: fetch a shared report while it is live and assert no monitor target appears anywhere in the rendered document

## Report types

- [ ] **SLA/SLO** — target versus actual, error budget consumed and remaining, burn rate, breach log with timestamps, and the denominator and maintenance policy printed on the report face
- [ ] **Uptime summary** — the default a solo user gets, with no SLO vocabulary anywhere on it until they ask for a target
- [ ] **Incident post-mortem** — drafted from the incident timeline: detection, acknowledgement, resolution, MTTD/MTTA/MTTR, affected components, alerts fired from the notification delivery log, which is retained 90 days precisely because post-mortems cite it
- [ ] MTTD is **reported as unknown when it is unknown**. `AutoOpened` is never set in Phase 1 and `DetectedAt` may be nil, so this is the common case, not the edge one. A post-mortem that invents a detection time is worse than one with a blank in it
- [ ] **Comparative** — period over period, monitor against monitor, group against group. Region against region is shaped for and absent: the data does not exist until Phase 4, and the shape is here so it does not need reshaping then
- [ ] **Certificate and domain expiry calendar** — everything expiring in the next 7/30/90 days, sortable, exportable, and schedulable as its own monthly report. The data exists; this is a report over it
- [ ] **Custom builder** — pick metrics, group by tag or group, choose a window, save as a reusable template. Deliberately a selection tool and **not a layout designer**

## REST API

Spec-first is unchanged and is not softened for this phase: the surface below is
already in the frozen spec, and no handler reshapes it. Anything that needs to
change goes through [COMPATIBILITY.md](../api/COMPATIBILITY.md) §2 first.

- [ ] `/api/v1/report-templates` — list, create, get, update, delete
- [ ] `/api/v1/report-templates/{id}/generate` — run now, with an optional window and format override
- [ ] `/api/v1/report-schedules` — list, create, get, update, delete, with `next_run_at` computed and a schedule that will never fire refused at write time rather than discovered by its silence
- [ ] `/api/v1/report-runs` and `/{id}` — cursor-paginated history with artifacts, deliveries, share state and `late`
- [ ] `/api/v1/report-runs/{id}/download` and `/artifacts/{artifactId}` — artifact-addressed download, `410` on an expired tombstone
- [ ] `/api/v1/report-runs/{id}/share` — create and revoke
- [ ] `/api/v1/public/reports/{shareToken}` and `/download` — the unauthenticated pair, on the public projection
- [ ] `/api/v1/brand-profiles` — list, create, get, update, delete, plus `/logo` upload
- [ ] `/api/v1/expiries` — the expiry calendar as a queryable collection
- [ ] `settings.retention.report_artifact_days` and `settings.report_storage` on the settings surface, with `secret_access_key_set` read back in place of the secret
- [ ] `brand_profiles:read` / `brand_profiles:write` added to the API key scopes, and enforced
- [ ] Cursor pagination and RFC 9457 problem details unchanged across the new surface
- [ ] Go and TypeScript clients regenerated in the same PR as any spec change, and never committed

## UI

- [ ] Reports list and template editor, with progressive disclosure holding: a solo user with three monitors sees one working default and nothing about SLO targets, error budgets or brand profiles until they ask
- [ ] Run history with state, `late`, per-format artifacts, and the delivery log with its last error on the run itself
- [ ] Preview and download, and the share link with its revoke
- [ ] Brand profile screen, with the SVG refusal explained where somebody is holding an SVG
- [ ] The custom builder — metrics, grouping, window, saved as a template
- [ ] Expiry calendar screen
- [ ] **Full historical browsing with drilldown into arbitrary past ranges, always labelled with the resolution actually used.** This is where the Phase 1 complaint about being week-blinkered is answered in the UI, and the labelling is the part that makes it honest rather than the drilldown

## Docs

- [ ] Reporting guide
- [ ] **SLA methodology page** stating exactly what counts as downtime, what leaves the denominator, and what the maintenance default is. This is the page that gets read when a figure is disputed
- [ ] Brand profile setup, including why a logo must be raster
- [ ] S3 configuration, with the drop and the mirror separated and the non-public bucket requirement stated
- [ ] The retention-versus-resolution table, so an operator can predict what a report over last March will contain
- [ ] **The revised backup procedure**, tested end to end rather than written — the reports directory is now part of it

## Quality gates and scale

- [ ] Contract tests green for `x-cairn-phase: 2`, in both directions: every Phase 2 operation has a handler, and nothing is served that the spec does not describe
- [ ] Golden-report visual regression, standing up with the second renderer
- [ ] Determinism test: the same model rendered twice is byte-identical
- [ ] Denominator tests over maintenance, unknown and skipped, written before the code they check
- [ ] **The 5,000-monitor gate extended: 50 concurrent report runs with check scheduling latency unchanged.** A regression blocks merge, not release. This is CI configuration and needs reviewing as such (AGENTS.md rule 7)
- [ ] The failure paths, each demonstrated rather than reasoned about — renderer failure falls back with the reason recorded, delivery failure retries and surfaces, a missed schedule is late and visible, a cancelled run leaves nothing half-written, and a full disk degrades one run rather than the schedule
- [ ] Crash-consistency: kill between fsync and commit, assert an orphan and not a dangling row, and assert the sweeper reclaims it

## Release

- [ ] Tagged release, images, checksums, SBOM, changelog, announcement — the automation exists and has now run, so this is a repeat rather than a first
- [ ] Buffer. **Features get cut before the release date or the quality gates do**

---

## What to do next, and why in this order

1. ~~**Revise the backup guide.**~~ Done 2026-08-30, ahead of the code and marked
   as such. What remains is the drill, and the drill needs artifacts — so this
   returns at Month 7 rather than blocking anything now. Two things fell out of
   writing it that were not in this list: the `schema_migrations` sample output
   on that page had gone stale against migrations `0006` and `0007`, and the
   snapshot ordering matters — **database first, then the reports directory**,
   because the write-fsync-then-commit rule means the reverse order can produce a
   row whose bytes are not in the backup.

2. ~~**Write migration `0008` and the repository interface.**~~ Both landed
   2026-08-30, and the interface turned out to be the smaller half: three of its
   six methods already existed on `*sqlite.Store`. **Four schema calls are open
   for review rather than settled**, each argued at its site in the migration —
   the brand logo stored in the database rather than on disk (an artifact's three
   objections do not apply to a capped image, and it keeps branding inside the
   backup procedure); `report_artifacts.path` stored rather than recomputed;
   schedule deliveries as a child table rather than JSON, because a sealed key
   needs a row to bind its AAD to; and `slo_target_percent` riding in `0008`
   rather than taking a number of its own. Any of the four is cheap to reverse
   now and expensive after the first tagged release, per data model §8.

3. ~~**`internal/report` computing the SLA and uptime model.**~~ Done, with the
   denominator tests written first as the plan asks. Two decisions were taken
   rather than referred, both because the question had stood open across
   turns and the phase could not proceed without an answer; both are isolated
   so that reversing either is a small edit.

   **The error-budget conversion.** Consumed seconds are the observed down
   proportion projected onto the window — exact when observation is complete,
   overstating in proportion to `unobserved_share`, which is printed beside it.
   The alternative, down checks times the interval, is wrong exactly when an
   interval changed mid-window or checks were missed, which is when figures get
   disputed. It lives in `ComputeSLA` alone.

   **The breach log reads the 1d tier.** Raw would give real minutes and is
   bounded by `raw_days` — seven by default — so a breach log built on it would
   be empty for exactly the completed months an SLA report covers. Incidents
   carry real timestamps and are human-declared, so the log would omit most
   outages. The daily tier is kept indefinitely and always answers; the
   boundaries are days, the duration inside them is the projected downtime
   rather than the span, and `meta.resolution` already states the tier that
   produced them. A day containing four minutes of downtime is a four-minute
   breach, not a 24-hour one.

4. **Then the primitive set and the SVG backend**, because HTML is canonical and
   because standing the golden test up against the first renderer is what makes
   the second one cheap. PDF follows. If it runs long it slips, and the phase
   still ships — that is the plan's own ordering and it is the reason for it.

5. **Scheduling, artifacts and delivery last, in that order**, because each is
   only testable against something that renders.

### Two things to decide before they decide themselves

**[ADR-009](../adr/009-pagination-sort-key.md) is Proposed and is not in the
Phase 2 plan.** It generalises the cursor from `(updated_at, id)` to
`(sort_field, id)` so a dashboard listing 5,000 monitors can be sorted
alphabetically — a real gap, argued well, and orthogonal to reporting. It carries
a migration on the busiest table in the schema and a collation decision that is a
silent performance cliff if it is got wrong. The plan's scope table is the
contract and does not include it, so it needs an explicit call: accept it into
this phase with the cost stated, or leave it Proposed until Phase 3. Drifting
into it because the monitors table is open for `slo_target_percent` anyway is the
one way to get it by accident.

**A report over 5,000 monitors produces 5,000 sections** and is bounded only by
`max_artifact_bytes`, which refuses after the work is done rather than before —
recorded as Q6 in the spec draft and deferred knowingly. The load gate extension
in this file is the thing most likely to surface it, and if it does, that is the
real install the deferral said to wait for.
