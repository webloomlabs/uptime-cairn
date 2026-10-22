# Uptime Cairn — Phase 3 Plan: Teams & Multi-Tenancy

**Duration:** Months 7–11 (immediately after Phase 2; v1.1.0 is tagged)
**Source:** [ROADMAP.md](../../ROADMAP.md) § Phase 3; [ADR-003](../adr/003-tenancy-model.md); [data model §3, §4.8, §4.12, §4.13](../data-model/README.md); Uptime-Cairn-Plan.pdf §3 (personas), §6 (Roadmap)
**Mandate:** Turn a single-owner install into one a team runs production from — many people, the right permissions, a record of who did what, incidents that open themselves, and a pager that reaches the right person at 3am.

**Exit condition:** A 50-person engineering org can run production on-call from this and nothing else.

The rule that governs the roadmap still governs this plan: **cut features, never phases.** Phase 3 is the largest phase so far, and §9 names the cut line in advance so that it is not drawn under deadline pressure.

---

## 1. What Phase 3 Ships (scope, in one view)

| Area | In Phase 3 | Deferred |
|---|---|---|
| Tenancy | Organisations with hard data isolation enforced in the application; a second organisation becomes possible; users belong to many organisations | Silo (database-per-org) option, RLS on Postgres (Phase 4, with the Postgres path) |
| Users & teams | Invitations, user management, teams, team membership | SCIM provisioning |
| RBAC | The six roles (owner / admin / editor / responder / viewer / billing), enforced; custom roles as named grant sets | Per-resource ACLs (e.g. "may edit monitors tagged `client-a` only") unless the tenancy ADR requires them |
| Authentication | Local auth (exists) + OIDC; SAML and LDAP per the cut line in §9 | Hardware-key-only policies, WebAuthn |
| Audit | Every configuration change and every authentication event written to `audit_log`; browsable, filterable, exportable | Tamper-evident hash chaining, external SIEM streaming beyond the existing webhook |
| White-labelling | Brand profiles bound to an organisation (or client, per the tenancy ADR); status pages able to reference one | Per-recipient branding |
| Incidents | Automatic opening from failing checks, acknowledgement, assignment, threaded timeline, status page linkage, auto-resolve | AI-drafted summaries (Phase 5) |
| Alert routing | Deduplication, grouping, quiet hours, business-hours routing | ML-based noise reduction |
| On-call | Schedules, rotations, overrides, holiday calendars, escalation policies, "who is on call" | Mobile apps with push acknowledgement (Phase 5), phone-call escalation beyond existing Twilio |
| Scale | The 5,000-monitor gate green with multiple organisations and escalation running; a total partition produces a bounded number of pages | Distributed escalation workers, HA (Phase 4) |

Progressive disclosure is not negotiable here, and Phase 3 is where it is most at risk. **A solo user with three monitors never sees an organisation switcher, a role picker, a team, or an escalation policy** until they invite a second person or ask for one. The install that runs `docker run` and adds a first monitor in sixty seconds must be byte-for-byte the same experience after this phase as before it.

---

## 2. What Phases 1 and 2 Already Give Us — and Where They Fall Short

Phase 3 was prepared for from the first migration. The preparation is real but uneven, and this section is candid about both halves, because the gap between them is the first month's work.

### 2.1 What is in place

