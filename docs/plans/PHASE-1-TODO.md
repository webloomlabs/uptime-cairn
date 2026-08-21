# Phase 1 — working checklist

Every deliverable in [PHASE-1-PLAN.md](PHASE-1-PLAN.md), as a list that can be
ticked. The plan is the contract and does not change; this is the tracker.

**Status: 2026-08-21.** There is a dashboard, and it is styled after the product
this one is measured against: a fixed left rail, near-black navy, one saturated
green carrying "up", an indigo reserved for the single primary action on a screen.
Monitors, incidents and status pages each have a screen; the monitor list and its
summary rail are one page rather than two. Styling it turned up two defects worth
recording because neither is visible in source review. Tailwind 4 emits a `@theme`
variable only where it can see a utility class using it, and every status colour
here is composed at runtime as `var(--color-{tone})` — four of six were dropped
from the stylesheet, which renders as a status marker that is present, sized and
transparent. And the plural machinery that had been deferred produced "1 monitors"
on the first screen anybody sees, so it is no longer deferred: plurals now go
through `Intl.PluralRules` rather than an inline `n === 1`, which is the rule that
holds for a language with four forms as well as for English.

There is a dashboard. It is an ordinary API client — one
file in it calls `fetch`, and everything else goes through that — so there is no
privileged endpoint behind it and no field it can set that a scoped API key
cannot. Monitors can be created, edited, paused, checked, filtered, searched, and
changed a thousand at a time from a browser; notification channels can be built
and test-fired with the template preview rendering through the server's own
renderer rather than a second one in the client. The public status page and the
two links subscriber mail carries are answered by real pages, which closes the one
thing the delivery work had to leave open.

It is built to [ADR-004](../adr/004-ui-state-synchronisation.md) rather than
merely near it: every list is cursor-paginated and filtered server-side with no
small-install shortcut, and a filtered view reconciles against the membership
signal instead of being pushed full state. The half that is missing is the
real-time one, and it is missing for a reason that is visible in the API — there
is no browser-facing channel for scoped diffs in this build, so staleness is
bounded by the poll interval rather than being near-zero on screen.

Building it found two bugs that had nothing to do with the frontend. `//go:embed
dist` silently skips every path beginning with `_`, and SvelteKit puts the whole
bundle under `_app/` — the binary compiled, started, and served an `index.html`
referencing files it did not contain. And the server had no SPA fallback, so
every deep link 404'd, including the three that go out in subscriber mail and
cannot be reissued. Both now have tests.

**2026-08-19.** The 5,000-monitor claim is measured against the real
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
| Engine & storage | 19 | 19 |
| Monitor types | 9 | 10 |
| Core monitoring features | 8 | 9 |
| Alerting & webhooks | 10 | 10 |
| Status pages | 4 | 6 |
| REST API | 22 | 23 |
| Kuma migration | 0 | 5 |
| UI | 11 | 15 |
| Security | 8 | 10 |
| Deployment & operations | 5 | 10 |
| Documentation | 1 | 8 |
| Quality gates | 4 | 8 |
| **Total** | **101** | **133** |

Three rows were stale again and are corrected here rather than carried forward.
Engine & storage read 18/19 with all nineteen ticked; Core monitoring features and
Status pages each had an item added to the body without its total being bumped, so
both under-counted what they contain. The counts above are now derived from the
checkboxes below rather than maintained beside them — which is the only version of
this that stays true, because a tracker that quietly disagrees with itself is the
thing this file exists to prevent.

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
- [x] Reader pool alongside the single writer — reads run on a separate read-only pool, opened `mode=ro` so a routing mistake fails immediately instead of surfacing as a rare lock error under load; the writer stays at exactly one connection, which is what keeps every check-then-act in the store exact. Roughly doubles monitor creation at 5,000 monitors (73/36/60 per second on one connection against 105/142/99 with the pool, measured back to back on the same machine because the absolute figures move with whatever else it is doing). `/metrics` reports both pools' wait counts, which is what separates a query that got slower from one that is queued behind somebody else's write — a question that could not be asked when everything shared one connection

## Monitor types

