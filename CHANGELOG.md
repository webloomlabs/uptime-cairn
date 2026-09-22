# Changelog

All notable changes to Uptime Cairn are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Entries are written for the person deciding whether to upgrade: what changed for
them, not which files moved.

## [Unreleased]

## [1.1.0] — 2026-09-22

The Phase 2 release: reporting, stable. It completes the Phase 2 scope. The
reporting subsystem itself is described under
[1.1.0-beta.1](#110-beta1--2026-09-04) below; this section is what changed since
that beta.

**This is the tag that freezes the reporting API.** Per
[COMPATIBILITY.md](docs/api/COMPATIBILITY.md) §1 the `/api/v1` compatibility
promise now covers the reporting operations — report templates, schedules, runs,
artifacts, share links, brand profiles and expiries. From here they change
compatibly or not at all.

**Upgrading** from `1.0.1` or `1.1.0-beta.1` is a binary or image swap. From
`1.0.1`, migrations `0008` and `0009` run at start-up; from the beta they are
already applied and nothing new runs. Reports are written to
`<data-dir>/reports/`, which now belongs in your backup beside the database —
see the [backup and restore guide](docs/operations/backup-restore.md).

### Added

- **Certificate and domain expiry calendar in reports.** A `custom` report can
  now include a **Certificates and domains** section: everything in the report's
  scope expiring within 90 days, soonest first, with anything already lapsed at
  the top carrying how long ago it went. This is the same collection
  `/api/v1/expiries` and the Expiries screen read, so a report and the screen
  cannot disagree about what is running out. It appears in the HTML and the PDF
  when the section is selected, and in the JSON artifact as `expiries` — the
  field the document schema has always declared and nothing filled.

  The section is offered on `custom` templates only, because that is the only
  report type whose document carries a calendar. A template switched away from
  `custom` drops the selection rather than keeping a block nobody can see to
  remove.

- **Preview a rendered report without downloading it.** Run history now offers
  **Preview** beside **Download** on an HTML artifact, showing the report inline.
  It serves the _stored_ artifact rather than re-rendering, so it answers the
  question people actually bring to it — what did we send? — and the figures
  cannot have moved since the client got theirs. The document is displayed in a
  fully sandboxed frame: no scripts, no access to the dashboard's session, its
  own opaque origin.

- **Browse history over any past range.** A monitor's availability chart gains
  90-day and 1-year presets and a **Custom range** control taking explicit start
  and end times, so "what happened in March" is now a question the dashboard can
  answer rather than only "how has it been lately".

  Every chart now states **the resolution it was actually drawn at** — per check,
  one minute, five minute, hourly or daily. The tier is chosen from the span, so
  a year of history is daily buckets and last week is far finer; a reader
  comparing the two without being told would be comparing different measurements.
  Where a range reaches further back than the install holds, the chart says when
  the data actually begins instead of drawing an outage-free stretch that is
  really an install that did not exist yet.

- **The load-test gate now covers report rendering.** The 5,000-monitor gate
  fires 50 concurrent report generations at its largest scale and asserts that
  check scheduling is unaffected — the report worker pool's exit criterion, which
  was previously an argument in a code comment. A regression blocks the merge.

- **Three native notification channels: Pushover, Mattermost and Google Chat.**
  Each was reachable before only indirectly — through Apprise, or for
  Mattermost its Slack-compatible webhook; native, they get config validation
  when saved, a test-send, message templates, and no dependency on the
  `apprise` binary. Secrets — Pushover's `api_token` and `user_key`, the
  Mattermost and Google Chat `webhook_url` — are encrypted at rest like every
  other channel's. Pushover takes an optional `priority` (-2 to 1), `sound` and
  `device`; Mattermost an optional `channel`, `username` and `icon_url`. The
  Uptime Kuma importer now maps all three, where it previously reported them as
  unsupported. See the [alerting guide](docs/guides/alerting.md). The
  channel-type enum has always told clients to tolerate new values, so this is
  additive under [COMPATIBILITY.md](docs/api/COMPATIBILITY.md) §3.

- **`cairn config validate`** checks a configuration — the same flags the
  server takes — without starting the server, opening the database or binding
  a port. It prints `configuration valid` and exits `0`, or
  names the problem and exits `1`, so a bad deploy can fail in CI or an init
  container rather than in a restart loop.

- **`cairn version`**, as a subcommand beside the existing `-version` flag. Both
  print the same line.

- **`cairn import kuma -report-json <path>`** writes the full import report as
  JSON, for scripting a migration and checking what did and did not come across.
  `-` writes it to stdout in place of the human-readable report.

### Changed

- **Contributions now need a signed Contributor License Agreement.** An
  individual ([CLA.md](CLA.md)) and a corporate ([CCLA.md](CCLA.md)) agreement,
  adapted from Apache's, collected by a bot on the pull request. Section 9 writes
  the governance limits into the agreement itself — licensing contributions
  under anything but Apache 2.0 needs the governance supermajority and 30 days'
  public notice, and must not remove a capability from the open edition or put
  one behind payment. Nothing changes for anyone running the software.

### Fixed

- The load harness measured check lateness against the wrong interval, reporting
  it three times more generously than intended. No released behaviour changes;
  the gate is now as strict as it was documented to be.

### Security

- **gRPC upgraded to v1.83.2 for CVE-2026-84445**, a high-severity
  denial-of-service in gRPC-Go's xDS server path. Uptime Cairn does not run an xDS
  server, so no install was exposed through it, but the vulnerable code was
  linked into the binary and image scanners flag it.

## [1.1.0-beta.1] — 2026-09-04

The Phase 2 reporting subsystem, released as a beta. Everything below is
implemented and installable; what it is not yet is frozen. Per
[COMPATIBILITY.md](docs/api/COMPATIBILITY.md) §1 the `/api/v1` freeze attaches at
the first stable tag, so the reporting operations stay editable until `1.1.0`.
That is the point of a beta, and it is the window in which a report about the
shape of these endpoints can still be acted on rather than deferred to
`/api/v2`.

### Changed

- **Licence is now Apache 2.0**, replacing AGPL 3.0 throughout the project —
  `LICENSE`, the API specification, the governance and security documents, the
  ADRs, and the web package metadata. Nothing about the build changes; the terms
  you receive it under do.
- README rewritten around what the tool does and how to install it, with
  screenshots of the dashboard, a monitor, and a status page.
- **The backup guidance for report artifacts is corrected.** It previously said
  the local copy of `<data-dir>/reports/` "may be skipped" where the S3 mirror is
  enabled. That was written before there was a mirror, and it is unsafe: an upload
  that fails is recorded rather than retried, so a mirror that has been quietly
  failing looks exactly like one that is working. Keep taking the local copy, or
  alert on `artifacts[].mirror.state`.
- The same page said report files are written `0640`; they are written `0600`.
- **A report covering a day or two now charts hours instead of days.** The
  availability strip and the response-time line were always drawn from the daily
  series, so a daily report produced a strip of one cell and a line of one point
  — a picture of a number printed directly beneath it. A window of 48 hours or
  less now draws both from the hourly tier, labelled in hours and captioned with
  the grain. Longer reports are unchanged, and the published `ReportDocument`
  keeps its daily array: `response_time.daily` is one point per day typed
  `format: date`, and the hourly series reaches the rendered page only.
- "Best day" and "worst day" are omitted from a rendered report when they are the
  same day, which is every daily report — three headings over one number invite a
  reader to look for a difference that cannot be there. Both stay in the JSON
  artifact, where a consumer can compare the dates itself.

### Added

- **Report schedules can be set up from the dashboard.** Reports → Schedules
  creates, edits and deletes them, and each report row has a **Schedule** link
  beside **Generate**. This is what sends a client their report on the 1st
  without anybody being at a keyboard, and until now it existed only as an API
  endpoint — every schedule had to be created by hand-writing JSON. The form
  covers frequency (including cron), the send time, the timezone the report's
  window is cut in, and any number of delivery targets: email, Slack, webhook,
  or an S3 drop, each with its own choice of formats.
- **A report template's `sections` now selects what the report contains.** The
  field was stored and round-tripped while nothing read it, so a `custom` report
  rendered the full document regardless. It now emits the blocks it names, in the
  order it names them, and the template editor has a **Content** picker showing
  each block's position. Selecting nothing keeps the standard blocks for the
  report type, so no existing template changes. Applies to the PDF and HTML; the
  JSON and CSV are data exports and still contain the full document. Unknown
  section names are now refused with the vocabulary listed, which they were not
  before — while nothing read the field, a typo was harmless.
- **Public share links for report runs.** `POST /api/v1/report-runs/{id}/share`
  returns a URL anyone can open; `DELETE` withdraws it immediately, leaving the
  files untouched. The link is shown **once** — the token is stored hashed for
  lookup and sealed for replay, so no later read can produce it — and the run
  thereafter reports only that a link exists, when it expires, and whether the
  recipient has opened it. The public path serves the **stored artifact, never a
  re-render**, so the figures a client bookmarked do not change when retention
  drops a tier. It answers on a separate projection carrying no run, template or
  monitor identifier, is `noindex` and rate limited, and distinguishes `410`
  ("this existed and is gone") from `404` ("no such link"), because those are
  different answers to somebody holding a bookmark. One live link per run:
  creating a second is refused rather than silently replacing the first.
- **An optional offsite mirror for report artifacts** (Settings → Report artifact
  mirror). Every rendered report is copied to an S3-compatible bucket under the
  same relative path it holds on disk. **Local storage stays the source of truth
  and the only read path**, so a failed upload is recorded against the artifact
  with the provider's own message and does not fail the report. Nothing retries a
  failed upload and nothing reconciles the bucket against the database — check
  `artifacts[].mirror.state` if you intend to rely on it. Configuration changes
  take effect on the next report, not the next restart.
- **`s3` as a report delivery target** — the "drop", which puts one schedule's
  files into a bucket under a readable key for a recipient. Previously refused
  with a message saying the client was not built. It is **not** the mirror and the
  documentation keeps them apart: a drop is a delivery, not a durability copy.
- An S3-compatible client written against the standard library — SigV4 with
  `crypto/hmac`, `crypto/sha256` and `net/http`, no vendor SDK and nothing added
  to `go.mod`. Selectable path-style addressing, an overridable endpoint, and
  server-side encryption headers passed through. Static credentials only.
- `NOTICE` file recording copyright and attribution, as Apache 2.0 expects.
- [`docs/why-uptime-cairn.md`](docs/why-uptime-cairn.md) — the design principles
  and the reasoning behind building another uptime monitor.

### Fixed

- **A cron report schedule could never be changed to a fixed frequency.** Saving
  it as daily, weekly, monthly or quarterly was refused with "cron is only
  accepted when frequency is cron", because clearing the expression was not
  expressible — `null` and an omitted field were indistinguishable, so the stored
  one carried forward. The expression is now cleared when the frequency moves off
  cron. Supplying an expression _alongside_ a fixed frequency is still refused,
  which is deliberate: a stored expression that never runs is a schedule you
  believe you configured.
- **A report whose file is missing from disk was offered for download anyway.**
  The dashboard showed a download link, and clicking it failed. Such an artifact
  is now shown as **File unavailable**, with its digest and size still listed so
  the file can be identified in a backup, and a shared link no longer offers a
  format it cannot serve. The row still reads `rendered`, because it is a record
  of what was produced; what changed is that the server checks the file is there
  before offering it.
- **A report whose file is missing from disk returned `500 Internal error`.** It
  now returns `410 Gone` with a message naming the reports directory. The state
  this covers is a database restored without `<data-dir>/reports/` — the silent
  half of the backup procedure — where "Internal error, the cause has been
  logged" sends you to a log and naming the missing file sends you to your
  backup. The run listing was already unaffected and still is. Found by running
  the documented backup and restore procedure end to end.

## [1.0.1] — 2026-08-22

### Fixed

- **Response-time statistics counted failed checks.** Averages, minimums, and
  maximums now include only checks that succeeded. A failing check still times
  something — milliseconds to a refused connection, a fast 500 — but that is a
  time to a failure, not the latency of the service, and it made the fastest
  response in particular meaningless. Applies to the rollup job, the raw-history
  query, and the uptime summary alike. Buckets can now report a non-zero
  `down_count` with a null `response_time_min`; that is correct. Existing rollup
  rows are not rewritten — the correction applies from upgrade onward.
- **Notification channel forms showed the wrong default.** Boolean fields the
  server defaults to _on_ — "use the instance SMTP settings", "verify the TLS
  certificate" — rendered as unchecked, so the first save failed on a setting
  that already looked the way the user wanted it. Field specs now carry their
  server-side default.

### Added

- `HistoryBar` component on the monitor detail page: per-bucket uptime with the
  gaps left as gaps, so a period with no data is not drawn as downtime.
- Hints on the email channel's SMTP host and from-address fields marking them
  required when the instance relay is turned off.

## [1.0.0-rc.1] — 2026-08-22

First public release. Self-hosted uptime monitoring in a single container, with
SQLite on disk and no database server to run.

### Added

- **Nine monitor types** — `http`, `tcp`, `icmp`, `dns`, `tls_expiry`,
  `domain_expiry`, `push`, `docker`, and `grpc`, with shared settings for
  intervals, retries, and dependency suppression.
  ([reference](docs/guides/monitor-types.md))
- **Thirteen notification channels** — email, webhook, Slack, Discord, Telegram,
  Matrix, Gotify, ntfy, Microsoft Teams, PagerDuty, Opsgenie, Twilio SMS, and
  Apprise, which reaches roughly ninety more. Webhooks take custom body
  templates. ([reference](docs/guides/alerting.md))
- **Public status pages** — grouped services, uptime history, incident timelines,
  a next-update countdown, your own domain and logo.
- **Incidents** — create, edit, and post updates against affected monitors.
- **Groups and tags** for organising monitors, with creation and editing in the
  UI.
- **Maintenance windows** that suppress alerting without polluting uptime
  figures.
- **REST API** covering everything the UI does, with cursor pagination and live
  monitor updates. ([OpenAPI spec](docs/api/openapi.yaml))
- **Uptime Kuma importer** — `cairn import kuma /path/to/kuma.db` brings across
  monitors, tags, notifications, and status pages, merges several Kuma databases
  into one install, and supports `--dry-run`.
  ([what doesn't come across](docs/guides/migrating-from-uptime-kuma.md))
- **Encryption at rest** for saved passwords and tokens, under a root key in
  `cairn.key`. Back that file up alongside `cairn.db` — without it, stored
  credentials cannot be read.
  ([backup guide](docs/operations/backup-restore.md))
- **Prometheus metrics** for probe health and instance telemetry.
- **Multi-architecture Docker images**, published to both
  `ghcr.io/webloomlabs/uptime-cairn` and `webloomlabs/uptime-cairn` on Docker Hub.
- Load-test gate in CI holding the single-instance target of 5,000 monitors.

[Unreleased]: https://github.com/webloomlabs/uptime-cairn/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/webloomlabs/uptime-cairn/compare/v1.1.0-beta.1...v1.1.0
[1.1.0-beta.1]: https://github.com/webloomlabs/uptime-cairn/compare/v1.0.1...v1.1.0-beta.1
[1.0.1]: https://github.com/webloomlabs/uptime-cairn/compare/v1.0.0-rc.1...v1.0.1
[1.0.0-rc.1]: https://github.com/webloomlabs/uptime-cairn/releases/tag/v1.0.0-rc.1
