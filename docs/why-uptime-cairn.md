# Why Uptime Cairn exists

The reasoning behind the project: the gap it is built to fill, the principles
every feature is tested against, and the one architectural decision that makes
the rest possible. The [README](../README.md) is the short version.

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
   multi-region, PDF reports, audit logs — all of it ships in the open source
   build. Uptime Cairn earns money from hosting and support, never by crippling
   the software.
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

