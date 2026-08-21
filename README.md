# Uptime Cairn

**An all-in-one open source uptime monitoring & reporting platform.**

> A cairn is a stack of stones built up by many passing travellers to mark the
> safe path for whoever comes next. Stacked stones also happen to look exactly
> like an uptime bar.

[![Licence: AGPL v3](https://img.shields.io/badge/licence-AGPLv3-blue.svg)](LICENSE)
[![Status: pre-release](https://img.shields.io/badge/status-pre--release%20(Phase%200)-orange.svg)](ROADMAP.md)

---

## ⚠️ Project status: not yet installable

**There is no release yet.** Uptime Cairn is in **Phase 0 — Foundations**: we are
writing the API specification and the architecture decision records *before*
writing the code, deliberately. The first usable release (v0.1) lands at the end
of Phase 1.

If you are here to run something today, you want
[Uptime Kuma](https://github.com/louislam/uptime-kuma) — it is excellent, and
when Cairn ships we will import your data from it in about thirty seconds.

If you are here to shape the thing before it sets, the timing is perfect. See
[CONTRIBUTING.md](CONTRIBUTING.md) — reviewing the API spec is genuinely the
highest-value thing anyone can do right now.

Watch the repo or follow the [roadmap](ROADMAP.md) for release news.

---

## What this is

One open source uptime monitoring and reporting tool that serves the entire
spectrum of users — a freelancer with three client websites, a 10-person
startup, an MSP managing 200 customers, and an enterprise with SOC 2 auditors —
without artificial feature gating, without a paid tier holding the good parts
hostage, and without forcing anyone to bolt together four separate tools.

This is **not** a plan optimised for extracting revenue at every layer. It is a
plan optimised for one tool being genuinely sufficient for everyone.

That objective has one direct architectural consequence, and it is the single
most important idea in the project:

> ### One codebase. One binary. Progressive disclosure.
>
> The freelancer never sees RBAC, escalation policies, or Terraform. The
> enterprise admin never has to migrate to a different product to get them. The
> same install grows with the user. Complexity is revealed on demand, never
> imposed by default.

## Why it should exist

The category leader has a well-documented ceiling. Uptime Kuma is the community
standard — 88k+ stars, ~40 monitor types, ~95 alert integrations, one Docker
container — and it is genuinely excellent at what it does. But there is a gap
between what it provides and what teams need, and the gap is widening:

| Missing capability | Consequence |
|---|---|
| No write REST API | Every mutation needs a Socket.IO client — a hard barrier for CI/CD and automation |
| No RBAC / multi-user | One admin account; anyone with dashboard access can delete anything |
| No distributed probing | Single location; if that server dies, alerting dies with it |
| No SSO | Blocks any organisation with an identity provider |
| No on-call scheduling | Alerts fire immediately, always — no rotation, no business-hours suppression |
| No incident management | Incidents are posted **by hand**, not opened automatically from a failing check |
| Weak historical reporting | Default graphs show roughly a week; long-term history is stored but not browsable |
| **Hard scale wall at ~300–600 monitors** | Socket.IO pushes full state to every browser client; the UI becomes unusable |

That last row is the decisive one. It is a hard, reproducible, numeric wall
caused by the state-synchronisation architecture — it cannot be tuned around.
And it hits precisely the people with the most to monitor: users report loving
Kuma right up until they cross ~300 monitors, with the community's only
suggested workaround being *"fork another instance of kuma on another host."*

The other signal worth reading carefully: two community projects
(`uptime-kuma-api`, `Uptime-Kuma-Web-API`) exist *solely* because there is no
official write API, and both are now stalling under that dependency — open
issues include *"This project seems to be abandoned"* and *"Update to match
Uptime Kuma 2.x."* This is not "someone should build a better wrapper." It is
proof that a monitoring tool whose API is an afterthought will spawn a fragile
satellite ecosystem that eventually strands its users. It is the strongest
possible argument for API-first design, and it is why our spec is frozen before
our code.

Meanwhile the alternatives each solve one slice: OpenStatus has excellent
platform engineering but narrow protocol coverage; Gatus is YAML-only; Kener has
the best status pages but no multi-region probing; Checkmk and Zabbix are
enterprise-capable but the wrong shape entirely for a freelancer; Better Stack
and Pingdom are polished, proprietary, priced per monitor, and your data lives
on someone else's infrastructure.

**No project occupies the square: broad protocol coverage + modern platform
engineering + team/enterprise controls + serious reporting, in one open source
install.** That square is the whole opportunity.

## Product principles

Non-negotiable. Every proposed feature is tested against these.

1. **Sixty seconds to first monitor.** `docker run` → open browser → monitor
   running. If onboarding is harder than Uptime Kuma's, adoption dies no matter
   how good the rest is.
2. **No feature is paywalled in the open source build.** RBAC, SSO,
   multi-region, PDF reports, audit logs — all of it ships in the AGPL build.
   Uptime Cairn earns money from hosting and support, never by crippling the
   software.
3. **API-first, literally.** The full surface is specified in OpenAPI *before*
   the UI exists. The dashboard is the first API client, not a privileged one.
4. **Progressive disclosure.** Advanced surfaces stay hidden until you opt in.
5. **Sensible defaults, deep overrides.** Works with zero configuration, bends
   fully when configured.
6. **Reporting is a first-class product, not a graph.** The most under-served
   need in the whole category.
7. **Your data is yours.** Full export in open formats, no lock-in, no
   phone-home, telemetry opt-in only.
8. **Never lose a heartbeat.** Monitoring you can't trust is worse than none.
9. **The UI must stay fast at 5,000 monitors.** A hard requirement from day one,
   load-tested every release — not an optimisation for later.
10. **Minimal dependency surface.** Few third-party packages, vendored and
    pinned, SBOM published, reproducible builds.

## The headline claim

> **5,000 monitors on one install, and the UI stays fast.**

No competitor in the open source space can say that today. It is enforced by an
automated load test in CI from the first commit, and it must be true at v0.1 —
not promised for later. Continuous load testing runs against a 10,000-monitor
synthetic workload to catch regressions.

## Planned quick start

> Not yet available — this is what v0.1 will look like.

```bash
docker run -d --restart=always -p 3000:3000 \
  -v uptime-cairn:/data \
  --name uptime-cairn \
  uptimecairn/uptime-cairn:latest
```

Then open `http://localhost:3000`. One binary, embedded probe, embedded UI,
SQLite. No external services, no Redis, runs on a Raspberry Pi.

### Migrating from Uptime Kuma

```bash
cairn import kuma /path/to/kuma.db
```

Reproduces your monitors, tags, notifications, and status pages. Point it at
several Kuma databases and it merges them into one install — which is the
migration story for everyone currently sharding by hand across hosts because of
the scale wall.

## Architecture

**Two deployment shapes, one codebase.**

```
SOLO MODE (default)                SCALED MODE (opt-in)

single binary                      Control plane #1   Control plane #2
├── control plane                            │               │
├── embedded probe                    Postgres + Timescale/ClickHouse
├── UI                                          │ gRPC / NATS
└── SQLite                            Probe: EU │ Probe: US │ Probe: VPC

docker run, 60 seconds
```

Same binary, different flags. The upgrade path from solo to scaled is a config
change and a database migration — **never a reinstall, never a different
product.**

The consequential decision is the **control plane / probe split**, made in week
one because it cannot be retrofitted. Probes are stateless agents that register
with the control plane over gRPC and pull their assignments. This one split
unlocks multi-region monitoring, private probes behind a firewall (the agent
dials out, so no inbound ports), horizontal scale, HA (a control plane failure
doesn't stop checks — probes buffer and replay), and consensus checking that
requires *N of M* regions to agree before declaring an outage, which kills the
single biggest source of false-positive pages.

In solo mode the probe is simply compiled in and runs in-process. You never know
the split exists.

**Stack:** Go backend and probe (single static binary, low memory, trivial
cross-compilation to ARM/Pi), SvelteKit + Tailwind frontend, SQLite or
Postgres + Timescale, gRPC + Protobuf transport, Playwright in an optional
sidecar for browser checks, Typst for PDF reports.

## Documentation

| | |
|---|---|
| **[Install](docs/guides/install.md)** | Docker, Compose, binary, Raspberry Pi |
| **[First monitor](docs/guides/quickstart.md)** | Account, monitor, alert, and how to prove it fires |
| **[Monitor types](docs/guides/monitor-types.md)** | The nine types and what each actually checks |
| **[Alerting](docs/guides/alerting.md)** | Every channel, webhook templating, maintenance windows |
| **[Migrating from Uptime Kuma](docs/guides/migrating-from-uptime-kuma.md)** | `cairn import kuma`, and exactly what does not come across |
| **[Operations](docs/operations/)** | Backups, upgrades, reverse proxies, what to alert on |
| **[API](docs/api/README.md)** | Conventions, and a [generated reference](docs/api/reference.md) of every operation |
| **[Security](SECURITY.md)** | Posture, and how to report a vulnerability |

## Roadmap in brief

| Phase | What | When |
|---|---|---|
| **0 — Foundations** | Spec, ADRs, governance, data model, load-test harness | Weeks 1–4 ← *we are here* |
| **1 — Solid Core** | 10 monitor types, 13 alert channels + Apprise, status pages, full REST API, 5,000-monitor gate | Months 1–4 |
| **2 — Reporting** | Scheduled white-label PDF/HTML/CSV reports, SLA/SLO + error budgets, post-mortems | Months 4–7 |
| **3 — Teams** | Orgs, RBAC, SSO/SAML, audit log, automatic incidents, on-call & escalation | Months 7–11 |
| **4 — Scale** | Multi-region consensus, private probes, HA, Terraform, YAML/GitOps, MCP server | Months 11–16 |
| **5 — Depth** | Anomaly detection, plugin SDK, mobile apps, template gallery | Month 16+ |

Full detail in [ROADMAP.md](ROADMAP.md). Phase plans:
[Phase 0](docs/plans/PHASE-0-PLAN.md) · [Phase 1](docs/plans/PHASE-1-PLAN.md).

## Contributing

Contributions are wanted, and right now the most valuable ones are not code —
they are API spec review, ADR review, and honest reports of what breaks at your
scale. Start with [CONTRIBUTING.md](CONTRIBUTING.md).

If you run 300+ monitors, shard several Kuma instances across hosts, or build
client uptime reports by hand every month, we would especially like to hear from
you. Open an issue.

- [Contributing guide](CONTRIBUTING.md)
- [Governance & succession plan](GOVERNANCE.md)
- [Code of Conduct](CODE_OF_CONDUCT.md)
- [Roadmap](ROADMAP.md)

**Security:** please don't file vulnerabilities in public issues — email
security@uptimecairn.dev.

## Licence

[AGPLv3](LICENSE), with a Contributor License Agreement.

AGPL is the correct instrument for this project's objective: it protects
openness rather than monetising restriction, and it prevents a hyperscaler
strip-mining the work into a closed SaaS without contributing back. Governance
explicitly bounds the CLA so it can never be used to paywall a feature in the
open build — see [GOVERNANCE.md §6](GOVERNANCE.md).

## Credit

Uptime Cairn deliberately echoes **[Uptime Kuma](https://github.com/louislam/uptime-kuma)**.
