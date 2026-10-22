# ADR-010: Tenancy — Flat Organisations, Instance-Wide Accounts with Memberships, and Isolation Enforced by a Scoped Store

- **Status:** Accepted
- **Date:** 2026-09-22
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** **Completes [ADR-003](003-tenancy-model.md)** and does not supersede it. ADR-003 decided the inert `org_id` column and explicitly deferred four questions to Phase 3 planning: pool versus silo, flat versus org + client hierarchy, the enforcement mechanism, and single versus multiple organisation membership. This ADR answers all four. ADR-003's decisions stand unmodified.

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

> **How this ADR was written, recorded for the repository's history.**
> [AGENTS.md](../../AGENTS.md) § *What agents may not do* (3) says agents do not
> draft ADRs. The project maintainer explicitly waived that restriction for this
> document on 2026-09-22, following the precedent at
> [data model §11.6](../data-model/README.md#116-secrets-at-rest). **The
> decisions are the maintainer's.** Each of the eight choices below was put to
> the maintainer as a question with its alternatives and answered by them. The
> prose, the evidence gathering and the consequences analysis were drafted by an
> agent. That is why the Alternatives section records the options exactly as
> they were presented. A reviewer should check the reasoning, not only the
> conclusions, before this moves to Accepted.

## Context

Phase 3 ([PHASE-3-PLAN.md](../plans/PHASE-3-PLAN.md)) must turn a
single-owner install into one that several people, and for the agency/MSP
persona several separate client businesses, can use. The roadmap requires
"organisations and workspaces with hard data isolation". ADR-003 prepared the
schema for this and deferred the design. This is the design.

**The starting position is less prepared than ADR-003 intended.** ADR-003's
mitigation for its own main risk was to "route tenant scoping through one shared
code path (even though it resolves to the sentinel org for two phases), so
Phase 3 changes what that path returns rather than rewriting every call site".
That path was not built:

- `model.SentinelOrgID` is referenced **156 times across 53 files** under
  `internal/`, most of them inside `internal/store/sqlite`.
- The API server holds a hardcoded `orgID` field set to the sentinel
  ([`internal/api/server.go`](../../internal/api/server.go)).
- The authenticated `Principal` carries no organisation
  ([`internal/api/auth.go`](../../internal/api/auth.go)), and `resolveAPIKey`
  ignores the `org_id` the key row already stores.
- Most repository methods (`GetMonitor(ctx, id)`, `GetBrandProfile(ctx, id)`,
  `ListAPIKeys(…)`) take no organisation. About 76 of roughly 265 SQL statements
  in the SQLite store filter on `org_id`. `rollup.go`, `heartbeats.go` and
  `report_shares.go` filter on none.

ADR-003 named exactly this as the thing that "becomes expensive to reverse".
So the enforcement decision below matters more than the others. It has to fix
the existing code as well as govern new code.

**Three things about the current schema also bear on the design**, and each
became one of the questions below:

1. **Accounts are per-organisation.** `users` carries `org_id` and `role`
   directly, with `UNIQUE (org_id, email)`
   ([`0002_identity.sql`](../../migrations/sqlite/0002_identity.sql)). A person
   in three organisations would need three accounts.
2. **Settings are one row per organisation**, covering general, appearance,
   retention, SMTP, monitoring, security and telemetry. Some of these describe
   the machine the operator runs, not any tenant's preferences.
3. **Three unauthenticated lookups resolve by a value that is only unique
   within an organisation.**
   - The public status page route is `/api/v1/public/status-pages/{slug}`, but
     `idx_status_pages_slug` is `UNIQUE (org_id, slug)`. With a second
     organisation, the URL is ambiguous.
   - The push heartbeat route `/api/v1/push/{pushToken}` resolves through
     `idx_monitors_push_token`, also `UNIQUE (org_id, …)`.
   - By contrast, custom domains (`idx_status_pages_domain`), report share
     tokens and subscriber unsubscribe tokens are already unique across the
     instance, because their requests arrive with nothing else to route on.

**Enforcement has only one layer in this phase.** There is no Postgres backend
yet ([`migrations/migrations.go`](../../migrations/migrations.go): "ADR-002 defers
the scaled path to Phase 4"), so ADR-003's RLS spike is moot until then.
Application-level scoping is the **only** enforcement Phase 3 has, not the
primary one of two. Whatever mechanism is chosen, its test coverage is the
isolation guarantee.

## Decision

**1. Hierarchy: flat organisations.** An organisation is the one tenancy
boundary, and there is no nested client level. An agency models each client as
its own organisation, and agency staff are members of each client organisation
they work on. **Every request has exactly one active organisation.** A
cross-organisation view (for instance, an agency-wide dashboard over all its
clients) is not part of Phase 3. If it comes later, it must be a read-only
aggregate built over this model, not a relaxation of it.

**2. Pool model.** All organisations share one database and one set of tables,
separated by `org_id`, as ADR-003 set up. There is no silo (database per
organisation) option in Phase 3. ADR-003 has already recorded that managed
hosting uses per-customer instances and does not need one.

**3. Identity: instance-wide accounts with memberships.** A user is one account
across the instance, with email unique instance-wide. Membership in an
organisation is a row in a new `memberships` table keyed by `(user_id, org_id)`,
and **the role lives on the membership**, so a person can be admin in one
organisation and viewer in another. `users.org_id` and `users.role` are
migrated into the existing owner's single membership and then removed. That is a
table rebuild, following the `0007_import_vocabulary` precedent. The owner's
login, password, TOTP secret and recovery codes are unchanged. What a role
permits is the business of the authorisation ADR, not this one.

**4. Instance administration is a separate flag, not a role.** A boolean
`is_instance_admin` on the user, set for the account created by first-run setup.
An instance admin manages instance-wide settings (decision 6) and creates
organisations. Organisation roles govern everything inside an organisation.
Being an instance admin grants **no** implicit access to an organisation's data.
An instance admin who wants to see inside an organisation must be a member of it.
This keeps "operator of the box" distinct from "owner of a client's data", and it
is what lets an agency host a client whose monitors the agency's server operator
does not read by default.

**5. Only instance admins create organisations.** There is no self-service
creation and no sign-up. This fits self-hosting, where the operator controls who
has a tenant.

**6. What is instance-wide.** These are owned by the operator, managed by
instance admins, and shared by every organisation:

| Item | Why |
|---|---|
| **SMTP relay** | One outbound mail path for the server. Organisations send through it. |
| **Retention** | Rollup tiers and raw retention are about the disk the operator owns. Per-organisation retention would let one tenant's history consume another's disk. |
| **Probes** | The embedded probe, and Phase 4's remote probes, are shared infrastructure. Organisations pin monitors to them. `probes.org_id` remains, pointing at the default organisation, and is documented as carrying no meaning. |
| **Status page custom domains** | Already unique across the instance, and unchanged. |
| **The root encryption key and `encryption_keys`** | Already instance-level, and unchanged. |

SMTP and retention move out of the per-organisation `settings` row into a
single-row instance settings table. Their per-organisation columns become inert.
**General, appearance, monitoring, security and telemetry settings stay
per-organisation.** No instance-wide security floor is introduced by this ADR.

**7. Instance-unique public identifiers.** Everything resolved on an
unauthenticated path is unique across the instance, not per organisation:

- Status page slugs: `UNIQUE (org_id, slug)` is replaced by `UNIQUE (slug)`.
- Push tokens: `idx_monitors_push_token` loses its `org_id` prefix.

An existing install has one organisation, so neither migration can meet a
duplicate, and every shipped public URL keeps working. The cost is that
organisations on a shared instance compete for slug names.

**8. The active organisation is stored on the session.**
- A signed-in request's organisation is `sessions.active_org_id`. It defaults to
  the user's only membership, or their most recently used one, and changes
  through an explicit switch operation whose shape the Phase 3 spec draft
  defines.
- An API key's organisation is the `org_id` it already stores, fixed at
  creation.
- There is no organisation header and no organisation segment in `/api/v1`
  paths. **Every shipped operation keeps its exact shape**, which is what
  [COMPATIBILITY.md](../api/COMPATIBILITY.md) requires.

**9. Enforcement: a scoped store handle.** The repository is split in two:

- **A tenant store**, obtained as `store.ForOrg(orgID)` and bound to one
  organisation. Its methods take **no** organisation parameter and apply the
  bound `org_id` to every statement. A handler cannot express a query in another
  organisation, because the handle it holds has no way to name one. The API
  layer builds this handle once per request, from the resolved `Principal`, and
  handlers receive nothing else.
- **A system store** for work that legitimately spans organisations: rollups,
  retention, the probe scheduler and ingest, the report scheduler, maintenance
  sweeps, orphan sweeping, and instance administration. It is a separate,
  explicitly named interface, not reachable from request handlers. Its rows keep
  their `org_id`, and any tenant-visible effect it produces (an event, a
  notification, an incident) carries the originating organisation onward.

`model.SentinelOrgID` survives only in the migration that seeds the default
organisation and in tests. **The sentinel organisation becomes an ordinary
organisation.** It is the one every existing install's data already lives in, and
nothing in the running code refers to it by id. The ADR-004 live channel
resolves a subscription through the tenant store, so an id from another
organisation is refused at the same boundary as any other read.

**10. Isolation is verified as a property.** A two-organisation test suite
asserts, for **every** tenant-scoped resource, that nothing in organisation A can
list, get, update, delete, subscribe to, report on, share, export or search
anything in organisation B. It has a per-resource floor so it cannot pass
vacuously. Because decision 9 is the only enforcement Phase 3 has, this suite
**is** the isolation guarantee. Any new tenant-scoped table ships with its entry
in the suite in the same PR.

## Consequences

**What this makes easy.**

- **Isolation is a compile-time property for request handlers.** The handle
  cannot name another organisation, so the easy mistake (a forgotten
  `WHERE org_id = ?`, or `SentinelOrgID` passed where the principal's
  organisation belonged) cannot be written in a handler at all. Reviewers look
  for system-store use, which is a short and greppable list, instead of auditing
  every query.
- **The API stays exactly compatible.** Single-organisation installs, every
  existing API key, every shipped public URL, and the dashboard's existing calls
  are unchanged. The user who never creates a second organisation sees no new
  concept anywhere, which preserves sixty seconds to first monitor.
- **SSO fits cleanly.** One account per person is what an identity provider
  asserts. Per-organisation accounts would have made the external identity ADR
  choose between duplicating accounts and inventing linking.
- **The agency case works without a second tenancy level.** Client A and
  Client B are separate organisations. A client user is a member of one. Agency
  staff are members of several. Per-client branding follows directly, because
  brand profiles are already organisation-scoped.

**What this makes hard, or forecloses.**

- **No cross-organisation view in Phase 3.** An agency operator with forty
  client organisations switches between them. The UI has no "all my clients"
  dashboard, and no single report can span clients. This is the most likely
  complaint from the persona this phase serves, and it was accepted knowingly in
  exchange for one boundary that is simple to enforce and test. If an aggregate
  view is later built, it reads through the system store under an explicitly
  authorised path. It must not widen the tenant handle.
- **One active organisation per session.** Two browser tabs share it. Switching
  in one tab changes what the other tab's next request sees. The dashboard must
  detect a switch made elsewhere, for instance by comparing the organisation on
  each response, and reload rather than render one organisation's data under
  another's page. This is a UI obligation and it is easy to miss.
- **Slug contention.** On a shared instance, the first organisation to take
  `status` owns `/status-pages/status`.
- **The instance admin is powerful but not all-seeing.** An operator who needs
  to debug a client's monitor must be added to that organisation, and the audit
  log records the addition. This is the intent, but it is friction, and an
  operator without a membership in any organisation (a pure operator account) is
  a supported state the UI must render sensibly.
- **Shared SMTP and retention.** A client who needs its own mail relay, or a
  longer retention than the instance allows, cannot have it without a later
  per-organisation override.
- **The migration is not small.** It covers the `users` table rebuild, the
  `memberships` table, the `is_instance_admin` flag, `sessions.active_org_id`,
  the instance settings table, and two index changes. Moving the 156 call sites
  onto the split store is the bulk of the work. It must land incrementally, file
  by file, with the 5,000-monitor gate green at each step
  ([AGENTS.md](../../AGENTS.md) rule 2 applies to it).

**What becomes expensive to reverse later.**

- **The flat hierarchy, from the first Phase 3 release that permits a second
  organisation.** Once agencies have modelled clients as organisations,
  introducing an org + client level means migrating their organisations into
  clients of a new parent, including memberships, API keys and public slugs.
- **Instance-unique slugs and push tokens, from the same point.** Making them
  per-organisation again would need an organisation segment in public URLs,
  which is a breaking change to shipped surface.
- **Memberships, from the first release.** The users table rebuild is one-way in
  practice. Downgrading from Phase 3 to 1.1.x after a second organisation or a
  second membership exists is not supported. The upgrade notes must say so.
- The scoped-store split is cheap to reverse and should not need to be.

## Alternatives considered

The options below are recorded as they were put to the maintainer.

**Hierarchy: org + client (two-level).** The agency is one organisation, with
clients as hard-isolated sub-tenants inside it. Agency staff see all clients at
once, and client users see only their own. This is the closest match to the MSP
persona and gives the cross-client dashboard for free. It lost because every
query then needs organisation **and** client scoping, the isolation suite
doubles, and the enforcement mechanism has to express "all clients of this org"
and "exactly this client" as two distinct handles. That is twice the surface for
the one guarantee Phase 3 can least afford to get wrong, while enforcement
exists in only one layer.

**Hierarchy: flat organisation, clients as soft groups.** One organisation, with
"client" as a label like a group or tag, and client users restricted to it by
resource scoping. This is the cheapest schema. It lost because isolation would
become a permission filter over shared data rather than a tenancy boundary. The
roadmap promises "hard data isolation", and a filter that one misapplied scope
bypasses is not that.

**Identity: per-organisation accounts.** Keep `users.org_id` and
`UNIQUE (org_id, email)`, so a person in three organisations has three accounts.
No schema change is needed. It lost as a poor fit for agency staff and for SSO,
which asserts one identity per person.

**Instance administration: owner of the first organisation.** No new concept is
needed. It lost because an agency that made a client its first organisation
would hand that client the server.

**Instance administration: config file and CLI only.** Simple and hard to misuse.
It lost because it breaks the API-first principle for those operations: the
dashboard could not do what the operator needs.

**Organisation creation: instance admins plus optional self-signup, or any
authenticated user.** Both suit a shared community or SaaS-like install. Neither
suits the self-hosted default, and self-signup can be added later as an
off-by-default setting without revisiting this ADR.

**Instance-wide scope also covering telemetry and a security floor.** This was
offered and not chosen. Both stay per organisation. An operator who wants every
organisation to require TOTP cannot enforce it from the instance level under
this ADR.

**Public slugs: organisation-prefixed paths.** A new
`/public/orgs/{orgSlug}/status-pages/{slug}`, with the old path resolving only
against the default organisation. It is additive, with no naming contention. It
lost because it leaves two URL shapes forever, and the old one silently means
"the default organisation": an implicit default on a public path.

**Active organisation: an optional request header.** Tabs could differ. It lost
because a missing header would fall back silently to a default, and implicit
defaults are how cross-organisation mistakes happen.

**Active organisation: organisation in the URL path.** The most explicit option.
It lost because it breaks every shipped operation and means `/api/v2`.

**Enforcement: an `orgID` argument on every repository method.** Explicit and
greppable. It lost because a caller can still pass the wrong organisation, most
likely `SentinelOrgID` copied from existing code, and it compiles.

**Enforcement: organisation read from `context` by the store.** It needs the
fewest signature changes. It lost because a missing value is a runtime error,
not a compile error. That is exactly the failure ADR-003 warned about, and on
the path with no second layer behind it.

**RLS as an enforcement layer.** Not available: there is no Postgres backend in
Phase 3. ADR-003's spike on TimescaleDB RLS over continuous aggregates returns
to the agenda with the Phase 4 Postgres path. If RLS is adopted then, it will be
defence in depth under this ADR's scoped store, not a replacement for it.

## Compliance with the product principles

- [x] Sixty seconds to first monitor is preserved. First-run setup still creates
      one account, which becomes the owner of the default organisation and an
      instance admin. No organisation concept is shown to a user with one
      membership.
- [x] Nothing is paywalled in the open source build. Multiple organisations,
      memberships and isolation ship in the open source build.
- [x] API-first: no privileged endpoints the dashboard uses and users cannot.
      Instance administration and organisation switching are API operations,
      specified in the Phase 3 spec draft before any handler exists.
- [x] Progressive disclosure: no new complexity imposed on the solo user. The
      organisation switcher and instance administration screens do not render
      until there is something to switch between or administer.
- [ ] The client is never sent full state; the UI stays fast at 5,000 monitors.
      **Not yet demonstrated.** Every tenant-scoped statement gains or keeps an
      `org_id` predicate. The `(org_id, …)` composite indexes from ADR-003 are
      designed for it. The 5,000-monitor gate, run with at least two
      organisations, is what proves it, and it must stay green at each step of
      the call-site migration.
- [x] Solo mode keeps zero required external dependencies.
- [x] Dependency surface stays minimal. Nothing new is introduced.

## References

- [ADR-003](003-tenancy-model.md): the inert `org_id` column, and the four
  questions this ADR answers.
- [ADR-004](004-ui-state-synchronisation.md): the live channel, whose
  subscriptions now resolve through the tenant store.
- [PHASE-3-PLAN.md](../plans/PHASE-3-PLAN.md) §2.2 and §3.1: the starting
  position and the questions as posed.
- [COMPATIBILITY.md](../api/COMPATIBILITY.md): the reason every shipped
  operation must keep its shape.
- [data model §3.1–§3.3](../data-model/README.md): organisations, users and
  sessions as they stand. Data model §8 on migration immutability after a tagged
  release.
- Migrations [`0001`](../../migrations/sqlite/0001_initial.sql),
  [`0002`](../../migrations/sqlite/0002_identity.sql) and
  [`0003`](../../migrations/sqlite/0003_alerting_and_pages.sql): the indexes
  named in the Context. [`0007`](../../migrations/sqlite/0007_import_vocabulary.sql)
  is the table-rebuild precedent.
- Follow-on decisions that this ADR constrains but does not make: the
  authorisation ADR (what each membership role permits; custom roles; API keys
  when a member's role changes or their membership ends), the external identity
  ADR (how an IdP login maps to an account and memberships), and organisation
  deletion (archive, export and purge), which is not decided here.
