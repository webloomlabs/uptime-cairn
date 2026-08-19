# Phase 1 — working checklist

Every deliverable in [PHASE-1-PLAN.md](PHASE-1-PLAN.md), as a list that can be
ticked. The plan is the contract and does not change; this is the tracker.

**Status: 2026-08-19.** The 5,000-monitor claim is measured against the real
engine rather than against the schema. The load-test harness starts a `cairn`,
creates the workload through the real API, and measures what the engine achieves:
239.6 heartbeats a second against the 250 its schedule implies, with 7,620
requests counted independently on the other side of the network — the one figure
the engine cannot fake. Then it breaks every monitored endpoint at once, because
that burst is what the delivery queues were sized against and nothing had counted
the deliveries on the other end. All 5,000 marked down in 20.6 seconds, all back
afterwards, 9,682 alerts and 9,682 webhook deliveries with nothing shed.

It found three things on its first real run, one of which was in the gate itself.
The `list: filter status=down` growth cap had never actually executed at the
scales it was written for — the harness had no committed `go.sum`, so CI refused
it before it got there — and when it did it failed at 4.8x against a 4.0x bound.
The query plan was identical at both scales; what grew was the matched set, 13
rows to 159, because the workload keeps a fixed proportion down. Sub-linear
latency against super-linear work is the hypothesis holding. The bound is now 6.0
with the evidence written beside it.

Creating monitors was quadratic — every
write reloaded and re-diffed the whole assignment set, 2,116 recomputations for
5,000 monitors, and the run never finished. It also caught its own first answer
being wrong, which is the more instructive half: it reported 499 heartbeats a
second against 250 implied, and the engine was fine. It was draining the backlog
built while seeding saturated the writer. Rows counted by check time said 250;
rows counted by write time said 500. Both were true, and the fix was to wait for
the rate to settle rather than to sleep a fixed interval and hope.

The REST API is finished. Every Phase 1 operation in the
frozen spec answers for real except the two Kuma import endpoints, which stay at
`501` on purpose — the endpoint without the importer behind it would accept a file
and report success for an import that never happened.

Monitors can now be edited, paused, resumed, checked on demand, and changed a
thousand at a time. A config patch merges against the *decrypted* configuration
and resolves a redaction marker back to the credential it stands for, so a form
that round-trips its own `GET` cannot destroy the password it was never shown —
verified live by watching the probe's next check send the original password under
the new username. Incidents, status pages, outbound webhooks, and instance
settings exist, and the public status page is a projection of its own rather than
a filtered monitor read, because a field cannot leak through a shape that has no
place to put it.

Two things the earlier work left open are now closed. `/settings` gives
instance-wide SMTP somewhere to live, so an email channel's `use_instance_smtp`
stops being refused at save time. And outbound webhooks put the event stream in
front of programs as well as people: signed over the exact bytes sent, with an
event id stable across retries and manual redelivery so a receiver can
deduplicate.

Monitors can be organised. Groups nest one level and roll
their worst status up from their children; tags carry a derived slug so two that
look alike cannot both exist; the monitor list filters by either — and now also by
status, type, enabled, and a search that matches the target as well as the name.
That taxonomy is what makes the maintenance windows built before it worth having —
a window targeting a tag keeps covering monitors created after it, which was
verified live by adding one mid-window.

It pages appropriately. A maintenance window silences
what it covers and annotates the history so the period can be excluded from an
SLA figure; a dependency parent silences everything behind it without touching
those monitors' own uptime. Those are deliberately different operations — one
says the observation is not about the target, the other says it is real and
nobody needs waking — and the three-way `maintenance` parameter on `/uptime`
now returns three different numbers, which is what it was always for.

Nothing in the database is a credential in the clear. The
last plaintext store — HTTP auth, Docker client keys, gRPC metadata sitting in
`monitors.config` — is closed, sealed through the same envelope the TOTP secret
and the notification channels use, and a pre-existing database is migrated on
start rather than left behind.

It alerts. Thirteen channel types deliver, every one of
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
| Engine & storage | 18 | 19 |
| Monitor types | 9 | 10 |
| Core monitoring features | 7 | 8 |
| Alerting & webhooks | 10 | 10 |
| Status pages | 3 | 5 |
| REST API | 22 | 23 |
| Kuma migration | 0 | 5 |
| UI | 0 | 8 |
| Security | 8 | 9 |
| Deployment & operations | 0 | 9 |
| Documentation | 1 | 8 |
| Quality gates | 4 | 8 |
| **Total** | **82** | **122** |

