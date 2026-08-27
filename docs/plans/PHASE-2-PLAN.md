# Uptime Cairn — Phase 2 Plan: Reporting

**Duration:** Months 4–7 (immediately after Phase 1)
**Source:** [ROADMAP.md](../../ROADMAP.md) § Phase 2; Uptime-Cairn-Plan.pdf §4 (Differentiator), §6 (Roadmap)
**Mandate:** Reporting is a **first-class subsystem, not a graphs page.** It is the most under-served need in the category and the feature most likely to make somebody switch, because it is the thing they currently produce by hand in a spreadsheet on the first of every month.

**Exit condition:** An agency can send 50 branded client reports on the 1st of every month without touching anything.

Shipped **before** the enterprise controls in Phase 3, deliberately. The rule that governs the roadmap still governs this plan: **cut features, never phases.**

---

## 1. What Phase 2 Ships (scope, in one view)

| Area | In Phase 2 | Deferred |
|---|---|---|
| Report engine | Definitions, scheduled runs, generated artifacts, delivery, retry, delivery log | Report authoring by non-owners (Phase 3 RBAC) |
| Schedules | Daily, weekly, monthly, quarterly; instance timezone; ad-hoc "run now" | Per-recipient timezones, business-calendar schedules |
| Formats | PDF, HTML, CSV, JSON, public shareable link | DOCX, XLSX, scheduled screenshots |
| Delivery | Email, Slack, webhook, S3-compatible object drop | SFTP, Google Drive, per-recipient digests |
| Artifacts | Files under the data directory, indexed in SQLite; optional S3 mirror; retention, checksums, size cap | S3 as primary store, local pruning after upload, remote DB/key backup (Phase 4) |
| Branding | Brand profiles: logo, colours, footer, report cover — shared with status pages | Per-client branding bound to a tenant (Phase 3) |
| Report types | SLA/SLO, uptime summary, incident post-mortem, comparative, certificate & domain expiry calendar | Cost/capacity reporting, anomaly narratives (Phase 5) |
| Builder | Pick metrics, group by tag/group, choose a window, save as a reusable template | Drag-and-drop layout design, custom chart types |
| History | Full historical browsing with drilldown into arbitrary past ranges | Sub-daily history beyond the retention tiers |
| Scale | Report generation runs inside the existing 5,000-monitor gate without regressing it | Distributed report workers (Phase 4) |

Progressive disclosure still holds. A solo user with three monitors sees "Reports" with one working default and nothing about SLO targets, error budgets, or brand profiles until they ask for them.

---

## 2. What Phase 1 Already Gives Us

Phase 2 is unusually cheap for what it delivers, because the load-bearing pieces were built in Phase 1 and were built with this phase in mind. Naming them precisely matters, because the temptation in a reporting phase is to build a second copy of each.

- **The rollup pipeline** ([`internal/rollup/rollup.go`](../../internal/rollup/rollup.go)) — raw → 1m → 5m → 1h → 1d, with each tier computed from the tier below. Crucially, the tiers store **a sum and a count rather than an average**, precisely so a coarse bucket can be re-weighted into an arbitrary reporting window. A report over "March" is an aggregation the storage layer was designed to answer.
- **Retention already favours reporting.** `DefaultRetention()` keeps the 1d tier **indefinitely** — the comment in that function says in as many words that this is the long history the reporting engine sells. Daily uptime for a client signed three years ago exists on disk today.
- **`store.HistoryBucket` distinguishes gaps from outages.** `Unknown` and `Skipped` are carried separately from `Down`, and `Maintenance` separately again. A report's denominator is therefore a *choice* the report makes explicitly, not an accident of a query. [`internal/status/doc.go`](../../internal/status/doc.go) already names rendering a probe failure as customer downtime "a lie that Phase 2's SLA reports would inherit". Do not inherit it.
- **Incidents already carry the MTT\* timestamps.** [`internal/model/incident.go`](../../internal/model/incident.go) has `DetectedAt`, `AcknowledgedAt`, `ResolvedAt`, `StartedAt` and `AutoOpened`, with a comment explaining that time-to-detect is the one figure that cannot be reconstructed after the fact. Post-mortem reports read these; they do not invent them.
- **A hand-written five-field cron parser** ([`internal/maintenance/cron.go`](../../internal/maintenance/cron.go)) with the day-of-month/day-of-week union rule already correct and tested. Report schedules use it. It moves to a shared package; it is not written twice.
- **A delivery discipline that already works.** [`internal/notify`](../../internal/notify) records every attempt, retries, and writes `last_error` where the UI shows it. [`internal/outbound`](../../internal/outbound) signs payloads, dedupes on a stable event id, and disables endpoints that fail repeatedly. Report delivery adopts both patterns rather than inventing a third.
- **Branding fields already exist** on status pages (`Theme`, `LogoURL`, `PrimaryColor`, `FooterText` in [`internal/model/statuspage.go`](../../internal/model/statuspage.go)) and, separately, in appearance settings. Phase 2 makes that a **brand profile** referenced by both, rather than a third copy.
- **`OrgID` is on every model already.** Reports carry it from the first migration, so Phase 3 tenancy is a permission change and not a re-architecture.

