# Phase 3 — working checklist

Every deliverable in [PHASE-3-PLAN.md](PHASE-3-PLAN.md), as a list that can be
ticked. The plan is the contract and does not change; this is the tracker. The
rule the Phase 1 and Phase 2 lists ran under applies unchanged and is the reason
they stayed honest: **a box needs a demonstrated run behind it.** "The code is
written" is not one, and neither is "the configuration was reviewed".

**Status: 2026-10-21. 1 of 109.** v1.1.0 is tagged and Phase 2 is closed. The one
tick is a decision rather than work: ADR-009 is accepted into this phase. The
five Phase 3 ADRs are **drafted and `Proposed`**, which ticks nothing — their
boxes close on acceptance. No code has been written. The plan's §2.2 is the
starting position, and
it is less prepared than the Phase 2 starting position was: the tenancy column
exists everywhere, but **the single scoping path ADR-003 asked for was never
built**. `model.SentinelOrgID` has 156 references across 53 files. So the
first month is paying that debt, not building features.

> ### Before anything is ticked: rule 8, and why this phase differs
>
> Phase 2's checklist records explicit maintainer waivers of
> [AGENTS.md](../../AGENTS.md) rules 1, 2, 7 and 8. **None of them carries over.**
> A waiver is given in a session for named work, and this file starts with none.
>
> This matters more here than it did in Phase 2. The share links and the S3
> signer were security-adjacent. Most of this phase *is* security code:
> identity providers, sessions, invitations, role enforcement and tenant
> isolation. Rule 8 exists for exactly this. **If a waiver is given for any
> box below, record it at the head of this file the day it is given**, with the
> Phase 2 wording's distinction intact: a tick means the code works and has
> tests behind it, and it does not mean a person has read it.

---

## Decisions and specification

These are human-authored ([AGENTS.md](../../AGENTS.md) rule 3). The plan
states the questions and does not answer them.

**All five are drafted and `Proposed` as of 2026-10-21. None is accepted.** Each was drafted by an agent under an explicit maintainer waiver of rule 3, with every decision inside it answered by the maintainer — each ADR says so at its head. A box is ticked when its ADR is **accepted in its own PR**, not when it exists.

- [ ] [ADR-010](../adr/010-tenancy-isolation-and-membership.md) — tenancy (plan §3.1). Flat organisations, pool model, instance-wide accounts with memberships, an instance-admin flag, SMTP/retention/probes instance-wide, instance-unique public slugs and push tokens, active org on the session, and **a scoped store handle as the enforcement mechanism**
- [ ] [ADR-011](../adr/011-authorisation-roles-and-grants.md) — authorisation (§3.2). Org-wide role scope (no team/tag narrowing), built-ins read-only in code with custom roles as per-org rows, `billing` reserved and not selectable, `editor` gains the report scopes, **API keys org-owned and unaffected by their creator's role**, and two-step organisation deletion
- [ ] [ADR-012](../adr/012-external-identity.md) — external identity (§3.3). OIDC and SAML in, **LDAP cut**, both library-backed with the SAML library a gate on the feature, invite-only provisioning linking only on a verified email in an allowed domain, optional group→role mapping re-applied each login, and instance admins keeping local login as the named break-glass path. **Rule 8 was not waived: read the warning at its head**
- [ ] [ADR-013](../adr/013-incident-automation-and-alert-routing.md) — incidents and routing (§3.4). Auto-open off by default and policy-gated, triggered on the existing `down` transition, `detected_at` from the first failing check, correlation by dependency root then group then selection, auto-resolve on all-up plus a quiet period, escalation as a second delivery path with monitor alerting unchanged, and external pagers warned about rather than suppressed
- [ ] [ADR-014](../adr/014-on-call-scheduling-semantics.md) — on-call semantics (§3.5). Local wall-clock handoffs with the two DST edge cases fixed, overrides above all layers with overlaps refused at creation, a mandatory policy fallback for coverage gaps plus a 28-day gap warning, manually entered holiday dates (no iCal, no RRULE), escalation as durable rows that resume late-and-visible after a restart, and quiet hours that hold rather than drop
- [x] [ADR-009](../adr/009-pagination-sort-key.md) decided (§3.6) — **accepted into Phase 3 on 2026-10-21, to land before the tenancy refactor** so the monitors table is rebuilt once. Its Status line changes in its own acceptance PR, which is the box below
- [ ] ADR-009's acceptance PR merged, and its migration landed ahead of the scoping work
- [ ] Spec draft 1, **reconciliation**: the 20 `x-cairn-phase: 3` operations checked against the ADRs, in `docs/api/drafts/`, reviewed, then merged
- [ ] Spec draft 2, **invention**: organisations, memberships, invitations, org switching, audit log, SSO config and callbacks, custom roles, holiday calendars, quiet hours, grouping policy, personal notification rules, session list/revoke. Human-drafted, reviewed, merged
- [ ] Compatibility confirmed against [COMPATIBILITY.md](../api/COMPATIBILITY.md): every change is additive to shipped surface. No new required header, field or path prefix for a single-org install

