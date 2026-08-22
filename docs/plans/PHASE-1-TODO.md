# Phase 1 — working checklist

Every deliverable in [PHASE-1-PLAN.md](PHASE-1-PLAN.md), as a list that can be
ticked. The plan is the contract and does not change; this is the tracker.

**Status: 2026-08-22.** Every workflow in this repository has now executed and
passed, which is the thing the previous entry said was missing. Four items were
open and all four were the same item — *nothing had been built as a container or
tagged as a release*. Two of them are now closed by evidence rather than by
argument, and the remaining two are closed by the act of cutting v1.0.0.

**The image builds, and it took a broken action reference to find out.** The
container job had never run: it failed at *Set up job* with `Unable to resolve
action aquasecurity/trivy-action@0.28.0`, because that repository stopped
publishing unprefixed tags and removed the old ones. So the Dockerfile — written
against no Docker daemon, and carrying a build order that would fail silently if
wrong — had never once been exercised. It is now built on every pull request and
scanned, and it passes with the scan set to fail on any fixable HIGH or CRITICAL.
The frontend-stage-first order is correct. What is still unproven is the arm64
half: `security.yml` builds for the runner's architecture only, and the
cross-compiled multi-arch manifest is built nowhere but `release.yml`.

**The 5,000-monitor gate ran with the live channel under it, and caught a real
one.** The previous entry listed this as done-but-never-measured at scale. It has
now run, and it failed: `list: dashboard page (include=)` grew 6.6x against a 3.0x
cap. The cause was `LastHeartbeats` — the `include=last_heartbeat` embed joined
against `MAX(time) ... GROUP BY monitor_id`, naming `monitor_id` without
`org_id`, so the only index on the table could not be used and SQLite scanned all
of it, twice. One page of twenty-five rows read 26ms of index at 500 monitors and
489ms at 5,000. Rewritten as the bounded-seek-per-monitor `UNION ALL` that
`RecentHeartbeats` beside it already used, it is 1.2x.

Two things about that are worth more than the fix. It is exactly the failure
ADR-004 exists to prevent — cost bounded by the viewport rather than by install
size — and it was in the endpoint the dashboard calls on every page load, which
is to say the gate caught the thing it was built to catch, on the first run it
was ever allowed to complete. And it hid because **SQLite skip-scans the index
when `ANALYZE` statistics are present, and nothing in this codebase ever runs
`ANALYZE`.** A benchmark that happened to run it would have reported the query
fast; it was fast nowhere it actually ran. There is now a test that asserts the
query plan seeks rather than scans, on a database with no statistics, because
that is the state every real one is in.

**The live channel holds flat at scale.** 1.2 updates a second per stream at 500
monitors and 1.3 at 5,000, across ten streams of twenty-five rows, while the
engine produced 250 results a second. That is the ADR-004 arithmetic confirmed
rather than asserted: the bus delivers to subscriptions holding the id and
nothing else, so per-client cost tracks the viewport and not the install.

**Three other workflow failures were configuration rather than code**, and all
three would have been found by a release: a duplicate `MonitorUpdate` key in the
OpenAPI spec (one schema silently shadowing the other, which is why the
stream-event schema had been dead the whole time); `go.mod` pinned to `go 1.25.0`
against 36 *called* stdlib advisories; and `@latest` on both client generators,
where an upstream release crashed on import before reading the spec. That last
one was also in `release.yml`, where the publish job depends on it — a tag cut
before it was fixed would have produced no GitHub release at all.

**Status: 2026-08-21 (later).** The remaining Phase 1 work landed, and what is
below is the tracker after it. The order it went in was decided by what unblocked
what, and four things are worth recording because none of them is visible from a
tick.

**ADR-004's live half exists, and the framing is a deviation the ADR anticipated
rather than one it forbids.** The ADR rejects Server-Sent Events as a substrate,
and the objection it raises is specific: changing subscription scope means
closing and reopening the stream, and paginating a list is the most ordinary
thing a user does. So the framing here answers that objection directly — one SSE
stream per view, and scope changes are a `PUT` against that stream's own id, so
the connection survives pagination, filtering and search. What sits underneath is
what the ADR actually decides and is unchanged: an in-process bus in solo mode,
NATS with `updates.{org_id}.{monitor_id}.status` subjects in scaled mode, behind
one interface the handler cannot see through. That interface is the ADR's own
open follow-up written down as a type.

**The seam that had blocked check-now turned out to be a package boundary rather
than a decision.** The mapping from `check.Observation` onto the protocol's types
lived in `internal/probe`, which neither the API nor the control plane may import.
It now lives in `internal/observation`, imported by the probe and the API and by
neither the control plane nor `internal/protocol` — and that is asserted
mechanically, in a test and again in CI, because the reason it was a problem in
the first place is that convention held right up until somebody needed the
shortest path.

**The contract test found drift on its first run.** `GET /api/v1/setup` was
served and was not in the spec — a compatibility alias for "anything already
written against it", against a project that has never released. It is gone. An
endpoint the spec does not describe is a privileged endpoint whatever the
intention was, because no other client knows it exists, and that is the promise
the dashboard is built to keep.