- **An `organisations` table and a sentinel org** ([`0001_initial.sql`](../../migrations/sqlite/0001_initial.sql), `model.SentinelOrgID` in [`internal/model/identity.go`](../../internal/model/identity.go)). Every tenant-scoped table carries `org_id` from its creating migration, as ADR-003 decided — including all seven Phase 2 reporting tables.
- **The six roles already exist in the schema** — a `CHECK` over owner/admin/editor/responder/viewer/billing on `users.role` ([`0002_identity.sql`](../../migrations/sqlite/0002_identity.sql)) — and **roles already resolve to scopes**: `auth.ScopesFor(role)` in [`internal/auth/scope.go`](../../internal/auth/scope.go), whose comment states the intent exactly: *"Phase 3's RBAC should be a table of grants rather than a rewrite of the authorisation path."* Every route is already guarded by `require(scope, …)` in [`internal/api/server.go`](../../internal/api/server.go).
- **Sessions, argon2id passwords, TOTP with recovery codes, hashed API keys with scopes**, and the rule that a key cannot mint a more powerful key (`Covers`). Local authentication is done; Phase 3 adds principals, not a login system.
- **The `audit_log` table exists** ([`0003_alerting_and_pages.sql`](../../migrations/sqlite/0003_alerting_and_pages.sql)), append-only by design, with actor type, before/after `changes` with secrets elided at write time, IP, user agent and request id, and indexes on `(org_id, created_at)` and `(org_id, entity_type, entity_id, created_at)`. Rollup retention deliberately never touches it.
- **Incidents already carry the Phase 3 fields**: `AutoOpened`, `DetectedAt`, `AcknowledgedAt`, `AcknowledgedBy`, `AssignedTo` ([`internal/model/incident.go`](../../internal/model/incident.go)), with `acknowledged_by` and `assigned_to` as foreign keys to users. MTTD/MTTA/MTTR are derived on read. Status page linkage, subscriber announcement, and `incident.opened/updated/resolved` events exist.
- **The state-transition decision lives in one place** — `apply()` in [`internal/controlplane/ingest.go`](../../internal/controlplane/ingest.go) — with retries, maintenance suppression and dependency-aware suppression already applied before an event fires. Automatic incident opening hangs off that one function, not off every channel.
- **PagerDuty and Opsgenie already dedupe** on a stable per-monitor key and auto-resolve ([`internal/notify/oncall.go`](../../internal/notify/oncall.go)). Teams that already page through those keep working unchanged.
- **The Phase 3 API surface is drafted**: 20 operations tagged `x-cairn-phase: 3` in [openapi.yaml](../api/openapi.yaml) — user create/update/delete, teams, on-call schedules (rotations, overrides, a business-hours `restriction`), `getWhoIsOnCall`, escalation policies, and `acknowledgeIncident`. Per the spec header and [COMPATIBILITY.md](../api/COMPATIBILITY.md), none of it has shipped, so all of it remains editable.

### 2.2 Where the preparation falls short

- **Tenant scoping does not go through one code path.** ADR-003's stated mitigation was to "route tenant scoping through one shared code path (even though it resolves to the sentinel org for two phases), so Phase 3 changes what that path returns rather than rewriting every call site." That path was not built. `model.SentinelOrgID` is referenced **156 times across 53 files** under `internal/`; the API server holds a hardcoded `orgID` field; the `Principal` carries no organisation; `resolveAPIKey` ignores `key.OrgID`; and most store methods (`GetMonitor(ctx, id)`, `GetBrandProfile(ctx, id)`, `ListAPIKeys`, …) take no organisation argument. Roughly 76 of about 265 SQL statements in the SQLite store filter on `org_id`, and `rollup.go`, `heartbeats.go` and `report_shares.go` filter on none. **ADR-003 named this exact outcome as the thing that becomes expensive to reverse.** It is the first work in this phase, and nothing user-visible should be built until it is done.
- **Nothing writes to `audit_log`.** The table is a skeleton; there is no package, no insert path and no API.
- **Incident events reach no notification channel.** The dispatcher resolves channels only through `ChannelsForMonitor(ev.Monitor.ID)`, and an incident event deliberately carries no monitor ([`internal/notify/event.go`](../../internal/notify/event.go)), so `incident.*` events currently go to outbound webhooks and nowhere else. On-call cannot be built on top of that; routing must be designed, not patched.
- **Nothing sets `AutoOpened`, `DetectedAt` or `AcknowledgedAt`**, and `POST /incidents/{id}/acknowledge` is in the spec with no route.
- **The `billing` role resolves to no scopes at all**, and `editor` holds no report or brand-profile scopes — so "report authoring by non-owners", deferred from Phase 2, has no role that can do it yet.
- **The spec has no surface for** organisations, memberships, invitations, the audit log, SSO configuration, custom roles, holiday calendars, quiet hours, or alert grouping. These are inventions, not reconciliations, and must go through spec review before any handler (§6).
- **There is no Postgres backend** ([`migrations/migrations.go`](../../migrations/migrations.go): "ADR-002 defers the scaled path to Phase 4"). ADR-003's RLS spike is therefore moot in this phase — **application-level filtering is the only enforcement Phase 3 has**, which makes its test coverage the isolation guarantee rather than a backstop to one.

---

## 3. The Problems to Settle Before Writing Phase 3 Code

