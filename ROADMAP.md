# Roadmap

The public plan for Uptime Cairn. This is a living document — it gets revised as
evidence arrives, and it is not defended as written. If you have data that says
a decision here is wrong, open an issue; that is the most useful contribution
you can make.

**The rule that governs this roadmap: cut features, never phases.** Each phase
must ship something independently useful. The normal failure mode for a project
this ambitious is 40% built and abandoned, and the defence against it is that
Phase 1 has to win real users standing entirely on its own.

Detailed plans for the current work live in
[docs/plans/PHASE-0-PLAN.md](docs/plans/PHASE-0-PLAN.md) and
[docs/plans/PHASE-1-PLAN.md](docs/plans/PHASE-1-PLAN.md).

---

## Where we are

**Phase 0 — Foundations.** Specifications, decisions, and infrastructure. No
product code yet, by design.

---

## Phase 0 — Foundations *(weeks 1–4)*

Repository, licence, code of conduct, governance, CI/CD. Data model. **The
OpenAPI spec is written and frozen before any code is written.** Probe protocol
design. Architecture Decision Records from day one.

One thing gets built in this phase, ahead of the product itself:

- **The load-test harness**, because "5,000 monitors on one install and the UI
  stays fast" is the project's central promise and it must be measured
  continuously from the first commit, not discovered late.

The Uptime Kuma importer was originally scoped here as a standalone tool. It has
moved to Phase 1, where it ships integrated into the product — same P0 priority,
one build instead of two.

**Exit:** an ADR set and an OpenAPI spec a stranger could implement against.

## Phase 1 — Solid Core *(months 1–4)* · must be independently useful

The release where Uptime Cairn becomes a real product you can run in production.