**Docker pinning is built and cannot be exercised.** Solo mode has one probe, so
a pinned monitor and an unpinned one land in the same place; the filter is
asserted at the seam it will run at instead. That is the point of building it now
rather than retrofitting it when Phase 4 makes it load-bearing.

One item below is unticked for a reason no amount of code fixes: **no container
image has been built**, because there is no Docker daemon in the environment this
was written in. The Dockerfile is unchanged, every release target still
cross-compiles clean, and the multi-arch build is wired into `release.yml` and
scanned in `security.yml` — but "the image builds" is a claim nobody here can
make, and the sixty-second `docker run` UX review needs the image and a human
holding a stopwatch.

**Status: 2026-08-21 (earlier).** There is a dashboard, and it is styled after the product
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
| Monitor types | 10 | 10 |
| Core monitoring features | 9 | 9 |
| Alerting & webhooks | 10 | 10 |
| Status pages | 6 | 6 |
| REST API | 23 | 23 |
| Kuma migration | 5 | 5 |
| UI | 16 | 16 |
| Security | 10 | 10 |
| Deployment & operations | 6 | 10 |
| Documentation | 8 | 8 |
| Quality gates | 8 | 8 |
| **Total** | **130** | **134** |

The previous table read 102 against 134 and every row of it was wrong, which is
worth stating plainly because this file exists to prevent exactly that. It was
last written by hand before the work it describes landed and then carried forward
unchanged; the checkboxes below had moved and the summary had not. Documentation
showed 1 of 8 with all eight ticked. Kuma migration showed 0 of 5 with all five.

So it is now counted rather than remembered — the numbers above are derived from
the checkboxes below, and anyone changing a box should re-derive them:

```
grep -c '^- \[x\]' docs/plans/PHASE-1-TODO.md    # done
grep -c '^- \[[ x]\]' docs/plans/PHASE-1-TODO.md  # total
```

Four items remain open and they are one item, as they were before: **the release
has never been run.** Three of them — the multi-arch manifest, the binary
archives, and the release automation itself — are closed by a tag and by nothing
else, and they stay unticked until one has actually completed, because the rule
above is that a box needs a demonstrated run behind it and "the configuration was
reviewed" is not one. The fourth needs a person and a stopwatch.

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

**Nine types ship, not ten.** [PHASE-1-PLAN.md](PHASE-1-PLAN.md) says "all 10
monitor types" in two places and its own table on §3.1 lists nine — the plan is
off by one against itself. Nine is what is built, what the API accepts, and what
[the guide](../guides/monitor-types.md) documents. Ten boxes appear below because
HTTP JSON-path assertions are tracked separately from `http`, being the part of
that type most likely to be quietly half-done. The plan is the contract and is
not edited to match; this note is here so the next person counting does not
conclude something is missing.

- [x] HTTP/HTTPS — status codes, keyword (4 modes), response-time threshold, custom method/headers/body, basic and bearer auth, redirect and TLS-verify options
- [x] HTTP JSON-path assertions — a deliberately small subset (root, field names, array indices); anything outside it is rejected at validation rather than ignored at check time
- [x] TCP port
- [x] ICMP ping, including restricted-container detection and TCP fallback — unprivileged datagram socket first, raw second, and unknown rather than down when neither opens
- [x] DNS record — all ten record types, a named resolver, the response code recorded, and the TCP retry on truncation. With no resolver named it walks every nameserver in `resolv.conf` in file order rather than querying only the first: that file is a fallback list, and a host whose primary nameserver is unreachable could otherwise never run a DNS monitor. The failure was quiet rather than loud, which is why it needed finding twice — an unreachable resolver is reported `unknown` rather than down, correctly, so the symptom was a monitor sitting on pending forever showing no failures while monitoring nothing. Every candidate shares one timeout, so three dead nameservers cannot cost three times what the operator configured
- [x] SSL/TLS expiry — the handshake is made unverified and the chain checked by hand, so an expired certificate is reported as expiry rather than as a generic TLS error
- [x] Domain expiry (RDAP/WHOIS), with a per-type minimum interval — RFC 9224 bootstrap, WHOIS fallback, one registry lookup a day per domain
- [x] Push / heartbeat dead-man's-switch — control-plane-side, never assigned to a probe
- [x] Docker container, with monitor-to-named-probe pinning — a monitor carries a `probe_id` (migration `0006`), and the assignment set is built per requesting probe rather than once for everybody: a pinned monitor is withheld from every probe but the one it names. The pin is *placement* rather than checking, so it lives on the monitor rather than inside a type-specific config, and the next type that needs it — a `grpc` monitor reachable only from one network segment — wants no second mechanism. On a `docker` monitor the server fills it in when the install has exactly one probe, which is every solo install, and refuses the write naming `/probe_id` when it has more: guessing which host somebody meant produces a monitor reporting a container missing that was never meant to be there. `GET /api/v1/probes` exists so a client can find out what the choices are, and it returns no credential — `token_hash` is absent from the response *and* from the struct behind it, which is a stronger guarantee than remembering to omit it. Solo mode cannot exercise the filter end to end, so it is asserted at the seam it will run at
- [x] gRPC health

