# Phase 1 — working checklist

Every deliverable in [PHASE-1-PLAN.md](PHASE-1-PLAN.md), as a list that can be
ticked. The plan is the contract and does not change; this is the tracker.

**Status: 2026-08-19.** It alerts. Thirteen channel types deliver, every one of
them test-fireable, with credentials encrypted at rest and redacted on read, and
webhook payloads templated against a variable catalogue the API publishes so the
UI's autocomplete cannot drift from the renderer. Failures are recorded, retried
where a retry could plausibly work, and surfaced on the channel itself — a
channel that has silently stopped working is the one failure mode this feature
cannot be allowed to have.

History is durable past the seven-day raw window and readable through the API:
the rollup tiers are computed, `/history` and `/uptime` serve them, retention is
enforced, and deleted monitors have their history purged in the background. The
Month 1 checkpoint is met — *"a monitor can be created via `curl`, checked on
schedule, and its history queried via the API"* — and it is met for every monitor
type the spec defines, not just HTTP. The API is authenticated: first-run setup,
sessions with CSRF, TOTP, and scoped API keys.
See [running.md](../development/running.md) for what the binary does today.

A box is ticked only when the thing works end to end and has a test or a
demonstrated run behind it. Anything half-built says so on the line, because a
checklist that flatters itself is worse than no checklist — it is the mechanism
by which "90% done" lasts three months.

| Area | Done | Total |
|---|---|---|
| Engine & storage | 13 | 15 |
| Monitor types | 9 | 10 |
| Core monitoring features | 3 | 8 |
| Alerting & webhooks | 10 | 10 |
| Status pages | 0 | 5 |
| REST API | 12 | 21 |
| Kuma migration | 0 | 5 |
| UI | 0 | 8 |
| Security | 6 | 8 |
| Deployment & operations | 0 | 9 |
| Documentation | 1 | 8 |
| Quality gates | 3 | 8 |
| **Total** | **57** | **115** |

---

## Engine & storage

- [x] SQLite schema from the Phase 0 data model — migration `0001`, monitoring core and time series
- [x] Migration runner: forward-only, numbered, checksummed, advisory-locked, rolled back on failure
- [x] Migrations run automatically on start, fatal on checksum mismatch
- [x] Sentinel organisation and embedded probe seeded by `0001`
- [x] Scheduler behind the probe interface — min-heap, deterministic dispersal, never assumes same-process execution
- [x] Bounded worker pool that sheds rather than queues, with a lateness budget
- [x] Result buffer with ack-gated release and transition-preserving shedding
- [x] Idempotent heartbeat batch writes
- [x] Control-plane state machine: consecutive failures, pending/up/down, `important` on transitions
- [x] Migration `0002`: users, sessions, API keys, recovery codes, settings, encryption keys
- [x] Migration `0003`: notification channels and deliveries, certificate and domain observations, maintenance windows and targets, incidents and updates, status pages and subscribers, outbound webhooks and deliveries, audit log, imports, plus the uptime cache and purge queue the jobs below need
- [x] Rollup pipeline: raw → 1m → 5m → hourly → daily, each tier from the tier below, buckets epoch-aligned and half-open, watermark derived from the data, every write an idempotent full recount
- [x] Retention enforcement per tier, with disk actually reclaimed on SQLite — `auto_vacuum=INCREMENTAL` had never actually been applied; the PRAGMA in `0001` is a no-op inside the migration runner's transaction, so it now lives in the connection DSN, and a test asserts the file shrinks
- [x] Asynchronous purge of a deleted monitor's history, in bounded batches
- [ ] Dependency-suppression handling in ingest — `resend_after` is done, derived from `consecutive_failures` so no second source of truth exists; parent-down suppression is not
- [ ] Reader pool alongside the single writer (one connection today)

## Monitor types

- [x] HTTP/HTTPS — status codes, keyword (4 modes), response-time threshold, custom method/headers/body, basic and bearer auth, redirect and TLS-verify options
- [x] HTTP JSON-path assertions — a deliberately small subset (root, field names, array indices); anything outside it is rejected at validation rather than ignored at check time
- [x] TCP port
- [x] ICMP ping, including restricted-container detection and TCP fallback — unprivileged datagram socket first, raw second, and unknown rather than down when neither opens
- [x] DNS record — all ten record types, a named resolver, the response code recorded, and the TCP retry on truncation
- [x] SSL/TLS expiry — the handshake is made unverified and the chain checked by hand, so an expired certificate is reported as expiry rather than as a generic TLS error
- [x] Domain expiry (RDAP/WHOIS), with a per-type minimum interval — RFC 9224 bootstrap, WHOIS fallback, one registry lookup a day per domain
- [x] Push / heartbeat dead-man's-switch — control-plane-side, never assigned to a probe
- [ ] Docker container, with monitor-to-named-probe pinning — **the checker works; the pinning does not exist.** In a multi-probe install "is this container running" is only answerable by the probe on that host, and nothing yet makes the assignment land there. Correct in solo mode, which is the only mode this build has
- [x] gRPC health