**Monitor types** — HTTP/HTTPS (status, keyword, JSON path, regex, response-time
threshold), TCP port, ICMP ping, DNS record, SSL/TLS expiry, domain expiry,
push/heartbeat (dead-man's switch), Docker container, gRPC.

**Alerting** — email (SMTP), webhook, Slack, Discord, Telegram, Matrix, Gotify,
ntfy, Microsoft Teams, PagerDuty, Opsgenie, Twilio/SMS — plus **Apprise as a
meta-provider**, which buys roughly 90 additional channels for a fraction of the
effort.

**Core** — groups, tags, dependency-aware suppression, retries, timeouts, custom
intervals down to 20 seconds, maintenance windows, public status pages with
custom domains, dark mode, i18n scaffolding, full REST API, Prometheus metrics.

**Webhook templating** — user-defined payload bodies, headers, and HTTP method,
with variable interpolation (`{{monitor.name}}`, `{{status}}`,
`{{response_time}}`) and a live preview. Small feature, disproportionately
appreciated.

**Scale from day one** — server-side pagination, filtering, and search; the
client is never sent full state. **Acceptance gate: 5,000 monitors on one
install with a UI that stays responsive**, verified by automated load test in
CI. This is the headline claim against the category leader and it must be true
at v0.1, not promised for later.

**ICMP handling** — detect restricted container environments where raw sockets
are unavailable, fail with a clear explanatory message rather than breaking
silently, and offer automatic fallback to TCP checks.

**Migration** — `import kuma <path-to-kuma.db>` reads an Uptime Kuma SQLite file
and reproduces monitors, tags, notifications, and status pages. This is a P0
deliverable, not a nice-to-have: every existing Kuma user is a 30-second
migration away. It explicitly handles merging **several** Kuma instances into
one install, for the people currently sharding by hand across hosts.

**Exit:** a solo user can replace Uptime Kuma entirely and never think about it
again — and an agency with 1,000 monitors can too, which Uptime Kuma cannot
serve at all.

## Phase 2 — Reporting *(months 4–7)* · the differentiator

Reporting is a first-class subsystem, not a graphs page. It is the most
under-served need in the category and the feature most likely to make someone
switch, because it is the thing they currently produce by hand in a spreadsheet
every month.

- **Scheduled reports** — daily, weekly, monthly, quarterly; auto-generated and
  auto-delivered by email, Slack, webhook, or S3 drop.
- **White-label branding** — upload a logo, set colours and a footer; the client
  receives a report that looks like their agency made it.
- **Multiple formats** — PDF, HTML, CSV, JSON, and a public shareable link.
- **SLA / SLO reports** — target vs. actual, error budget consumed and
  remaining, burn rate, breach log with timestamps. What auditors and contract
  reviews actually need.
- **Incident post-mortems** — auto-drafted from the incident timeline: detection,
  acknowledgement, resolution, MTTD/MTTA/MTTR, affected components, alerts fired.
- **Comparative reporting** — period over period, monitor vs. monitor, region vs.
  region.
- **Custom report builder** — pick metrics, group by tag/client/region, choose a
  window, save as a reusable template.
- **Full historical browsing** — retention limited only by disk, with real
  drilldown into arbitrary past ranges.
- **Certificate & domain expiry reports** — a forward-looking calendar, not an
  alert three days before it breaks.

Shipped **before** the enterprise controls, deliberately.

**Exit:** an agency can send 50 branded client reports on the 1st of every month
without touching anything.

## Phase 3 — Teams & Multi-Tenancy *(months 7–11)*

Organisations and workspaces with hard data isolation. RBAC (owner / admin /
editor / responder / viewer / billing) with custom roles. Local auth + OIDC +
SAML + LDAP. Full audit log — who changed what, when, from where. Per-client
white-labelling.

**Incident management** — incidents open **automatically** from failing checks,
with acknowledgement, assignment, a threaded timeline, status page linkage, and
auto-resolve.

**On-call** — schedules, rotations, overrides, holiday calendars, escalation
policies, business-hours-only routing, alert deduplication and grouping, quiet
hours.

**Exit:** a 50-person engineering org can run production on-call from this and
nothing else.

## Phase 4 — Scale & Platform *(months 11–16)*

Multi-region probes with consensus checking (require *N of M* regions to agree
before declaring an outage). Private probe agents for VPC and firewalled targets.
HA control plane. Postgres/Timescale path with tiered rollups. Terraform
provider. YAML + CLI `apply` with a GitHub Action. MCP server so assistants can
manage monitors. Browser/synthetic checks via Playwright. Multi-step API flows.
SNMP, MQTT, Radius, database checks (Postgres/MySQL/Mongo/Redis).
Backup/restore and disaster recovery.

**Exit:** feature-complete against every commercial competitor.

## Phase 5 — Depth *(month 16+)*

Latency anomaly detection against learned baselines. Predictive expiry and
capacity warnings. AI-drafted incident summaries. Plugin SDK for custom monitor
types and notification channels. Mobile apps. Public template gallery for status
pages and report layouts.

---

## Success metrics

| Milestone | Target |
|---|---|
| Phase 1 release | 500 GitHub stars; 50 self-hosted installs reporting in (opt-in) |
| Phase 2 release | 2,000 stars; 10 agencies using white-label reports in production |
| Phase 3 release | 5,000 stars; 3+ regular external committers; first 100-person org in production |
| Phase 4 release | 15,000 stars; listed as a first-class alternative on major comparison sites |
| Health, always | Median issue first-response < 48h; no more than 2 consecutive weeks without a release |

## What is deliberately *not* on this roadmap

- A paid tier that unlocks features. RBAC, SSO, multi-region, PDF reports, and
  audit logs all ship in the AGPL build, in the phase listed above. Funding comes
  from hosting and support.
- A separate "enterprise edition" codebase. One codebase, one binary.
- Telemetry that is on by default.
- A required Redis (or any other) dependency. Optional, never mandatory.

## Influencing the roadmap

Open an issue. The most persuasive input is evidence — your monitor counts, the
thing that broke at scale, the report you build by hand every month. Feature
requests are weighed against the product principles: sixty seconds to first
monitor, progressive disclosure, and a UI that stays fast at 5,000 monitors. A
feature that serves one persona by degrading another's first-run experience is
the wrong trade.