## Core monitoring features

- [x] Retries, timeouts, intervals down to 20s
- [x] Upside-down mode
- [x] Groups — one level of nesting, and a parent reports the worst status among its monitors *and its children's*, because a parent group showing green during an outage underneath it is the worst thing a monitoring tool can do. Deleting a group ungroups its monitors and promotes its child groups; it never deletes what it contained
- [x] Tags — the slug is derived from the name rather than supplied, so two tags that render identically in a list cannot both exist; a name colliding on slug is a `409` naming the slug
- [x] Dependency-aware suppression — transitive up the chain, and a parent under maintenance suppresses its children as surely as a parent that is down: taking the router down for a firmware upgrade is the most known problem there is. The child's own heartbeat still records the real outage, so its uptime figure is unaffected; only the page is withheld
- [x] Maintenance windows: single, daily, weekly, monthly, and cron, evaluated in the window's own IANA zone with the zone database embedded in the binary — "02:00 every Sunday" survives a daylight-saving transition still meaning 02:00. Targets resolve by query through monitors, groups, and tags, so a window covering a tag keeps covering monitors added later. **Groups and tags have tables and no API**, so only monitor targets can be created today; referencing a group or tag is a validation error naming the field rather than a foreign-key failure
- [x] Certificate and domain expiry surfaced as upcoming-expiry data — `/monitors/{id}/certificate`, `include=certificate`, and the overview's expiring-soon counts read tables the ingest path now writes. The TLS and HTTP checkers report what the handshake presented and the domain checker reports the registration behind the name, both on `Result.certificate` / `Result.domain` ([protocol §7.4](../probe/protocol.md#74-observations)); ingest stores one row per monitor, replaced in place. The observation rides the result frame on change and once an hour otherwise, because it is several hundred bytes against a hundred for the result carrying it and sending it on every check would cut the probe buffer's outage coverage to a fifth — so `observed_at` means "last confirmed on the wire" to within an hour, and a renewal lands on the next check. `monitor.certificate_expiring` and `monitor.domain_expiring` fire when the countdown crosses the monitor's own `days_remaining_threshold`, again when the certificate is replaced by one still inside it, and once a day after that; deduplicated against the stored row rather than memory, so a restart does not re-page. An `http` monitor has no such threshold and is deliberately recorded without being alerted on — the operator who wants the page adds a `tls_expiry` monitor, which is the type that asks for the line
- [x] Check-now refreshes the certificate as well as the verdict — the seam decision, taken: the mapping from `check.Observation` onto the protocol's types moved out of `internal/probe` into `internal/observation`, which the probe and the API import and the control plane does not. One definition rather than two, which was the actual objection — written twice they agree on the day they are written and diverge the first time a field is added, and the symptom is a certificate panel correct after a scheduled check and stale after a manual one. The observation is unconditional on this path rather than rate-limited: the hourly resend interval exists because a probe's buffer is a fixed size, and a manual check has no buffer and happens when somebody presses a button. ADR-001 holds and is now checked by `go list -deps` in a test and again in CI
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
- [x] Custom domains, with reverse-proxy and ACME documentation — and the routing, which turned out to be the part that mattered. The previous recipe was a redirect with the slug still visible in the address bar, because an internal proxy rewrite cannot work: the dashboard reads its slug from the browser's path and a rewrite leaves that path at `/`. The resolution therefore happens where the answer can reach the client — a request whose `Host` matches a published page's `custom_domain` is served the application shell with that slug in it, and the frontend renders the page at whatever path was asked for. The status page became a component so it can render from two addresses; the hostname map is cached with a 30-second TTL and dropped on every status page write, because the one moment somebody is watching for this to work is right after they saved it. Published pages only — a draft answering on a customer's hostname is the one thing an operator setting one up must not get by accident. The ACME half is written for all three proxies, including Caddy on-demand TLS with the warning that matters: without an `ask` endpoint it is a rate-limit exhaustion attack against your own Let's Encrypt account, and this build has no `ask` endpoint yet
- [x] Uptime bars, incident and maintenance display — read from the 1d rollup tier, and a day with no observations carries a null ratio rather than being drawn as downtime, which is the single most common way a status page lies
- [x] Subscribe-to-updates via the API, and delivery to the people who subscribed — double opt-in with the confirmation actually sent through the instance relay, and incident updates announced to the confirmed subscribers of every page an incident names, when `notify_subscribers` says to. Email and webhook subscribers both. Every message carries a one-click unsubscribe link, and a message whose link cannot be rendered is not sent at all: the token is stored hashed *and* encrypted (migration `0005`), because it is verified when somebody follows it and replayed at the foot of every message, and a subscription predating that column is issued a fresh token on first use rather than mailed without one. Bulletins are recorded in `notification_deliveries` against their incident, so "did my customers hear about the outage" has an answer. **Two conditions, both reported rather than assumed:** with no instance SMTP relay or no `general.base_url`, a bulletin is recorded `suppressed` naming which is missing, and `/system/info` reports `subscriber_delivery: false` so a dashboard can hide a subscribe box the install cannot honour
- [x] Unauthenticated read path that holds up under load — the golden-path test reads it while an outage is live and asserts two things: that it says something other than `operational` while the monitor is down, and that the monitor's target does not appear anywhere in the rendered document. The second is the one worth automating. It is a separate projection rather than a filtered monitor read precisely so a field cannot leak through a shape with no place to put it, and this is the endpoint where a leak reaches strangers

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
- [x] Generated Go and TypeScript clients published from the spec — generated on every pull request and attached to every release, and **never committed**. Committing them would put thousands of lines of machine output into every spec diff, and a reviewer skimming past generated code is a reviewer skimming past the spec change that produced it. What CI has to prove is that the spec *can* be generated from — a `$ref` to a schema that does not exist lints clean and produces a client that will not compile — and the generation is that proof