The Engine & storage row read 14/15 until now and was simply stale — two items had
been ticked without the table being updated. A tracker that quietly disagrees with
itself is the thing this file exists to prevent, so it is corrected rather than
carried forward.

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
- [x] Migration `0004`: the encrypted half of a monitor's configuration, with the plaintext half left as queryable JSON
- [x] Migration `0003`: notification channels and deliveries, certificate and domain observations, maintenance windows and targets, incidents and updates, status pages and subscribers, outbound webhooks and deliveries, audit log, imports, plus the uptime cache and purge queue the jobs below need
- [x] Rollup pipeline: raw → 1m → 5m → hourly → daily, each tier from the tier below, buckets epoch-aligned and half-open, watermark derived from the data, every write an idempotent full recount
- [x] Retention enforcement per tier, with disk actually reclaimed on SQLite — `auto_vacuum=INCREMENTAL` had never actually been applied; the PRAGMA in `0001` is a no-op inside the migration runner's transaction, so it now lives in the connection DSN, and a test asserts the file shrinks
- [x] Asynchronous purge of a deleted monitor's history, in bounded batches
- [x] `resend_after` and dependency-suppression handling in ingest — both derived rather than stored: the resend from `consecutive_failures`, the suppression from the parent's current state at the moment the result lands, because a parent and its children can fail within the same second and a sweep would be a tick behind
- [x] Engine self-metrics: heartbeats written and results ingested counted separately, so a probe redelivering is distinguishable from the system doing twice the work; alerts published and shed; and each probe's own report — shed results, skipped checks, due-queue depth, buffer depth, clock offset — republished from the result stream, because a probe has no inbound port to scrape. This is what the load test reads, and reading the same endpoint an operator scrapes is the point: a harness with a private back door measures a system nobody else can see
- [x] Assignment publishing coalesces a burst of writes into one recompute. Each write used to reload and re-diff the whole assignment set, making the creation of N monitors O(N²) — the load-test harness measured 2,116 full recomputations while creating 5,000 through the API, and the run never finished. A one-second settle window is invisible against the 20-second interval floor
- [ ] Reader pool alongside the single writer (one connection today) — **now measured rather than assumed.** Monitor creation runs at 1,144/sec at 500 monitors and 38/sec at 5,000, because the assignment reload holds the single connection while it scans every assignable monitor and writes queue behind it. That is the shape of an import of somebody's existing install, which is the first thing this product asks a new user to do

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
- [x] Groups — one level of nesting, and a parent reports the worst status among its monitors *and its children's*, because a parent group showing green during an outage underneath it is the worst thing a monitoring tool can do. Deleting a group ungroups its monitors and promotes its child groups; it never deletes what it contained
- [x] Tags — the slug is derived from the name rather than supplied, so two tags that render identically in a list cannot both exist; a name colliding on slug is a `409` naming the slug
- [x] Dependency-aware suppression — transitive up the chain, and a parent under maintenance suppresses its children as surely as a parent that is down: taking the router down for a firmware upgrade is the most known problem there is. The child's own heartbeat still records the real outage, so its uptime figure is unaffected; only the page is withheld
- [x] Maintenance windows: single, daily, weekly, monthly, and cron, evaluated in the window's own IANA zone with the zone database embedded in the binary — "02:00 every Sunday" survives a daylight-saving transition still meaning 02:00. Targets resolve by query through monitors, groups, and tags, so a window covering a tag keeps covering monitors added later. **Groups and tags have tables and no API**, so only monitor targets can be created today; referencing a group or tag is a validation error naming the field rather than a foreign-key failure
- [ ] Certificate and domain expiry surfaced as upcoming-expiry data — `/monitors/{id}/certificate`, `include=certificate`, and the overview's expiring-soon counts are built and read the tables migration `0003` created. **Nothing writes those tables:** the TLS and HTTP checkers see the certificate and report only an expiry verdict, and carrying the observation to the control plane is a new field on the probe protocol's result frame. So the endpoint answers `404` for every monitor, which is the honest answer to "what certificate was observed" when none has been
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

