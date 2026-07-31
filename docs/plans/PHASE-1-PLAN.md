# Uptime Cairn — Phase 1 Plan: Solid Core

**Duration:** Months 1–4 (immediately after Phase 0)
**Source:** Uptime-Cairn-Plan.pdf §6 (Roadmap), §2 (Principles), §4–5 (Differentiator, Architecture)
**Mandate:** Phase 1 must be **independently useful and production-deployable for real users**. If it can't win real users standing alone, no later phase saves the project (source plan §9, "scope collapse" — the critical risk). Cut features, never the phase.

**Exit condition:** A solo user can replace Uptime Kuma entirely and never think about it again — **and an agency with 1,000 monitors can too**, which Uptime Kuma cannot serve at all.

---

## 1. What Phase 1 Ships (scope, in one view)

| Area | In Phase 1 | Deferred |
|---|---|---|
| Monitoring engine | All 10 monitor types, retries, timeouts, intervals down to 20s, dependency-aware suppression | Browser/synthetic checks, SNMP/MQTT/DB checks (Phase 4) |
| Alerting | 13 native channels + Apprise meta-provider (~90 more), webhook payload templating | On-call schedules, rotations, escalation (Phase 3) |
| API | Full REST API per the frozen Phase 0 OpenAPI spec, scoped API keys, contract tests in CI | — |
| UI | Full dashboard, dark mode, i18n scaffolding, status pages with custom domains | White-labelling (Phase 2/3) |
| Scale | Server-side pagination/filter/search everywhere; **5,000-monitor CI acceptance gate** | Multi-region probes, HA, Postgres path (Phase 4) |
| Migration | `cairn import kuma` integrated end-to-end, incl. multi-instance merge | — |
| Reporting | Prometheus `/metrics`, uptime history browsing | The reporting engine (Phase 2 — the differentiator) |
| Multi-user | Single admin account (2FA), tenancy keys in schema | RBAC, SSO, teams (Phase 3) |

Progressive disclosure is enforced from the start: the solo user never sees fields for features that don't exist yet, and the schema quietly carries tenancy/team keys so Phases 2–3 are never a re-architecture.

---

## 2. Architecture in Phase 1

Solo mode only, but with the seams built correctly:

