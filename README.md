# Uptime Cairn

**An all-in-one open source uptime monitoring & reporting platform.**

> A cairn is a stack of stones built up by many passing travellers to mark the
> safe path for whoever comes next. Stacked stones also happen to look exactly
> like an uptime bar.

[![Licence: AGPL v3](https://img.shields.io/badge/licence-AGPLv3-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/webloomlabs/uptime-cairn?label=release)](https://github.com/webloomlabs/uptime-cairn/releases)
[![CI](https://github.com/webloomlabs/uptime-cairn/actions/workflows/ci.yml/badge.svg)](https://github.com/webloomlabs/uptime-cairn/actions/workflows/ci.yml)

---

## Project status: v1.0 — Phase 1 complete

**Uptime Cairn is installable.** Nine monitor types, thirteen alert channels plus
Apprise, status pages, a complete REST API, and the Uptime Kuma importer all ship
in v1.0. One binary, embedded probe, embedded dashboard, SQLite. See
[Install](docs/guides/install.md) or jump to the [quick start](#quick-start)
below.

The headline claim is enforced rather than asserted: a 5,000-monitor gate runs in
CI against a real engine on every change to the code it measures, and it fails the
build on a regression. It has already caught one — a dashboard query whose cost
grew with install size rather than with what was on screen, which is precisely the
wall this project exists to avoid.

**What v1.0 does not have yet**, so nobody discovers it after installing:
scheduled PDF/CSV reports and SLA/error budgets are Phase 2; organisations, RBAC,
SSO, and on-call scheduling are Phase 3; multi-region probing and HA are Phase 4.
The control plane / probe split those depend on is built and shipping — in solo
mode the probe is simply compiled in — so they arrive as a config change rather
than as a different product. The [roadmap](ROADMAP.md) has the detail.

Coming from Uptime Kuma? `cairn import kuma /path/to/kuma.db` reproduces your
monitors, tags, notifications, and status pages, and merges several instances
into one install. Read
[what does not come across](docs/guides/migrating-from-uptime-kuma.md) first —
half that guide is the honest half.

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
automated load test in CI from the first commit, and it is true at v1.0 rather
than promised for later. The gate runs a real engine at 500 and 5,000 monitors,
compares the two, and fails the build when a per-page cost grows with install
size instead of with the viewport. That is not a hypothetical guard: it caught
exactly that regression in the dashboard's own listing query before v1.0 shipped.

## Quick start

```bash
docker run -d --restart=always -p 127.0.0.1:3000:3000 \
  -v uptime-cairn:/data \
  --name uptime-cairn \
  webloomlabs/uptime-cairn:latest
```

Then open `http://localhost:3000` and create the administrator account. One
binary, embedded probe, embedded UI, SQLite. No external services, no Redis, runs
on a Raspberry Pi.

Images are published to both Docker Hub and GitHub Container Registry, and they
are the same image — pick whichever your environment already trusts:

```
webloomlabs/uptime-cairn:latest          # Docker Hub
ghcr.io/webloomlabs/uptime-cairn:latest  # GHCR
```

Pin the tag in anything you intend to keep. A release publishes three: `:1.0.1`
is exact and never moves, `:1.0` follows the patch series, and `:latest` follows
everything stable. There is deliberately no `:1` — a tag that silently carries
you across a minor version is not a pin.

The bind is `127.0.0.1:3000` rather than `0.0.0.0:3000` deliberately. The binary
has no TLS flags and will not grow any, so anything reachable off the host
belongs behind a reverse proxy — there are
[recipes for Caddy, nginx, and Traefik](docs/operations/reverse-proxy.md), and
all three deny `/metrics` at the edge.

Prefer a binary, Compose, or a Pi? [Install](docs/guides/install.md) covers all
four, and [first monitor](docs/guides/quickstart.md) takes you from an empty
install to an alert you have watched fire.

### Migrating from Uptime Kuma

```bash
cairn import kuma /path/to/kuma.db
```

Run it against a stopped install — SQLite takes one writer, and `--dry-run`
produces the whole report without writing anything.

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
| **0 — Foundations** | Spec, ADRs, governance, data model, load-test harness | Weeks 1–4 · **done** |
| **1 — Solid Core** | 9 monitor types, 13 alert channels + Apprise, status pages, full REST API, 5,000-monitor gate | Months 1–4 · **done — v1.0** |
| **2 — Reporting** | Scheduled white-label PDF/HTML/CSV reports, SLA/SLO + error budgets, post-mortems | Months 4–7 ← *we are here* |
| **3 — Teams** | Orgs, RBAC, SSO/SAML, audit log, automatic incidents, on-call & escalation | Months 7–11 |
| **4 — Scale** | Multi-region consensus, private probes, HA, Terraform, YAML/GitOps, MCP server | Months 11–16 |
| **5 — Depth** | Anomaly detection, plugin SDK, mobile apps, template gallery | Month 16+ |

Full detail in [ROADMAP.md](ROADMAP.md). Phase plans:
[Phase 0](docs/plans/PHASE-0-PLAN.md) · [Phase 1](docs/plans/PHASE-1-PLAN.md).

## Contributing

Contributions are wanted, and now that v1.0 is installable the most valuable ones
have changed: honest reports of what breaks at your scale, and what the sixty-
second onboarding actually felt like on your machine. Spec and ADR review are
still welcome and still matter. Start with [CONTRIBUTING.md](CONTRIBUTING.md).

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