- [x] Multiple pages per install, monitor and group selection — ordered sections, a monitor in at most one section per page, and a slug collision answered `409` naming what is taken
- [ ] Custom domains, with reverse-proxy and ACME documentation — **the column and its cross-organisation uniqueness are enforced; the reverse-proxy and ACME recipes are not written**, and a hostname without them routes nowhere
- [x] Uptime bars, incident and maintenance display — read from the 1d rollup tier, and a day with no observations carries a null ratio rather than being drawn as downtime, which is the single most common way a status page lies
- [x] Subscribe-to-updates via the API — double opt-in, the address encrypted at rest because a notification replays it, and a repeat request answered identically to a first so the endpoint is not a membership oracle. **Nothing delivers to a subscriber yet:** confirmation and notification mail need the instance relay wired to a sender, which is the next thing this unblocks
- [ ] Unauthenticated read path that holds up under load — it exists and is correct, and "under load" is a claim the load-test harness has to make

## REST API

- [x] `GET /healthz`, `GET /readyz`
- [x] `GET /api/v1/monitors` — cursor pagination, server-side, limit clamped
- [x] `POST /api/v1/monitors` — validation returning one entry per invalid field
- [x] `GET /api/v1/monitors/{id}`
- [x] `DELETE /api/v1/monitors/{id}`
- [x] `GET /api/v1/monitors/{id}/heartbeats`, including `important_only`
- [x] `GET|POST /api/v1/push/{pushToken}` — the unauthenticated dead-man's-switch ingest
- [x] RFC 9457 problem responses with stable `type` URIs
- [x] `PATCH /api/v1/monitors/{id}`, pause, resume, check-now, bulk operations — the config patch merges against the decrypted configuration and resolves a redaction marker back to the stored credential, so a form that round-trips its own `GET` cannot destroy the password it was never shown. Check-now runs the checker in the API and hands the result to the control plane's ingest, because the control plane must not import `probe/check` (ADR-001) and because a manual check that took a different path would be testing the path. Rate-limited per monitor, not per caller: the thing being protected is somebody else's server. Bulk reports each identifier separately, because an endpoint that fails a thousand-monitor batch over one deleted id is useless at the size it exists for
- [x] `GET /api/v1/monitors/membership` — ADR-004's change signal and filtered count, sharing one filter reader with the listing so the two cannot disagree about what a filter means. `state_version` now moves on a configuration edit as well as a check result, or a rename would leave every open list view showing the old name until something failed
- [x] Monitor list filters — `status`, `type`, `enabled`, and `search` join `tag_id` and `group_id`, with the spec's rule that repeated values OR within a parameter and AND across them. `search` matches the target as well as the name, because the question that brings somebody to the search box is usually "what else points at this host?". An unrecognised value is a `400` rather than an empty page: silently returning nothing for a typo is how somebody concludes their monitors have been deleted
- [x] `include=last_heartbeat|uptime|tags|group` — one query per embed for the whole page, never one per row, which is the difference the 5,000-monitor gate measures. Opt-in, so a response without `include=` is byte-for-byte what it was before the embeds existed. `uptime` reads the precomputed cache and reports null for a monitor it has not computed, because zero is a claim of total downtime made by a table that had not run
- [x] `/history` and `/uptime` — auto resolution, coarsened rather than refused when a request would return too many buckets, and read from raw rather than the tiers whenever raw covers the range
- [x] First-run setup, login, logout, session description
- [x] TOTP enrolment, confirmation, and removal
- [x] Notification channels — CRUD, test-fire, template preview, and the published variable catalogue
- [x] Maintenance windows — CRUD, with a schedule that will never fire refused at write time rather than discovered by its silence
- [x] Groups and tags — CRUD, monitor counts, group status rollup, and assignment from the monitor write path
- [x] Status pages, incidents, settings — plus `/system/info`, `/overview`, `/users`, and the unauthenticated `/public/` read path. Incidents advance through the timeline rather than through `PATCH`, so every state change carries the sentence explaining it. The public page is a separate projection rather than a filtered monitor read, because a field cannot leak through a shape with no place to put it — and that is the one endpoint where a leak reaches strangers
- [x] Scoped API keys: permissions, expiry, last-used, revocation
- [x] Outbound webhooks for every state change — signed with an HMAC over the exact bytes sent, with a stable event id across retries and manual redelivery so a receiver can deduplicate, and self-disabling after a sustained run of failures. A separate dispatcher from notification channels, because one delivers an envelope to a program and the other renders a sentence for a person
- [x] Prometheus `/metrics` — hand-written text exposition, no client library and no new dependency, unauthenticated from loopback and `metrics:read` from anywhere else. `cairn_monitor_status` keeps the full status vocabulary rather than collapsing to up/down, so a Prometheus alert does not fire during a maintenance window the operator declared. **OpenTelemetry export is not built:** it means the SDK's dependency tree on a binary whose pitch is that it is one static file, which is a decision to take deliberately rather than in passing
- [ ] Generated Go and TypeScript clients published from the spec — codegen belongs in CI, and CI is not changed without being asked

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
- [x] Monitor and notification credentials encrypted through the same layer — HTTP basic and bearer auth, Docker client TLS material, gRPC metadata, and every channel's secrets are split out of `config` at the storage boundary and sealed with AAD binding them to their row, so a read path cannot serialise what is not there. Which fields are secret is declared by the checker that owns the config schema, and a test asserts that declaration against the `writeOnly` properties of the frozen spec. Migration `0004` adds the column; monitors written before it are re-sealed on start, verified live against a database that predated it
- [x] Every credential the new surface introduced goes through the same envelope: a webhook's signing secret and its headers, the instance SMTP password, and a status page subscriber's address are all sealed with AAD binding them to their row. The subscriber's address is encrypted rather than hashed because a notification replays it; a status page's password is hashed rather than encrypted because it is only ever verified against — "hash what you verify, encrypt what you replay", applied one more time
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
- [x] Load-test harness pointed at the real engine — the `http` target starts a `cairn`, creates the workload through the real API against an endpoint the harness serves, and measures what the engine achieves rather than what storage can absorb. Those are opposite claims and the report labels which is which: a driven rate is a ceiling, an observed rate is bounded by arithmetic, and a rate *above* the schedule means a backlog draining rather than headroom. It also breaks every monitored endpoint at once, because a burst that marks several thousand monitors down inside one scheduler tick is what the delivery queues were sized against and nothing had ever counted the deliveries on the other end. The 5,000-monitor gate passes: 239.6 heartbeats/sec against 250 implied, 5,000/5,000 marked down in 20.6s, 9,682 alerts and 9,682 webhook deliveries with nothing shed
- [ ] 5,000-monitor CI acceptance gate against the engine, then the UI benchmark — **the gate runs and passes locally; the workflow still points at the SQLite target.** Wiring the engine run in is a change to `.github/workflows/load-test.yml`, which is CI configuration and not edited without being asked (AGENTS.md rule 7). The command is one line and takes about eight minutes
- [ ] Crash-recovery test: kill mid-cycle, assert at most one tick lost and checks resume