## Kuma migration

- [x] `cairn import kuma <path>` CLI — against a stopped install, which the help text says because SQLite takes one writer and the alternative is somebody discovering it. `--dry-run` produces the whole report and writes nothing
- [x] Guided UI import flow — `POST /api/v1/imports/kuma`, asynchronous with a job to poll, and the same importer through the same seam as the CLI. Two write paths would be two sets of rules and the one that drifts is always the one nobody exercises, which for an import is the CLI a user runs exactly once. The upload is spooled to a mode-0600 file, processed, and deleted on every path out including the panic one: a `kuma.db` is a file full of somebody's URLs and credentials. One import at a time, because two would both read the existing-names catalogue before either wrote anything and both would decide the same name was free
- [x] Multi-instance merge — several databases in one run, with identity kept per file: two Kuma instances both have a monitor with id 1. Names collide constantly, which is what `--on-conflict` and `--name-prefix` are for, and `rename` is the default because it is the only one of the three that cannot lose one of them. **`replace` behaves as `skip` and the report says so** — honouring it literally would mean deleting an existing monitor and its whole history to make room for one named the same, during a migration, which is exactly when nobody is able to notice
- [x] Import report naming everything that did not map cleanly — every source entity appears exactly once, and both the CLI and the dashboard lead with what needs attention and put the tally after it. A report that opens with "1,204 monitors imported" and buries thirty unsupported types below the fold is a report that gets skimmed, and skimming it is how somebody finds out during an outage that a monitor they thought they had was never created. The job state is `succeeded`, `partial`, or `failed`, and `partial` exists because collapsing it into either of the others is how somebody concludes their migration finished when thirty monitors are missing
- [x] Close the three open gaps in the schema mapping ([data model §10](../data-model/README.md)) — all three, and none of them by adding schema. **Unsupported types** are recorded with their name and type and nothing is written: skipping silently loses the name, interval, tags and notification attachments somebody spent an afternoon on, and importing as "the nearest type" invents a check — an MQTT monitor imported as a TCP check on 1883 is green while the broker rejects every publish, which is a monitoring tool lying. **`monitor_tag.value`** splits into one tag per value, so `env` attached as `production` and `staging` becomes two tags and each monitor gets the one it actually had; a nullable `value` column is cheap in the schema and expensive in the tag filter, the bulk operation, the status page display and the API resource, all of which would have to decide what a value means. **Per-monitor proxies** have no home and are not given one from inside a migration tool: the monitor imports without the proxy and the report says the check will now be made from this host directly, which is a statement somebody can act on and is materially different from the monitor quietly going red

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
- [x] Groups and tags have a screen — they could be assigned everywhere and created nowhere. The monitor form's pickers, the list's two filters and the bulk bar's add-tag all hide themselves when their list is empty, and nothing in the dashboard could make either list non-empty, so a fresh install had every one of those controls permanently invisible with no way to find out why: the API had supported the whole resource since Month 2 and only the UI was missing. One screen for both, because a group is exclusive and hierarchical, a tag is many-to-many and flat, and somebody organising monitors is choosing between them rather than visiting two places. The parent picker offers only top-level groups and withholds itself entirely for a group that already has children, which is the same pair of rules the server enforces — the point is not to duplicate the check but to stop offering the option that will be refused. The tag slug is deliberately *not* previewed while typing: it is derived server-side so two tags cannot render identically, and reproducing that derivation in the client would be a second implementation of one rule, the kind that agrees in testing and disagrees on the first name with an accent in it. Verified against a running build rather than reasoned about: the counts are real, a rename moves the slug, and deleting a group leaves its monitor behind ungrouped and its child group promoted to the top level, which is what the confirmation promises
- [x] Write screens for incidents and settings — an incident can be opened, edited, advanced and deleted from the dashboard, and instance settings are editable. The incident detail page is the only place a state change can be made, which is the server's rule rather than a simplification: an incident advances through its timeline so that every state change carries the sentence explaining it, and a dropdown that moved one from investigating to identified with nobody saying what was identified is the record a post-mortem cannot reconstruct. Settings follows the status page editor's rule for the SMTP password — an empty box means *keep the stored one* and never *clear it*, because the read path cannot return it and a form that round-trips its own `GET` must not be able to destroy a credential it was never shown
- [x] A run of recent checks under each monitor in the list — closed where it actually was, in the API: `include=heartbeats` resolves a bounded run for the whole page in one statement, with `heartbeats_limit` clamped rather than refused. The query is the part worth reading. A window function is the textbook answer and is quietly the wrong one: SQLite has to produce every row in a partition before it can number them, and a monitor on a 60-second interval holding a week of raw history has ten thousand of them — twenty-five of those on one page is a quarter of a million rows read to return five hundred. So each monitor gets its own bounded index seek and they are stitched with `UNION ALL` into one statement: one round trip, one plan, and a row count bounded by page size times limit rather than by how long the install has been running. The frontend asks for thirty and draws them; the load gate now measures the payload
- [x] The real-time half of ADR-004 — scoped diffs, over an SSE stream whose subscription changes in place. The ADR rejects SSE and names why: changing scope means closing and reopening the stream. That objection is answered rather than worked around — `PUT /api/v1/live/{streamId}/scope` replaces what a stream carries and the connection keeps running, so paginating, filtering and searching all happen without a reconnect. Underneath is the part the ADR actually decides and it is unchanged: `internal/live.Bus`, an in-process bus in solo mode because a broker would break "solo mode keeps zero required external dependencies", and NATS with the specified subject shape in scaled mode. The handler cannot tell which it holds, which is the ADR's own open follow-up written down as a type. **Reconciliation was not removed**, and deleting it because "we have push now" is the specific mistake the ADR's Consequences section warns about — nothing is subscribed to a monitor that is off-screen, so nothing can tell a filtered view that one has started matching it. The global summary is its own channel, computed server-side and debounced: a burst that transitions several thousand monitors inside one scheduler tick must not become several thousand scans of `monitor_state` on the ingest path, which is the case the harness constructs on purpose. A slow subscriber loses messages rather than blocking, and the loss is counted, because from the browser's side a dropped update is a row that simply stopped moving
- [x] The UI benchmark the load-test gate is supposed to hold — two additions, because the unmeasured invariant is two claims. The harness now records the **response body size** on every list scenario and asserts its growth across scales, with a scenario that sends exactly what the dashboard sends (`include=last_heartbeat,heartbeats,uptime`). That catches the change of kind rather than of degree: adding `tags` to the default include set, or raising `heartbeats_limit`, multiplies the payload and moves no timing figure in the report. And it opens ten **live streams** scoped to a page each and reports updates per second per stream, plus a count of diffs delivered for a monitor the receiving stream never subscribed to — which is a correctness failure with no tolerance, because ADR-004's entire design rests on the channel being scoped. Measured at 50 and 200 monitors: 1.0 and 1.3 updates/sec/stream, zero foreign. `cairn_live_subscribers` is on `/metrics` for the same reason — it is the one cost that scales with connected clients rather than with monitor count