- **One Go binary**: control plane + in-process probe + embedded UI + SQLite. `docker run` → open browser → first monitor in under 60 seconds. This onboarding bar is tested in UX review every release.
- **The probe runs behind the ADR-001 gRPC interface even in-process.** Solo users never see the split; Phase 4 turns it on with `--mode=probe`. Nothing about the check scheduler may assume same-process execution.
- **SQLite in WAL mode** with the tiered rollup pipeline (raw → 1m → 5m → hourly → daily) implemented now — it is what keeps 5,000 monitors' history queryable without unbounded disk growth, and what makes Phase 2 reporting possible.
- **API-first, literally**: the dashboard consumes only `/api/v1`. No privileged internal endpoints. Contract tests in CI verify every endpoint against the frozen spec.
- **UI state model per ADR-004**: paginated, filtered, server-side queries; scoped live updates (only what's on screen); the client is never sent full state. This is the direct answer to Kuma's 300–600-monitor wall.
- **Frontend**: SvelteKit (or React) + Tailwind + shadcn/ui, embedded into the binary.
- **Minimal dependency surface**: pinned and vendored deps, SBOM published with every release, reproducible builds.

---

## 3. Feature Specifications

### 3.1 Monitor Types (10)

| Type | Details |
|---|---|
| HTTP/HTTPS | Status code match, keyword present/absent, JSON path assertion, regex, response-time threshold; custom methods/headers/body; auth (basic/bearer); redirect and TLS-verify options |
| TCP port | Connect success/latency |
| ICMP ping | With **explicit restricted-container handling**: detect environments where raw sockets are unavailable, fail with a clear explanatory message (never silent breakage), and offer automatic fallback to TCP checks (source plan §1.4, secondary notes) |
| DNS record | Record type + expected value(s), choice of resolver |
| SSL/TLS expiry | Days-remaining threshold, chain validation |
| Domain expiry | WHOIS/RDAP-based, days-remaining threshold |
| Push / heartbeat | Dead-man's-switch: monitor fires if the expected inbound ping does not arrive within its window |
| Docker container | Container running/health status via Docker socket/API |
| gRPC | Health-check protocol support |

Common to all: retries before state change, per-monitor timeout, custom intervals down to **20 seconds**, upside-down mode where sensible, notes/description.

### 3.2 Core Monitoring Features

- **Groups and tags** — organisational primitives used everywhere (filtering, status pages, bulk operations, later reporting).
- **Dependency-aware suppression** — a parent going down suppresses child alerts (router down ≠ 40 pages).
- **Maintenance windows** — scheduled, recurring, attached to monitors/groups/tags; suppress alerts and annotate history.
- **Certificate & domain expiry surfaced in the UI** as upcoming-expiry data (full calendar report is Phase 2).
- **Uptime history browsing** — retention limited only by disk, with drilldown into arbitrary past ranges via rollups. The default UI must not be week-blinkered like Kuma's.

### 3.3 Alerting

Native channels: **email (SMTP), generic webhook, Slack, Discord, Telegram, Matrix, Gotify, ntfy, Microsoft Teams, PagerDuty, Opsgenie, Twilio/SMS** — plus **Apprise as a meta-provider**, buying ~90 additional channels for a fraction of the effort.

- Per-monitor and default notification assignments; test-fire button on every channel.
- Resend/repeat policy, recovery notifications, configurable "down after N failures".

### 3.4 Webhook Payload Templating

Directly answers source plan §1.4(c) — cheap to build, disproportionately appreciated:

- User-defined payload body, headers, and HTTP method per webhook.
- Variable interpolation: `{{monitor.name}}`, `{{status}}`, `{{response_time}}`, timestamps, tags, etc.
- **Live preview** in the UI showing the rendered payload before saving.

### 3.5 Public Status Pages

- Multiple status pages per install; pick monitors/groups per page.
- Custom domains (with docs for reverse-proxy + ACME setup).
- Uptime bars, incident/maintenance display, subscribe-to-updates via the API.
- Clean unauthenticated read path that also holds up under load.

### 3.6 REST API & Integration Surface

- Full CRUD over every Phase 1 entity per the frozen OpenAPI spec; versioned `/api/v1`.
- Scoped API keys: per-token permissions, expiry, last-used tracking, revocation.
- Outbound webhooks for every state change.
- Native Prometheus `/metrics` (per-monitor status, response time, cert days-remaining) and OpenTelemetry export.
- Generated API clients published (at minimum Go + TypeScript) from the spec.

### 3.7 Kuma Migration

- `cairn import kuma <path-to-kuma.db>` integrated into the product (CLI + guided UI flow): monitors, tags, notifications, status pages.
- Multi-instance merge: point it at several Kuma databases, get one Cairn install — the story nobody else can offer.
- Import report: what mapped cleanly, what needs attention, nothing silently dropped.

### 3.8 UI & Polish

- Dashboard: live status overview, monitor detail with history charts, bulk operations (multi-select enable/disable/tag — agencies need this at 1,000 monitors).
- Dark mode; i18n scaffolding with English complete and the translation pipeline documented for contributors.
- Empty states, sensible defaults everywhere: works with zero configuration, bends fully when configured.

---

## 4. Production-Readiness (what "ready to deploy for real users" means)

Phase 1 is a real release, not a beta drop. The following are in-scope deliverables, not afterthoughts:

### 4.1 Security

- Single admin account with strong password hashing (argon2id), session management, CSRF protection, rate-limited login, and **TOTP 2FA**.
- API-key auth from day one (the §1.2 lesson: users begged Kuma for key-based auth).
- Security headers, TLS guidance, no telemetry by default (opt-in only, per principle 7: "your data is yours").
- A published `SECURITY.md` with a disclosure process; dependency and container scanning in CI.

### 4.2 Deployment & Operations

- **Docker image** (multi-arch amd64/arm64, small, distroless-or-similar) and a one-line `docker run` that reaches first-monitor in under 60 seconds.
- `docker-compose.yml` reference, systemd unit example, raw binary downloads with checksums.
- Reverse-proxy recipes (Caddy, nginx, Traefik) including custom-domain status pages.
- **Backup & restore**: documented, tested SQLite backup path (online backup command + restore verification).
- **Upgrades**: forward-only versioned migrations run automatically on start; documented rollback stance; semver with an explicit compatibility promise for `/api/v1`.
- Self-monitoring: the process exposes its own health endpoint and internal metrics ("who watches the watchman" starts here).

### 4.3 Documentation

- Install guide (Docker, compose, binary, Pi), first-monitor quickstart, per-monitor-type reference, alerting setup per channel, API reference generated from the spec, Kuma migration guide, backup/upgrade/ops guide.
- Docs ship with the release — a self-hosted tool with thin docs is not "ready for real users."

### 4.4 Quality Gates (CI, every release)

- Unit + integration tests; **contract tests** verifying the server against the frozen OpenAPI spec.
- E2E smoke of the golden path: install → create monitor → down event → alert fires → recovery → status page reflects it.
- **The acceptance gate: 5,000 monitors on one install with a UI that stays responsive**, verified by the automated load test in CI. This is the headline claim against the category leader and it must be true at v0.1, not promised for later. Continuous load testing against a 10,000-monitor synthetic workload runs from Phase 1 onward to catch regressions.
- Never lose a heartbeat: crash-recovery test — kill the process mid-cycle, verify no corrupted state and checks resume correctly.

---

## 5. Milestone Breakdown

### Month 1 — Engine & API skeleton
- Scheduler + check-execution engine behind the probe interface; HTTP/S, TCP, ping (with container-ICMP handling) working.
- SQLite schema + migrations from the Phase 0 data model; heartbeat writes + rollup pipeline.
- API server bootstrapped from the OpenAPI spec; auth (admin + API keys); contract tests green for implemented endpoints.
- Load-test harness now runs against the real engine in CI.
- **Checkpoint:** a monitor can be created via `curl`, checked on schedule, and its history queried via the API — no UI yet.

### Month 2 — Full monitor coverage & alerting
- Remaining monitor types: DNS, TLS expiry, domain expiry, push/heartbeat, Docker, gRPC.
- Retries, timeouts, intervals, groups, tags, dependency suppression, maintenance windows.
- All 13 native alert channels + Apprise integration + webhook templating with preview rendering (API-level).
- **Checkpoint:** full monitoring + alerting lifecycle runs headless via API; 5,000-monitor load test passes at the engine/API layer.

### Month 3 — UI & status pages
- Dashboard (list/detail/history/bulk ops), monitor CRUD, notification setup with test-fire, webhook template live preview.
- Public status pages with custom-domain support; dark mode; i18n scaffolding.
- ADR-004 state model proven: UI benchmark added to the CI load test — list, filter, and paginate stay responsive at 5,000 monitors.
- **Checkpoint:** the golden-path E2E passes; UX review confirms 60-seconds-to-first-monitor and that no Phase 2+ concepts leak into the solo UI.

### Month 4 — Migration, hardening, release
- `cairn import kuma` integrated (CLI + UI flow), multi-instance merge, import report; tested against real community Kuma databases.
- Security hardening pass (2FA, rate limiting, headers, scanning); backup/restore and upgrade paths implemented and tested.
- Docs complete; docker-compose/systemd/reverse-proxy recipes; generated Go + TS clients published.
- Release engineering: tagged v0.1.0, multi-arch images, binaries + checksums + SBOM, announcement post.
- Buffer: this month absorbs slippage — **features get cut before the release date or quality gates do**.

---

## 6. Exit Criteria (all must be true)

- [ ] All 10 monitor types working with retries, timeouts, 20s minimum intervals.
- [ ] All 13 native alert channels + Apprise + webhook templating with live preview.
- [ ] Groups, tags, dependency suppression, maintenance windows, history drilldown.
- [ ] Public status pages with custom domains.
- [ ] Full REST API matching the frozen spec; contract tests green; scoped API keys; Prometheus metrics; state-change webhooks.
- [ ] **CI load-test gate green: 5,000 monitors, responsive UI** (server-side pagination verified; client never sent full state).
- [ ] `cairn import kuma` migrates a real Kuma install — and merges several — in under a minute of user effort.
- [ ] `docker run` → first monitor in under 60 seconds, verified in UX review.
- [ ] 2FA, backups, automatic migrations, docs, SBOM, reproducible multi-arch release artifacts all shipped.
- [ ] **The test that matters:** a solo user replaces Uptime Kuma and never thinks about it again; an agency with 1,000 monitors runs on one install.

## 7. Success Metric (from source plan §8)

Phase 1 release target: **500 GitHub stars; 50 self-hosted installs reporting in (opt-in telemetry only).**

## 8. Phase 1 Risks

| Risk | Severity | Mitigation |
|---|---|---|
| Scope creep toward Phases 2–4 (reporting, on-call, HA) | Critical | The scope table in §1 is the contract. Reporting is deliberately *absent* so Phase 2 lands as its own splash; anything not in the table needs a plan revision, not a quiet addition |
| 5,000-monitor gate discovered failing late | High | Gate runs in CI from Month 1 against the real engine; UI benchmark joins in Month 3. Regressions block merge, not release |
| Alert-channel breadth eats the schedule | Medium | Apprise covers the long tail; native channels are ordered by user demand and can be cut past the top 6 (email, webhook, Slack, Discord, Telegram, ntfy) without missing exit criteria intent — cut features, never quality gates |
| ICMP-in-container support burns time | Low | The plan already prescribes the shape: detect, explain clearly, fall back to TCP. Do exactly that and move on |
| Single-maintainer burnout | Critical | Carry-over from Phase 0: responsive `good-first-issue` triage, public roadmap, monthly progress notes; committer recruitment remains a P0 deliverable through Phase 1 |