## Core monitoring features

- [x] Retries, timeouts, intervals down to 20s
- [x] Upside-down mode
- [ ] Groups — tables exist, no API or engine behaviour
- [ ] Tags — tables exist, no API or engine behaviour
- [ ] Dependency-aware suppression (parent down suppresses child alerts)
- [ ] Maintenance windows: scheduled, recurring, attached to monitors/groups/tags
- [ ] Certificate and domain expiry surfaced as upcoming-expiry data
- [x] Uptime history browsing over arbitrary past ranges via rollups

## Alerting & webhooks

- [x] Email (SMTP) — implicit TLS, STARTTLS and plaintext, base64 bodies so a long template stays inside SMTP's line limit, and threading headers so an outage and its recovery are one conversation. **Per-channel SMTP only:** `use_instance_smtp` defaults to true, instance-wide settings have nowhere to live until `/settings` exists, and the channel is refused at save time with the alternative spelled out rather than accepted and silently undeliverable
- [x] Generic webhook — user-defined method, headers and body, all interpolated, with the default event envelope when no template is given
- [x] Slack, Discord, Telegram
- [x] Matrix, Gotify, ntfy — Matrix sends with the event id as its transaction id, so a retry after a timeout that actually succeeded posts the same message rather than a second copy
- [x] Microsoft Teams
- [x] PagerDuty, Opsgenie — an outage and its recovery are two edges of one incident, keyed by monitor, so the recovery closes the alert the failure opened
- [x] Twilio / SMS
- [x] Apprise meta-provider — URLs written to a mode-0600 file rather than passed as arguments, because an Apprise URL embeds its own credentials and an argument vector is readable through `ps`. Reported as unavailable when the binary is not installed, rather than offered and failing on first use
- [x] Per-monitor and default channel assignment, test-fire on every channel — an absent `notification_channel_ids` attaches the defaults, an empty array means a deliberately silent monitor, and the two are distinguishable
- [x] Webhook payload templating: user-defined body/headers/method, variable interpolation, live preview — the variables are published as an endpoint and are the same list the renderer resolves against, and JSON escaping follows the declared content type so a monitor named `He said "hi"` does not produce a payload the receiver rejects

## Status pages

- [ ] Multiple pages per install, monitor and group selection
- [ ] Custom domains, with reverse-proxy and ACME documentation
- [ ] Uptime bars, incident and maintenance display
- [ ] Subscribe-to-updates via the API
- [ ] Unauthenticated read path that holds up under load

## REST API

- [x] `GET /healthz`, `GET /readyz`
- [x] `GET /api/v1/monitors` — cursor pagination, server-side, limit clamped
- [x] `POST /api/v1/monitors` — validation returning one entry per invalid field
- [x] `GET /api/v1/monitors/{id}`
- [x] `DELETE /api/v1/monitors/{id}`
- [x] `GET /api/v1/monitors/{id}/heartbeats`, including `important_only`
- [x] `GET|POST /api/v1/push/{pushToken}` — the unauthenticated dead-man's-switch ingest
- [x] RFC 9457 problem responses with stable `type` URIs
- [ ] `PATCH /api/v1/monitors/{id}`, pause, resume, check-now, bulk operations
- [ ] `GET /api/v1/monitors/membership` — ADR-004's change signal and filtered count
- [ ] Monitor list filters: status, type, tag, group, enabled, search
- [ ] `include=last_heartbeat|uptime|tags|group`
- [x] `/history` and `/uptime` — auto resolution, coarsened rather than refused when a request would return too many buckets, and read from raw rather than the tiers whenever raw covers the range
- [x] First-run setup, login, logout, session description
- [x] TOTP enrolment, confirmation, and removal
- [x] Notification channels — CRUD, test-fire, template preview, and the published variable catalogue
- [ ] Groups, tags, status pages, incidents, maintenance windows, settings — the rest of the specified surface, currently answering `501`
- [ ] Scoped API keys: permissions, expiry, last-used, revocation
- [ ] Outbound webhooks for every state change
- [ ] Prometheus `/metrics` and OpenTelemetry export
- [ ] Generated Go and TypeScript clients published from the spec

## Kuma migration