## Security

- [x] First-run setup: administrator account, argon2id hashing
- [x] Session management with CSRF protection
- [x] Rate-limited login
- [x] TOTP two-factor, with single-use recovery codes
- [x] API-key authentication with scope enforcement and no privilege escalation
- [x] Encryption at rest: AES-256-GCM envelopes, AAD-bound rows, wrapped data keys, root-key precedence — carrying the TOTP secret today
- [x] Monitor and notification credentials encrypted through the same layer — HTTP basic and bearer auth, Docker client TLS material, gRPC metadata, and every channel's secrets are split out of `config` at the storage boundary and sealed with AAD binding them to their row, so a read path cannot serialise what is not there. Which fields are secret is declared by the checker that owns the config schema, and a test asserts that declaration against the `writeOnly` properties of the frozen spec. Migration `0004` adds the column; monitors written before it are re-sealed on start, verified live against a database that predated it
- [x] Every credential the new surface introduced goes through the same envelope: a webhook's signing secret and its headers, the instance SMTP password, and a status page subscriber's address are all sealed with AAD binding them to their row. The subscriber's address is encrypted rather than hashed because a notification replays it; a status page's password is hashed rather than encrypted because it is only ever verified against — "hash what you verify, encrypt what you replay", applied one more time
- [x] `/metrics` is no longer unauthenticated to the internet behind a same-host reverse proxy — and the default changed, not just the knob. A loopback peer **carrying `X-Forwarded-For`** from a peer that is not a declared proxy is no longer exempt: all three published recipes set that header, so a proxied request stops passing as a local scrape while a Prometheus connecting directly, which sends no such header, still does. `--trusted-proxy` then makes the header evidence rather than a claim — from a declared peer the chain is read right to left, skipping our own hops, and the exemption is applied to the *real* client. The header is believed nowhere else in the process and `--trusted-proxy` is the only thing that makes it believed anywhere. Denying `/metrics` at the edge is still the recommendation and still in every recipe
- [x] `SECURITY.md`, dependency and container scanning in CI — the policy names what is in scope and, more usefully, what is not, so nobody spends a weekend on a report that will be closed. Scanning is `security.yml`: `govulncheck` over both modules, `npm audit` at high-and-above (every one of those 69 packages is build tooling and nothing but Svelte reaches the browser, so failing on a moderate advisory in a bundler's transitive dependency would train everybody to pass `--force`), and Trivy against a built image — reporting everything through SARIF and *failing* only on findings with a fix available, because a gate that blocks every pull request over an unfixed base-image advisory is a gate somebody disables. On a weekly schedule as well as on pull requests, and the schedule is the half that matters: a disclosure against a dependency already shipped does not arrive with a pull request attached

## Deployment & operations

- [ ] Multi-arch Docker image (amd64/arm64), small base — **the image builds and is scanned; the arm64 half is still unproven.** It had never been built at all, and not for the reason recorded here twice: `security.yml`'s container job failed at *Set up job* with `Unable to resolve action aquasecurity/trivy-action@0.28.0` — that repository stopped publishing unprefixed tags and removed the old ones — so the Dockerfile written against no Docker daemon had never once been exercised by anything. It now builds on every pull request and Trivy scans the layers with `exit-code: 1` on anything fixable at HIGH or CRITICAL, and it passes. The build order that would have failed silently is correct: the frontend stage runs first, because `//go:embed` reads `internal/ui/dist` at compile time and a Go stage that ran first would embed the committed placeholder and ship a binary that starts perfectly and serves an `index.html` referencing a bundle it does not contain. Both stages pin to `BUILDPLATFORM` and cross-compile rather than running under QEMU, which is available precisely because `modernc.org/sqlite` needs no cgo: `GOARCH` is a flag, not a toolchain. Alpine rather than distroless, and the ~8 MB is spent on three things an operator needs at 3am — the `sqlite` CLI the documented backup path calls, something `HEALTHCHECK` can make a request with, and a shell. `ca-certificates` is not a convenience at all: without it every HTTPS and TLS-expiry monitor fails verification and reports its target down. **What remains open is narrow and real:** CI builds for the runner's architecture only, so `linux/arm64` and the multi-arch manifest are produced nowhere but `release.yml`, and that runs on a tag
- [ ] `docker run` → first monitor in under 60 seconds, verified in UX review — needs the image above to exist and a human holding a stopwatch, and neither has happened. The path it would follow is written down now ([quickstart](../guides/quickstart.md)), which at least means the review has something to review against
- [x] `docker-compose.yml` reference and systemd unit example — the compose file publishes to `127.0.0.1` rather than `0.0.0.0`, because the binary has no TLS flags and never will, so any other bind puts session cookies on the wire in the clear. Both set `net.ipv4.ping_group_range` / `CAP_NET_RAW` in comments rather than by default: ICMP tries the unprivileged datagram socket first and reports `unknown` rather than `down` when it cannot open one, so a ping monitor without the grant degrades honestly instead of paging somebody about a container permission. The unit is hardened to `ProtectSystem=strict` with `ReadWritePaths` naming only the data directory, which is worth having because `systemd-analyze security` will check it for you
- [ ] Binary releases with checksums and SBOM — **still never run, because nothing has been tagged**; a workflow that has not executed is a workflow that does not work until proven otherwise. One thing that would have broken it is fixed: the client generators it depends on were pinned to `@latest`, and an upstream release crashed on import before reading the spec — the same failure that broke `ci.yml`, except that here the publish job `needs` it, so a tag would have produced binaries, an image, an SBOM, and no GitHub release. It now also generates and attaches the Go and TypeScript clients, and ships `SECURITY.md` inside each archive. Five targets, including `linux/armv7`: "runs on a Raspberry Pi" is a claim in the README, and the Pi that needs a binary is the one that predates the 64-bit images. All five cross-compile clean today. The frontend is built once and shared across the matrix rather than rebuilt per target — "the amd64 build has a different dashboard than the arm64 build" is not a bug anybody would find quickly
- [x] Reverse-proxy recipes (Caddy, nginx, Traefik) — all three deny `/metrics` at the edge, and that is the point of the page rather than a detail in it. `/metrics` requires `metrics:read` **except from loopback**, and the check is on the connection's remote address, so behind a same-host proxy every request arrives from `127.0.0.1` and the exemption meant for a local scraper applies to the entire internet. The recipes also set the security headers the server does not set itself. Custom-domain status pages are documented honestly: `custom_domain` is stored and subscriber mail already prefers it, but nothing resolves a request by `Host`, and the page reads its slug from the browser's path — so an internal rewrite cannot work and the recipe is a redirect with the slug still visible in the address bar
- [x] Backup and restore: online backup plus verified restore — run end to end, not reasoned about. The central hazard is measured: with the process running, `cp cairn.db` produced a database that passed `integrity_check` and was missing a monitor — one row against the live database's two, because the writes were still in an 848 KB WAL beside a 4 KB main file. `VACUUM INTO` took the snapshot correctly in 28 ms without blocking writers. The restore was verified through the API rather than at the file level: the monitor came back with its stored credential decrypting, and setup reported itself already complete, which together prove the database, the key, and the encryption envelope all survived. Restoring without `cairn.key` refuses to start with the message it should
- [x] Upgrade path documented, with the rollback stance stated — and the stance is stated because it was tested rather than assumed. There are no down migrations and there will not be; restore-from-backup is the documented recovery path and the only one anyone exercises. What the testing turned up is worth its own line below
- [x] Self-monitoring: internal metrics surfaced, including probe health already on the wire — this was already true and is now written down. Thirty-one series, and the doc names the five worth alerting on rather than listing all of them, because each of those five is a way for the product to be broken while looking fine: alerts shed by a full queue, results shed by a full probe buffer, results that could not be attributed, heartbeats going flat, and writer-pool contention. The per-monitor series carry `monitor_id`, `monitor`, and `type` — 15,000 series at the install size this product is built for, with the name label churning on every rename, so the page also says how to drop them at scrape time and what you lose by doing it (nothing, about Cairn's own health)
- [x] Refuse to start against a schema version newer than the binary knows — the guard is a comparison against `max(version)` before the migration loop runs, and it names the offending migration, because "schema too new" without a number leaves the operator guessing which backup to reach for. Against the *maximum* rather than the set: a gap in the middle is a different fault — a migration file deleted from the binary — and the checksum pass is what catches that. What this catches is the downgrade, which is the one an operator performs deliberately and at speed
- [ ] Release automation: tagged release → binaries, images, checksums, SBOM — **reviewed as configuration and agreed to, which is what AGENTS.md rule 7 asks for and was the outstanding half; still never executed, which is the other half and the reason this stays unticked.** `BUILD_DATE` comes from the commit rather than the clock, so re-running the workflow on the same tag produces the same bytes. It now publishes to **two** registries: GHCR, which needs no secret and is what every compose file and install recipe here points at, and Docker Hub, which is where `docker run uptimecairn/uptime-cairn` resolves and therefore where the README's own quick start has to work. Publishing to one and documenting the other is how a quick start silently stops working, and it was that way until now. The Docker Hub login fails the release rather than skipping quietly when its credentials are absent, because a tag that publishes to half its registries and reports success is discovered by a stranger a week later. **Ticked for the wiring, not for a run:** the workflow has still never executed, and the first tag is the test

## Documentation

- [x] Development run-through ([running.md](../development/running.md))
- [x] Install guide (Docker, compose, binary, Pi) — [guides/install.md](../guides/install.md). Every flag in it was read off `--help` rather than remembered, which is the one thing an install guide cannot get wrong
- [x] First-monitor quickstart — [guides/quickstart.md](../guides/quickstart.md). It ends by telling somebody to point a monitor at a URL that will fail on purpose, because the most valuable minute anybody spends on a monitoring tool is the one where they make it fire
- [x] Per-monitor-type reference — [guides/monitor-types.md](../guides/monitor-types.md). It opens with the status vocabulary rather than with the types, because `unknown` against `down` — a statement about the probe against a statement about the target — is the distinction that runs through the whole product and the one that reads as a bug until it is explained
- [x] Alerting setup per channel — [guides/alerting.md](../guides/alerting.md), including the full template variable catalogue and the reason the three states of `notification_channel_ids` are kept apart
- [x] API reference generated from the spec — [api/reference.md](../api/reference.md), produced by `tools/apidoc` and checked in CI, so it cannot come to disagree with the contract it describes. The generator reads the spec with a scanner rather than a YAML library, on the same reasoning as the migration runner: a hundred lines of our own code against a build-time dependency on a project that publishes an SBOM. It refuses anything it does not understand rather than skipping it, because a reference missing endpoints is the failure it exists to prevent
- [x] Kuma migration guide — [guides/migrating-from-uptime-kuma.md](../guides/migrating-from-uptime-kuma.md). Half of it is what does *not* come across, which is the half somebody deciding whether to migrate actually needs
- [x] Backup, upgrade, and operations guide — [operations/observability-and-ops.md](../operations/observability-and-ops.md) indexes the three that existed and adds the parts that only matter when you are running it: the five series worth alerting on, retention on an SD card, and the one thing a control-plane outage does not survive

## Quality gates

- [x] Unit tests over the paths where a bug would be silent — checker classification, transition table, buffer shedding, migration checksums, write idempotency
- [x] `go test -race` clean, in CI
- [x] gofmt, vet, build, and buf lint/format/breaking in CI
- [x] Contract tests verifying the server against the frozen OpenAPI spec — both directions, and the second is the one that earns its keep. Every Phase 1 operation in the spec has a handler (91 exercised, and the test fails if it finds fewer than 60, because a contract test that quietly exercises nothing passes forever), *and* nothing is served that the spec does not describe. The routing table is read out of the syntax tree rather than from a parallel list on the server, because a parallel list is the same class of bug one level up. It found drift on its first run
- [x] Golden-path E2E: install → create monitor → down event → alert fires → recovery → status page reflects it — through the whole binary, with a real HTTP target it can break at a known instant and a real webhook receiver counting deliveries. What it catches that no unit test can is wiring: every component in that chain has tests, and none of them proves the composition root joined the dispatcher to the control plane. A product whose parts all work and are not joined up is a product silently doing nothing
- [x] Load-test harness pointed at the real engine — the `http` target starts a `cairn`, creates the workload through the real API against an endpoint the harness serves, and measures what the engine achieves rather than what storage can absorb. Those are opposite claims and the report labels which is which: a driven rate is a ceiling, an observed rate is bounded by arithmetic, and a rate *above* the schedule means a backlog draining rather than headroom. It also breaks every monitored endpoint at once, because a burst that marks several thousand monitors down inside one scheduler tick is what the delivery queues were sized against and nothing had ever counted the deliveries on the other end. The 5,000-monitor gate passes: 239.6 heartbeats/sec against 250 implied, 5,000/5,000 marked down in 20.6s, 9,682 alerts and 9,682 webhook deliveries with nothing shed
- [x] 5,000-monitor CI acceptance gate against the engine, then the UI benchmark — `load-test.yml` now runs both targets as separate jobs, because they measure opposite things and a report presenting one number for both would be lying: a driven rate is a ceiling, an observed rate is bounded by arithmetic. The engine job builds the frontend first, which is load-bearing rather than tidy — `//go:embed` reads `internal/ui/dist` at compile time, so a Go build that runs first embeds the committed placeholder and every scenario still passes, which is the worst version of that mistake. `-write-seconds 30` rather than the storage gate's 5: a five-second window at 5,000 monitors is 1,250 heartbeats, few enough that the scheduler's dispersal lands unevenly inside it and the ratio measures the window rather than the engine. **This is CI configuration and needs reviewing as such (AGENTS.md rule 7).**
- [x] Crash-recovery test: kill mid-cycle, assert at most one tick lost and checks resume — and it is honest about what it does not test. A process cannot SIGKILL itself and survive to assert anything, so what runs is stop-and-restart against the same data directory: the history survives, the state row survives (a restart that reset every monitor to pending would look like a fleet-wide outage on the dashboard), and checking resumes on its own within roughly an interval. The torn-write half is SQLite's WAL, which is its own project's problem and is tested by its own project. It deliberately does **not** assert zero heartbeats lost — a check in flight when the process died produced no result, and claiming to recover one would be inventing it

---

## What to do next, and why in this order

Four items are open and they are one item: **nothing has been tagged as a
release.** Everything else in this file is written, tested, and has been run —
including, now, every workflow in the repository.

0. **The version number is a deviation from the plan, taken deliberately.**
   [PHASE-1-PLAN.md](PHASE-1-PLAN.md) §6 says "tagged v0.1.0" and the README said
   the first release would be v0.1. It is being cut as **v1.0.0** instead. The
   argument for it is that the OpenAPI spec was frozen before the code was
   written and the contract tests hold the server to it in both directions, so
   the compatibility promise semver attaches to a 1.0 is one this project can
   actually make — which is not usually true of a first release. The argument
   against is that Phases 2 to 5 are most of the roadmap, and a 1.0 reads to some
   people as "finished". The status section of the README answers that directly
   by naming what is not in it.

1. **Cut a prerelease first, and treat it as the test it is.** `release.yml` has
   still never executed, and the parts most likely to be wrong are the ones no
   local check exercises: the artifact pattern the publish job downloads with,
   the checksum ordering, the arm64 half of the image, and whether the Docker Hub
   credentials are actually set. `v1.0.0-rc.1` costs nothing and answers all four
   — the workflow marks anything carrying a prerelease suffix as such, so it
   cannot be picked up as `latest` by an unpinned installer, and
   `docker/metadata-action` does not apply the `{{major}}.{{minor}}` tag to a
   prerelease, so it does not claim the moving `1.0` tag either. A version number
   spent on a run that half-failed is the one cost that cannot be undone.

2. **Then tag `v1.0.0`.** This closes three of the four boxes above at once —
   binaries with checksums and an SBOM, the multi-arch manifest including the
   arm64 half that CI cannot build, and the release automation itself.

3. **The sixty-second review, with the image and a stopwatch.** It is the one
   claim in the README that cannot be tested by a test, and the image now exists
   to review against. [The quickstart](../guides/quickstart.md) is written down,
   so the review has a written path rather than somebody's memory of the flow.
   This is the last box, and it is the one no tag can tick.

### Two things that are done and are worth re-measuring rather than trusting

**The assignment reload is still an O(N) scan of every assignable monitor.** The
reader pool moved it off the write connection and the settle window collapsed a
burst of writes into one recompute, so it is no longer blocking anything while it
runs. What is left is the cost of the scan, and it now has a second consumer: the
Kuma importer creates monitors through the same path, which is exactly the
workload that found the quadratic behaviour in the first place. It coalesces
correctly. It is still a scan. The load gate reports it as a warning on every run
— 370 monitors a second at 500 and 93 at 5,000 — which is the shape of an import
slowing down as the install it is importing into grows.

**No query in this codebase is planned against `ANALYZE` statistics, because
nothing runs `ANALYZE`.** That is what let `LastHeartbeats` scan the whole
heartbeats table for a page of twenty-five rows without anyone noticing, and the
fix was to write a predicate that does not need statistics to be planned well.
The general question is still open: either the store should maintain statistics
(`PRAGMA optimize` on a schedule, which takes the write connection), or every
index-sensitive read needs the same plan assertion the embeds now have. One of
those is a decision; neither is a bug today.