---

## 3. The Problems to Settle Before Writing Report Code

Four problems, each expensive to reverse. Three are settled by ADR; the fourth is a constraint to be honest about rather than a choice. Nothing in §4 is now blocked on a decision.

ADRs are discussed in their own PR before implementation. [AGENTS.md](../../AGENTS.md) § *What agents may not do* (3) places them with a human; where that restriction is waived for a particular document, the waiver is recorded in the document itself, following the precedent at [data model §11.6](../data-model/README.md#116-secrets-at-rest).

### 3.1 Latency percentiles — settled by ADR-006

**Settled. See [ADR-006](../adr/006-report-latency-statistics.md).** Recorded here because the constraint governs §4.3 and because the reasoning is worth carrying in the plan rather than only in the decision record.

The problem was narrower and worse than it first appears: there is **no true percentile over any window longer than a single one-minute bucket**, except inside raw retention. [`RollUpRaw`](../../internal/store/sqlite/rollup.go) computes a real per-minute p95 from raw; [`RollUpTier`](../../internal/store/sqlite/rollup.go) stores `MAX(response_time_p95)` above that, and [`HistoryFromTier`](../../internal/store/sqlite/history.go) substitutes `NULL` for it on read rather than serve an unlabelled approximation. A quantile is a rank statistic and does not merge — the stored maximum is a provable upper bound but an arbitrarily loose one, capable of overstating by a hundredfold.

ADR-006 decides that **Phase 2 reports use only statistics that are exact over an arbitrary window**, and that the monthly percentile is not provided, not approximated, and not implied. The five figures are specified in §4.3. The mergeable histogram is deferred rather than rejected, with named triggers to reopen it; the cost of that deferral — history accumulated before the histogram ships never acquires percentiles retroactively — is recorded in the ADR as the strongest argument the other way.

Two consequences bind the rest of this plan. **Error budgets are computed from uptime counts, never from percentiles**, which is the direct answer to the instruction [data model §11.5](../data-model/README.md#115-percentile-strategy) left for this phase. And **no ADR-006 work sits on the critical path**: nothing in §4.3 requires a schema change, a migration, or a backfill.

### 3.2 Resolution, not existence, is what limits historical drilldown

The roadmap promises "retention limited only by disk, with real drilldown into arbitrary past ranges". The default policy delivers that for daily figures and only that: 1m for 30 days, 5m for 90, 1h for a year, 1d forever. This is the right default and the report UI must be honest about it — a request for minute resolution over last March returns hourly data **labelled as hourly**, never silently upsampled. The API already resolves a tier from the requested span (`resolveHistoryTier` in [`internal/api/history.go`](../../internal/api/history.go)); reporting extends that contract rather than bypassing it.

### 3.3 Report rendering — settled by ADR-007

**Settled. See [ADR-007](../adr/007-report-rendering.md).** The stack note in AGENTS.md named Typst; that line was a Phase 0 expectation with no ADR behind it, and ADR-007 supersedes it.

The question was originally posed as "how do we render HTML to PDF", and that framing is what made it expensive — HTML needs a browser engine because HTML is an arbitrary layout language. A report is not: it is a cover, headings, key–value blocks, a table, a chart, and a footer. **The renderer consumes the report model, not another renderer's output**, and the browser question does not arise.

Four constraints eliminated most of the option space before rendering quality entered it. `CGO_ENABLED=0` is set in both the [Dockerfile](../../Dockerfile) and [`release.yml`](../../.github/workflows/release.yml) across five targets, and darwin cross-compiles from an ubuntu runner only because of it — so a cgo-linked renderer breaks the release matrix structurally rather than expensively. The binary distribution is a **single-file tarball** with no mechanism to ship a second executable, which disqualifies every subprocess design; it would deliver PDF to Docker users and withhold it from binary and Raspberry Pi installs. Image size has already been argued at eight-megabyte granularity in the Dockerfile, against a browser engine at 300 MB and up. And an AGPL-or-commercial PDF library would reimport the licensing position the project just deliberately left.

The decision: **a pure-Go PDF writer, in-tree, over the report model.** One drawing primitive set with two backends — SVG for HTML, PDF operators for PDF — so charts are written once. One embedded TrueType face. Deterministic output, with the creation date and `/ID` derived from the run rather than the clock, which is what makes §5's re-runnable generation a property rather than an aspiration. Non-Latin scripts are out of scope for Phase 2 and are not foreclosed: the text primitive takes a shaped run, so a shaping layer inserts later without changing callers.

Three consequences bind work later in this plan, and are the likeliest sources of a month-6 surprise. **SVG logos are the expected case, not the exotic one** — status pages accept an arbitrary `LogoURL` and the project's own mark is `web/static/logo.svg` — so brand profiles (§4.6) either accept PNG and JPEG only, stated at upload time, or the writer needs a minimal SVG-path translator. **Branded text fields are plain text**, since a field that renders in HTML and not in PDF is worse than one that renders nowhere. And **a visual regression test over a golden report is required, not optional**: two renderers over one model is two layouts that can drift.

### 3.4 Artifact storage and share links — settled by ADR-008

**Settled. See [ADR-008](../adr/008-report-artifact-storage.md).**

The plan originally offered an escape hatch: artifacts could be documented as reproducible-on-demand and left out of the backup story. **That hatch is closed, because the claim behind it is false.** Retention erases the inputs — a report over last March regenerated in 2028 reads whatever tier survives and returns daily figures where it once returned hourly ones. A corrected incident timeline changes any post-mortem regenerated from it. And the artifact is what was sent to a client: if an uptime claim is ever disputed, evidence that regenerates differently is not evidence.

**An artifact is a record, not a cache**, and everything else follows from that.

The decision: **files under `<data-dir>/reports/<yyyy>/<mm>/`, with the database holding the index and not the bytes**, plus an **optional S3-compatible mirror** for offsite durability. Local remains the source of truth and the only read path in every configuration. Blobs in `cairn.db` were the strongest alternative and lost on three specifics — every `VACUUM INTO` backup growing in proportion, fifty artifact writes contending with heartbeat ingest on SQLite's single writer during the monthly burst, and `database/sql` offering no incremental blob access for the hundred-megabyte CSV a full-estate annual report produces.

Four things bind work later in this plan. **The file is written and fsynced before the row commits**, so a crash leaves an inert orphan rather than an artifact the UI offers and the disk cannot supply. **The on-disk name derives from the artifact id, never the report title**, so a definition called `../../etc` has nowhere to go. **`ReportArtifactDays` joins [`RetentionSettings`](../../internal/model/settings.go)** at 365 days by default, deliberately independent of the rollup tiers because an artifact is expected to outlive the data behind it. And **a per-artifact size cap** is a real limit with a clear error, because the case that hits it is a CSV over 5,000 monitors for a year, not a PDF.

**The backup documentation changes in the same PR** — that is the price of choosing files, paid openly: `VACUUM INTO` for the database, a copy of the reports directory, and the key separately as before. Where the S3 mirror is enabled the reports directory is already offsite and the local step may be skipped, which is a reason to enable it beyond durability.

**One constraint the ADR records because this phase introduces the bucket that makes the mistake convenient:** the root key must not be written to the same bucket as a database backup. [backup-restore.md](../operations/backup-restore.md) is unambiguous — a backup that puts the key beside the database it protects has encrypted nothing against the threat that actually happens. Remote backup of the database and key is a **Phase 4** roadmap item and is not in scope here; ADR-008's S3 client makes it cheap when its phase arrives.

---

## 4. Feature Specifications

### 4.1 Report Definitions

A definition is the saved thing; a run is one execution of it; an artifact is one rendered file. Keeping the three separate is what makes "re-send last month's report", "regenerate it after we corrected the incident record", and "the PDF failed but the HTML went out" all expressible.

A definition holds: the report type, the monitor selection (explicit ids, a group, a tag, or all), the window (rolling — last 30 days — or calendar — last complete month), the timezone the window is cut in, the brand profile, the formats to render, the recipients, and the schedule.

**Monitor selection is by rule, not by list, wherever possible.** An agency that adds a monitor to a client's tag expects it in that client's next report without editing the report.

### 4.2 Scheduled Reports

- Daily, weekly, monthly, quarterly, plus a cron expression for anything else, using the existing parser.
- **Calendar windows are cut in a stated timezone**, from `GeneralSettings.Timezone`, overridable per definition. A "monthly" report that starts at midnight UTC for an Australian agency is wrong by a working day and will be reported as a bug.
- Generation is decoupled from delivery: a run produces artifacts, then delivery attempts are recorded per recipient with retry, exactly as notification deliveries are. The S3-compatible **drop** is a delivery target and is distinct from ADR-008's optional S3 **mirror**, which is a durability copy of every artifact; they share a client and nothing else.
- **A missed schedule is visible.** If the instance was down at 09:00 on the 1st, the run is late, not lost, and the UI says which.

### 4.3 SLA / SLO Reports

The report auditors and contract reviews actually want: target versus actual, error budget consumed and remaining, burn rate, and a breach log with timestamps.

The definitional work is the hard part and it is not optional:

- **What counts against the SLO.** `down` does. `maintenance` does not, by default, and that default must be stated on the report face. `unknown` and `skipped` are not downtime and are excluded from the denominator — with the excluded share reported, because an SLA computed over 60% observation is not an SLA.
- **The denominator is stated on the report.** Observed checks, not wall-clock, unless wall-clock is chosen explicitly.
- **Targets** are per-monitor or per-group, expressed as a percentage over a window, stored on the monitor or the group so a single number can serve alerting later.
- **Error budgets come from `up_count` and `down_count`**, which are additive at every tier, so the budget over any window is exact rather than estimated. Per [ADR-006](../adr/006-report-latency-statistics.md), no SLO figure is derived from a percentile.

**Response-time reporting**, per ADR-006. Five figures, and no others in the latency block:

| Figure | How it is computed | Window |
|---|---|---|
| Average | `SUM(response_time_sum) / SUM(response_time_count)` across buckets in range — exact at any tier, because sum and count are additive | The report window |
| Daily average series | One point per day from the 1d tier; the report's primary latency exhibit | The report window |
| Best and worst day | Extremes of that series | The report window |
| Days over target | Days whose average exceeded the response-time target, with their dates; present only when a target is set | The report window |
| p95 | `UptimeFromRaw`, a real nearest-rank percentile, gated by `RawCovers` | **Trailing 7 days only**, labelled, in a visually separate block |

**Window-level minimum and maximum are not reported.** Over a month they are extreme-value statistics — the single slowest and single fastest successful check out of tens of thousands — and the maximum in particular reads as alarming while carrying no signal. Day-level extremes give the same shape of information from a statistic that only moves on sustained degradation.

Two constraints that are part of the decision rather than details of implementation. The weekly p95 depends on `RawDays`, which the operator can lower to as little as one; `RawCovers` gates it and the report omits it, labelled, rather than printing a three-day figure under a seven-day heading. And a response-time threshold breach already marks a check **down** ([`http.go`](../../internal/probe/check/http.go)), so `up_count / (up_count + down_count)` is already an exact latency SLI for any monitor carrying a threshold — but `Class` is [not persisted](../../internal/observation/observation.go), so stored history cannot distinguish "too slow" from "did not answer". That figure is honestly described as *"met the response-time target"* and dishonestly described as *"was slow"*.

### 4.4 Incident Post-Mortems

Auto-drafted from the incident timeline: detection, acknowledgement, resolution, MTTD/MTTA/MTTR, affected components, alerts fired.

The honest caveat that must be handled rather than papered over: `AutoOpened` is never set in Phase 1 and `DetectedAt` may be nil, so **MTTD is frequently unknowable** for incidents recorded by hand. Report it as unknown. A post-mortem that invents a detection time is worse than one with a blank in it.

Alerts fired come from the notification delivery log, which is retained 90 days by design because — per the comment in `rollup.go` — post-mortems cite these rows.

### 4.5 Comparative Reporting

Period over period (this month against last), monitor against monitor, and group against group. Region against region is specified but **degrades to a single region until Phase 4 ships multi-region probes**; the report shape accommodates it now so it does not need reshaping then.

### 4.6 White-Label Branding

- A **brand profile**: logo upload, primary and accent colours, footer text, report cover text, and a "prepared for" client name.
- Referenced by report definitions and by status pages both, replacing the duplicated fields rather than adding a third set.
- Default profile derives from the existing appearance settings, so an install that never opens this screen still produces a report that does not look unbranded.

### 4.7 Formats and Share Links

- **HTML** is the canonical rendering; every other format derives from the same computed report model.
- **PDF** per [ADR-007](../adr/007-report-rendering.md): rendered in-tree from the report model by a pure-Go writer, never by converting the HTML. Plain-text branded fields; raster logos unless the SVG-path translator is built.
- **CSV and JSON** are the data, not the layout: one row per bucket per monitor, plus a machine-readable summary block. These are what a client's own BI tool consumes and they are cheap to get right.
- **Share links** are unguessable tokens on an unauthenticated read path, per [ADR-008](../adr/008-report-artifact-storage.md). That path follows the status page discipline in [`internal/model/statuspage.go`](../../internal/model/statuspage.go): a **separate public projection**, not a filter over the private shape, because a field cannot leak through a projection that has no place to put it. The token is stored twice — hashed for the lookup index, sealed for replay — following the unsubscribe token precedent in that same file. Links are revocable, may carry an expiry, and are `noindex` and rate limited. **A share link serves the stored artifact, never a re-render**, so the figures a client bookmarked do not change when retention drops a tier.

### 4.8 Certificate and Domain Expiry Calendar

A forward-looking view, not an alert three days before it breaks: everything expiring in the next 7/30/90 days, sortable, exportable, and schedulable as its own monthly report. The data exists already; this is a report over it.

### 4.9 Custom Report Builder

Pick metrics, group by tag or group, choose a window, save as a reusable template. Deliberately bounded: **a selection tool, not a layout designer.** Drag-and-drop layout is not in this phase and is not missed by the agency the phase is for.

### 4.10 Full Historical Browsing

Drilldown into arbitrary past ranges in the dashboard, at the best resolution retention holds, always labelled with the resolution actually used. This is where the Phase 1 complaint about being "week-blinkered like Kuma's" is finally answered in the UI.

---

## 5. Architecture in Phase 2

- **A `internal/report` package that computes, and renderers that format.** The computation produces a backend-independent report model from the repository interface; renderers turn it into HTML, PDF, CSV, JSON. The same discipline as `internal/rollup`: the contract lives in the package, the backend supplies only the queries, and Timescale later computes the same numbers.
- **Renderers are siblings, not a chain** ([ADR-007](../adr/007-report-rendering.md)). Each consumes the model; none consumes another's output. Charts are drawn once against a small primitive set — text run, rectangle, line, path, image — with an SVG backend for HTML and a PDF content-stream backend for PDF. Every primitive added is one both backends implement forever, so the set stays small.
- **Report generation never competes with the probe scheduler.** Runs execute on a bounded worker pool with an explicit concurrency limit, off the check path. Fifty PDFs at 09:00 on the 1st must not delay a single check, and the load test says so rather than the author.
- **Reads go through the rollup tiers, never raw**, except inside raw retention where a real percentile is being computed. A report that scans heartbeats for a year is the exact failure ADR-004 exists to prevent, one layer up.
- **Generation is idempotent and re-runnable.** A definition plus a window plus a data snapshot yields the same artifact; a re-run after a correction is a first-class action with both artifacts kept.
- **Delivery reuses the existing channel plumbing.** Email through the configured relay, Slack through the existing provider, webhooks signed as `internal/outbound` signs them. A report is a payload, not a new transport.
- **`OrgID` on every new table from the first migration.**

---

## 6. API and the Spec-First Rule

The Phase 0 rule is unchanged and is not softened for this phase: **the OpenAPI spec is extended and reviewed before any handler is written**, and agents do not add, rename, or reshape endpoints ([AGENTS.md](../../AGENTS.md) § 4).

Phase 2 adds, at minimum: report definitions (CRUD), runs (list, get, trigger, cancel), artifacts (get, download), share links (create, revoke), brand profiles (CRUD), and SLO targets on monitors and groups. Cursor pagination and the problem-details error shape apply unchanged. Contract tests are green before the milestone is called done, and the generated Go and TypeScript clients are regenerated in the same PR as the spec change.

---

## 7. Milestone Breakdown

### Month 4 — Decisions and the computation core
- ADR-006 (latency statistics), ADR-007 (rendering) and ADR-008 (artifact storage) are all decided; none gates the work below. The AGENTS.md stack line is corrected in ADR-007's PR, and the backup guide is updated in ADR-008's.
- OpenAPI spec extension for reports merged and frozen.
- `internal/report` computes the SLA/uptime model from the rollup tiers over an arbitrary window, with the denominator rules of §4.3 and tests over maintenance, unknown, and skipped.
- The five response-time figures of §4.3 read from the existing tiers — no schema change — including the `RawCovers` guard on the trailing-seven-day p95.
- **Checkpoint:** `curl` a computed SLA report as JSON for a monitor over an arbitrary past month, with a stated denominator, honest nulls, and a latency block that omits what it cannot compute.

### Month 5 — Rendering, branding, scheduling
- The drawing primitive set and its SVG backend; HTML renderer as the canonical output; CSV and JSON from the same model. The PDF backend follows against the same primitives, so that if it runs long, HTML plus print CSS ships and PDF slips without disturbing anything else.
- Golden-report visual regression test, standing up with the second renderer rather than after it.
- Brand profiles, with status pages migrated onto them.
- Report definitions, scheduling on the shared cron parser, calendar windows in the configured timezone, the bounded worker pool, and run/artifact lifecycle.
- Artifact storage per ADR-008: the dated directory layout, id-derived filenames, write-fsync-then-commit ordering, SHA-256 and size on the row, the orphan sweeper, and `ReportArtifactDays`.
- **Checkpoint:** a monthly definition fires on schedule and produces a branded PDF and CSV on disk, indexed, checksummed, and reclaimed on expiry.

### Month 6 — Delivery, sharing, and the rest of the report types
- Delivery over email, Slack, webhook, and S3-compatible drop, with per-recipient attempts, retry, and a visible delivery log. The standard-library SigV4 client lands here, with selectable path-style addressing, an overridable endpoint, and the secret key sealed per the `SMTPSettings.PasswordSealed` precedent — serving both the drop and ADR-008's optional mirror.
- Share links on the public projection, revocable and expiring.
- Incident post-mortems, comparative reporting, expiry calendar.
- Report UI: list, definition editor, run history, preview, download, and the custom builder.
- Full historical browsing with resolution labelling in the dashboard.
- **Checkpoint:** the exit scenario runs end to end — 50 definitions, one schedule, 50 branded emails, nothing touched.

### Month 7 — Hardening, scale, release
- Load gate extended: **50 concurrent report runs across a 5,000-monitor install, with check scheduling latency unchanged.** A regression blocks merge, not release.
- Failure paths: renderer failure falls back to HTML with the reason recorded; delivery failure retries and surfaces; a missed schedule is late and visible; a run cancelled mid-flight leaves no half-written artifact.
- Docs: a reporting guide, an SLA methodology page stating exactly what counts as downtime, brand profile setup, S3 configuration for both drop and mirror, the retention/resolution table, and **the revised backup procedure — the reports directory is now part of it**.
- Release engineering: tagged release, images, checksums, SBOM, changelog, announcement.
- Buffer. **Features get cut before the release date or the quality gates do.**

---

## 8. Exit Criteria (all must be true)

- [ ] The implementation matches [ADR-006](../adr/006-report-latency-statistics.md), [ADR-007](../adr/007-report-rendering.md) and [ADR-008](../adr/008-report-artifact-storage.md).
- [ ] Scheduled reports: daily, weekly, monthly, quarterly, cron; calendar windows cut in the configured timezone; missed runs visible as late.
- [ ] SLA/SLO reports with target vs. actual, error budget consumed and remaining, burn rate, and a breach log — **with the denominator and the maintenance policy stated on the report face**.
- [ ] Response-time reporting is the five exact figures of §4.3, with the p95 confined to the trailing seven days, gated by `RawCovers`, and labelled with its own window. No approximation is presented as a percentile, and no window-level minimum or maximum appears.
- [ ] Error budgets are computed from uptime counts. No SLO figure is derived from a percentile.
- [ ] Incident post-mortems auto-drafted with MTTD/MTTA/MTTR, unknown values shown as unknown.
- [ ] Comparative reporting: period over period, monitor vs. monitor, group vs. group.
- [ ] Formats: PDF, HTML, CSV, JSON, and a revocable public share link on a separate public projection, serving the stored artifact rather than a re-render.
- [ ] Artifacts are durable, indexed, checksummed, size-capped and expired on a retention policy; orphans are reclaimed; **the backup guide has been updated and the revised procedure tested end to end**.
- [ ] Delivery: email, Slack, webhook, S3-compatible drop, each with recorded attempts, retry, and a visible failure.
- [ ] Brand profiles shipped and shared with status pages; no duplicated branding fields remain.
- [ ] Custom report builder: metrics, grouping, window, saved as a reusable template.
- [ ] Certificate and domain expiry calendar, browsable and schedulable.
- [ ] Full historical browsing with the resolution actually used labelled honestly.
- [ ] Spec-first honoured: contract tests green, Go and TypeScript clients regenerated.
- [ ] **The 5,000-monitor gate still green, with 50 concurrent report runs underneath it.**
- [ ] Docs shipped with the release, including the SLA methodology page.
- [ ] **The test that matters:** an agency sends 50 branded client reports on the 1st without touching anything.

---

## 9. Success Metric

Phase 2 release target, from the roadmap: **2,000 GitHub stars; 10 agencies using white-label reports in production.**

The second number is the real one. Stars measure attention; ten agencies on the first of the month measures whether the differentiator differentiates.

---

## 10. Phase 2 Risks

| Risk | Severity | Mitigation |
|---|---|---|
| PDF rendering swallows the phase | High | ADR-007 removes the packaging risk — no subprocess, no sidecar, no cgo, no new dependency — leaving only layout effort, which is bounded and ours. Table page-breaking is the known time-sink and is budgeted as such. HTML is canonical and ships regardless; **PDF is the cuttable feature, reporting is not the cuttable phase** |
| The PDF and HTML layouts drift apart | Medium | Two renderers over one model is the cost of ADR-007. A golden-report visual regression test stands up alongside the second renderer, not after it |
| A wrong SLA number reaches an auditor | Critical | The denominator rules are specified before code (§4.3), tested against maintenance/unknown/skipped explicitly, and printed on the report face. A methodology doc ships with the release |
| A percentile approximation reaches a report | High | ADR-006 removes the mechanism rather than policing it: no report reads a coarse-tier percentile, and `HistoryFromTier` keeps substituting `NULL`. Tests assert the substitution and assert that the weekly p95 is omitted when `RawCovers` says coverage is short |
| The missing monthly percentile costs a user | Medium | Accepted knowingly and recorded in ADR-006 with named triggers to reopen. A client contract specifying a percentile SLO over a monthly window is the trigger most likely to fire; the histogram is designed and deferred, not undesigned |
| Report generation degrades monitoring | High | Bounded worker pool off the check path; the load gate is extended to run reports concurrently and blocks merge on regression |
| An operator's backup silently omits the reports directory | High | The known cost of ADR-008's choice of files over blobs, and the failure is silent until a client disputes a figure. Mitigated by the revised guide, the optional S3 mirror, and a warning where artifacts exist with no mirror configured — none of which makes it zero |
| A misconfigured public bucket exposes client reports | Medium | A breach with no code defect behind it. ADR-008 requires the bucket to be non-public and passes through server-side encryption headers; the rest is documentation |
| Scope creep into Phase 3 (per-tenant branding, RBAC on reports, on-call) | Medium | The scope table in §1 is the contract. `OrgID` is carried so Phase 3 is a permission change; nothing else about tenancy is built here |
| Object-storage SDK sprawl | Low | SigV4 over the standard library, justified in the PR per the dependency policy |
| Single-maintainer burnout | Critical | Carry-over and unchanged: public progress notes, responsive triage, committer recruitment remains a P0 deliverable |

---

## 11. Deliberately Not in Phase 2

Named so they are refused consistently rather than argued each time:

- **RBAC, teams, SSO, audit log** — Phase 3. Reports are owned by the install, not by roles, until then.
- **Automatic incident opening and on-call** — Phase 3. Post-mortems in this phase read incidents a human declared.
- **Multi-region comparison with real regions** — Phase 4. The report shape accommodates it; the data does not exist yet.
- **A paid tier.** PDF reports, white-labelling, and SLA reporting ship in the open source build. This is stated in the roadmap and is not revisited here.
