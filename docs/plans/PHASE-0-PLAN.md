# Uptime Cairn — Phase 0 Plan: Foundations

**Duration:** Weeks 1–4
**Source:** Uptime-Cairn-Plan.pdf §6 (Roadmap), §10 (Immediate Next Steps)
**Exit condition:** An ADR set and an OpenAPI spec that a stranger could implement against.

Phase 0 produces **no product code** except the load-test harness. Everything else is decisions, specifications, and infrastructure — because the two decisions that cannot be retrofitted (probe/control-plane split, API-first contract) must be locked before a single feature is built.

---

## 1. Goals

1. Freeze the architectural decisions that cannot be changed later (probe protocol, storage strategy, state-sync model, tenancy model).
2. Write and publish the complete OpenAPI v1 spec **before any product code exists** — the dashboard will be the first API client, never a privileged one.
3. Stand up the repository, governance, licence, and CI/CD so the project is contributable from day one (bus factor is the top existential risk — §7 of the source plan).
4. Build the load-test harness **before the product**, so the 5,000-monitor claim is measured continuously from the first commit, not discovered late.
5. Validate assumptions with 10 user interviews, weighted toward agencies/MSPs running 300+ monitors.

## 2. Non-Goals

- No monitor engine, no UI, no alerting code. Phase 1 owns all of that.
- No hosting/SaaS work of any kind.
- No premature scaling work beyond what the ADRs specify on paper (Postgres/Timescale path is designed now, built in Phase 4).
- **No Kuma importer.** It was originally scoped here as a standalone tool; it now ships in Phase 1, integrated into the product (CLI + guided UI flow), where it was already a P0 deliverable. Phase 0 keeps only the paper mapping of Kuma's schema onto our data model — see 3.4.

---

## 3. Workstreams

### 3.1 Repository, Governance & Legal (Week 1)

| Deliverable | Detail |
|---|---|
| GitHub org + repo | Org `uptimecairn`, monorepo (Go backend + probe, frontend, docs, importer) |
| Licence | Apache 2.0 with CLA. CLA bot wired into PR flow from the first external PR |
| `GOVERNANCE.md` | Documented succession plan; target of 3+ committers with merge rights before v1.0 stated explicitly |
| `CODE_OF_CONDUCT.md` | Contributor Covenant |
| `ROADMAP.md` | Public phase roadmap distilled from the project plan |
| `CONTRIBUTING.md` | Dev setup, PR conventions, ADR process, `good-first-issue` triage policy |
| Name & asset registration | Register `uptimecairn.dev` (canonical) plus `.com`, `.net`, `.co`; GitHub org, Docker Hub, `@uptimecairn` npm scope, social handles — **all on the same day**. CLI ships as `cairn` with an `uptimecairn` alias. Run trademark clearance before any paid spend (bare "Cairn" is heavily contested) |

### 3.2 Architecture Decision Records (Weeks 1–2)

ADRs live in `docs/adr/`, numbered, immutable once accepted. The four mandatory ADRs, in priority order:

1. **ADR-001 — Probe / control-plane split.** The single most consequential decision; made in week one because it cannot be retrofitted. Probes are stateless agents that register with the control plane over gRPC (NATS optional for fan-out) and pull assignments. Agent dials out — no inbound ports. In solo mode the probe is compiled in and runs in-process; the user never knows the split exists. This one split unlocks multi-region, private probes, horizontal scale, HA (probes buffer and replay), and N-of-M consensus checking.
2. **ADR-002 — Storage strategy.** Solo: embedded SQLite (WAL mode), zero dependencies, runs on a Pi. Scaled: PostgreSQL for config/relational state; TimescaleDB or ClickHouse for heartbeat time-series (decision recorded with benchmarks/criteria). Automatic tiered rollups: raw → 1-minute → 5-minute → hourly → daily. Redis is optional, never required. Upgrade solo→scaled is a config change plus a migration — never a reinstall.
3. **ADR-003 — Tenancy model.** Schema-level design for organisations/workspaces with hard data isolation, even though multi-tenancy ships in Phase 3. Every Phase 1 table carries the tenancy key from day one so isolation is never a retrofit.
4. **ADR-004 — UI state-synchronisation model.** The specific thing Uptime Kuma got wrong (hard scale wall at ~300–600 monitors from Socket.IO pushing full state to every browser). Cairn's model: server-side pagination, filtering, and search; incremental/scoped subscriptions only; the client is **never** sent full state. This is a hard requirement, load-tested every release.

Additional ADRs as needed: language/stack confirmation (Go backend + probe; SvelteKit or React frontend; gRPC + Protobuf transport), dependency policy (few packages, vendored and pinned, SBOM published, reproducible builds).

### 3.3 API Contract (Weeks 2–3)

- **OpenAPI v1 spec written and frozen before code.** Full CRUD over every entity: monitors, tags, groups, notifications, status pages, incidents, maintenance windows, users, teams, schedules, reports.
- Versioned under `/api/v1` with an explicit written deprecation policy.
- Scoped API keys specified: per-token permissions, expiry, last-used tracking, revocation.
- Webhooks-out specified for every state change.
- Prometheus `/metrics` and OpenTelemetry export specified.
- **Publish the spec for public comment before writing code** (GitHub Discussions / RFC issue). Community review is part of the audience-building strategy.
- Contract-test scaffolding: spec-driven tests generated into CI so that the moment the server exists, every endpoint is verified against the spec. The API is a promise; §1.2 of the source plan (two abandoned wrapper projects) is what happens when it isn't.