## Tenancy foundations

- [ ] One resolver: the active organisation comes from the session's membership or the API key's `org_id` and is carried in request context. `Principal` carries it; `resolveAPIKey` stops ignoring `key.OrgID`
- [ ] The repository interface requires the organisation for every tenant-scoped read and write, so an unscoped query does not compile rather than failing review
- [ ] Every `SentinelOrgID` call site migrated, file by file, with the 5,000-monitor gate green after each step. `SentinelOrgID` survives only in the seeding migration and the default-org resolver
- [ ] The unfiltered stores scoped: `rollup.go`, `heartbeats.go`, `report_shares.go`, and the mostly-unfiltered `identity.go`, `incidents.go`, `statuspages.go`, `taxonomy.go`, `maintenance.go`, `monitors.go`. Each new predicate is shown to use an `(org_id, …)` index by the query-plan assertion Phase 1 introduced
- [ ] Background work scoped: rollups, retention, report runs and schedules, maintenance sweep, orphan sweeper, importer
- [ ] Live channel ([ADR-004](../adr/004-ui-state-synchronisation.md)) refuses a subscription id from another organisation
- [ ] The instance-global list from the tenancy ADR implemented and documented (custom domains stay globally unique)
- [ ] **Two-organisation isolation suite**: for every tenant-scoped resource, nothing in org A can list, get, update, delete, subscribe to, report on, share, export or search anything in org B. Floor-checked so it cannot pass vacuously, and extended by every later box that adds a resource
- [ ] A second organisation can be created; a multi-membership user can switch; the switcher does not render for one membership
- [ ] Upgrade path: a 1.1.x database migrates with no behaviour change. Demonstrated on a real 1.1.0 database, not a fresh one

## Users, invitations and teams