---

## What to do next, and why in this order

1. **The reader pool alongside the single writer.** No longer a hunch: monitor
   creation runs at 1,144/sec at 500 monitors and 38/sec at 5,000, because the
   assignment reload holds the one connection while it scans every assignable
   monitor. That is precisely the shape of an import of somebody's existing
   install — the first thing this product asks a new user to do — and it is the
   only measured number in the system that gets worse as the install grows.
2. **Certificate and domain observations.** `/monitors/{id}/certificate`,
   `include=certificate`, and the overview's expiring-soon counts are built and
   read tables nothing writes. Closing that needs the observation carried from the
   checker to the control plane — a field on the probe protocol's result frame,
   which is the one change in this list that is not API work.
   `monitor.certificate_expiring` is an event type with nothing raising it, and it
   is the alert people actually want from a TLS monitor.
3. **Delivery to status page subscribers.** Subscriptions are recorded, encrypted,
   and double opt-in, and nothing sends the confirmation mail. The instance relay
   now exists to send it through.

The UI comes after those deliberately: the plan puts it in Month 3, and every
surface it needs now exists — which was the point of finishing the API first. The
membership signal is the one number to look at before starting it: 6.2ms per poll
at 5,000 monitors, and its cost scales with connected clients rather than with
monitor count, which is the dimension the gate does not exercise.
