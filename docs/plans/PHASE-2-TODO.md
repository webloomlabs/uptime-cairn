# Phase 2 — working checklist

Every deliverable in [PHASE-2-PLAN.md](PHASE-2-PLAN.md), as a list that can be
ticked. The plan is the contract and does not change; this is the tracker. The
same rule the Phase 1 list ran under applies here and is the reason that list
stayed honest: **a box needs a demonstrated run behind it.** "The code is
written" is not one, and neither is "the configuration was reviewed".

**Status: 2026-09-04. 120 of 129, and the reporting subsystem is end to end.** A
report is defined in the dashboard, generated or fired by a schedule, rendered in
four formats, filed on disk with a digest, mirrored offsite, delivered by email,
webhook or an S3 drop, published at a revocable public link, reclaimed when it
expires, and downloaded from a run-history screen that tells `partial` from
`succeeded` and a tombstone from a missing file. Every claim below that is ticked
has a demonstrated run behind it, which is the rule this list has run under since
Phase 1.

> ### The rule 8 waiver, and what a tick does not mean
>
> **Share links and the S3 client were built on 2026-09-03 by an agent, under an
> explicit maintainer waiver of [AGENTS.md](../../AGENTS.md) rule 8** ("Security
> work is human-led. Do not generate authentication, session, crypto, or
> access-control code"). The waiver was given in the session that produced them;
> it is recorded here rather than in a commit message because this file is what
> somebody reads in a year.
>
> The count above is now three larger than it was: the live provider test, the
> share-link documentation, and preview split out from the share-link box it had
> been sharing.
>
> **A tick in those two sections means the code works and has tests behind it. It
> does not mean a person has read it.** That distinction is the entire reason rule
> 8 exists, and the two sections say so at their heads. What the tests can and
> cannot carry is worth being exact about:
>
> - The signer is checked against **AWS's own published PUT vector**, not against
>   itself, and against a real MinIO including a key containing `+`, a space, `=`
>   and `:`. A wrong secret is refused by a server that actually verifies it,
>   which is what distinguishes a checked signature from an ignored one.
> - The public projection's freedom from leaks is **structural rather than
>   filtered** — `ReportDocument` has no field for a monitor target — and the
>   golden-path assertion the plan asks for passes against a real monitor whose
>   target was chosen to be recognisable.
> - Neither of those is a person who can defend the code at 3am, which is what the
>   rule is actually protecting.
>
> Two things found by running it rather than by reading it are recorded in place,
> because they are the best available evidence of what review is still for: the
> public read path was rate-limited at **five requests per fifteen minutes**
> (fixed), and the mirror was resolved **at start-up rather than per run**, which
> reintroduced exactly the drift `report_artifact_days` had just been moved out of
> (fixed). Both were caught by driving the running binary, and neither was caught
> by writing the code.

**The backup drill was run on 2026-09-04** and is ticked below. It found a real
defect rather than confirming what was already believed: a database restored
without its reports directory answered **`500 Internal error`** on a download,
which contradicted the frozen spec — that operation declares `200`, `401`, `404`
and `410` and no `500` — and contradicted ADR-008's Consequences, which
anticipate that exact state and require it to render "as a missing file rather
than an error page". It now answers `410` naming the missing directory, which is
the one thing an operator in that position can act on, and
`TestAnArtifactWhoseFileIsGoneAnswersGoneNotInternalError` holds it. **That is
the argument for drilling rather than reading**: the behaviour had a written
justification in a code comment, and the justification was wrong.

**What is left is smaller and none of it is security work.** The extended load
gate is **CI configuration under rule 7**. The remainder is `sections` selecting
blocks in `Compose` (which is the custom builder), the expiry calendar as a
*report type* rather than as a collection, the historical drilldown screen, and
an HTML preview.

**Runs now outlive the definitions that made them** (maintainer's ruling,
2026-08-31). Writing the store found the first cut of `0008` contradicting the
frozen spec: `deleteReportTemplate` is documented as *"Already-generated runs and
their artefacts are retained"*, and `report_runs.report_template_id` was `NOT
NULL … ON DELETE CASCADE`, so deleting a template deleted every report it had
produced and, through `report_artifacts`, the record of what each client was
sent. It contradicted the migration's own reasoning too: the column immediately
below used `SET NULL` because *"the artifact is a record of what a client was
sent, and it outlives the arrangement that sent it"* — an argument about the
template as much as the schedule.

The ruling was **soft delete**, and it is the better of the two available
answers rather than the softer one. A nullable foreign key would retain the run
and lose the definition, leaving a record that cannot say what it was a report
*of* — which is the first question anybody asks of one, and the question that
gets asked precisely because somebody tidied the template up. Keeping the row
means "we sent them this, under this definition, on this schedule" survives the
client leaving. It also **needed no spec change**: the sentence the spec already
carries becomes true.

`report_templates` and `report_schedules` gained `deleted_at`; the runs table now
references both with `RESTRICT`, so the rule is an invariant of the database
rather than a convention of the handlers — a hard `DELETE` from a SQL shell is
refused while runs exist, and a test proves it rather than reading the DDL.
Deleting a template soft-deletes its schedules in the same transaction, because a
deleted report that keeps arriving in a client's inbox is worse than either
deleting it or not. The standing cost is stated where it will be paid: every read
path must filter `deleted_at IS NULL`, and the cursor indexes are **partial on
that predicate** so the filter is the access path — both the listing and the
scheduler's due query still plan as a `SEARCH` with no scan, checked against a
real database along with `integrity_check` and `foreign_key_check`.
`brand_profiles` is deliberately *not* soft-deleted: a template referencing a
deleted profile falls back to the default, which is what a template with no
profile already does, so there is nothing for the row to preserve.

**`0008` was edited in place rather than corrected by an `0009`.** It is
pre-release and says so in its own header, and SQLite cannot alter a foreign key
without rebuilding the table — so a correction migration would rebuild
`report_runs` to fix a file nothing has shipped. The consequence is real and
worth knowing: **the migration runner verifies checksums, so any database with
the earlier `0008` applied will refuse to start until it is recreated**, and it
names the file when it does.

**Three gaps in the frozen spec, found by implementing against it.** None is
changed; all three are recorded because a spec nobody has built against is a spec
whose gaps are invisible. (1) `deleteReportTemplate` promised retained runs while
`0008` cascaded them — resolved by the soft-delete ruling above. (2) The run
list's `state` filter enumerates `queued, running, succeeded, failed` and omits
**`partial`**, so the state a missing font produces most often is the one state
that cannot be filtered for. (3) `BrandProfile.logo_url` is defined with no
operation that serves the bytes: `PUT /brand-profiles/{id}/logo` has no `GET`
beside it, so the field is emitted as null rather than naming an endpoint that
answers `405`. Each is `x-cairn-phase: 2` with no release behind it, so each is
free to fix under [COMPATIBILITY.md](../api/COMPATIBILITY.md) §2 — and each is the
maintainer's.

**A smaller finding about this checklist.** `generate` is specified as `202`
returning a **run to poll**, not the document — so the Month 4 curl is generate,
poll, download, and it needs the runs table, artifact storage on disk and the
download path before it works. The paragraph below said "one handler away" and
that was optimistic; it is the artifact-storage section away.

**The Month 4 checkpoint is met, by `curl` against a running instance
(2026-08-31).** A monitor, a month of seeded history, `POST
/report-templates/{id}/generate` over March 2026, a `202` with a run to poll, and
the JSON downloaded from `/report-runs/{id}/download?format=json`. The figures
verify by hand: 8,582 of 8,592 observed checks is 99.884%; the error budget is
0.1% of 31 days, 2,678 seconds; consumed is 3,000 seconds, which is exactly the
sum of the two breaches; remaining is **−322** rather than floored at zero; the
burn rate is 1.12; and the p95 is absent with `insufficient_raw_retention` beside
it. The `X-Cairn-SHA256` header matched `shasum -a 256` of the downloaded file.

The same run produced HTML and CSV, and **failed the PDF with a stated reason** —
"no embedded font family" — leaving the run `partial` with three artifacts
rendered and one failed row carrying its own error. That is ADR-007 item 7 taking
a real case rather than a hypothetical one on the first instance it ever ran on.

**And the run found a defect no unit test had.** The first attempt seeded only the
`1d` tier, so the window totals (read at `1h`, which retention legitimately
covers for a March window) came back empty while the breach log came back full:
the document said *two breaches, fifty minutes* beside *budget consumed: 0*.
`ComputeSLA` returned early on the window bucket, discarding the downtime the
daily series had already established. This is the **same contradiction the golden
report caught once already, arriving through a different door** — and the rule
that fixed it then holds now: consumed is the breach total on every path through
that function. It is reachable in production wherever the two tiers disagree
about what is known, which an install that imported daily history or pruned its
hourly rows does. The rendered page now reads "Budget used 50m" above a table of
40m and 10m.

**Schedules fire unattended** (2026-09-03). A daily schedule at 07:30
Australia/Sydney, created through the API, was backdated by half a minute and
picked up by the next tick without anything touching it: a run appeared, both
formats rendered, `last_run_at` was recorded and `next_run_at` advanced to
tomorrow's 07:30 Sydney. The period it covered is `2026-09-01T14:00Z →
2026-09-02T14:00Z` — **midnight to midnight in Sydney, visibly not a UTC day**,
which is the whole reason a schedule stores its own zone. Backdated by half an
hour instead, the same schedule produced a run marked `late` with a log line
naming how far behind it was.

**The lifecycle closed** (2026-09-03). Four things that existed as code nothing
ran are now wired and held by tests. **Artifacts are reclaimed**: an hourly
sweeper expires bytes past `report_artifact_days` into a tombstone and collects
the orphan files write-then-commit deliberately leaves behind, with the grace
period proved by putting a file exactly where a racing write would put one — and
the delete order is the mirror of the write, row-then-file, so a crash leaves an
orphan rather than a row promising bytes that are gone. **An interrupted run
finishes**: cancellation is checked between formats and never inside one, because
abandoning a half-rendered PDF is the one way to produce the artifact this
subsystem promises never to leave; a restart tidies what a crash left at
`running`, at start-up rather than on a timer, which is the only moment that is
safe without a threshold long enough to make a stuck run look live. **Settings
are read per run**, closing real drift — `report_artifact_days` was validated on
the settings surface and the runner used a compiled-in constant, so an operator
could set thirty days, watch the API accept it, and find a year later that
nothing had expired. And **an unbranded install is no longer anonymous**: with no
brand profile the report takes the instance's name and the dashboard's primary
colour, which is the solo user's path and the common one.

**Delivery works, and it is somebody else's machinery with a report on it**
(2026-09-03). Email goes through the instance SMTP relay that alerts and
status-page bulletins already use; Slack and webhooks read a notification
channel's own configuration rather than a copy, which is what makes a rotated
token a one-place change. Every attempt is a row, retried three times rather than
twenty — a monthly report's value does not decay in minutes the way an alert's
does — and a **skipped** delivery is recorded as loudly as a failed one, because
"the auditor's PDF did not go out because the PDF did not render" is the sentence
somebody needs and it is invisible without a row. A Slack webhook URL is redacted
in the log before it is stored: the path *is* the credential, and the log gets
pasted into support conversations.

**A fourth spec gap, found by building against it.** The plan asks for report
webhooks "signed exactly as `internal/outbound` signs them", and they are not,
because **there is nowhere to put the key**. An HMAC needs a shared secret; the
frozen `ReportSchedule.deliveries` target carries `type`, `recipients`,
`notification_channel_id`, `url`, an `s3` block and `formats`, and a webhook
*notification channel* has secret headers rather than a signing key — the HMAC
key lives on `outbound.Webhook`, a separate resource a delivery target has no
field to reference. The choice was between inventing a field on a frozen spec
(rule 4, not an agent's to make) and using the authentication the configuration
actually offers, and it does the second: a target naming a webhook channel sends
that channel's secret headers. A test asserts the *absence* of a signature so the
gap stays visible.

**All four formats render, and a real instance has produced all four.** On
2026-09-01 a branded SLA report over March 2026 came out of `POST
/report-templates/{id}/generate` as `succeeded` with a PDF, an HTML page and a
JSON document, from a template naming a brand profile created through the API.
The PDF is three A4 pages: a cover, a 31-day uptime strip with two red days and
one **grey** one for the day the probe was off, an SLA block reading 99.9% target
against 99.884% achieved, and a breach table of 40m and 10m under a "Budget used"
of 50m. The figures agree with the JSON and with arithmetic done by hand.

**All four formats render.** JSON and CSV come off the model as siblings, never
as a chain. HTML is canonical, and the PDF is a pure-Go writer over the same
seven elements and the same five primitives — never a converted page, which is
[ADR-007](../adr/007-report-rendering.md) item 1. The charts were written once
against the primitives and both backends draw them from the same calls, so the
uptime strip and the latency line arrived on the PDF side as a by-product rather
than as a second implementation. **One thing is missing and only a human can
supply it: the font file.** The reader, the embedder and the two-weight family
are written and tested against a synthetic face; which TrueType family to vendor
is a licence choice and a visual identity commitment, and it is the maintainer's.

**The golden report caught a defect the unit tests could not.** The rendered
page read "Budget used 3h 36m" above a breach table summing to 2h 24m: the error
budget projected the window-level down proportion onto the window's whole
length, attributing the observed failure rate to a day the probe was off. That
is the same invention the denominator rules refuse one layer up, and no test of
either figure alone could see it — only the two side by side on a page. Consumed
budget is now the sum of the breach durations, so the two agree by construction
rather than by arithmetic coincidence.

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
- [x] **The backup guide, revised in ADR-008's own terms, and the procedure drilled end to end** — the guide was written 2026-08-30 ahead of the code and carried a caveat saying so; **the drill was run 2026-09-04 and the caveat is gone.** `VACUUM INTO` against a *running* install, then `rsync` of the reports directory in that order, then the key separately; `integrity_check`, `foreign_key_check` and the migration list on the snapshot; a restore into a clean data directory alongside the original still running, because a restore that only works once the source is gone has proved nothing. **The assertion that matters is the last one**: all nine artifacts across three month-shards downloaded from the restored install and hashed to the digests recorded before the backup, and a share link a client was already holding resolved against the restored install and served bytes matching the same digest. The `--encryption-key-file` recovery path was exercised too, and the key was confirmed not to be written into the data directory when it comes from outside
- [x] Contract tests select `x-cairn-phase: 2` as they already select `1` (2026-09-03). **113 operations exercised, 91 Phase 1 and 22 Phase 2**, in both directions: every covered operation has a handler, and nothing is served that the spec does not describe. The selection is per operation by **skip list rather than allow-list**, which is the stronger direction and the one the file already argues for — every Phase 2 operation is checked unless it is named with a reason, where an allow-list is a list somebody forgets to add to and the forgetting is silent. The four share-link operations are named there as **human-led rather than unbuilt**, and deleting those four lines is the last step of building them

## Schema and storage

- [x] Migration [`0008_reporting.sql`](../../migrations/sqlite/0008_reporting.sql) — **seven tables, not the four the data model named**: brand profiles, templates, schedules, schedule deliveries, runs, artifacts, share links, delivery attempts. `org_id` on every one from this first migration, so Phase 3 tenancy is a permission change and not a re-architecture. Applied over `0001`–`0007` against a real database with `integrity_check` and `foreign_key_check` clean, and each constraint exercised until it refused something: `sla_target = 100`, a second default brand profile, a second live share link on one run, a repeated `token_hash`, a second PDF for one run, `format = 'docx'`. Deleting a template cascades runs → artifacts → share links to zero. The three scheduled queries — due schedules, the expiry sweep, the share-token lookup — each plan onto their index as a `SEARCH`, with no scan
- [x] `slo_target_percent` on monitors and groups — one nullable column each, `NULL` meaning no target, with the `< 100` bound enforced by a `CHECK` rather than only at the API: 100 refused and 99.95 accepted, tested against a real monitor row and a real group row. **The API field is now written too** (2026-09-03), on both resources, and it is refused at the handler as well as by the `CHECK` — because the API is the layer where the *reason* can be said rather than only enforced: a 100% target has an error budget of zero seconds, which makes burn rate undefined and every report a breach report. `PATCH` uses `json.RawMessage` so **removing a target is expressible**, which a `*float64` would have made impossible in the same way it did for the template's `sla_target`. Demonstrated live: created with `99.9`, read back as `99.9`, and resolved onto the report's SLA block
- [x] Of the four things whose **column landed in `0008` and whose Go half was not written**, three are now written: `report_artifact_days` inside the existing `retention` JSON, exempt from the coarser-outlives-finer rule as ADR-008 requires; the artifact row's `sha256`, `size_bytes` and `state`, with `expired` as the tombstone behind a `410`; and `path`, stored rather than derived. All three are now **read at run time rather than at start-up**, which closed real drift — `report_artifact_days` was validated on the settings surface while the runner used a compiled-in constant, so an operator could set thirty days, watch the API accept it, and find a year later that nothing had ever expired. `report_storage` is now the eighth settings section, with `secret_access_key_sealed` following `SMTPSettings.PasswordSealed` exactly — sealed rather than hashed because SigV4 replays it on every request, inside the section's JSON so the column holds no plaintext, and with the `-` tag on the in-memory field. It is sealed under its **own** AAD binding (`settings`/`report_storage`) rather than the SMTP column's, so an SMTP password cannot be relocated into it. A ninth thing arrived with it: migration `0009` adds the `mirror_state`, `mirror_uploaded_at` and `mirror_error` columns the frozen `ReportArtifact` schema requires and `0008` had nowhere to put — a separate migration rather than an edit, because `0008` has shipped and its checksum is recorded
- [x] **CRUD for brand profiles, templates, schedules, runs and artifacts is landed and tested**, and the expiry calendar joins them as a read-only collection over data migration `0003` already held. [`report.go`](../../internal/model/report.go) holds the domain types for all seven tables; [`report_brands.go`](../../internal/store/sqlite/report_brands.go) and [`report_templates.go`](../../internal/store/sqlite/report_templates.go) are the two write paths that exist. [`report_runs.go`](../../internal/store/sqlite/report_runs.go) adds runs, artifacts and the delivery log. Thirty tests, and several are about decisions rather than plumbing. **Starting a run is conditional on it still being queued**, which is the property a bounded worker pool rests on: two workers that pick up the same run cannot both render it, one updates a row and the other gets `ErrConflict`, and the pool needs no lock — a lock being a thing to get wrong at 09:00 on the first of the month. Expiry is a **tombstone**, so a bookmarked link answers 410 rather than 404, and the path goes with the bytes because a path that no longer resolves invites the next reader to go looking. A `skipped` delivery is recorded rather than omitted, for the same reason a suppressed notification is: silence with no row behind it is indistinguishable from a system that is not running. Three more are about decisions rather than plumbing: a colour round-trips **exactly as written**, because a brand colour is a string pasted from a brand guide and handing it back in a different case is what makes a white-label feature feel like somebody else's; the scope stores **readable UUIDs** rather than base64 bytes, because the reader of that column is a human in a support conversation about why a monitor is or is not in a client's report; and the logo is **not carried on the profile read**, because a list of twelve clients would otherwise move twelve megabytes to render twelve names. Marking a profile default demotes the previous one in the same transaction rather than letting the unique index refuse the write — "there is already a default" is not something an operator can act on when making this one the default is exactly what they asked for
- [x] The SQLite half of [`report.Store`](../../internal/report/report.go) — [`reports.go`](../../internal/store/sqlite/reports.go): `MonitorsInScope` unions ids, groups (reaching child groups) and tags in one predicate so a monitor selected three ways appears once, and includes paused monitors, which still have history in the window; `WindowTotals` and `DailySeries` are batched, seek per monitor by the `(monitor_id, bucket_start)` primary key, and return `NULL` for p95 at every tier including `1m`. Absent monitors stay absent rather than arriving zero-valued, because zero up and zero down is a real state meaning "observed nothing" and a report has to tell the two apart. Nine tests, `-race` clean
- [x] **Data model updated** — the reporting tables moved out of §4.13's one-line list into a **§4.14 of their own**, with each of the three the old list did not name given the decision it followed from: brand profiles (spec Q2), share links (ADR-008), and the configured-target/attempt split that mirrors `notification_channels` and `notification_deliveries`. The soft-delete rule is recorded there in full — `deleted_at`, `RESTRICT` from runs, the partial indexes, and the argument for why a nullable foreign key looks equivalent and is not — along with the standing cost stated where it is paid: every read path must filter `deleted_at IS NULL`

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
- [x] A test asserts `HistoryFromTier` still substitutes `NULL` for the coarse-tier percentile, and a second asserts the weekly p95 is absent when coverage is short — [`adr006_test.go`](../../internal/store/sqlite/adr006_test.go) and `TestShortCoverageOnOneMonitorOmitsThePercentile`. ADR-006 removes the mechanism rather than policing it, and a removal is invisible: nothing in the type system stops the stored column going back into that `SELECT` while somebody tidies the two branches into one. The first test seeds the same p95 into **all four tiers** so a pass cannot be read as "the coarse tiers happen to be empty" — the column is populated and the query still has to decline it — and the 1m tier still answers, so the rule is about approximations rather than a blanket refusal. The second holds the gate the ADR is actually about: retention policy permits the figure, the monitor's raw rows were pruned behind the daily tier that summarised them, and the p95 is withheld rather than printed under a seven-day heading it does not cover. Both were checked by deleting the rule and watching them fail
- [x] The response-time SLI is described as *"met the response-time target"* and never as *"was slow"*, and the reason is now **on the report's face** rather than only in this checklist: the methodology note says that a check exceeding the threshold is recorded as down and is not separable, in stored history, from one that did not answer. That sentence appears only where a target exists, because a rule nobody set is noise on a page kept short on purpose. Held by [`sli_wording_test.go`](../../internal/report/render/sli_wording_test.go), which bans the word outright over the whole composed document — a blunt check nobody can argue with, and one that caught the word inside the very sentence added to explain the rule

## Rendering

- [x] The drawing primitive set — [`draw.go`](../../internal/report/render/draw.go): text run, rectangle, line, path, image, and nothing else. A test fails the moment a sixth appears, so adding one means deleting that line and meaning it. The text primitive takes a `Run` rather than a string (ADR-007 item 5), so a shaping layer inserts later without changing a caller; no alpha channel, because transparency costs a PDF `ExtGState` resource per value and a lighter colour does the job
- [x] SVG backend — [`svg.go`](../../internal/report/render/svg.go). Self-contained: no stylesheet, no script, no font reference, because an artifact has to render years from now from a saved file with no network. Text is escaped (a client called `Smith & Co <Ltd>` gets a report, not a parse error), stroke-only shapes state `fill="none"` rather than inheriting SVG's black default, and coordinates are rounded before printing so determinism does not depend on float formatting
- [x] The charts, written once against the primitives and drawn by both backends — [`chart.go`](../../internal/report/render/chart.go). **The latency line breaks at a day with no measurement rather than interpolating across it**, the Phase 1 monitor-chart rule carried onto the client's page; an isolated observed day still gets a mark, because a month with one measurement is not a month with none. The uptime strip draws an unobserved day **grey, not red**. A test drives one chart into two backends and asserts they receive identical calls, which is the whole of ADR-007 item 2
- [x] The bounded document model of ADR-007 item 3 — [`element.go`](../../internal/report/render/element.go): cover, heading, paragraph, key–value block, table, chart, footer, and a test that fails if `Compose` emits an eighth. [`compose.go`](../../internal/report/render/compose.go) is the one place the report's *face* is decided, and it discharges §4.3's two obligations there rather than in a template: **the denominator and the maintenance policy are on the face**, in a methodology note placed before any figure, because a denominator explained after the number has already been misread
- [x] The HTML renderer as the canonical output — [`html.go`](../../internal/report/render/html.go). Self-contained by construction: inline styles, inline SVG charts, a data-URI logo, `noindex`, and a test that fails on `<script>`, `<link>`, `url(`, or any remote URL. Canonical in ADR-007's sense — the format the PDF is *measured against*, never the one it is made from
- [x] CSV and JSON from the same model — [`render/`](../../internal/report/render/), 8 tests. CSV is one well-formed file with a `row_type` discriminator (`daily`, `monitor_total`, `estate_total`) rather than a header block above the data, because a CSV with a second table above the header is not a CSV. **A null is an empty field, never a zero** — the single most likely way a figure from this product ends up wrong in front of a client is a null uptime ratio charted as a day of total downtime. Numbers are formatted `'f'` rather than `'g'`: `9.99e-01` in a spreadsheet column is a support conversation
- [x] The JSON artifact is `ReportDocument` verbatim, no envelope, carrying `meta.schema_version`. Wire shapes are hand-written against the spec following [`internal/api/dto.go`](../../internal/api/dto.go) and its stated reasoning, so the domain types stay tag-free. Key names are pinned by a test, since a BI tool binds to them and outlives several releases
- [x] PDF backend over the same primitives — [`pdf.go`](../../internal/report/render/pdf.go) writes the objects, cross-reference table and content streams by hand; [`pdflayout.go`](../../internal/report/render/pdflayout.go) flows the seven elements onto A4 in one pass. Validated by something other than its own tests: **poppler parses the file, reports it as a 3-page A4 PDF 1.7, and extracts its text**, and the rasterised pages were looked at. Coordinates are converted from the primitives' top-left space at each emission rather than through a flipping CTM, which would mirror every glyph and then need a second flip inside every text object
- [x] **One embedded TrueType family — Roboto, regular and bold** (maintainer's choice, 2026-09-01), embedded **whole** as a CID-keyed Type0 font with `/FontFile2`, an Identity-H encoding and a `ToUnicode` CMap. Whole rather than subset is [ADR-007](../adr/007-report-rendering.md) item 4's own wording for the first cut; the line here previously said "subset" and the ADR wins. About 310 KB of the binary, against the ADR's budget of a few hundred kilobytes to a megabyte.

  Two properties were **measured from the files rather than assumed**, and both are now acceptance tests beside [`fonts.go`](../../internal/report/render/fonts.go). **The figures are tabular**: all ten digits share one advance width in both weights, so a column of percentages lines up on the decimal point — which matters more here than in most typography, because the PDF backend applies no OpenType features, `tnum` is unreachable, and a face with proportional figures would give every table a drifting decimal point while the HTML report's CSS lined its own up. And **the face covers what the report actually draws**, including the `×` of the burn rate, the en dash of a period range and the ellipsis of a truncated cell. Two further tests catch the mistakes that are invisible until somebody looks at a page: the bold face is genuinely heavier than the regular one (vendoring the same file twice), and a full sample report draws no `.notdef`.

  Licensed under the **SIL Open Font License 1.1**, with no Reserved Font Name. The licence text sits beside the files at `internal/report/render/fonts/OFL.txt` **and** is repeated in [NOTICE](../../NOTICE), because the OFL requires the licence to accompany the font wherever it goes — and here it goes inside a compiled binary, whose release archive ships `NOTICE` and not the source tree. **That NOTICE change is legal-facing and wants a human's eye**, which is the one part of this item an agent should not be the last reader of

- [x] Deterministic output — creation date and `/ID` derived from the run rather than the clock, now held for all four formats. Nothing in the PDF writer reads a clock or a random source: object numbers are assigned in emission order, page resources are sorted before they are written, colours and coordinates go through the same rounding formatter the SVG backend uses, and the `/ID` array is derived from the run id and the run's timestamp. A test renders the same model four times and compares bytes, and a second asserts that two runs *generated at different times* differ — determinism that cannot tell two runs apart is a constant, not a property
- [x] **The drift guard, which ADR-007 calls required rather than optional.** Two pieces: the golden HTML report, which stood up with the first renderer rather than after the second; and a test that takes every figure the composition produced and asserts it appears in **both** documents — reading the PDF back by decoding the glyph ids in its content streams through the face's `cmap`, which checks the mapping as well as the text. A figure present in one artifact and missing from the other now fails the suite rather than reaching a client
- [x] Table page-breaking — the time-sink the ADR said to budget for, and all four of its named hazards are handled and each is tested. Header repetition on every page a table reaches; the header never orphaned at a page foot, since it needs itself **and at least one row** or the whole table starts overleaf; widow control, so a continuation page is never a repeated header and one line; and a row taller than the page made impossible by clamping a cell to three lines with an ellipsis, because that row has to either overflow the margin or lose text and losing it visibly is the lesser harm. Every break rule was checked by **deleting it and watching the test fail** — two of the first drafts passed against the mutated code, because a fixture that cannot reach a page boundary tests nothing, and both were rewritten to drive the cursor across the boundary directly. Above them sits one invariant rather than a rule per element: **nothing is drawn below the bottom margin**, swept over sixty layouts
- [x] Print CSS on the HTML report as a complement, never as the substitute — page-break rules that keep a figure block or a table row from splitting, and a repeating table header. It does not pretend to be the PDF
- [x] Render failure degrades the run to `partial` with the reason recorded per artifact, and the formats that succeeded are still delivered. **Demonstrated against a running instance before the font was vendored** — the case was real on every build until then — and the same discipline now covers a full disk, which takes the identical path: from the client's side a storage failure and a render failure are one event, a format that did not arrive

## Branding

- [x] Brand profiles: logo, primary and accent colours, footer text, cover text, "prepared for" client name — [`report_brands.go`](../../internal/store/sqlite/report_brands.go) and [`brands.go`](../../internal/api/brands.go). A colour round-trips **exactly as written** rather than normalised, because it is a string somebody pasted from a brand guide. Marking a profile default demotes the previous one in the same transaction, so the unique index never refuses a write an operator meant
- [x] Referenced by report templates only. **Status pages keep their inline branding**, and the resulting three-way duplication — profile, status page, `settings.appearance` — is stated at the top of both the migration and the handler file rather than discovered. **Deleting a profile a live template names is refused with a `409` that counts what is in the way.** The foreign key would allow it and let the template fall back to the default; the spec refuses instead, and its reasoning wins — the fallback is invisible until an agency's client receives an unbranded document, whereas the refusal happens while somebody is looking at the screen. A *soft-deleted* template does not hold a profile hostage, because it renders nothing
- [x] Logo upload accepts PNG and JPEG and **refuses SVG at upload time with the reason and the fix** — "export the logo as PNG — at about 480 pixels wide it will be sharp on both the page and the print". The format is decided **from the bytes, not from the declared `Content-Type`**, which is the half that makes the refusal reliable: an SVG labelled `image/png` is exactly how one reaches the renderer, and browsers and curl both mislabel often enough that trusting the header would fail in the case this exists for. Nothing is stored on a refusal, so a profile is never left claiming a logo it has no bytes for
- [x] Branded text fields are plain text, and nothing downstream interprets markup in them — every backend escapes. It is a rendering constraint rather than a storage one and is recorded as such at both ends: the PDF writer has no rich-text pipeline, and a field that renders in HTML and not in PDF is worse than one that renders nowhere
- [x] The default profile derives from `settings.appearance`, so an install that never opens this screen still produces a report that does not look unbranded — the instance name and the dashboard's primary colour, and **nothing else**, because there is no logo, footer or client name in that section and inventing a footer would put words on a client's document that nobody wrote. A profile that exists but sets no colour still gets the instance's: creating a profile to set a client name should not silently undo the appearance somebody already chose. The colour reaches the page in **both backends at the same two places** — the rule under the cover title and the accent bar beside each figure — and deliberately nowhere else: the uptime strip's green, red and grey are the *legend* the caption names by colour, so a brand that recoloured them would produce a figure contradicting its own caption. That restraint is a correctness property rather than taste, and a test holds it
- [x] Brand resolved at generation time and denormalised into `meta.brand`, so a stored document renders standalone after the profile has been edited or deleted. An agency rebrands in June and every January report they ever sent still says what it said when it was sent; a reference would make "who was this prepared for?" a function of a mutable row, and the question gets asked precisely because somebody tidied the profile up. The copy is **narrower than the render-side type on purpose**: the accent colour and the cover text are not in the spec's `meta.brand` object, and `logo_url` stays null because no operation serves those bytes — the third recorded spec gap, held rather than invented around. An instance with no branding at all emits `null` rather than an object of empty strings, because "there is no branding" and "there is branding that says nothing" are different answers to a consumer

## Definitions, schedules, and runs

- [x] Templates, schedules and runs kept as three things, all three now built. That separation is what makes "re-send last month's", "regenerate it after we corrected the incident record" and "the PDF failed but the HTML went out" all expressible. **And the separation survives a deletion**: deleting a template soft-deletes its schedules in the same transaction and keeps every run, so a definition somebody tidied up does not take the record of what a client was sent with it — three things that vanish together are one thing wearing three names
- [x] The five-field cron parser moved out of `internal/maintenance` into [`internal/cron`](../../internal/cron/cron.go) and both callers use it. **The maintenance suite passes unchanged**, which is what makes it a move rather than a rewrite. Its tests moved with it and were split honestly: the ones that drive a cron *through a maintenance window* stayed in that package, because what they check is a question about windows; the parser's own tests came here. The rule most likely to diverge in a second copy is the one this keeps single — day-of-month and day-of-week are a **union** when both are restricted, so "0 0 1 * 1" is the first of the month *and* every Monday. The package gained `Next`, which is what schedules need and windows never did
- [x] Daily, weekly, monthly, quarterly and cron, each at `send_at` in the schedule's own timezone — [`schedule.go`](../../internal/report/schedule.go). **Every named frequency is expressed as a cron and answered by the shared parser**: `daily` is `0 9 * * *`, `weekly` is `0 9 * * 1`, `monthly` is `0 9 1 * *`, `quarterly` is `0 9 1 1,4,7,10 *`. The alternative was five arms of calendar arithmetic beside a sixth that called the parser, and the two halves would have disagreed about daylight saving — which is the thing this file exists to get right and the thing hand-rolled month arithmetic gets wrong. Weekly fires on **Monday** because `window.go` already cuts weeks from Monday, so the report arriving on Monday morning covers the week that just ended; monthly and quarterly fire on the first, for the period that just closed. Neither is configurable because the spec gives a schedule no weekday field — an operator who wants Thursdays writes a cron, which is what cron is for
- [ ] A bounded worker pool with an explicit concurrency limit, off the check path. **The pool is written and tested** — [`pool.go`](../../internal/report/runner/pool.go), two workers by default, a bounded queue that refuses rather than grows, and a `Submit` that does not block because the handler answering `202` must not wait for a worker. A full queue is a `503` naming the reason; an unbounded backlog would turn a bad morning into memory pressure and then into an OOM kill, taking the monitoring down with the reporting. **The box stays open because the exit criterion is the load test**: fifty PDFs at 09:00 on the 1st must not delay a single check, and *the load test says so rather than the author*
- [x] Generation is re-runnable, and **the run's recorded window wins over the template's period** — which is what makes it first-class rather than approximate: a report regenerated after a correction covers the same window as the one it replaces, not whatever "last month" resolves to on the day somebody presses the button. Both artifacts are kept, because a run is a separate row and nothing overwrites another. The determinism half is held by the renderers, which take `now` and the run id as parameters and read no clock
- [x] `late` is set and shown, and **demonstrated on a running instance**: a schedule backdated half an hour produced a run with `late: true` and a warning naming how far behind it was. The threshold is fifteen minutes, because ticks are a minute apart and a busy pool adds a few more — marking ordinary jitter late would make the flag meaningless on the screen where it matters. **A missed schedule fires once, not once per missed period**: the next firing is computed from *now* rather than from the firing that was missed, so an instance down for three days owes a daily client one report rather than three copies of yesterday's arriving as an apology
- [x] `partial` is a real state end to end — one format produced, another not, both halves visible. **Demonstrated on a running instance**, not reasoned about: a template asking for all four formats produced JSON, CSV and HTML, failed the PDF for want of an embedded font, and left the run `partial` with a failed artifact row carrying its own reason. Asking to download that format is a `409` naming the cause rather than a `404`. Collapsing it into `succeeded` or `failed` is how somebody concludes a delivery went out whole
- [x] A run cancelled mid-flight leaves no half-written artifact — and, the half that is easy to miss, **leaves nothing at `running` either**. The first is structural: `artifact.Write` renames a complete temporary file into place, so no partial file ever exists at a real path. The second is a decision. Cancellation is checked **between formats and never inside one**, because abandoning a half-rendered PDF is the only way to produce the artifact this promises never to leave; what it buys is that the next format is not begun and the run is recorded as interrupted. The formats that completed are kept — each is a whole file with a committed row and a digest, so `partial` where anything landed and `failed` where nothing did — and `FinishReportRun` runs on a detached context, because using the context that was just cancelled would guarantee the row was never written. A restart finishes what a crash left at `running`, **at start-up rather than on a timer**: no worker has begun, so every such row belongs to a process that is gone, where a threshold sweep would need to be long enough not to kill a slow CSV over five thousand monitors and would leave a stuck run looking live for exactly that long

## Artifact storage (ADR-008)

- [x] `<data-dir>/reports/<yyyy>/<mm>/<artifact-id>.<ext>`, directories `0750` and files **`0600`** — [`internal/artifact`](../../internal/artifact/store.go). The line here previously said `0640`; [ADR-008](../adr/008-report-artifact-storage.md) item 3 says `0600`, "consistent with the `0600` the root key is written with", and the ADR is the one that is immutable. Checked on the disk rather than on the return value
- [x] **The on-disk name derives from the artifact id and format, never the template title.** There is no sanitisation step to get wrong because there is no user input on the path — a stronger property than any amount of escaping, and the test says so in those terms. `Open` and `Remove` clamp to the root anyway; the naive `filepath.Join` version of that function opens *and deletes* the database file, which is what the test catches
- [x] **Write the file, fsync, then commit the row** — in that order, and the directory entry is fsynced too, because a rename is not durable until its parent is. Bytes go to a temporary name and are renamed in, so a file at the final path is always complete: a crash mid-write cannot leave something that reads as a real artifact to anything short of the digest
- [x] SHA-256 and size on every row, and the digest is offered to the downloader as `X-Cairn-SHA256`. Verified end to end against `shasum -a 256` of the file a client actually receives. This is what makes an artifact evidence rather than a file: "is this the document we sent?" is answerable, and a truncated write from a full disk is detectable rather than served silently
- [x] The orphan sweeper, for the files the ordering above deliberately leaves behind — [`sweeper.go`](../../internal/report/runner/sweeper.go), running hourly from `app.go`. The part worth keeping is the grace period: ADR-008 writes the file before the row, so there is always an interval in which a good artifact has bytes and no row, and a sweeper with no grace races a running report and deletes the file out from under it — the failure then looks like a disk fault rather than like a bug in the sweeper. It is a correctness requirement, not a tuning knob, and the test puts a file **exactly where a racing write would put one** and asserts it survives while two-hour-old residue beside it does not. Deleting the grace period makes that test fail. A `.partial` from a crashed write is reclaimed too, which is the case a sweeper that only considered known extensions would leak one of per crash, forever
- [x] Retention on `report_artifact_days`: bytes reclaimed, row kept as an `expired` tombstone, `410` on the download path. **The value is read from the settings row on every run**, which closed real drift — it was validated on the settings surface and the runner used a compiled-in constant, so an operator could set thirty days, watch the API accept it, and find a year later that nothing had ever expired. The rollup runner already reads its retention on every pass for this reason and this follows it; the report's *tiers* now come from the same read, so a document is labelled with the resolution the current policy permits rather than one a restart ago allowed. The delete order is **row-then-file**, the mirror of the write: a crash between the two leaves an orphan for the second pass, where the reverse leaves a row promising bytes that are gone. Both orders are held by tests that fail when swapped
- [x] A per-artifact size cap enforced with an error naming the limit and the size reached — "csv is 4.0 KB, limit is 1.0 KB", asserted on all three of those. The case that hits it is a CSV over 5,000 monitors for a year — roughly 1.8 million daily rows — not a PDF. A refused write leaves no residue for the sweeper to find
- [x] Disk-full and write failure degrade the run and record the reason rather than aborting the schedule, taking the **same path a render failure does** — from the client's side they are one event: a format that did not arrive. Tested by making the artifact store fail on one format and asserting the other still shipped and the run's error carries "no space left on device"

## The S3 client, the mirror, and the drop

**Built 2026-09-03 under an explicit maintainer waiver of rule 8, and that waiver is the first thing a reviewer needs to know.** This is agent-written crypto and access-control code. It is tested, and one of those tests checks the signer against AWS's own published PUT vector rather than against itself — but a passing test is not the same as a person who can defend the code at 3am, which is what [AGENTS.md](../../AGENTS.md) exists to protect. **These boxes record that the code works, not that it has been reviewed.**

- [x] SigV4 over the standard library — [`internal/s3`](../../internal/s3/sigv4.go), about 200 lines against a vendor SDK's dependency tree, for a client that touches four verbs. Static credentials only: no instance profiles, no STS, no credential chain. **The assertion that makes this trustworthy is the vector test** — every other test in the package compares the code against itself, which a consistent misreading of the specification would pass, so the expected signature comes from AWS's "Example: PUT Object" walkthrough. Headers are signed by prefix (`x-amz-*`) rather than from a fixed list, which is why server-side encryption needs no second code path, and `canonicalURI` does its own percent-encoding because Go leaves `+`, `=` and `:` alone in a path where S3 signs them encoded — a key that transmits one way and signs the other is a 403 that names no byte
- [x] Selectable path-style addressing and an overridable endpoint, both demonstrated against a real MinIO. The path-style switch is not cosmetic: without it the request goes to `<bucket>.<endpoint>`, which needs DNS and a certificate a self-hosted server generally does not have, and the failure is a TLS error naming a hostname the operator never typed
- [x] Server-side encryption headers passed through — verified in the honest direction: MinIO with no KMS refused an `AES256` upload with `NotImplemented`, which is the header arriving and being acted on. The non-public-bucket requirement is stated **on the settings screen** as well as in [reporting.md](../guides/reporting.md) and [backup-restore.md](../operations/backup-restore.md), because a misconfigured bucket is a breach with no code defect behind it and documentation nobody opens is not a defence
- [x] The **mirror** is a durability copy of every artifact and its failure is recorded rather than fatal — [`mirror.go`](../../internal/report/runner/mirror.go). Demonstrated end to end against MinIO: three formats uploaded under the same relative paths they hold on disk, with the bucket copy, the local copy and the row's `sha256` all matching. Then the bucket was removed and the same schedule run: the run still `succeeded`, every artifact stayed `rendered` and downloadable, and each carried `mirror.state = failed` with the provider's own message. **It is resolved per run rather than at start-up**, which was a real correction — the first cut built the client in `app.go` and reintroduced exactly the drift that `report_artifact_days` had just been moved out of: an operator enables the mirror, the settings surface accepts it, and nothing is uploaded until somebody happens to restart. Enabling it now takes effect on the next report, which was demonstrated in one process
- [x] The **drop** is a delivery target for one run's files, sharing a client with the mirror and nothing else. Kept apart by name in the model, the settings section, the schedule form, [reporting.md](../guides/reporting.md) and a comparison table there whose whole purpose is that configuring one when you meant the other looks like success from the outside. The key layouts differ on purpose: the mirror reproduces the on-disk path because its reader is a restore, and the drop is named from the template and period because its reader is a person
- [x] The guard: **the root key must not be written to the same bucket as a database backup.** Stated on the settings screen, in the reporting guide, and in [backup-restore.md](../operations/backup-restore.md). Remote backup of database and key remains Phase 4 and is not built here
- [x] **An opt-in live test against a real provider** — [`live_test.go`](../../internal/s3/live_test.go), skipped unless `CAIRN_S3_TEST_ENDPOINT` is set so that `go test ./...` on a laptop with nothing running stays green. It is what the vector test cannot be: a wrong secret is *refused* by a server that actually checks it, which is the only way to tell a verified signature from an ignored one. Run against MinIO on 2026-09-03, including a key containing `+`, a space, `=` and `:`

**One thing was deliberately not built, and it is a gap an operator can be hurt by.** Nothing retries a failed upload and nothing reconciles the bucket against the database — ADR-008's Consequences say so in as many words, so this follows the ADR rather than departing from it. The cost is that a mirror which has been quietly failing for a fortnight looks exactly like one that is working, from everywhere except the artifact rows. `backup-restore.md` was corrected in the same change: it previously said the local copy "may be skipped" where the mirror is enabled, which was written before there was a mirror to be wrong about. It now says the opposite, and names the query to alert on.

## Delivery

- [x] Email through the configured relay, Slack through the existing provider, webhooks authenticated by the channel's own headers, and the S3 drop refused with its reason — [`internal/report/delivery`](../../internal/report/delivery/delivery.go). **A report is a payload, not a new transport**, and that sentence is the whole design: email reuses the SMTP conversation alerts and status-page bulletins already share, and Slack and webhooks read a notification channel's configuration rather than a copy. What is genuinely new is a multipart body — the mail package's own doc said "one text/plain part with no attachments", which was true until a report needed to travel as a file, and the split is deliberate because an alert with an attachment is a worse alert. **Attached rather than linked**, because a link needs a share link and a link to an authenticated endpoint is one the client it was sent to cannot open. Slack gets an announcement rather than a file, honestly labelled as one: an incoming webhook cannot carry an upload, and the message neither claims an attachment nor offers a URL that would ask the reader to log in. **Signing is the fourth spec gap**, recorded above rather than papered over
- [x] Delivery decoupled from generation: a run produces artifacts and finishes, and handing them over is a separate step hanging off the pool. **A delivery failure is not a run failure** — the report exists, is on disk with a digest beside it, and can be downloaded and re-sent; marking the run failed because a mailbox was full would tell somebody the document does not exist when it does. Retry is three attempts rather than the dispatcher's twenty, on purpose: an alert is worth retrying hard because its value decays in minutes, and a monthly report's does not decay at all. An **ad-hoc run writes no delivery rows at all** — a "run now" is a report somebody is about to download, and a log full of entries saying nobody asked for this to be sent would bury the rows that matter
- [x] Every attempt recorded with its outcome, count and error — **including the ones that will be retried**, because recording only the last would make "it took three goes tonight and two last month" invisible, which is the shape of a problem about to become an outage. A **skipped** delivery is a row and is not a failure: no relay configured, or nothing rendered in a format this target takes. And the recorded target is redacted before it is stored — a Slack incoming webhook URL *is* the credential, the path is the secret, and this log is read on screen and pasted into support conversations
- [x] A delivery that names a notification channel uses the channel's configuration rather than restating it, so a rotated Slack token is rotated once. Read at delivery, never copied at configuration: a delivery holding its own copy would keep working after a rotation and then, when the old credential was revoked, fail in a way nobody would connect to the rotation. A disabled channel is a **skip** rather than a failure, because somebody turned it off on purpose. The reserved headers go on last, so a configured header can add to a request and can never replace the run's identity — a receiver whose deduplication key is settable by a typo in a settings field has no deduplication

## Share links

**Built 2026-09-03 under the same explicit maintainer waiver of rule 8.** The token in the URL is the whole of the authorisation on an unauthenticated path, which is precisely the code rule 8 puts in a person's hands. **These boxes record that it works, not that it has been reviewed**, and this is the section to read most adversarially.

- [x] Unguessable token on an unauthenticated read path — 256 bits from `crypto/rand` via the existing `auth.NewToken`, well past ADR-008's 128 — stored twice, hashed for lookup and sealed for replay, following `Subscriber`'s unsubscribe token exactly. The sealed envelope is AAD-bound to its own row, so a ciphertext lifted from one link fails to open against another rather than opening as somebody else's credential. Looked up **by hash against the unique index**, so a guess costs one indexed probe rather than a walk, and the plaintext never reaches the store layer
- [x] A **separate public projection** — `publicReportJSON`, assembled field by field, not a filter and not a struct embedding the private shape. The guarantee is structural: a field added to `reportRunJSON` next year cannot appear here without somebody typing it into `publicReport`
- [x] Revocable, optionally expiring, `noindex`, and rate limited. Revocation is immediate, is a column rather than a delete so a withdrawn link answers `410` instead of looking like a typo, and leaves the artifacts untouched. A second live link is a **409 rather than a silent replacement** — quietly revoking a link a colleague already sent to a client is a support call that begins "the report link you sent me stopped working". `X-Robots-Tag: noindex, nofollow` and `Referrer-Policy: no-referrer` go on the refusals as well as the successes, because a 410 naming a client's report is still a page
- [x] **A share link serves the stored artifact, never a re-render**, and the demonstration is a byte comparison: the public download and the authenticated download of the same artifact are identical, and both match the `sha256` on the row
- [x] The golden-path assertion the status page taught, and it passes: a monitor was created with the target `https://secret-internal-host.acme.invalid/health`, a report was shared, and the string appears nowhere in the public response. It holds **structurally rather than by filtering** — `ReportDocument` has no field for a monitor target at all

**The rate limit was wrong on the first cut and a live run caught it in one command.** Reusing `loginLimiter` gave the public path five requests per fifteen minutes: right for credential guessing, absurd for a document. A client who opens the report, downloads two formats and refreshes had already spent the budget, and the sixth request told them to go away — on a link somebody had sent them. It now has its own limiter, keyed per token, and a test asserts the ordinary journey twenty times over rather than asserting the limit.

**One tension in the frozen spec had to be resolved, and it is recorded rather than resolved quietly.** `PublicReport` is described as carrying "no run id, no template id, no delivery log and no monitor identifier" — and it also carries `document`, which is `ReportDocument`, whose `meta` holds `report_run_id` and `report_template_id`. Both statements are in the spec and they disagree. It was resolved in favour of the document, because the spec *also* mandates `/download?format=json`, which serves the stored artifact byte for byte: those identifiers are one query parameter away regardless, so stripping them from the inline copy would buy nothing and would make the inline document differ from the file — the one property ADR-008 item 15 will not have. `TestTheSharedDocumentIsTheStoredDocument` exists so the decision is visible and fails loudly if the document's shape ever grows a target. **This is a maintainer's call to confirm or reverse.**

## Report types

- [x] **SLA/SLO** — target versus actual, error budget consumed and remaining, burn rate, breach log with timestamps, and the denominator and maintenance policy printed on the report face. Demonstrated live from the Month 4 checkpoint onwards; the figures verify by hand
- [x] **Uptime summary** — the default a solo user gets, and it now carries **no SLA block even where the monitors in scope have targets**, which is what makes the promise real rather than accidental. It was previously true only because a solo user had set no targets; a monitor that had one would have produced error-budget vocabulary on a report that never asked for it. There is a second use for the rule that is worth as much: an agency running an uptime summary for a client does not publish the internal target it set on that client's monitors. Choosing the type is the choice, and a test drives the same fixture through both types to show the omission is about the type rather than about the data
- [x] **Incident post-mortem** — drafted from the incident timeline, with MTTD/MTTA/MTTR per incident and the means across them. **The arithmetic is `model.Incident.Metrics()` rather than a second copy of it**, which found a real defect while it was being wired: the first cut computed time-to-detect *backwards*, and the test had been written to match the bug. Two implementations would have shipped an incident screen and a post-mortem disagreeing about one outage's time to resolve, with nothing in either document to say which was right — a test now asserts they agree. The aggregate carries **how many incidents supplied each mean**, because "22 minutes, from one incident of nine" is a very different claim from "22 minutes". `alerts_fired` is reported as unknown rather than zero: the delivery log is not on this package's read-side contract, and zero would read as *nobody was told*, which is one of the more serious findings a post-mortem can carry and not one a missing query should manufacture
- [x] MTTD is **reported as unknown when it is unknown**, and the live run made the point better than any test: two real incidents produced `mttd = unknown`, `mtta = unknown` and a real time to resolve, and the rendered page reads *"Mean time to detect — unknown — not recorded on any incident in this period"* rather than a dash or a zero. A zero there would claim the outage was noticed the moment it began. A **negative** interval is unknown too: incident timestamps are editable, so an ordering nothing enforces is a fact about somebody's typing rather than about the outage
- [x] **Comparative** — period over period, monitor against monitor, group against group. Region against region is shaped for and **absent rather than present-and-empty**: a fourth mode returning nothing would read as a broken feature rather than an unbuilt one. It costs **no new store method** — every mode is `WindowTotals` and `DailySeries` asked a second time with different arguments — which keeps a comparative report over a large estate at two batched reads per series rather than two per monitor. The previous period is the **same length placed immediately before**, not the previous calendar period: February beside March would put 28 days against 31 and make every count differ for reasons that are about the calendar rather than about the service, and the report says so in a caption. Demonstrated live over seeded history: March 8,620/8,928 = 96.550% against 7,980/8,064 = 98.958% for the 31 days before it, both verified by hand
- [ ] **Certificate and domain expiry calendar** — the collection is built and queryable (`/api/v1/expiries`, above), with the 7/30/90-day horizons expressible as `within_days`. What is not built is the calendar as a **report type**: a `ReportDocument` has no element for an expiry table, `report.Store` deliberately has no method for one, and adding it means a document shape and a composed page rather than another query. It is the smallest of the remaining report types and the data is now one call away
- [ ] **Custom builder** — the `custom` type computes everything, and `sections` is stored and round-tripped. What is missing is the half that makes it a *builder*: `sections` does not yet choose which blocks the composition emits, so a custom template renders the full document rather than the selection. It is a change to `Compose` and a control on the template editor, and it is deliberately the last of the five because the other four are what a scheduled report actually is

## REST API

Spec-first is unchanged and is not softened for this phase: the surface below is
already in the frozen spec, and no handler reshapes it. Anything that needs to
change goes through [COMPATIBILITY.md](../api/COMPATIBILITY.md) §2 first.

- [x] `/api/v1/report-templates` — list, create, get, update, delete. `PATCH` tells **absent from null**, which needed `json.RawMessage` rather than the `**T` the first cut used: `encoding/json` flattens both to nil, so the double pointer compiles, looks right, and quietly makes "remove this SLA target" impossible. A test that patches a null and reads the value back is what found it
- [x] `/api/v1/report-templates/{id}/generate` — `202` with a run to poll, per the spec, queued onto the bounded pool rather than rendered inside the request. An instance with no worker answers `501` rather than recording a run nothing will execute: a row stuck at `queued` forever reads as a hung report rather than as a missing feature
- [x] `/api/v1/report-schedules` — list, create, get, update, delete, with `next_run_at` computed on every write and **a schedule that will never fire refused at write time rather than discovered by its silence**. "0 0 30 2 *" parses cleanly and matches nothing; a zone that does not exist is refused by name rather than falling back to UTC; a cron on a named frequency is refused rather than stored and ignored. An **s3 delivery target is refused with the reason** — the SigV4 client is not built, and accepting a `secret_access_key` that nothing can use would leave an operator believing a credential was saved
- [x] `/api/v1/report-runs` and `/{id}` — cursor-paginated history with artifacts, deliveries and `late`, filtered by template and by state. Artifacts for a page come back in **one query rather than one per run**. Share state waits on share links, which are human-led work (AGENTS.md rule 8). The list keys on `created_at`: a run has no `updated_at`, and a state change an hour later has not made it newer history
- [x] `/api/v1/report-runs/{id}/download` and `/artifacts/{artifactId}` — both paths, with the right content type per format, a `Content-Disposition` naming the artifact id, and the digest in a header. `410` on an expired tombstone and `409` on a format that failed to render, each naming what happened; an artifact addressed under the wrong run is a `404`, because the pair is the address and honouring a mismatch would make the run id decorative on a path a share link resolves
- [x] `/api/v1/report-runs/{id}/share` — create and revoke
- [x] `/api/v1/public/reports/{shareToken}` and `/download` — the unauthenticated pair, on the public projection. **The four lines naming these in the contract test's skip list are deleted**, which was always the last step of building them, and the test now exercises all four against the running handlers rather than explaining why it cannot
- [x] `/api/v1/brand-profiles` — list, create, get, update, delete, plus `/logo` upload. `413` over a megabyte, `415` on a format the PDF writer cannot embed, `409` on a delete something still uses. **`logo_url` is null and stays null**: the spec defines the field and defines no operation that serves the bytes — `PUT .../logo` exists with no `GET` beside it — so a URL there would name an endpoint answering `405`, and inventing the `GET` is not an agent's to do ([AGENTS.md](../../AGENTS.md) rule 4). It is the third spec gap this phase has turned up and is listed with the others below
- [x] `/api/v1/expiries` — the expiry calendar as a queryable collection, over data that has existed since migration `0003`. Two deliberately different tables — a registration has a registrar and a source, and no subject, chain or serial — unioned into one ordered list **here rather than in the schema**, because the calendar is the only reader that wants them together. It carries `monitors:read`, which is the spec's choice and the right one: a key that can see a monitor can already read its certificate one at a time. Two decisions worth the line. `within_days` bounds the **future only**: something that expired eleven days ago is the most urgent row on the page, and filtering it out would leave the screen looking calm on the worst possible day — so `days_remaining` is signed rather than floored, and −11 is a real answer. And it rounds **towards** the expiry, so twenty-three hours left reads as zero days rather than one, because "one day left" on something that dies before tomorrow's stand-up is how a renewal gets missed. The plan is stated honestly rather than claimed clean: a `SEARCH` on each table's expiry index with a temporary B-tree for the monitor-id tiebreak, which sorts only rows sharing an expiry millisecond
- [x] `settings.retention.report_artifact_days` **is on the settings surface, and is now actually read** — validated with a minimum of zero, which means "keep indefinitely" as every other field in that section does, and deliberately **not** fed into the rollup runner's coherence check, because an artifact is not a tier and is expected to outlive the data it was computed from (ADR-008 item 6). `settings.report_storage` and `secret_access_key_set` are not, and wait on the S3 client
- [x] `brand_profiles:read` / `brand_profiles:write` added to the API key scopes and enforced on all six operations (maintainer, 2026-08-31). It turned out to be **drift rather than a decision**: both scopes were already in the frozen `ApiKeyScope` enum, and [`scope.go`](../../internal/auth/scope.go) states its own policy — "scopes for unbuilt features are listed too… rejecting it now would make every such key a migration later" — which `reports:*` follows and these two did not. A key naming `brand_profiles:read` was therefore refused by a file that says it should not be. **Nothing tests that `AllScopes` matches the spec's enum**, which is why it went unnoticed; that test is worth writing and is not written
- [x] Cursor pagination and RFC 9457 problem details unchanged across the new surface. The expiry calendar is the one that had to think about it: its keyset is `(expires_at, monitor_id)` rather than the `(updated_at, id)` the shared cursor type's field names suggest, so the type is reused and what its two components *mean* is the collection's business. The one collision it cannot see — a single monitor holding both a certificate and a registration expiring in the same millisecond — is stated at the query rather than defended against with a third cursor component on a type every collection shares
- [x] Go and TypeScript clients regenerated in the same PR as any spec change, and never committed — **vacuously, because nothing in this turn changed the spec.** Every gap found was recorded rather than closed, which is the rule: `slo_target_percent`, `comparison`, `meta.brand` and `/api/v1/expiries` were all already in the frozen surface and had no Go half

## UI

- [x] Reports list and template editor, with progressive disclosure holding — and the line is drawn at the **report type** rather than at an "advanced" toggle. Choosing `uptime`, the default, hides the SLA target, the error-budget vocabulary and the comparison entirely, because none of them applies to that report; choosing `sla` reveals the target and `comparative` reveals the comparison. An advanced section is a place fields go to be ignored, while the type is a decision the user has already made and every revealed field follows from it. **The brand picker is absent rather than empty** on an instance with no profiles: an empty picker is a feature that looks broken, and its absence is a feature nobody has set up — the report is still branded, from the instance's own name and colour
- [x] Run history with state, `late`, per-format artifacts, and the delivery log with its last error on the run itself. Three things it makes legible that a single status word hides: **`partial` is a real state** — collapsing it into "succeeded" is how somebody concludes a delivery went out whole, and into "failed" hides three good documents; **`late` is a fact about the schedule rather than about the run**; and an **`expired` artifact is a tombstone rather than a missing file**, so it is labelled instead of offered as a link that would answer `410`. The delivery log comes from the **single-run read** rather than the list, which is the server's own decision — fifty runs each carrying their deliveries is a query fan-out per row for a panel nobody has opened — and that was a real defect in the first cut of this screen, caught against a live instance
- [x] Download and the **share link** are built. A link per rendered artifact, with the failed and expired ones labelled rather than offered; and a share panel per run that creates a link with an optional expiry, shows it **once** with a copy control, and thereafter reports only that a link exists, when it expires and whether the recipient has opened it. The URL is held in component state and nowhere else — a reload loses it, which is intended rather than a limitation: a screen that can re-display a live credential leaks one the first time it is screenshotted or pasted into a ticket. Each artifact also carries its offsite state, deliberately in the muted colour rather than the failure colour, because a bucket that was briefly unreachable must not make a perfectly downloadable report look damaged

- [ ] **Preview is still not built.** It needs a viewer for an HTML artifact, and the control has nothing to call. Unchanged by this pass and named here so it is not read as done alongside the share link it used to be listed with
- [x] Brand profile screen, with the SVG refusal explained where somebody is holding an SVG — and the server's own sentence shown verbatim rather than replaced by the client's, so the two cannot drift. The `409` on deleting a profile in use is surfaced the same way, because the count of what is in the way is the useful part. One list with an inline editor rather than three routes: there are rarely more than a handful of these, and the job is one job
- [ ] The custom builder — metrics, grouping, window, saved as a template. Waits on `sections` actually selecting blocks in `Compose`, which is the item above in the report-types section
- [x] Expiry calendar screen — 7/30/90-day horizons, and **an expired entry sorts to the top with a negative count** rather than being filtered out by the horizon, because that is the row somebody opened the page to find and hiding it leaves the screen looking calm on the worst possible day. A row observed more than a week ago is flagged, since a calendar built on a stale observation can be confidently wrong
- [ ] **Full historical browsing with drilldown into arbitrary past ranges, always labelled with the resolution actually used.** The document already carries `meta.resolution` — tier, whether it was downgraded, and where coverage starts — so the labelling has a source; what is missing is the browsing screen itself. This is where the Phase 1 complaint about being week-blinkered is answered in the UI, and the labelling is the part that makes it honest rather than the drilldown

## Docs

- [x] [Reporting guide](../guides/reporting.md) — the three things kept apart and why, the five types, scope resolved at run time, windows and zones, the resolution table, formats, branding, schedules, delivery, and where the files live
- [x] **[SLA methodology page](../guides/sla-methodology.md)** stating exactly what counts as downtime, what leaves the denominator, and what the maintenance default is — the page that gets read when a figure is disputed, and it is written to be handed to somebody who disagrees with a number. It carries the worked 90/200 example three ways, the three maintenance percentages side by side, why there is only one percentile, and the short paragraph the report itself prints
- [x] [Brand profile setup](../guides/brand-profiles.md), including why a logo must be raster — and the part that matters more, why the format is decided from the bytes rather than from the declared `Content-Type`: an SVG labelled `image/png` is exactly how one reaches the renderer
- [x] S3 configuration, with the drop and the mirror separated and the non-public bucket requirement stated — [reporting.md](../guides/reporting.md) grows two sections: a comparison table whose whole purpose is that configuring one when you meant the other looks like success from the outside, and the three fields that actually go wrong (`region` required for the signature even where the provider ignores it, `path_style`, `endpoint`). The non-public-bucket requirement is on the settings screen as well as in the docs, and so is the key-beside-the-backup guard

- [x] Share links documented — what is shown once and why it cannot be shown again, the three answers (`404`, `410`, `429`) and why "it is gone" and "it was never here" are different facts, one live link per run and why a second is refused rather than replacing the first
- [x] The retention-versus-resolution table, in [reporting.md](../guides/reporting.md), so an operator can predict what a report over last March will contain — with the three gates on the one percentile beside it
- [x] **The revised backup procedure**, tested end to end rather than written — the reports directory is now part of it, and the drill added two things to the page that were not there: a **row-to-file consistency check** for the snapshot (run in both directions — silent on a complete backup, naming the path on an incomplete one), and the correction that the S3 mirror is a second copy rather than a substitute for the local one, since nothing retries a failed upload

- [x] **The two documented failure modes, confirmed rather than assumed.** Restoring without the key refuses to start with the exact message the guide prints and **does not generate a replacement** — the failure that would otherwise leave every stored credential unreadable while appearing to work. And restoring `cairn.db` *without* `reports/` leaves an install that starts, lists every run, and answers the download honestly

## Quality gates and scale

- [x] Contract tests green for `x-cairn-phase: 2`, in both directions: every Phase 2 operation has a handler, and nothing is served that the spec does not describe. The floor that stops the test passing vacuously is now **per phase** — a single total would let Phase 2 drop to zero without the number moving much
- [x] Golden-report regression — [`testdata/golden_report.html`](../../internal/report/render/testdata/golden_report.html), standing up **with the first renderer** as ADR-007 requires, so the PDF backend arrives with something to be measured against on its first day. `-update` rewrites it; the failure message says to read the diff rather than accept it
- [x] Determinism test: the same model rendered twice is byte-identical, held for all four formats. The PDF is the one that could have been fudged and is not — object numbers assigned in emission order, page resources sorted before writing, no clock and no random source anywhere in the writer
- [x] Denominator tests over maintenance, unknown and skipped, written before the code they check — [`uptime_test.go`](../../internal/report/uptime_test.go), nine of them, and the reasoning is in the tests rather than only in the code: 90 up, 10 down and 100 unmade checks is 90% over half the window, never 45% and never 95%; one bucket yields 80%, 90% or 40% under the three maintenance settings; and excluding maintenance deliberately does not improve `unobserved_share`
- [ ] **The 5,000-monitor gate extended: 50 concurrent report runs with check scheduling latency unchanged.** A regression blocks merge, not release. This is CI configuration and needs reviewing as such (AGENTS.md rule 7)
- [x] The failure paths, each demonstrated rather than reasoned about. Renderer failure falls back with the reason recorded (live, before the font was vendored). Delivery failure retries and surfaces — three attempts against a receiver that answers 502 twice, three rows, the third a success. A missed schedule is late and visible, with `behind=30m43s` in the log. **A cancelled run leaves nothing half-written**, and a restart finished a run left at `running`: the live instance was killed mid-run, restarted, and logged `finished report runs interrupted by a restart runs=1`, leaving the run `partial` with a stated reason and its three artifacts kept. A full disk degrades one run rather than the schedule, tested by making the artifact store fail on one format
- [x] Crash-consistency — [`crash_test.go`](../../internal/report/runner/crash_test.go). The *state* a kill at that instant leaves is reproduced rather than the kill itself: a test that forked and SIGKILLed a child would be testing the harness, and the only observable difference between the two orderings is the file present with the row absent. Three assertions, each a different failure — the file at the final path is **complete**, because the rename makes a partial one impossible there; **no row refers to it**, which is what makes it an orphan rather than a dangling row; and the sweeper reclaims it once past the grace period, so the residue is bounded rather than permanent. A second test states the harm the ordering avoids as a checkable claim rather than a comment: a dangling row is **invisible to the sweeper**, because walking the disk cannot find a missing file — a different fault with a different answer, and one that deleting nothing can fix

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