### 3.4 Data Model (Weeks 2–3)

- Entity-relationship design covering every OpenAPI entity, plus heartbeats/check-results, rollup tables, and audit-log skeleton.
- Written for SQLite first with a documented mapping to Postgres/Timescale (types, indexes, partitioning strategy for time-series).
- Migration tooling chosen and versioned-migration convention established (forward-only, numbered, tested in CI against both empty and seeded databases).
- Validated on paper against Uptime Kuma's own schema: a written field-by-field mapping of Kuma's monitors, tags, notifications, and status pages onto this model, naming anything that has no home. If Kuma's data can't be mapped cleanly, the model is wrong. The executable proof — an actual round-trip of a real Kuma database — moves to Phase 1 with the importer, so this mapping is the only guard Phase 0 has and it needs to be done carefully.

### 3.5 Probe Protocol Design (Weeks 2–4)

- Protobuf/gRPC definitions: probe registration, assignment pull, check-result streaming, heartbeat/health, buffering & replay semantics on control-plane outage.
- Explicit versioning and compatibility policy (old probes must keep working against newer control planes within a major version).
- Security model: probe auth tokens, mutual TLS posture, outbound-only connectivity.
- Documented well enough that a third party could implement a probe (this is the "spec a stranger could implement against" bar, applied to the probe side).

### 3.6 CI/CD & Engineering Infrastructure (Weeks 1–2)

- GitHub Actions: lint, unit tests, contract tests (once server exists), reproducible builds, SBOM generation, multi-arch Docker builds (amd64/arm64 — the Pi user matters).
- **Load-test harness built before the product:** a 5,000-monitor synthetic workload generator plus a UI-responsiveness benchmark, wired into CI from the first commit. The scale claim ("5,000 monitors on one install and the UI stays fast") is Uptime Cairn's central promise; it is measured continuously, and grows to a 10,000-monitor continuous test from Phase 1 onward.
- Release automation skeleton: tagged releases → binaries + Docker images + checksums.

### 3.7 User Interviews (Weeks 1–4, parallel)

- Interview 10 people; weight heavily toward **agencies and MSPs running 300+ monitors** — the persona actively being ejected from Uptime Kuma by the scale wall with nowhere to go.
- Validate the reporting thesis (branded/scheduled client reports as the killer feature) in the same conversations.
- Findings recorded as evidence notes; the plan is a living document — revise as evidence arrives, don't defend it as written.

---

## 4. Week-by-Week Summary

| Week | Focus |
|---|---|
| **1** | Repo, licence + CLA, governance docs, CI skeleton, name/domain/handle registration, ADR-001 (probe split) drafted, interviews begin |
| **2** | ADR-002/003/004 drafted and reviewed; OpenAPI spec drafting; data model drafting; load-test harness started |
| **3** | OpenAPI spec published for public comment; data model frozen incl. the Kuma schema mapping; probe protocol drafted; load-test harness runs in CI |
| **4** | Spec revisions from feedback → freeze; probe protocol reviewed; interview synthesis; Phase 1 kickoff review |

---

## 5. Exit Criteria (all must be true)

- [ ] ADR-001 through ADR-004 accepted and published in `docs/adr/`.
- [ ] OpenAPI v1 spec frozen, published, and reviewed publicly — detailed enough that a stranger could implement a conforming server or client against it.
- [ ] Probe protocol (protobuf + semantics doc) reviewed and versioned.
- [ ] Data model documented with migration tooling in place, including a written field-by-field mapping from Uptime Kuma's schema.
- [ ] Load-test harness (5,000-monitor synthetic workload + UI benchmark) runs in CI.
- [ ] Repo has licence, CLA flow, `GOVERNANCE.md`, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, `ROADMAP.md`, and green CI.
- [ ] Domains, org names, package scopes, and social handles registered; trademark clearance done.
- [ ] 10 interviews completed with findings written up and fed back into the Phase 1 scope.

## 6. Phase 0 Risks

| Risk | Mitigation |
|---|---|
| Spec-first stalls into analysis paralysis | Hard 4-week timebox; spec freezes at end of week 4 with a versioned change process afterward — perfection is what `/api/v2` is for |
| ADRs made abstractly, wrong in practice | The load-test harness is now the only executable validator in this phase and exercises the scale assumptions before Phase 1 code; the data model is checked on paper against Kuma's schema |
| Data model reaches Phase 1 unproven | Moving the importer out of Phase 0 removed the round-trip that used to prove the model held real data. The written Kuma schema mapping (3.4) is the substitute, and it is weaker. Phase 1 should build the importer early — Month 1, alongside the schema — rather than at Month 4 as currently planned, so a wrong model surfaces while it is still cheap to change |
| Solo-founder bottleneck from day one | Publish everything (spec, ADRs, roadmap) for comment; treat committer recruitment as a P0 deliverable, not background hope |
