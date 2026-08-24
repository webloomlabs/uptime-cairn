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

### Added

- `NOTICE` file recording copyright and attribution, as Apache 2.0 expects.
- [`docs/why-uptime-cairn.md`](docs/why-uptime-cairn.md) — the design principles
  and the reasoning behind building another uptime monitor.

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