- [ ] Memberships (or the tenancy ADR's alternative), with migration of the existing owner
- [ ] Invitations: single-use, expiring, stored hashed; accept creates or links an account; resend and revoke
- [ ] `createUser`, `updateUser`, `deleteUser` per the reconciled spec
- [ ] The last owner cannot be removed, demoted or deactivated. Tested
- [ ] Deactivation revokes sessions and API keys immediately. Demonstrated against a live session
- [ ] Teams and team membership (`/teams` CRUD)

## Roles and permissions

- [ ] `ScopesFor` replaced by grants stored as data. Every existing `require(scope, …)` guard unchanged in shape
- [ ] The six built-in roles enforced for sessions and keys, each with a test proving what it can and cannot do, across every scope
- [ ] `billing` given a defined meaning (currently no scopes); `editor` (or the ADR's choice) given report and brand-profile scopes, which closes Phase 2's "authoring by non-owners" deferral
- [ ] Custom roles as named scope sets, with the dashboard describing them in the API reference's words
- [ ] Denials stay `403 insufficient-scope` naming the scope. Never a silent drop, and never a misleading `404`
- [ ] Key/role interaction per the ADR, tested for the case where a creator is demoted after minting a key
- [ ] Resource scoping, **only if** the authorisation ADR requires it, applied in the query and not as a post-filter, and measured by the load gate

## Authentication

- [ ] Organisation-level "require TOTP" policy (extends `security.require_totp`, which exists for the single org today)
- [ ] OIDC: discovery, auth-code + PKCE, ID-token verification, group → role mapping, per the ADR
- [ ] OIDC demonstrated against Keycloak in CI and one hosted IdP by hand
- [ ] SAML, per the ADR and the cut line. A maintained library justified under the dependency policy, never hand-rolled XML-DSig
- [ ] LDAP, per the ADR and the cut line
- [ ] Owner recovery with the IdP down, demonstrated
- [ ] Session list and revoke, for the user ("sign out everywhere") and for admins over their organisation
- [ ] Login rate limiting holds per organisation and per IdP path, not only for local login

## Audit log

- [ ] An audit writer that shares the mutation's transaction, so a change without its audit row cannot commit. Tested by failing the audit insert
- [ ] Every mutating `/api/v1` operation audited. A test enumerates the spec's mutating operations and fails on any that write no row
- [ ] Secret fields elided at write time. A test seeds every sealed field and asserts that none reaches `changes`
- [ ] Authentication events audited: login success and failure, TOTP, lockout, IdP login, logout, key created, key revoked
- [ ] System actors (auto-open, auto-resolve, escalation, retention, scheduler) written as `actor_type = system`
- [ ] Browse, filter and export (CSV, JSON), server-side paginated
- [ ] Never pruned by retention (already guaranteed in `rollup.go`; asserted by a test). Explicit owner-only prune action, itself audited
- [ ] Audit list stays within the list-endpoint budget over an organisation with millions of rows

## White-labelling

- [ ] Brand profiles bound per organisation (or client, per the tenancy ADR); isolation suite covers them
- [ ] Status pages may reference a brand profile: nullable `brand_profile_id` taking precedence over inline fields. Additive, with no shipped field changed
- [ ] The "branding lives in three places" note in the Phase 2 docs updated to what is now true
- [ ] Client-branded login, **only if** the tenancy ADR lets clients log in

## Incidents

- [ ] Automatic opening per the routing ADR, with `auto_opened = true` and `detected_at` from the **first failing check**, not the transition
- [ ] Phase 2 post-mortems show real MTTD for auto-opened incidents with no change to `internal/report`. Demonstrated on a generated post-mortem
- [ ] `POST /incidents/{id}/acknowledge` routed; stamps `acknowledged_at`/`acknowledged_by`; stops escalation
- [ ] Acknowledge from a notification (signed link) without a dashboard login. Security code, so rule 8 applies
- [ ] Assignment to a user or team, with the assignee notified
- [ ] Threaded timeline including system events: opened, acknowledged, escalated, paged whom and on which channel, delivered or failed, recovered, auto-resolved
- [ ] Status page linkage for automatic incidents only by explicit policy. A public page never announces an internal failure by default
- [ ] Auto-resolve per the ADR, never overriding a human's manual state change, with the reason in the timeline
- [ ] **Incident events reach people**: the notification dispatcher gains a routing path for events with no monitor. Today they reach only outbound webhooks

## Alert routing

- [ ] Deduplication: one incident and one page per escalation step, not one per failing check or retry
- [ ] Grouping by dependency root / group / tag within a window, per the ADR
- [ ] Quiet hours: non-urgent notifications held, not dropped, and delivered as a digest. Urgent pages never held
- [ ] Business-hours routing via the rotation `restriction`, in the schedule's timezone
- [ ] Native escalation and existing PagerDuty/Opsgenie integrations do not double-page. Demonstrated with both configured

## On-call and escalation

- [ ] Schedules with layered rotations (daily/weekly/biweekly/custom, handoff time, timezone)
- [ ] Overrides, with precedence per the ADR
- [ ] Holiday calendars, within the ADR's bound
- [ ] `GET /schedules/{id}/on-call` answers for now and for time T
- [ ] Calendar view of the coming weeks
- [ ] DST tests: spring-forward and fall-back nights in at least three zones, including a handoff inside the skipped hour
- [ ] Escalation policies: ordered steps, delays, targets (user/team/schedule/channel), `repeat_count`, stopped by acknowledgement
- [ ] **Escalation state is durable.** Kill the process mid-escalation, restart, and the next step fires; a late step is marked late in the timeline
- [ ] Escalation runs off the check path
- [ ] Personal notification rules: per-user contact methods for pages vs updates, reusing existing providers
- [ ] Test page to self

## REST API

- [ ] Every Phase 3 operation implemented against the merged spec, with none added outside it
- [ ] Contract tests green for `x-cairn-phase: 3` in both directions, with the per-phase floor
- [ ] Go and TypeScript clients regenerated in the same PR as each spec change
- [ ] Every Phase 1 and 2 contract test still green, with existing API keys unchanged

## UI

- [ ] Progressive disclosure: none of org switcher, roles, teams, audit, on-call or escalation renders on a single-user install until asked for. **The sixty-second first run timed again** on a fresh install
- [ ] Members and invitations screen
- [ ] Roles and custom-role editor
- [ ] Teams screen
- [ ] Audit log screen (filter, export)
- [ ] SSO configuration screen
- [ ] Incident screen: acknowledge, assign, timeline
- [ ] Schedules with calendar view; overrides
- [ ] Escalation policy editor
- [ ] Personal notification rules and test page
- [ ] **Clicked, not only built.** Phase 2 closed two screens with "the data paths were driven but nobody clicked the controls" because the repo has no browser-driver tooling. Either that tooling arrives (a dev-only dependency, justified), or each screen is clicked through by a human and recorded here

## Docs

- [ ] Teams and roles guide: what each role can do, in a table generated from or checked against the grants
- [ ] SSO setup guides per provider actually shipped, with every flag and URL tested
- [ ] Audit log reference: what is recorded, what is elided, and that it is never pruned
- [ ] Incidents guide: auto-open policy, acknowledgement, auto-resolve
- [ ] On-call guide: schedules, overrides, DST behaviour stated plainly, escalation, quiet hours
- [ ] Tenancy and isolation page: what is isolated, what is instance-global, and that enforcement is application-level on SQLite (no RLS until Phase 4). **Written before any compliance claim is made to anyone**
- [ ] Upgrade notes from 1.1.x
- [ ] [sla-methodology.md](../guides/sla-methodology.md) updated (it says nothing raises incidents automatically before Phase 3)
- [ ] ROADMAP.md "Where we are" and AGENTS.md "Status" updated to Phase 3

## Quality gates and scale

- [ ] 5,000-monitor gate green with tenancy enforced and at least two organisations
- [ ] Total-partition scenario: a bounded number of incidents and pages (bound set by the routing ADR), `cairn_alerts_dropped_total` at zero
- [ ] Escalation during the partition leaves check lateness within the report-burst tolerance
- [ ] Failure paths demonstrated: IdP down; a page to a failing channel escalates; restart mid-escalation; invitation accepted twice; last-owner demotion refused
- [ ] **Load-gate changes are CI configuration under rule 7**, written or reviewed by a human, and recorded as such

## Release

- [ ] Tagged release, images, checksums, SBOM, changelog, announcement. The tag is a maintainer's decision (rule 6). It also freezes `/api/v1` for every Phase 3 operation
- [ ] Buffer. **Features get cut before the release date or the quality gates do.** Cut in the plan's order (§9): LDAP, SAML, holiday import, custom roles. Every cut is recorded here and moved on the roadmap

---

## What to do next, and why in this order

1. **Write the tenancy ADR first, and settle ADR-009 alongside it.** Every
   later box depends on "a request has exactly one active organisation" meaning
   something specific. ADR-009 goes with it because both touch the monitors
   table and its indexes, and opening that table twice is how one of them gets
   decided by accident.

2. **Then build the scoping path and migrate the call sites, before anything a
   user can see.** It has no user-visible payoff, which is exactly why it gets
   skipped, and ADR-003 already recorded what skipping it costs. The isolation
   suite stands up with it rather than after it, the same way Phase 2's golden
   report stood up with the first renderer.

3. **Audit log next.** It has the fewest open questions (the schema exists and
   is documented), every later feature mutates something, and a feature built
   before the audit writer is a feature that has to be revisited.

4. **Authorisation and users, then identity providers.** Local users with real
   roles prove the permission model. OIDC then adds a way in, not a new model.

5. **Incidents and routing before on-call.** A schedule with nobody to page
   is useless, and today incident events reach no channel at all. Fix the
   routing, then build who gets routed to.

6. **The routing ADR and the partition gate go together.** The bound on
   incidents and pages during a total partition is the grouping design's
   acceptance test, so write the number down in the ADR before the code
   exists.

### Three things to decide before they decide themselves

**Whether any rule 8 waiver is given for this phase, and for what.** Phase 2
showed that agent-built security code can work and pass tests while nobody has
read it. The waiver notes said so honestly. A phase whose exit condition is
"run production on-call" needs a person who can defend the login path.
Decide that up front, and look for that person as a committer, the roadmap's
own Phase 3 metric.

**SAML's dependency.** No part of it should be written in-tree. If no library
passes the dependency policy, SAML is cut, and the cut is recorded, not
discovered in month 11.

**What `billing` means in an install with no billing.** The role is in the
schema's `CHECK`, the spec's `UserRole` enum and the roadmap. It currently
grants nothing. Either it gets a meaning (reports and usage read-only, say)
or it is documented as reserved. Leaving it granting nothing while it is
offered in a role picker is the one option that is plainly wrong.
