# Changelog

All notable changes to Uptime Cairn are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Entries are written for the person deciding whether to upgrade: what changed for
them, not which files moved.

## [Unreleased]

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

### Added

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
  server defaults to *on* — "use the instance SMTP settings", "verify the TLS
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

[Unreleased]: https://github.com/webloomlabs/uptime-cairn/compare/v1.0.1...HEAD
[1.0.1]: https://github.com/webloomlabs/uptime-cairn/compare/v1.0.0-rc.1...v1.0.1
[1.0.0-rc.1]: https://github.com/webloomlabs/uptime-cairn/releases/tag/v1.0.0-rc.1
