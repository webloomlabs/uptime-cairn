# ADR-004: UI State Synchronisation — Server-Side Pagination with ID-Scoped Live Diffs and Periodic Reconciliation

- **Status:** Accepted
- **Date:** 2026-08-05
- **Deciders:** [Shakil Ilham](https://github.com/silham)

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

## Context

This is the specific mechanism behind Uptime Kuma's hard scale ceiling
(project plan §1.4a): Socket.IO broadcasts the full monitor state to every
connected browser on every heartbeat cycle. Two multipliers stack — message
size (all N monitors, every push) and client count (every open tab) — and
at 300–600 monitors with a handful of browsers open, both server egress and
client-side render cost fall over. Community reports describe the UI as
"stuck on websocket errors." This is not a database or query problem; it's
a fan-out problem, and it is the single most consequential UX/architecture
decision left unaddressed after ADR-002 (storage) and ADR-003 (tenancy),
per the project plan's own next-steps list (§10).

The fix has two genuinely separate halves. **Initial load** — the paginated,
filtered, server-side-queried monitor list — is unremarkable and not really
in dispute; cursor-based pagination keyed on `(updated_at, id)` avoids the
reordering problems offset pagination gets into when rows change status in
real time.

**Live updates** are the actual hard problem, because filtered views and
real-time push fight each other. If a client subscribes only to the exact
monitor IDs currently rendered on its screen, the Kuma fan-out problem
disappears completely — but a monitor that changes status *outside* the
current viewport, and would now match an active filter (e.g. "status =
down"), has no subscription telling the server anyone cares about it. The
client's filtered view silently goes stale until the user manually
refreshes or re-paginates.

Two ways to close that gap were considered:

- **Filter-aware pub/sub** — clients subscribe to a live predicate (e.g.
  `status=down AND tag=production`), and the server evaluates every state
  change against every active subscription's predicate, pushing add/remove
  events as view membership changes. Fully real-time and correct, but this
  is not something subject-based transports like NATS can express natively
  — subject matching is structural (segments of a predefined hierarchy),
  not predicate evaluation. Building this means a stateful
  filter-registry-and-evaluator service, scaling with connected-client
  count, layered on top of whatever transport carries raw change events.
  Real, ongoing complexity, on top of a Phase 1 that already has to deliver
  full monitor-type coverage, ~10+ alert channels, and the Kuma importer.
- **Scoped diffs (ID-based) plus periodic reconciliation** — clients
  subscribe to exactly the monitor IDs currently on screen for real-time
  in-place updates, and separately poll a cheap "has this view's membership
  changed?" signal (a version counter or count+hash) on a short interval,
  triggering a re-fetch of just the affected page boundary when it has.
  Composes almost for free with NATS, which is already committed to for
  probe fan-out (§5.2, §5.5) — ID-scoped subjects are exactly the kind of
  matching NATS is built for. The staleness window for filtered-view
  membership is bounded by the reconciliation interval (seconds), not
  unbounded, which is judged an acceptable trade against the meaningfully
  larger build for full real-time filter correctness.

Team preference, given Phase 1's own scope pressure and the project's
general instinct (established in ADR-002 and ADR-003) to build the
proportionate version now and revisit with real signal later, is the
second option.

## Decision

**Initial view load is always server-side paginated, filtered, and
searched.** The client never requests or receives the full monitor set;
every list view is a cursor-paginated query (`updated_at, id`) against the
API, with filters and search applied server-side. This applies uniformly
regardless of monitor count — there is no "small install" exception where
the full set is sent because it happens to be small enough today.

**Live updates use ID-scoped subscriptions over NATS, not filter-aware
pub/sub.** When a client renders a page of monitors, it subscribes to
exactly those monitor IDs via subjects shaped
`updates.{org_id}.{monitor_id}.status` (see Compliance section on the
tenancy-isolation side-benefit of this shape). State changes for monitors
currently on screen are pushed as scoped diffs, not full-state blobs.
Monitors outside the current viewport do not generate any push to that
client, by construction — this is what removes the fan-out problem
entirely rather than tuning around it.

**Filtered-view membership is kept fresh by periodic reconciliation, not
real-time push.** Each active filtered view polls a lightweight
membership-check endpoint (a version counter, or count+hash, scoped to the
active filter) on a short interval — starting at 5 seconds, tunable per
deployment size. If the signal indicates membership may have changed, the
client re-fetches the affected page boundary. This bounds staleness to the
reconciliation interval rather than requiring a manual refresh, without
requiring the server to track and evaluate arbitrary live predicates.

**Global summary aggregates (the "X up, Y down" header) are computed
server-side and pushed independently of per-monitor subscriptions.** This
is explicitly not built by having the client sum whatever it happens to be
subscribed to — that would silently couple a global number to viewport
state. It's a small, separate real-time channel, decoupled from monitor
count and viewport size entirely.

**Two invariants are load-tested in CI on every release, not just once at
Phase 1 launch:**
1. Server-side resource use (memory, CPU, message throughput) stays flat as
   *total* monitor count grows toward and past 5,000, regardless of how
   many are actively being viewed by any connected client.
2. Client-side payload size and render cost stay bounded by *viewport
   size* (the current page/filter), never by total monitor count.

A benchmark that checks only one of these could pass while the other
regresses silently, so both are asserted explicitly, not inferred from a
single combined metric.

## Consequences

**What this makes easy.**
The Kuma fan-out failure mode is structurally impossible to reproduce —
push volume to any client is bounded by that client's current viewport,
never by total monitor count, so there's no scale threshold to cross. The
transport layer is not a new piece of infrastructure; it's the same NATS
deployment already committed to for probe fan-out, so ops surface doesn't
grow. ID-scoped subjects also give the live-update channel a broker-level
tenancy backstop for free (see Compliance) without any additional work
beyond subject naming.

**What this makes hard, or forecloses.**
Filtered views are not fully real-time — a monitor that changes status
while off-screen and now matches an active filter will not appear until
the next reconciliation tick, not instantly. This is a real, visible UX
trade-off, most noticeable to someone staring at a "status = down" filtered
view waiting for the next incident to appear. If user feedback or the
agency/MSP persona's actual usage patterns show this staleness window is a
real problem in practice, filter-aware pub/sub remains available as a
later addition (see Alternatives) — but it is not built now.

**What becomes expensive to reverse later.**
The reconciliation interval and the ID-scoped subscription pattern are
both easy to tune or extend without a rewrite, since neither is baked into
the wire protocol in a way that forecloses adding a predicate-evaluation
layer on top later — the NATS transport and subject shape chosen here are
compatible with either option. What would be expensive to reverse is
building the frontend to assume manual-refresh-only or full-broadcast
semantics; as long as the frontend is built against "subscribe to visible
IDs, reconcile on interval" as its model from the start, upgrading the
reconciliation mechanism to something smarter later is additive, not a
rewrite.

## Alternatives considered

**Filter-aware pub/sub from Phase 1.** Fully real-time, no staleness
window, most correct. Lost because it requires a stateful
predicate-evaluation service scaling with connected-client count, which is
meaningfully more build and operational surface than Phase 1's other
commitments justify right now, and because NATS's subject-based matching
doesn't express it natively — it would be a distinct component layered on
top of the transport already chosen for other reasons. Left open as a
Phase 2+ enhancement if real usage shows the reconciliation-interval
staleness is a genuine problem, most likely for the agency/MSP persona
watching many clients' status simultaneously.

**Full-state broadcast, tuned or throttled (e.g. reduce push frequency,
paginate the broadcast itself).** This is close to describing what Kuma
already does, with knobs turned. Rejected outright — the plan's own
research (§1.4a) shows this fails structurally past a few hundred
monitors regardless of tuning; throttling delays the wall, it doesn't
remove it, and 5,000 monitors is explicitly the target this project has to
clear, not approach cautiously.

**Server-Sent Events (SSE) instead of NATS for the browser-facing
channel.** Simpler to reason about — one unidirectional HTTP stream per
view, easy to debug with ordinary HTTP tooling. Lost because changing
subscription scope (e.g. paginating to a new page of monitors) generally
means closing and reopening the stream rather than adjusting a
subscription in place, and because it doesn't reuse the NATS
infrastructure already committed to for probe fan-out — adopting it would
mean running and reasoning about two independent real-time transports
instead of one.

## Compliance with the product principles

- [x] Sixty seconds to first monitor is preserved — pagination, live
      updates, and reconciliation are all invisible to a solo user with a
      handful of monitors; nothing about this decision adds a setup step.
- [x] Nothing is paywalled in the open source build — the entire
      state-synchronisation model ships in the AGPL build; there is no
      "real-time updates" tier.
- [x] API-first — no privileged endpoints the dashboard uses and users
      cannot — the paginated list endpoints and the membership-
      reconciliation endpoint are ordinary API surface, usable by any API
      client, not dashboard-only internals.
- [x] Progressive disclosure — no new complexity imposed on the solo user
      — a solo user's "page" is simply all of their monitors; pagination
      and reconciliation exist but are invisible at that scale.
- [x] The client is never sent full state; the UI stays fast at 5,000
      monitors — this is the ADR's central claim, not a side effect. It is
      the specific thing being fixed, and is asserted by the two-invariant
      load test in CI on every release, not just at launch.
- [x] Solo mode keeps zero required external dependencies — solo mode runs
      the probe in-process (per §5.2); the live-update channel in that mode
      can run over an in-process event bus rather than requiring a NATS
      instance, since there's no multi-process fan-out to solve for a
      single binary. NATS is required only once scaled mode is in use.
- [ ] Dependency surface stays minimal — **carried over from ADR-002's
      framing, not a new exception.** NATS was already accepted as a
      dependency for probe fan-out; this ADR extends its use to
      browser-facing updates rather than introducing a second real-time
      transport, which is the minimal-surface choice given NATS is already
      committed to. No new dependency is introduced by this decision.

## References

- Project plan §1.4(a) — the documented Kuma scale ceiling and its root
  cause (Socket.IO full-state broadcast), which this ADR exists to avoid
  reproducing.
- Project plan §5.2 — control plane / probe split and the choice of NATS
  for probe fan-out, reused here for browser-facing updates.
- Project plan §2.9 — "the UI must stay fast at 5,000 monitors... this is
  a hard requirement from day one, not an optimisation... load-test it
  every release," the principle this ADR directly implements.
- ADR-002 (storage engine) — cursor pagination and the reconciliation
  endpoint both query against the same PostgreSQL/TimescaleDB layer
  decided there; no new storage requirement is introduced by this ADR.
- ADR-003 (tenancy model) — the `updates.{org_id}.{monitor_id}.status`
  subject shape gives the live-update channel a broker-level isolation
  backstop consistent with, though not a replacement for, the enforcement
  work scoped to Phase 3 there.
- Open follow-up: if agency/MSP usage after Phase 1 shows the
  reconciliation-interval staleness window is a real problem for
  filtered/live-incident views, evaluate filter-aware pub/sub as a
  targeted addition — likely scoped to the specific views that need it
  rather than the whole dashboard — rather than revisiting this ADR
  wholesale.
- Open follow-up: confirm the in-process event bus used for solo mode's
  live-update path and the NATS-backed path for scaled mode share the same
  client-facing API contract, so the frontend does not need to know which
  mode it's talking to.
