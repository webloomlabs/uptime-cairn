# ADR-002: Storage Engine — TimescaleDB Mandatory for Scaled Mode in Phase 1; Plain-PostgreSQL Fallback Deferred to Phase 2/3

- **Status:** Accepted
- **Date:** 2026-07-31
- **Deciders:** [Shakil Ilham](https://github.com/silham)

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

## Context

Scaled mode (§5.3 of the project plan) needs to store heartbeat time-series
data at a volume that supports the plan's headline claim: 5,000 monitors on
one install with the UI staying responsive, plus years of history browsable
for reporting (§4). That requires tiered rollups — raw → 1-minute →
5-minute → hourly → daily — so disk stays bounded while long-range reports
stay queryable. TimescaleDB gives hypertables, continuous aggregates, and
native compression that map directly onto this requirement with little
custom code.

An earlier draft of this ADR proposed making TimescaleDB optional from
Phase 1 — auto-detected, with a hand-built plain-PostgreSQL fallback
(native partitioning + `pg_partman` + application-level rollup jobs) so
that customers on Google Cloud SQL or AlloyDB, which do not support the
`timescaledb` extension and offer no manual install path, would still have
a fully-functional scaled mode.

That fallback path is real engineering work: partition lifecycle
management, rollup materialization, and compression handling, all built
and tested as a second backend behind a shared interface, on top of
everything else Phase 1 already has to deliver (§6 — full monitor type
coverage, ~10 alert channels plus Apprise, the Kuma importer, and the
5,000-monitor load-test gate itself). Building both backends
simultaneously is the more defensible long-term architecture, but it is
also more surface area than a single-backend Phase 1 needs to carry, and
Phase 1's own exit criteria (§6) is demanding enough without it.

The GCP gap is real but narrow: it excludes customers who (a) want scaled
mode, not solo mode, (b) are on a managed-Postgres-only infrastructure
policy, and (c) specifically use GCP rather than AWS or Azure, both of
which do support TimescaleDB's Apache-2.0 edition natively. That is
realistically an enterprise-tier concern, and enterprise-grade controls are
explicitly Phase 3/4 work, not Phase 1 (§6). Shipping a solid,
single-backend Phase 1 sooner serves more of the roadmap's own stated exit
criteria — "a solo user can replace Uptime Kuma entirely... and an agency
with 1,000 monitors can too" — than shipping a slower Phase 1 with two
storage backends neither of which has been battle-tested yet.

## Decision

**Phase 1: TimescaleDB is a mandatory dependency of scaled mode.**
Solo mode remains embedded SQLite with zero external dependencies,
unchanged. Scaled mode requires PostgreSQL with the TimescaleDB extension
installed; the control plane checks for it at startup and fails to start
in scaled mode with a clear, actionable error if it's absent, rather than
degrading silently. Hypertables, continuous aggregates, and native
compression are used directly for heartbeat storage and the tiered
rollups.

**The storage layer is still written against an internal repository
interface** (`HeartbeatStore`, `RollupStore`, or equivalent), not by
calling Timescale-specific SQL (`time_bucket()`, hypertable DDL) directly
from business logic, dashboard queries, or the reporting engine. This
costs little extra in Phase 1 — it is good separation regardless of how
many backends exist — and is what keeps the door open for a second backend
later without a rewrite.

**A plain-PostgreSQL backend (native partitioning + `pg_partman` +
application-managed rollups) is deferred to Phase 2 or Phase 3**, timed
against real signal rather than committed to now: either enterprise/GCP
demand shows up concretely (a prospect or customer blocked specifically by
this), or Phase 3's enterprise-controls work (§6) creates natural overlap
with hardening the storage layer anyway. Until then, GCP Cloud SQL and
AlloyDB are documented as unsupported for scaled mode, with self-hosted
Postgres, RDS, Azure Flexible Server, or Timescale's own Tiger Cloud
offered as the supported paths.

## Consequences

**What this makes easy.**
Phase 1 ships with one storage backend to build, test, load-test, and
document, which is a meaningfully smaller lift than two. Continuous
aggregates and native compression are available immediately rather than
hand-built, which directly de-risks the 5,000-monitor load-test gate that
is Phase 1's own exit criterion (§6) — Timescale is more likely to clear
that bar with less custom engineering than a hand-rolled rollup system
would be on a first attempt. The Kuma importer, reporting engine (Phase 2),
and dashboard all get one clear set of query semantics to build against
from day one, rather than an abstraction that has to work for two
different backends' performance characteristics simultaneously.

**What this makes hard, or forecloses.**
Any customer whose infrastructure policy requires GCP-managed Postgres
cannot run scaled mode at all until the fallback backend ships. That is a
real, known gap for the entire Phase 1–2 window, not a hypothetical one —
it should be stated plainly in scaled-mode documentation and in any sales
or support conversation with prospects, not discovered by a customer
mid-evaluation. It also means Uptime Cairn cannot yet make an unqualified
"runs on any managed Postgres" claim, which is a minor but real dent in
the "one tool for everyone" positioning from §0 until the fallback lands.

**What becomes expensive to reverse later.**
Nothing about *this* decision is expensive to reverse, provided the
repository-interface discipline is actually followed in Phase 1 — that is
the one commitment carried over from the original draft, and it is
intentionally cheap to enforce now via code review. If Timescale-specific
calls leak into dashboard, API, or reporting-engine code because the
interface boundary is skipped "just this once" under Phase 1 deadline
pressure, then adding the plain-Postgres backend in Phase 2/3 becomes a
retrofit across all of that code instead of a second implementation behind
an existing seam. The point of no return is therefore not a phase boundary
but a code-review discipline: the interface has to be real from the first
commit that touches heartbeat storage, even though only one implementation
of it exists for now.

## Alternatives considered

**Dual-backend from Phase 1 (the original draft of this ADR).**
Removes the GCP gap immediately and never requires telling any customer
"not yet." Lost because it adds a full second backend's worth of build,
test, and load-test surface to a Phase 1 that already has to deliver ~10
monitor types, ~10+ alert integrations plus Apprise, the Kuma importer, and
the 5,000-monitor CI load-test gate (§6) simultaneously. The risk of
shipping a slower or shakier Phase 1 — the phase the roadmap explicitly
says "must be independently useful" and win real users on its own — was
judged worse than the risk of a documented, narrow gap that a later phase
closes.

**ClickHouse for heartbeat time-series.**
Considered and rejected in the prior version of this ADR for the same
reasons that apply here with more force: a second database system to
operate, with no managed-Postgres-equivalent story on any cloud provider,
solving a scale problem plain partitioned Postgres or Timescale likely
don't have below 5,000 monitors. Not revisited by this decision.

**Making TimescaleDB mandatory permanently, with no fallback ever planned.**
Simplest of all options, and arguably defensible given how narrow the GCP
gap is. Lost because it forecloses the enterprise persona (§3) — "needs
everything," explicitly including infrastructure flexibility — for a
segment the plan identifies as strategically important, and because
§2.10's dependency-minimalism principle is a standing product principle,
not a one-phase concern. This ADR treats the fallback as deferred, not
cancelled.

## Compliance with the product principles

- [x] Sixty seconds to first monitor is preserved — solo mode is SQLite and
      untouched by this decision.
- [x] Nothing is paywalled in the open source build — TimescaleDB (Apache
      2.0 edition) ships as a required but fully open-source dependency of
      the AGPL build; no capability sits behind a paid tier.
- [x] API-first — no privileged endpoints the dashboard uses and users
      cannot — unaffected; the storage backend sits behind the repository
      interface regardless of which backend is active.
- [x] Progressive disclosure — no new complexity imposed on the solo user —
      solo mode is unaffected. The TimescaleDB requirement is visible only
      to whoever stands up scaled mode, and surfaces as a clear startup
      error rather than a confusing runtime failure.
- [ ] The client is never sent full state; the UI stays fast at 5,000
      monitors — **not yet confirmed.** As in the prior version of this
      ADR, this remains a Phase 1 load-test gate (§6), not something this
      decision can assert. Making Timescale mandatory rather than optional
      *improves* the odds of clearing this gate on the first attempt,
      since continuous aggregates are used directly rather than
      reimplemented.
- [x] Solo mode keeps zero required external dependencies — unaffected;
      this ADR concerns scaled mode only.
- [ ] Dependency surface stays minimal — **partial exception, explicitly
      time-boxed.** TimescaleDB becomes a hard dependency of scaled mode
      for Phase 1–2, which is a real exception to §2.10, not a technicality.
      It is accepted here because scaled mode is opt-in (solo mode carries
      zero dependencies) and because the interface boundary keeps the
      exception reversible. This checkbox should flip back to compliant
      once the plain-Postgres fallback ships in Phase 2/3, or this ADR
      should be superseded if the fallback is ultimately dropped.

## References

- Project plan §1.4(a) — the ~300–600 monitor ceiling in the incumbent that
  motivates the 5,000-monitor target this decision must ultimately satisfy.
- Project plan §5.3 — original storage proposal (Postgres + Timescale/
  ClickHouse).
- Project plan §2.10 — minimal dependency surface principle; tracked here
  as a known, time-boxed exception rather than silently overridden.
- Project plan §6 — Phase 1 exit criteria and scope, the primary reason
  the fallback backend is deferred rather than built concurrently.
- Google Cloud SQL PostgreSQL extension allow-list documentation — confirms
  `timescaledb` and `citus` are absent with no manual install path, and
  that AlloyDB does not offer them either. This is the specific gap being
  accepted, not solved, by this decision.
- AWS RDS PostgreSQL extension support — confirms Apache-2.0 edition of
  TimescaleDB is installable; one of the supported managed-cloud paths for
  Phase 1 scaled mode.
- Azure Database for PostgreSQL Flexible Server extension list and release
  notes — confirms TimescaleDB (Apache-2.0 edition) support; the other
  supported managed-cloud path for Phase 1 scaled mode.
- Prior draft of this ADR (dual-backend-from-Phase-1) — superseded in
  content by this version while still Proposed; kept in version control
  history rather than as a separate superseding ADR since it was never
  accepted.
- Open follow-up: revisit this decision explicitly at Phase 2 planning and
  again at Phase 3 planning, with a concrete trigger (documented GCP-blocked
  prospect, or natural overlap with Phase 3 enterprise-controls work) rather
  than a fixed date.