- [x] HTTP/HTTPS — status codes, keyword (4 modes), response-time threshold, custom method/headers/body, basic and bearer auth, redirect and TLS-verify options
- [x] HTTP JSON-path assertions — a deliberately small subset (root, field names, array indices); anything outside it is rejected at validation rather than ignored at check time
- [x] TCP port
- [x] ICMP ping, including restricted-container detection and TCP fallback — unprivileged datagram socket first, raw second, and unknown rather than down when neither opens
- [x] DNS record — all ten record types, a named resolver, the response code recorded, and the TCP retry on truncation. With no resolver named it walks every nameserver in `resolv.conf` in file order rather than querying only the first: that file is a fallback list, and a host whose primary nameserver is unreachable could otherwise never run a DNS monitor. The failure was quiet rather than loud, which is why it needed finding twice — an unreachable resolver is reported `unknown` rather than down, correctly, so the symptom was a monitor sitting on pending forever showing no failures while monitoring nothing. Every candidate shares one timeout, so three dead nameservers cannot cost three times what the operator configured
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
- [x] Certificate and domain expiry surfaced as upcoming-expiry data — `/monitors/{id}/certificate`, `include=certificate`, and the overview's expiring-soon counts read tables the ingest path now writes. The TLS and HTTP checkers report what the handshake presented and the domain checker reports the registration behind the name, both on `Result.certificate` / `Result.domain` ([protocol §7.4](../probe/protocol.md#74-observations)); ingest stores one row per monitor, replaced in place. The observation rides the result frame on change and once an hour otherwise, because it is several hundred bytes against a hundred for the result carrying it and sending it on every check would cut the probe buffer's outage coverage to a fifth — so `observed_at` means "last confirmed on the wire" to within an hour, and a renewal lands on the next check. `monitor.certificate_expiring` and `monitor.domain_expiring` fire when the countdown crosses the monitor's own `days_remaining_threshold`, again when the certificate is replaced by one still inside it, and once a day after that; deduplicated against the stored row rather than memory, so a restart does not re-page. An `http` monitor has no such threshold and is deliberately recorded without being alerted on — the operator who wants the page adds a `tls_expiry` monitor, which is the type that asks for the line
- [ ] Check-now refreshes the certificate as well as the verdict — **it refreshes the verdict only.** The inline checker sees the certificate and drops it, because carrying it means mapping `check.Observation` onto the protocol's types and that mapping lives in `internal/probe`, which neither the API nor the control plane may import (ADR-001). The scheduled check picks it up within one interval, so the gap self-heals; closing it properly is a seam decision about where that mapping belongs rather than a patch
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
- [x] The two frontend routes subscriber mail links to — `/subscriptions/confirm/{token}` and `/subscriptions/unsubscribe/{token}`, plus `/status/{slug}` for the page itself ([the contract is written down](../api/README.md#links-a-status-page-sends-to-its-subscribers)). Built, and the server now answers an unknown document path with the application shell rather than a 404, which is what those three paths needed and a file server does not do. Confirmation runs on load and unsubscribing waits for a button press, which is the same reasoning that kept `List-Unsubscribe` off RFC 8058 one-click: mail clients prefetch, security appliances follow every link in a message, and acting on load would quietly remove people who never clicked. A test names all three paths, because a link in an inbox cannot be reissued
- [ ] Custom domains, with reverse-proxy and ACME documentation — **the column and its cross-organisation uniqueness are enforced; the reverse-proxy and ACME recipes are not written**, and a hostname without them routes nowhere
- [x] Uptime bars, incident and maintenance display — read from the 1d rollup tier, and a day with no observations carries a null ratio rather than being drawn as downtime, which is the single most common way a status page lies
- [x] Subscribe-to-updates via the API, and delivery to the people who subscribed — double opt-in with the confirmation actually sent through the instance relay, and incident updates announced to the confirmed subscribers of every page an incident names, when `notify_subscribers` says to. Email and webhook subscribers both. Every message carries a one-click unsubscribe link, and a message whose link cannot be rendered is not sent at all: the token is stored hashed *and* encrypted (migration `0005`), because it is verified when somebody follows it and replayed at the foot of every message, and a subscription predating that column is issued a fresh token on first use rather than mailed without one. Bulletins are recorded in `notification_deliveries` against their incident, so "did my customers hear about the outage" has an answer. **Two conditions, both reported rather than assumed:** with no instance SMTP relay or no `general.base_url`, a bulletin is recorded `suppressed` naming which is missing, and `/system/info` reports `subscriber_delivery: false` so a dashboard can hide a subscribe box the install cannot honour
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

- [x] SvelteKit project in `web/`, built into `internal/ui/dist` — SvelteKit 2 on Svelte 5 runes, Tailwind 4, static output embedded at compile time with no Node process in production. 69 packages, all build tooling; **nothing ships in the bundle except Svelte**, which is a deliberate deviation from the plan's shadcn/ui and is argued in [web/README.md](../../web/README.md#dependencies) for a reviewer to accept or reject. Two things the embed gets wrong silently are now held by tests: `//go:embed` skips any path beginning with `_`, and SvelteKit puts every hashed asset under `_app/` — the bare pattern compiles, starts, and serves an `index.html` referencing a bundle that is not in the binary. And a single-page application needs the server to answer an unknown document path with the shell, while still 404ing a missing *asset*, because serving HTML where a script was requested turns a broken build into a MIME error three layers from the cause. `internal/ui/dist/index.html` is no longer committed: it is generated, and tracking it meant every developer who built the frontend had a dirty tree
- [x] Dashboard list view with server-side pagination, filter, and search — cursor-paginated, filtered and searched server-side, with the [ADR-004](../adr/004-ui-state-synchronisation.md) reconciliation loop: `/monitors/membership` polled on an interval, a changed version raising a *stale* banner rather than reordering rows under the pointer, and a refresh that re-reads the rows currently held rather than the collection. There is no small-install shortcut that fetches everything when the count happens to be low today. Filters cover status, type, enabled, group, tag, and a debounced search
- [x] Monitor detail with history charts — uptime across three windows, a hand-drawn response-time chart whose line *breaks* at a bucket with no measurement rather than interpolating across it, downtime marked separately from latency underneath it, the recent-check strip, and the certificate panel including what `observed_at` actually means
- [x] Monitor CRUD forms — one form for create and edit, driven by a per-type field table. A type the server offers and the table does not describe falls back to editing the config as JSON rather than hiding the monitor type behind a build. Validation is entirely the server's: failures come back as RFC 9457 with a JSON pointer per bad field, and those pointers are what highlight the controls, so the form has no opinion of its own to disagree with. Editing round-trips the redaction marker untouched, so a form that re-submits its own `GET` cannot destroy a password it was never shown
- [x] Bulk operations (multi-select enable/disable/tag) — including delete and the partial-success contract reported honestly as two numbers, because a bar that says "done" after failing a third of a batch is how somebody discovers next week that their tag never landed
- [x] Notification setup with test-fire and webhook template live preview — all thirteen channel types, with the fields mirroring `internal/notify/config.go`. Test-fire is a real delivery and reports what the provider said verbatim. The preview renders through the server's own renderer rather than a second one in the client, and the variable list is fetched from `/notification-channels/template-variables` rather than hardcoded — a preview that renders through different code than delivery is a preview that lies at the moment somebody is trusting it. A channel's last error is shown on the channel itself, because a channel that has quietly stopped working is the failure mode this feature cannot have
- [x] Dark mode — three states, not two: light, dark, and follow the system. Applied before first paint by an inline script, because reading the preference from a module leaves one frame of the wrong theme on every load. The palette is semantic tokens defined once, with a separate soft variant for status colours used behind text — the same green that reads well as a 12px dot fails contrast as body text. Status is never colour alone
- [x] i18n scaffolding, English complete, translation pipeline documented — every user-facing string in one flat catalogue with dotted identifier keys, a missing key rendering *as the key* rather than silently falling back to English, and locale negotiated by language subtag so `en-AU` resolves to `en`. Adding a language is a JSON file and one line in a map; it never touches a component. No plural machinery yet, deliberately, and the keys are shaped so adding `Intl.PluralRules` later renames nothing. The pipeline is in [web/README.md](../../web/README.md#internationalisation)
- [x] The dashboard is styled after the reference the product is measured against — a fixed left rail, the accent-dotted page titles, a near-black navy ground with one saturated green carrying "up" and an indigo reserved for the single primary action per screen. Status is never colour alone: the round marker carries a glyph, because roughly one in twelve men cannot reliably tell the up green from the down red and it is the most repeated element in the product. The monitor list and the summary rail are one screen rather than two, which is what the reference does and what removes a dashboard that only restated the list. Light mode is a full second palette, not an afterthought — the three-state toggle still means light, dark, and follow the system
- [x] Read screens for incidents and status pages — the incident table with state, impact, duration and the pages each incident names; the status-page list with its visibility, published state, monitor count and a link to the public view. Both are client-filtered on purpose and unlike the monitor list: incidents are bounded by how often things break rather than by how much is monitored, so a hundred is a busy quarter
- [x] The status page editor — create, edit, and delete a page, its ordered sections, and the monitors inside them, plus visibility and password, custom domain, theme and accent, the display toggles, subscriptions, custom CSS and the analytics id. The monitor picker is a server-side search rather than a `<select>` of everything, because a control that ships the whole collection to the browser is the exact mechanism [ADR-004](../adr/004-ui-state-synchronisation.md) exists to prevent; monitors already placed are filtered out of it, since the server's one-section-per-monitor rule is easier to obey than to recover from. An empty password box means *keep the stored one* and never *clear it*, because the read path cannot return a hash — but turning a page public does clear it, rather than leaving a credential behind for a mode that is switched off. The page's subscriber list is on the same screen, with addresses left masked exactly as the server sends them
- [ ] Write screens for incidents and settings — **read works, create and edit do not.** Opening an incident, composing an update, and editing instance settings are all `curl` today
- [ ] A run of recent checks under each monitor in the list — the reference draws one and this does not, and the reason is in the API rather than in the styling: the list endpoint embeds the *last* heartbeat and there is no embed for a run of them, so a real strip would be one request per row, which is the precise fan-out both ADR-004 and the `include=` design exist to prevent. Drawing one from a single beat, or from an uptime ratio, would be inventing a history the client has not been told — so the row shows the two figures it genuinely has and the strip lives on the detail page. Closing this properly means an `include=heartbeats` that resolves a bounded run for the whole page in one query, which is spec surface and therefore not a frontend decision
- [ ] The real-time half of ADR-004 — **reconciliation is built; scoped diffs are not.** The ADR specifies push over NATS, or an in-process bus in solo mode, for the monitor IDs on screen, and this build has no browser-facing channel for one. Staleness is therefore bounded by the poll interval rather than being near-zero for on-screen rows. The list controller is written against "subscribe to visible IDs, reconcile on interval" as its model, which the ADR notes is the thing that must be true from the start for the upgrade to be additive rather than a rewrite
- [ ] The UI benchmark the load-test gate is supposed to hold — [ADR-004](../adr/004-ui-state-synchronisation.md) asserts two invariants on every release, and only the server-side one is measured today. Client payload size and render cost bounded by *viewport* rather than by total monitor count is the half a frontend can break on its own, and nothing yet checks it

## Security

- [x] First-run setup: administrator account, argon2id hashing
- [x] Session management with CSRF protection
- [x] Rate-limited login
- [x] TOTP two-factor, with single-use recovery codes
- [x] API-key authentication with scope enforcement and no privilege escalation
- [x] Encryption at rest: AES-256-GCM envelopes, AAD-bound rows, wrapped data keys, root-key precedence — carrying the TOTP secret today
- [x] Monitor and notification credentials encrypted through the same layer — HTTP basic and bearer auth, Docker client TLS material, gRPC metadata, and every channel's secrets are split out of `config` at the storage boundary and sealed with AAD binding them to their row, so a read path cannot serialise what is not there. Which fields are secret is declared by the checker that owns the config schema, and a test asserts that declaration against the `writeOnly` properties of the frozen spec. Migration `0004` adds the column; monitors written before it are re-sealed on start, verified live against a database that predated it
- [x] Every credential the new surface introduced goes through the same envelope: a webhook's signing secret and its headers, the instance SMTP password, and a status page subscriber's address are all sealed with AAD binding them to their row. The subscriber's address is encrypted rather than hashed because a notification replays it; a status page's password is hashed rather than encrypted because it is only ever verified against — "hash what you verify, encrypt what you replay", applied one more time
- [ ] `/metrics` is unauthenticated to the internet behind a same-host reverse proxy — the endpoint requires `metrics:read` except from loopback, so that a local Prometheus needs no credential, and the check is `isLoopback(clientIP(r))` against `RemoteAddr`. A reverse proxy on the same host connects from `127.0.0.1`, so every proxied request qualifies. What leaks is the full monitor inventory: `cairn_monitor_status` carries every monitor's id, name, and type. `X-Forwarded-For` is deliberately not trusted here and should not start being trusted to fix this; a `--trusted-proxy` setting, or dropping the exemption for a separate bind address, are the two shapes worth considering. Denying `/metrics` at the proxy is the documented mitigation and it depends on the operator reading the page
- [ ] `SECURITY.md`, dependency and container scanning in CI

## Deployment & operations

- [ ] Multi-arch Docker image (amd64/arm64), small base — **the Dockerfile is written and every release target cross-compiles clean, but no image has been built: there is no Docker daemon in the environment it was written in.** It builds the frontend first and the binary second, which is the only order that works — `//go:embed` reads `internal/ui/dist` at compile time, so a Go stage that runs first embeds the committed placeholder and ships a binary that serves an `index.html` referencing a bundle it does not contain. Both stages pin to `BUILDPLATFORM` and cross-compile rather than running under QEMU, which is available precisely because `modernc.org/sqlite` needs no cgo: `GOARCH` is a flag, not a toolchain. Alpine rather than distroless, and the ~8 MB is spent on three things an operator needs at 3am — the `sqlite` CLI the documented backup path calls, something `HEALTHCHECK` can make a request with, and a shell. `ca-certificates` is not a convenience at all: without it every HTTPS and TLS-expiry monitor fails verification and reports its target down
- [ ] `docker run` → first monitor in under 60 seconds, verified in UX review — needs the image above to exist and a human holding a stopwatch, and neither has happened
- [x] `docker-compose.yml` reference and systemd unit example — the compose file publishes to `127.0.0.1` rather than `0.0.0.0`, because the binary has no TLS flags and never will, so any other bind puts session cookies on the wire in the clear. Both set `net.ipv4.ping_group_range` / `CAP_NET_RAW` in comments rather than by default: ICMP tries the unprivileged datagram socket first and reports `unknown` rather than `down` when it cannot open one, so a ping monitor without the grant degrades honestly instead of paging somebody about a container permission. The unit is hardened to `ProtectSystem=strict` with `ReadWritePaths` naming only the data directory, which is worth having because `systemd-analyze security` will check it for you
- [ ] Binary releases with checksums and SBOM — the workflow exists and has never run, because nothing has been tagged. Five targets, including `linux/armv7`: "runs on a Raspberry Pi" is a claim in the README, and the Pi that needs a binary is the one that predates the 64-bit images. All five cross-compile clean today. The frontend is built once and shared across the matrix rather than rebuilt per target — "the amd64 build has a different dashboard than the arm64 build" is not a bug anybody would find quickly
- [x] Reverse-proxy recipes (Caddy, nginx, Traefik) — all three deny `/metrics` at the edge, and that is the point of the page rather than a detail in it. `/metrics` requires `metrics:read` **except from loopback**, and the check is on the connection's remote address, so behind a same-host proxy every request arrives from `127.0.0.1` and the exemption meant for a local scraper applies to the entire internet. The recipes also set the security headers the server does not set itself. Custom-domain status pages are documented honestly: `custom_domain` is stored and subscriber mail already prefers it, but nothing resolves a request by `Host`, and the page reads its slug from the browser's path — so an internal rewrite cannot work and the recipe is a redirect with the slug still visible in the address bar
- [x] Backup and restore: online backup plus verified restore — run end to end, not reasoned about. The central hazard is measured: with the process running, `cp cairn.db` produced a database that passed `integrity_check` and was missing a monitor — one row against the live database's two, because the writes were still in an 848 KB WAL beside a 4 KB main file. `VACUUM INTO` took the snapshot correctly in 28 ms without blocking writers. The restore was verified through the API rather than at the file level: the monitor came back with its stored credential decrypting, and setup reported itself already complete, which together prove the database, the key, and the encryption envelope all survived. Restoring without `cairn.key` refuses to start with the message it should
- [x] Upgrade path documented, with the rollback stance stated — and the stance is stated because it was tested rather than assumed. There are no down migrations and there will not be; restore-from-backup is the documented recovery path and the only one anyone exercises. What the testing turned up is worth its own line below
- [x] Self-monitoring: internal metrics surfaced, including probe health already on the wire — this was already true and is now written down. Thirty-one series, and the doc names the five worth alerting on rather than listing all of them, because each of those five is a way for the product to be broken while looking fine: alerts shed by a full queue, results shed by a full probe buffer, results that could not be attributed, heartbeats going flat, and writer-pool contention. The per-monitor series carry `monitor_id`, `monitor`, and `type` — 15,000 series at the install size this product is built for, with the name label churning on every rename, so the page also says how to drop them at scrape time and what you lose by doing it (nothing, about Cairn's own health)
- [ ] Refuse to start against a schema version newer than the binary knows — found while writing the rollback stance and verified: an older binary opened a database carrying an unknown-higher `schema_migrations` row and started cleanly, with nothing in the log. The runner iterates the migrations the binary carries, finds each already applied with a matching checksum, and never looks at versions beyond them. Whether that is harmless depends entirely on what the newer migration did, and the failure it produces when it is not — write errors, per row, scattered through the log — is the expensive kind. The guard is a comparison against `max(version)` at startup. Documented as an operator burden in the meantime, which is second best
- [ ] Release automation: tagged release → binaries, images, checksums, SBOM — written as a new `release.yml` and never executed. **This is CI configuration and AGENTS.md rule 7 puts it behind explicit human instruction; it needs review as such rather than as documentation.** `BUILD_DATE` comes from the commit rather than the clock, so re-running the workflow on the same tag produces the same bytes — a wall-clock timestamp defeats reproducible builds on its own. `ci.yml` and the load-test gate are untouched

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

1. **The assignment reload itself.** The reader pool moved it off the write
   connection; it is still an O(N) scan of every assignable monitor, and creation
   still slows as the install grows — it is simply no longer blocking anything
   while it does. What is left is the cost of the scan rather than the queue in
   front of it, and `cairn_db_pool_wait_total` is now the number that tells the
   two apart.
2. **The load test against the observation path.** The 5,000-monitor gate has
   never run with certificates on the wire. The arithmetic says it is on the order
   of one row a second and a few hundred extra bytes on one result an hour per
   monitor, and the harness is what turns that from arithmetic into a number —
   the figure to watch is `cairn_db_pool_wait_total`, because the observation is
   the first thing on the ingest path that reads before it writes.

3. **The UI benchmark.** [ADR-004](../adr/004-ui-state-synchronisation.md) asserts
   two invariants on every release and the gate measures one of them. The
   unmeasured half — client payload size and render cost bounded by viewport
   rather than by total monitor count — is the half a frontend can break on its
   own, and there is now a frontend to break it. The membership signal is the
   number to start from: 6.2ms per poll at 5,000 monitors, with a cost that scales
   with connected clients rather than with monitor count, which is the dimension
   the gate does not exercise at all.

The dashboard now exists and consumes the API like any other client, which was the
point of finishing the API first. Status pages are editable from it, which closes
the gap that stung most — subscriber delivery worked end to end while nobody could
create a page to use it. What it still does not have is screens for incidents and
settings — the API is complete for both — and the real-time half of ADR-004, which
needs a browser-facing channel this build has no endpoint for.