- [ ] `cairn import kuma <path>` CLI
- [ ] Guided UI import flow
- [ ] Multi-instance merge
- [ ] Import report naming everything that did not map cleanly
- [ ] Close the three open gaps in the schema mapping ([data model §10](../data-model/README.md))

## UI

- [ ] SvelteKit project in `web/`, built into `internal/ui/dist`
- [ ] Dashboard list view with server-side pagination, filter, and search
- [ ] Monitor detail with history charts
- [ ] Monitor CRUD forms
- [ ] Bulk operations (multi-select enable/disable/tag)
- [ ] Notification setup with test-fire and webhook template live preview
- [ ] Dark mode
- [ ] i18n scaffolding, English complete, translation pipeline documented

## Security

- [x] First-run setup: administrator account, argon2id hashing
- [x] Session management with CSRF protection
- [x] Rate-limited login
- [x] TOTP two-factor, with single-use recovery codes
- [x] API-key authentication with scope enforcement and no privilege escalation
- [x] Encryption at rest: AES-256-GCM envelopes, AAD-bound rows, wrapped data keys, root-key precedence — carrying the TOTP secret today
- [ ] Monitor credentials encrypted through the same layer — **notification credentials now are.** A channel's secrets are split out of `config` at the storage boundary and sealed with AAD binding them to their row, so a read path cannot serialise what is not there; verified by grepping the database file. HTTP bearer tokens and basic-auth passwords, Docker client keys, and gRPC metadata still reach `monitors.config` in plaintext
- [ ] `SECURITY.md`, dependency and container scanning in CI

## Deployment & operations

- [ ] Multi-arch Docker image (amd64/arm64), small base
- [ ] `docker run` → first monitor in under 60 seconds, verified in UX review
- [ ] `docker-compose.yml` reference and systemd unit example
- [ ] Binary releases with checksums and SBOM
- [ ] Reverse-proxy recipes (Caddy, nginx, Traefik)
- [ ] Backup and restore: online backup plus verified restore
- [ ] Upgrade path documented, with the rollback stance stated
- [ ] Self-monitoring: internal metrics surfaced, including probe health already on the wire
- [ ] Release automation: tagged release → binaries, images, checksums, SBOM

## Documentation

- [x] Development run-through ([running.md](../development/running.md))
- [ ] Install guide (Docker, compose, binary, Pi)
- [ ] First-monitor quickstart
- [ ] Per-monitor-type reference
- [ ] Alerting setup per channel
- [ ] API reference generated from the spec
- [ ] Kuma migration guide
- [ ] Backup, upgrade, and operations guide

## Quality gates

- [x] Unit tests over the paths where a bug would be silent — checker classification, transition table, buffer shedding, migration checksums, write idempotency
- [x] `go test -race` clean, in CI
- [x] gofmt, vet, build, and buf lint/format/breaking in CI
- [ ] Contract tests verifying the server against the frozen OpenAPI spec
- [ ] Golden-path E2E: install → create monitor → down event → alert fires → recovery → status page reflects it
- [ ] Load-test harness pointed at the real engine (`http` target still refuses to run)
- [ ] 5,000-monitor CI acceptance gate against the engine, then the UI benchmark
- [ ] Crash-recovery test: kill mid-cycle, assert at most one tick lost and checks resume

---

## What to do next, and why in this order

1. **Monitor credentials through the encryption layer.** Nine monitor types
   accept secrets in their config — bearer tokens, basic-auth passwords, Docker
   client keys, gRPC metadata — and every one of them is stored in plaintext. The
   layer that would fix it now carries the TOTP secret *and* every notification
   channel's credentials, so the pattern to copy is written and tested; this is
   the last plaintext credential store in the product.
2. **Maintenance windows and dependency suppression.** Both suppress alerts, both
   have their tables, and neither exists in ingest. Now that alerting works, they
   are the difference between "it pages me" and "it pages me appropriately" —
   a router going down should page once, not forty times.
3. **The load-test harness against the real engine.** The 5,000-monitor claim is
   the project's central promise, and every week it goes unmeasured against real
   code is a week the number is an assumption. It now has a third thing to
   measure: a partition that marks several thousand monitors down within one
   scheduler tick is precisely the burst the delivery queue is sized against, and
   that size is currently an argument rather than a measurement.
4. **`/monitors/{id}/certificate` and the observation writers behind it.** The
   TLS and domain checkers see everything that endpoint reports and store none of
   it; migration `0003` created the tables in the same pass, and
   `monitor.certificate_expiring` is an event type with nothing raising it.

The UI comes after those deliberately: the plan puts it in Month 3, and an API
that is not finished is a UI that gets rewritten.
