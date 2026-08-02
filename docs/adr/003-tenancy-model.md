# ADR-003: Tenancy Model — Inert Org Key from Phase 1; Isolation Model, Hierarchy, and Enforcement Deferred to Phase 3

- **Status:** Proposed
- **Date:** 2026-08-01
- **Deciders:** [Shakil Ilham](https://github.com/silham)

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

## Context

Multi-tenancy with hard data isolation is planned for Phase 3 (project plan
§6): organisations/workspaces, RBAC, and per-client white-labelling for the
agency/MSP persona (§3). The question in front of us is not *whether* to
build it — it's already scoped to Phase 3 — but whether Phase 1's schema
should carry a tenancy key from day one, and how much of the tenancy
*design* (as opposed to the column) should be decided now versus at Phase 3
planning.

Working through this surfaced three things worth recording:

**Isolation is not needed by most of the user spectrum.** Re-reading §3
persona-by-persona: freelancer, startup, enterprise, and homelab all need
multi-*user* support with roles (RBAC) — not isolation between anything.
The only persona where hard isolation is load-bearing rather than a nice
UX grouping is agency/MSP, whose clients are external, sometimes
competing, businesses. Enterprise wants audit logs and SSO, and sometimes
soft grouping by team or business unit, but not the same security boundary
an agency needs between Client A and Client B. So "multi-tenancy" as a
hard-isolation feature is scoped to one persona, not the whole spectrum —
which matters for how much design effort it deserves before Phase 3.

**Managed hosting does not require shared multi-tenancy.** The team's
planned managed-hosting offering was initially assumed to require the same
shared-database tenancy model as the in-app MSP feature. It doesn't: the
simpler and more defensible path for managed hosting is one dedicated
instance per hosted customer (container + database per customer,
automated via Coolify or equivalent), which sidesteps RLS, shared-table
tenant filtering, and cross-customer leak risk entirely at the
infrastructure layer, at the cost of more compute per customer. This
decouples the hosting-business decision from the in-app tenancy decision
completely — nothing about the managed offering forces shared multi-tenancy
into the product.

**TimescaleDB (ADR-002) has a real gap specific to isolation.** Row-level
security enforces correctly on raw hypertables, but has documented,
acknowledged limitations enforcing on continuous aggregates — the exact
mechanism ADR-002 relies on for tiered rollups, and the mechanism the
reporting engine (§4, Phase 2) queries almost exclusively. This means RLS
alone cannot be the isolation story for rollup/report queries; application-
level filtering has to be the primary enforcement there, with RLS as
defense-in-depth on raw data, not a uniform guarantee across the whole
query surface. This needs to be tested and understood well before any
compliance claim is made to an enterprise or SOC-2-adjacent customer.

Given all three points, the cost of retrofitting a tenancy *column* later
is low — modern PostgreSQL adds a column with a constant default as a
metadata-only operation, and Phase 1's backfill is unambiguous since every
row belongs to one implicit tenant. What's expensive is not the column; it
is the query-filtering discipline, the isolation-enforcement design, and
the hierarchy decision (flat org, or org+client nesting for the MSP case).
None of that needs to be solved now to avoid a retrofit later — it needs to
be solved well, with real signal, at Phase 3.

## Decision

**Phase 1: every tenant-scoped table carries an `org_id` foreign key,
populated by a single sentinel organisation row created in the first
migration.** This includes config tables (monitors, tags, notification
channels, status pages) and the heartbeat hypertable from ADR-002, where
`org_id` is added as a leading column in composite indexes (`org_id,
monitor_id, time desc`) alongside the time-based partitioning, per the
technical discussion above — not as a second hypertable partitioning
dimension, which risks chunk explosion given Uptime Cairn's expected shape
of many small tenants rather than a few large ones.

**The column is inert infrastructure in Phase 1, not a feature.** No RLS
policies, no multi-org UI, no org-switching, no enforcement logic beyond
"every row points at the one sentinel org." Application code is not
required to filter by it yet, since there is nothing to isolate from.

**The following are explicitly deferred to Phase 3 planning, not decided
here:**
- Pool model (shared tables, filtered by `org_id`) versus a silo option
  for enterprise customers with stricter isolation requirements.
- Flat organisation model versus a two-level org + client hierarchy, which
  the MSP persona's per-client isolation and white-labelling needs likely
  require.
- Enforcement mechanism: RLS on raw hypertables and config tables as
  defense-in-depth, with application-level filtering as the primary
  enforcement for continuous-aggregate/rollup queries, pending a
  Phase 3 spike (see Immediate Next Steps below) to confirm current
  TimescaleDB RLS behavior on continuous aggregates before this is relied
  upon for any compliance claim.
- Whether users belong to a single org or many (an MSP employee or
  consultant working across multiple agencies), which shapes whether
  tenancy is a column on `users` or a separate `memberships` join table —
  the latter is likely correct given the persona, but is a Phase 3 design
  decision, not a Phase 1 one.

**Managed hosting is decoupled from this decision entirely.** The planned
managed-hosting offering will use per-customer dedicated instances, not
the in-app shared-tenancy model, and this ADR does not treat hosting
architecture as a driver of the tenancy design.

## Consequences

**What this makes easy.**
Phase 3 does not have to run a schema migration to introduce tenancy, does
not have to backfill ambiguous historical data, and does not have to audit
tables created across two prior phases to find ones that were built
without a tenancy key. Every table built in Phase 1 and Phase 2 already
has the column; Phase 3's job becomes "add real enforcement and a second
row to `organisations`," not "invent the column's existence retroactively
across the whole codebase." The heartbeat table's index shape is decided
alongside ADR-002 rather than reworked later, avoiding a second pass over
the hypertable design.

**What this makes hard, or forecloses.**
Nothing is foreclosed by this decision — that's deliberate. Because the
column is inert and none of pool-vs-silo, hierarchy, or enforcement
mechanism is decided yet, Phase 3 still has full latitude to choose any of
those paths. The cost of that latitude is that Phase 3 planning has to
actually revisit and decide these open questions rather than inheriting
finished answers; this ADR buys schema convenience, not design certainty.

**What becomes expensive to reverse later.**
If application code in Phase 1 or Phase 2 is written in a way that assumes
a single implicit tenant — for instance, caching or query patterns that
hardcode the sentinel org rather than reading it from context — that
becomes retrofit work at Phase 3, similar in kind to the risk flagged in
ADR-002 about interface discipline. The mitigation is the same: route
tenant scoping through one shared code path (even though it resolves to
the sentinel org for two phases), so Phase 3 changes what that path
returns rather than rewriting every call site.

## Alternatives considered

**Add the `org_id` column at Phase 3, when the feature actually ships.**
Simplest possible Phase 1, and given the column-add is a cheap metadata
operation with unambiguous backfill, the "retrofit" framing that applied
to ADR-002's storage decision doesn't really apply here with the same
force. Lost narrowly, on habit-formation grounds rather than migration
cost: waiting means every table added across Phase 1 *and* Phase 2 needs
auditing at Phase 3 to confirm none were missed, versus the column simply
existing everywhere from the start. Given how cheap it is to add now, the
audit risk later wasn't worth taking on for a marginal savings today.

**Design the full tenancy model (hierarchy, enforcement, pool/silo) now,
alongside the column.**
Would remove open questions from Phase 3 planning entirely. Lost because
none of these questions have real signal behind them yet — no enterprise
prospect has been blocked by a specific isolation gap, and the org+client
hierarchy shape depends on decisions (how deep does white-labelling need to
go, does an agency's client ever get direct login access) that are
better made close to Phase 3, informed by actual MSP conversations, than
guessed at now. This mirrors the reasoning in ADR-002 for deferring the
plain-Postgres fallback: build the cheap insurance now, defer the
judgment calls to when there's real information to make them well.

**Build shared multi-tenancy now because managed hosting will need it.**
This was the original premise going into this discussion and turned out
to be false on inspection — managed hosting is better served by
per-customer dedicated instances, which need provisioning automation, not
a shared-tenancy data model. Rejected once the hosting and in-app tenancy
concerns were separated; conflating them would have pulled Phase 3-scale
design work into Phase 1 for no real benefit to the hosting business.

## Compliance with the product principles

- [x] Sixty seconds to first monitor is preserved — the sentinel org is
      created automatically in the first migration; the solo user never
      sees an organisation concept exists.
- [x] Nothing is paywalled in the open source build — the column, and
      whatever tenancy model Phase 3 builds on top of it, ships in the
      AGPL build per §2.10.
- [x] API-first — no privileged endpoints the dashboard uses and users
      cannot — unaffected; no tenancy-specific API surface exists yet.
- [x] Progressive disclosure — no new complexity imposed on the solo user
      — the column is invisible; there is no UI, setting, or concept
      exposed by this decision.
- [ ] The client is never sent full state; the UI stays fast at 5,000
      monitors — **not directly affected by this ADR**, but flagged as a
      dependency: the composite indexes chosen here (`org_id, monitor_id,
      time desc`) need to be validated against the same Phase 1 load-test
      gate as ADR-002, since they're being decided now rather than at
      Phase 3.
- [x] Solo mode keeps zero required external dependencies — unaffected;
      the sentinel-org pattern applies to scaled mode's schema design, not
      to solo mode's dependency footprint.
- [x] Dependency surface stays minimal — no new dependency introduced;
      this is a schema decision, not a library or service choice.

## References

- Project plan §3 — persona table; the basis for concluding hard isolation
  is a single-persona (agency/MSP) requirement, not a whole-spectrum one.
- Project plan §4 — reporting engine as the primary consumer of
  TimescaleDB continuous aggregates, and therefore the surface most
  exposed to the RLS/continuous-aggregate gap noted below.
- Project plan §6 — Phase 3 scope (organisations, RBAC, per-client
  white-labelling), the feature this ADR prepares for without building.
- Project plan §2.10 — dependency-minimalism principle; not implicated
  directly by this decision but consistent with keeping Phase 1 additions
  inert rather than speculative.
- ADR-002 (storage engine) — this decision's index and hypertable design
  is deliberately consistent with, not a revision of, ADR-002's TimescaleDB
  choice.
- TimescaleDB RLS-on-continuous-aggregates limitation — documented,
  acknowledged gap on Timescale's own issue tracker; the reason
  application-level filtering is flagged as the primary (not backup)
  enforcement mechanism for rollup/report queries at Phase 3.
- TimescaleDB chunk-explosion risk with multi-dimensional (space)
  partitioning under many-small-tenants workloads — the reason this ADR
  chooses composite indexing over space-partitioning by `org_id`.
- Open follow-up for Phase 3 planning: a short technical spike to confirm
  current TimescaleDB RLS behavior on hypertables and continuous
  aggregates against the version in use at that time, before RLS is relied
  upon in any compliance-facing claim.
- Open follow-up: managed-hosting architecture (per-customer dedicated
  instance vs. shared infrastructure) deserves its own ADR once that
  offering is closer to being built; this ADR only records that it does
  not drive the in-app tenancy design.