Five problems, each expensive to reverse. **All five are now drafted, and none is accepted yet** — each ADR is `Proposed` and awaits review in its own PR:

| Problem | ADR | Status |
|---|---|---|
| §3.1 Tenancy, isolation, membership | [ADR-010](../adr/010-tenancy-isolation-and-membership.md) | Proposed |
| §3.2 Authorisation, roles, grants | [ADR-011](../adr/011-authorisation-roles-and-grants.md) | Proposed |
| §3.3 External identity | [ADR-012](../adr/012-external-identity.md) | Proposed |
| §3.4 Incident automation and routing | [ADR-013](../adr/013-incident-automation-and-alert-routing.md) | Proposed |
| §3.5 On-call semantics | [ADR-014](../adr/014-on-call-scheduling-semantics.md) | Proposed |

The five were drafted on 2026-09-22 and 2026-10-21 **by an agent under an explicit maintainer waiver of [AGENTS.md](../../AGENTS.md) § *What agents may not do* (3)**, following the precedent at [data model §11.6](../data-model/README.md#116-secrets-at-rest). Every decision inside them was put to the maintainer as a question with its alternatives and answered by them; the prose, the evidence gathering and the consequences analysis are the agent's. Each ADR records that at its head. **A reviewer should check the reasoning, not only the conclusions** — and [ADR-012](../adr/012-external-identity.md) carries an extra warning, because rule 8 (security work is human-led) was *not* waived and that document describes the authentication path.

What follows below is the question each ADR answers, the constraints already in force, and the options that were visible — kept as written, because it is the record of what was asked before anything was decided.

### 3.1 Tenancy model — isolation, hierarchy, membership

ADR-003 deferred four questions to exactly this point; all four are now due.

- **Hierarchy.** A flat organisation, or organisation + client for the agency/MSP persona? ADR-003 records that the answer depends on "does an agency's client ever get direct login access" and "how deep does white-labelling need to go" — questions to put to real agencies, given Phase 2's success metric is ten of them using white-label reports.
- **Membership.** A `memberships` join table (user ↔ org, role per membership) or a column on `users`. ADR-003 leans to the join table for the MSP consultant working across agencies. It interacts with the existing `UNIQUE (org_id, email)` on `users` and with how a session selects its active organisation.
- **Enforcement.** Application-level only, on SQLite, for this phase (§2.2). The ADR must say *how* it is made structural rather than remembered — e.g. an organisation carried in the request context and required by the repository interface, so that an unscoped query does not compile rather than fails review.
- **What is global.** Some things are legitimately instance-wide: status page custom domains are unique across all organisations today ([`0003`](../../migrations/sqlite/0003_alerting_and_pages.sql)) and should stay so; probes, the root key, SMTP relay and retention may be too. The ADR names the list, because each item on it is a place where one tenant can observe another.

**Consequence that binds everything after it:** the rest of this plan is written against "a request has exactly one active organisation, carried in context". If the ADR chooses organisation + client, §4.1 and §4.6 change shape; nothing else in the plan should.

### 3.2 Authorisation model — roles, custom roles, and what a scope is

- Roles become **grants stored as data** rather than a `switch` in `ScopesFor`, per that function's own comment. Custom roles are then a named set of scopes.
- Open questions for the ADR: are custom roles per organisation or instance-wide; are the six built-ins editable (likely not — they are documentation as much as data); what does `billing` actually grant, given there is no billing in an open source install with no paid tier; does `responder` acknowledge but not edit; and **is a scope enough, or do teams need resource scoping** ("team A edits team A's monitors")? The last question is the expensive one: resource-level authorisation touches every list query at 5,000 monitors.
- API keys already carry scopes and `Covers` already prevents escalation. The ADR must state how a key relates to its creator's role when that role later shrinks.

### 3.3 External identity — OIDC, SAML, LDAP

- **This is security code, and [AGENTS.md](../../AGENTS.md) rule 8 applies in full.** Phase 2 recorded waivers of rule 8 for share links and the S3 signer. **That precedent should not be assumed to extend here**: an identity provider integration is the authentication path itself.
- **Dependency policy is the crux.** OIDC is feasible on the standard library plus JOSE verification; SAML requires XML canonicalisation and XML-DSig verification, which the standard library does not provide and which has a long history of signature-wrapping vulnerabilities when hand-rolled; LDAP requires an LDAP client. Each is a dependency decision under [CONTRIBUTING.md](../../CONTRIBUTING.md)'s policy, needing human justification in its PR, and the SAML one in particular is a "write it ourselves" answer that should be refused.
- Questions for the ADR: just-in-time provisioning or invite-only; account linking by verified email (and the takeover risk if an IdP does not verify it); group-to-role mapping; whether local login can be disabled per organisation and how an owner recovers when the IdP is down; how a 30-day fixed session (`sessionLifetime` in [`internal/api/auth.go`](../../internal/api/auth.go)) interacts with IdP-side deprovisioning.

### 3.4 Incident automation and alert routing

- **When does a failing check open an incident?** On every `down` transition, or only per policy (by monitor, group, tag, severity)? An incident per monitor, or one correlated incident per dependency root or group? A total partition at 5,000 monitors must not open 5,000 incidents and page someone 5,000 times — the harness already simulates exactly that partition (§8).
- **Auto-resolve**: on recovery of every linked monitor, after a quiet period, and never when a human has changed state by hand?
- **Routing**: today routing is monitor → channels. On-call needs event → escalation policy → person → their channels. The ADR decides whether escalation policies *replace* monitor-bound channels for incidents or run alongside them, and how incident events acquire recipients given they carry no monitor (§2.2).
- **Deduplication and grouping keys**, and how they relate to the PagerDuty/Opsgenie dedup key already in use, so a team paging through PagerDuty does not get double-paged once Cairn escalates natively.

### 3.5 On-call scheduling semantics

- **Time is the hard part.** Rotations hand off at a local time in a named timezone; DST transitions create 23- and 25-hour days; overrides overlap rotations; holidays remove days. The ADR fixes the semantics — which timezone governs, what happens to a handoff at 02:30 on a spring-forward night — before code, the same way Phase 2 fixed the SLA denominator before code.
- **Escalation must survive a restart.** A page scheduled to escalate in 10 minutes is state, not a goroutine timer. The Phase 2 precedent applies: a missed schedule is *late, not lost*, and the UI says which.
- **Holiday calendars**: source and format. Importing iCalendar is the obvious route, and full `RRULE` support is far larger than it looks; the ADR should bound it (e.g. dated all-day events only) or choose another source.

### 3.6 Also due: ADR-009

[ADR-009](../adr/009-pagination-sort-key.md) (cursor on `(sort_field, id)`) was still **Proposed** when this plan was written, deferred to this phase by the Phase 2 checklist.

**Decided on 2026-10-21: accepted into Phase 3, and it lands before the tenancy refactor.** Its migration touches the monitors table and its cursor indexes — the same table the scoping work opens — so doing it first means that table is rebuilt once rather than twice. The cost is that month 7 also carries ADR-009's collation decision, which is a silent performance cliff if it is got wrong. ADR-009's own Status line is updated in its acceptance PR, not here.

---

## 4. Feature Specifications

Each subsection states requirements, not designs; where the design depends on an ADR in §3, it says which.

### 4.1 Organisations and Tenancy

- **One scoping path.** The active organisation is resolved once per request — from the session's selected membership or the API key's `org_id` — carried in context, and required by every repository method that reads or writes tenant data. `SentinelOrgID` survives only in the migration that seeds it and the code path that resolves "the default organisation" for single-org installs.
- **A second organisation is possible**, created by an instance owner; a user with memberships in several can switch between them. The switcher does not render for a user with one membership.
- **Isolation is tested as a property, not per endpoint.** For every tenant-scoped resource, a fixture with two organisations asserts that no list, get, update, delete, stream subscription, report, share link, export or search in one organisation can observe the other. This suite is the isolation guarantee (§2.2), so it grows with every resource added after it.
- **The live channel ([ADR-004](../adr/004-ui-state-synchronisation.md)) respects organisation boundaries.** Subscriptions are by id; the bus must refuse an id from another organisation, not merely not offer one.
- **Background work is scoped too**: rollups, retention, report runs, maintenance sweeps and the importer run per organisation or explicitly across all with the organisation carried on each row.

### 4.2 Users, Invitations and Teams

- Invite by email with a single-use, expiring token (stored hashed, following the session-token precedent); accept creates or links an account. User create/update/delete per the drafted spec operations.
- **The last owner cannot be removed or demoted.** Deactivating a user revokes their sessions and API keys immediately, and the audit log records it.
- Teams and team membership per the drafted `/teams` operations. Teams are escalation targets (§4.9) and — if §3.2 decides so — authorisation scopes.

### 4.3 Roles and Permissions (RBAC)

- The six built-in roles enforced for every principal, with scopes as data per §3.2. Every route keeps its `require(scope, …)` guard; the change is where the scopes come from, not how they are checked.
- `editor` (or a role §3.2 chooses) gains report and brand-profile scopes, closing the Phase 2 deferral "report authoring by non-owners".
- **Custom roles** as named scope sets, with the dashboard showing exactly what a role can do in the same words the API reference uses.
- A permission denial is a `403 insufficient-scope` problem document naming the missing scope, as today — never a `404` that hides the resource from someone who can see it exists, and never a `200` with the field silently dropped.

### 4.4 Authentication

- Local auth as today, with an organisation-level "require TOTP" policy (a `security.require_totp` setting exists for the single org).
- **OIDC**: discovery, authorisation-code flow with PKCE, ID-token verification, group claim → role mapping, per §3.3.
- **SAML and LDAP** per §3.3 and the cut line in §9.
- Session listing and revocation for the user ("sign out everywhere") and for admins over their organisation.
- Every authentication event — success, failure, TOTP challenge, lockout, IdP login, logout, key creation and use-from-new-IP if cheap — goes to the audit log.

### 4.5 Audit Log

- **Every mutating API call writes one audit row in the same transaction as the change**, so a change without an audit row cannot commit. `actor_type`, `actor_id`, `action` (the event vocabulary extended with configuration verbs, per the column comment), `entity_type`, `entity_id`, before/after `changes` with **secret fields elided at write time**, `ip`, `user_agent`, `request_id`.
- Because the dashboard uses only `/api/v1`, auditing at the API layer audits the dashboard for free. System actors (scheduler, auto-open, auto-resolve, retention) write with `actor_type = system`.
- Browse, filter (actor, entity, action, time range) and export (CSV, JSON) — server-side paginated like everything else; an organisation's audit log at five years is a large table.
- **Never pruned by retention**, as `rollup.go` already guarantees. An organisation that must prune for data-protection reasons gets an explicit, audited, owner-only action, not a retention knob.

### 4.6 Per-Client White-Labelling

- Brand profiles become organisation-bound (or client-bound, per §3.1), so an agency's Client A and Client B are branded separately and neither sees the other's profile.
- **Status pages may reference a brand profile.** Phase 2 deferred this because replacing the inline branding columns would break a shipped schema. The additive form — a nullable `brand_profile_id` that, when set, takes precedence over the inline fields — is not breaking, and closes the three-places-for-branding duplication the Phase 2 plan documented rather than discovered.
- A client-facing login, if §3.1 decides clients log in, shows the client's brand.

### 4.7 Automatic Incidents

- Incidents open automatically per the policy §3.4 decides, with `auto_opened = true` and `detected_at` set **from the first failing check, not the transition time**, because time-to-detect is the one figure that cannot be reconstructed afterwards (per the comment in `incident.go`). Post-mortems written in Phase 2 already read these fields and will start showing real MTTD without change.
- **Acknowledgement** (`POST /incidents/{id}/acknowledge`, already in the spec) stamps `acknowledged_at` and `acknowledged_by`, stops escalation, and appears in the timeline. Acknowledgement is also possible from the notification itself where the channel allows it (e.g. a signed link in email or Slack), without a dashboard login.
- **Assignment** to a user or team; the assignee is notified.
- **Threaded timeline**: human updates, system events (opened, acknowledged, escalated, paged whom, on which channel, delivered or failed, monitor recovered, auto-resolved) in one ordered list.
- **Status page linkage** as today; automatic incidents link to status pages only by explicit policy — a customer-facing page must never announce an internal check failing by default.
- **Auto-resolve** per §3.4, recorded in the timeline with the reason.

### 4.8 Alert Routing: Deduplication, Grouping, Quiet Hours, Business Hours

- **Deduplication**: one incident, one page per escalation step — not one per failing check or per retry.
- **Grouping**: failures that share a dependency root, group or tag within a window collapse into one incident per §3.4. Dependency-aware suppression already exists at the monitor level; grouping is its counterpart at the incident level.
- **Quiet hours**: per-user and per-policy windows during which non-urgent notifications are held (not dropped) and delivered as a digest afterwards. Urgent pages are never held; the definition of urgent is part of the policy, stated in the UI.
- **Business-hours routing** via the `restriction` already drafted on schedule rotations (weekdays, start and end time), evaluated in the schedule's timezone.

### 4.9 On-Call and Escalation

- **Schedules** with one or more layered rotations (daily/weekly/biweekly/custom, handoff time, timezone), **overrides** (a person covers a range), and **holiday calendars** per §3.5. `GET /schedules/{id}/on-call` answers "who is on call now and at time T".
- A **rendered calendar view** of the next several weeks, because nobody trusts a rotation they cannot see.
- **Escalation policies**: ordered steps, each with a delay and targets (user, team, schedule, notification channel — as drafted), repeated `repeat_count` times, stopped by acknowledgement.
- **Escalation state is durable**: pending steps are rows, not timers; a restart resumes them, and a step that fires late says so in the timeline.
- **Personal notification rules**: each user chooses which of their contact methods (email, SMS, Slack, Telegram, push via ntfy/Pushover/Gotify, …) are used for pages versus updates. Existing channel providers are reused; no new transport is invented.
- A **test page** a user can send to themselves, because the first time someone finds out their phone number is wrong must not be 3am.

---

## 5. Architecture in Phase 3

- **Tenancy is a context value, not a parameter anyone remembers to pass.** The request resolves an organisation once; the repository interface requires it; tests in §4.1 prove it. This is ADR-003's mitigation, built three phases late.
- **Authorisation stays at the route.** `require(scope, …)` keeps working; scopes come from grants. If §3.2 introduces resource scoping, it is applied in the query (a filter), not after it (a post-filter that breaks pagination and leaks counts).
- **Audit writes share the mutation's transaction.** No asynchronous audit queue: a queue that can drop is a log that can lie.
- **Escalation is a scheduler of rows**, in the same shape as report schedules: durable, idempotent per step, late-not-lost, and **off the check path** — escalation work must not compete with the probe scheduler, exactly as report generation must not.
- **Incident events acquire recipients through routing**, not through a fake monitor on the envelope; the notification dispatcher learns a second resolution path.
- **No new infrastructure.** No Redis, no queue broker, no external scheduler. Solo mode keeps zero required external dependencies; identity-provider libraries are the only dependencies this phase is expected to consider, each justified in its PR.
- **`org_id` on every new table from its first migration**, as always.

---

## 6. API and the Spec-First Rule

Unchanged and not softened: **the OpenAPI spec is extended and reviewed before any handler is written**, and agents do not add, rename or reshape endpoints ([AGENTS.md](../../AGENTS.md) § 4).

Phase 3's spec work is two different jobs, and they should be two drafts:

1. **Reconciliation** of the 20 drafted `x-cairn-phase: 3` operations (users, teams, schedules, escalation policies, acknowledge) against the ADRs of §3 once they exist — the same exercise [phase-2-reports.yaml](../api/drafts/phase-2-reports.yaml) performed. The drafted `Incident` schema already describes `auto_opened` and MTTA as Phase 3.
2. **Invention**, which Phase 2 largely avoided and Phase 3 cannot: organisations and memberships, invitations, organisation switching, the audit log, SSO configuration and callbacks, custom roles, holiday calendars, quiet hours, grouping policy, personal notification rules, sessions list and revoke. This is new public surface; it is drafted by a human in `docs/api/drafts/phase-3-*.yaml` and reviewed before merge.

**Compatibility is the constraint most likely to be missed.** Every Phase 1 and 2 operation is shipped and frozen ([COMPATIBILITY.md](../api/COMPATIBILITY.md)). Tenancy must be additive: a single-org install, and every existing API key, must behave exactly as before — no new required header, no new required field, no path gaining an `/orgs/{id}` prefix. A key already carries its organisation; a session selects one. If the tenancy ADR concludes otherwise, that is `/api/v2`, and it should be argued as such.

Contract tests select `x-cairn-phase: 3` with the per-phase floor Phase 2 introduced. Go and TypeScript clients are regenerated in the same PR as the spec change.

---

## 7. Milestone Breakdown

### Month 7 — Decisions, and the scoping path
- The five ADRs of §3 written, discussed and accepted; ADR-009 accepted or rejected.
- **The single tenant-scoping path built and every `SentinelOrgID` call site moved onto it**, with no behaviour change and the 5,000-monitor gate green throughout. This lands file by file, not as one sweep ([AGENTS.md](../../AGENTS.md) rule 2).
- The two-organisation isolation suite standing, initially over the resources that exist.
- Spec drafts for reconciliation and invention opened for review.
- **Checkpoint:** a second organisation created by SQL can be logged into by a second user who sees nothing of the first — proven by the isolation suite, not by clicking.

### Month 8 — People, permissions, audit
- Memberships, invitations, user management, teams; organisation switcher (hidden for single-membership users).
- Roles as grants; custom roles; the billing and editor gaps closed.
- Audit log written transactionally for every mutation and authentication event; browse, filter, export.
- Session list and revocation.
- **Checkpoint:** an owner invites an editor and a viewer; each can do exactly what their role says and nothing else, and every step appears in the audit log with the right actor.

### Month 9 — Identity providers, white-labelling, incidents
- OIDC end to end against at least two real providers (e.g. Keycloak in CI, and one hosted IdP by hand).
- SAML and LDAP per the cut line (§9).
- Organisation-bound brand profiles; status pages able to reference one.
- Automatic incidents, acknowledgement, assignment, the threaded timeline, auto-resolve; incident events routed to recipients.
- **Checkpoint:** a failing check opens an incident with `detected_at` from the first failure, the on-call person acknowledges it from a notification, and it auto-resolves on recovery — each step in the timeline.

### Month 10 — On-call
- Schedules, rotations, overrides, holidays, "who is on call", calendar view.
- Escalation policies with durable state; personal notification rules; test page.
- Deduplication, grouping, quiet hours, business-hours routing.
- **Checkpoint:** a two-layer weekly rotation across a DST change hands off at the right local time, escalates to the secondary when the primary does not acknowledge, and a restart mid-escalation resumes rather than restarts or forgets.

### Month 11 — Hardening, scale, release
- Load gate extended (§8).
- Failure paths demonstrated, not reasoned about: IdP down (local owner recovery works); a page to a failing channel escalates rather than stalls; a restart mid-escalation; an invitation accepted twice; the last owner demoted (refused).
- Docs (§10 of the checklist).
- Release engineering: tagged release, images, checksums, SBOM, changelog, announcement.
- Buffer. **Features get cut before the release date or the quality gates do.**

---

## 8. Scale in Phase 3

The headline promise does not get a pass because this phase is about people:

- **The 5,000-monitor gate stays green with tenancy enforced**, including at least two organisations — the extra `org_id` predicate on every query must use the `(org_id, …)` composite indexes ADR-003 put in place, and the gate is what proves it.
- **The existing total-partition scenario becomes the grouping test.** A partition that marks every monitor down within two intervals must open a **bounded** number of incidents and send a **bounded** number of pages (the bound is set by the §3.4 ADR), with none dropped — `cairn_alerts_dropped_total` stays at zero as it does today.
- **Escalation does not delay checks**: with escalation running during the partition, check lateness stays within the same tolerance the report-burst phase uses.
- The audit log at volume: list and filter over an organisation with millions of audit rows answers within the list-endpoint budget.

These are CI configuration under [AGENTS.md](../../AGENTS.md) rule 7, written and reviewed by a human.

---

## 9. Exit Criteria (all must be true)

- [ ] The implementation matches the accepted ADRs of §3.
- [ ] Every tenant-scoped query goes through the single scoping path; `SentinelOrgID` survives only where §4.1 permits.
- [ ] The two-organisation isolation suite covers every tenant-scoped resource, the live channel, reports, share links and exports, and is green.
- [ ] A single-organisation install upgraded from 1.1.x behaves identically: same first-run experience, same API, every existing API key working unchanged.
- [ ] Users, invitations, teams, and the six roles enforced; custom roles; the last owner protected.
- [ ] OIDC login end to end; SAML and LDAP shipped or explicitly cut and moved on the roadmap.
- [ ] Every mutation and authentication event audited in the same transaction, with secrets elided; the audit log browsable, filterable, exportable, and never pruned by retention.
- [ ] Brand profiles bound per organisation (or client); status pages able to use one.
- [ ] Incidents open automatically with a truthful `detected_at`; acknowledge, assign, timeline, status page linkage, auto-resolve.
- [ ] Incident events reach people through routing, not only outbound webhooks.
- [ ] Schedules, rotations, overrides, holiday calendars, escalation policies, business-hours routing, deduplication, grouping, quiet hours.
- [ ] Escalation survives a restart; a late step is visible as late.
- [ ] Spec-first honoured: drafts reviewed before handlers; contract tests green for phase 3; clients regenerated.
- [ ] **The 5,000-monitor gate green with tenancy enforced, and a total partition producing a bounded number of incidents and pages.**
- [ ] Docs shipped with the release.
- [ ] **The test that matters:** a 50-person engineering org runs production on-call from this and nothing else.

### The cut line

Named now so it is not drawn in month 11. If the phase runs long, cut in this order — each is independently useful to leave out and cheap to add later:

1. **LDAP** — the least-requested of the three and the one OIDC-fronted directories (Keycloak, Authentik, Azure AD) already cover.
2. **SAML** — a real enterprise requirement, and the one with the heaviest security and dependency cost; OIDC covers most IdPs that also speak SAML.
3. **Holiday calendar import** — overrides express a holiday by hand.
4. **Custom roles** — the six built-ins cover the exit condition.

**Not cuttable**: the scoping path and isolation suite, RBAC on the built-in roles, the audit log, automatic incidents with acknowledgement, schedules, and durable escalation. Without any one of them the exit condition is false.

---

## 10. Success Metric

Phase 3 release target, from the roadmap: **5,000 GitHub stars; 3+ regular external committers; first 100-person org in production.**

The third number is the real one; the second is the one that keeps the project alive. A phase this security-heavy is also the best argument yet for recruiting reviewers who can defend auth code at 3am.

---

## 11. Phase 3 Risks

| Risk | Severity | Mitigation |
|---|---|---|
| **A cross-tenant data leak** | Critical | Application-level filtering is the only enforcement this phase (§2.2). One scoping path the compiler enforces, the two-organisation property suite over every resource, and no user-visible multi-org feature until both exist |
| The tenancy refactor regresses the 5,000-monitor gate | High | It lands incrementally with the gate green at each step; every new predicate uses an `(org_id, …)` index, and the query-plan assertion precedent from Phase 1 applies |
| Security code without a human who can defend it | Critical | Rule 8 applies in full to identity, sessions, invitations and RBAC. Phase 2's waivers are not precedent for this phase. Recruiting a second reviewer for auth is a P0 deliverable, not a hope |
| SAML signature handling | High | Never hand-rolled; a maintained library justified under the dependency policy, or SAML is cut (§9) |
| Solo users see enterprise complexity | High | Every new screen is hidden until the install has a second user, a second organisation, or the feature is asked for; the sixty-second first-run is tested every release as before |
| Alert storms during a partition | High | Grouping and deduplication designed first (§3.4) and gated by the existing partition scenario (§8) |
| Escalation lost on restart | High | Durable rows, not timers; tested by killing the process mid-escalation |
| DST and timezone bugs in rotations | Medium | Semantics fixed in the §3.5 ADR before code; table-driven tests over spring-forward and fall-back nights in several zones |
| Breaking a shipped API while adding tenancy | High | Additive only (§6); a single-org install and existing keys unchanged, asserted by the contract tests |
| Double paging for teams already on PagerDuty/Opsgenie | Medium | Dedup key relationship decided in §3.4; native escalation off by default where an external pager is configured |
| Scope creep into Phase 4 (HA, multi-region, Postgres) | Medium | The scope table in §1 is the contract |
| Single-maintainer burnout | Critical | Carried over and unchanged; this phase makes it worse, which is why committer recruitment is named in §10 |

---

## 12. Deliberately Not in Phase 3

Named so they are refused consistently rather than argued each time:

- **Postgres, Timescale, RLS, HA, multi-region probes** — Phase 4. Isolation in this phase is application-level on SQLite.
- **A silo (database-per-organisation) option** — Phase 4 at the earliest, and only if an enterprise need appears; managed hosting is per-instance and does not need it (ADR-003).
- **SCIM provisioning** — after SSO has real users.
- **Mobile apps and push acknowledgement** — Phase 5. Acknowledgement from existing channels covers the exit condition.
- **AI-drafted incident summaries** — Phase 5.
- **A paid tier.** RBAC, SSO and the audit log ship in the open source build, as the roadmap states. This is not revisited here.
