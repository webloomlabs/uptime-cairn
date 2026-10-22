# ADR-011: Authorisation — Organisation-Wide Roles, Built-ins in Code with Custom Roles as Data, and Organisation-Owned API Keys

- **Status:** Accepted
- **Date:** 2026-10-21
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** **Builds on [ADR-010](010-tenancy-isolation-and-membership.md).** ADR-010 decided that a role lives on a membership and deliberately left what a role *permits* to this ADR. Nothing here changes ADR-010.

> ADRs are **immutable once accepted** — if we later change our minds, write a
> new ADR that supersedes this one and update the Status line above. The
> reasoning trail is the point: it has to survive for whoever maintains this
> project after us.

> **How this ADR was written.** [AGENTS.md](../../AGENTS.md) § *What agents may
> not do* (3) says agents do not draft ADRs. The project maintainer explicitly
> waived that restriction on 2026-09-22, following the precedent at
> [data model §11.6](../data-model/README.md#116-secrets-at-rest). **The
> decisions are the maintainer's** — each was put to them as a question with its
> alternatives and answered. The prose, the evidence and the consequences
> analysis were drafted by an agent, which is why Alternatives records the
> options as they were presented. Check the reasoning, not only the conclusions.

## Context

Phase 3 enforces the six roles that have been in the schema since
[`0002_identity.sql`](../../migrations/sqlite/0002_identity.sql) and adds custom
roles. The authorisation *path* already exists and works, which narrows this
decision considerably:

- **Scopes are the primitive.** `internal/auth/scope.go` defines 33 scopes;
  every route is guarded by `require(scope, handler)` in
  [`internal/api/server.go`](../../internal/api/server.go); a write scope
  implies the matching read scope; and `Covers` already stops an API key minting
  a more powerful key.
- **Roles already resolve to scopes.** `auth.ScopesFor(role)` maps
  owner/admin/editor/responder/viewer to scope sets, and its comment states the
  intent for this phase exactly: *"Phase 3's RBAC should be a table of grants
  rather than a rewrite of the authorisation path."*
- **Three gaps are known.** `billing` resolves to no scopes at all. `editor`
  holds no report or brand-profile scopes, so Phase 2's deferred "report
  authoring by non-owners" has no role that can do it. And custom roles do not
  exist.

Two further questions arrive with multi-user installs and are decided here
because both are expensive to change afterwards: what happens to an API key when
the member who created it is demoted or leaves, and how an organisation is
deleted.

`api_keys` already carries `org_id`, `scopes`, a soft `revoked_at` (so a revoked
key's audit entries stay resolvable) and `created_by` referencing `users`.
`pending_purges` exists for deferred deletion but its `entity_type` CHECK admits
only `'monitor'`.

## Decision

**1. A role applies to the whole organisation.** There is no team-, tag- or
resource-level narrowing of permissions. A 50-person organisation is one blast
radius; an organisation that needs a smaller one uses a second organisation,
which ADR-010 makes cheap. Teams exist in this phase as **escalation targets and
a grouping of people**, not as an authorisation boundary. Every list, count,
search and report query therefore keeps the shape it has today, with `org_id` as
its only tenancy predicate.

**2. The six built-in roles are defined in code and are read-only.** They stay
where `ScopesFor` has them, as a fixed table of grants that is the same on every
install. An installation cannot redefine `viewer`, because a role name that
means something different on each install cannot be documented, supported, or
usefully tested.

**3. Custom roles are rows, scoped to one organisation.** A custom role is a
name and a set of scopes (`roles(id, org_id, name, scopes)`), created by an
owner or admin, and selectable as a membership's role alongside the built-ins. A
custom role may hold only scopes its creator holds — the same containment rule
`Covers` already applies to API keys, reused rather than reinvented. Deleting a
custom role is refused while any membership uses it.

**4. `billing` is reserved and not selectable.** It stays in the `users` /
membership role CHECK so no migration is needed if a hosted billing context ever
needs it, and it is documented as reserved. The API rejects it as a membership
role and it does not appear in the role picker. A role that is offered and
grants nothing is worse than one that is absent.

**5. `editor` gains the report and brand-profile scopes**, which closes Phase 2's
"report authoring by non-owners" deferral. `responder` keeps incident and
maintenance write access and gains acknowledgement (ADR-013) and nothing more.
The full grant table ships as documentation generated from or checked against
the code, so the docs cannot drift from the grants.

**6. API keys belong to the organisation, not to their creator.** `created_by`
is provenance only. A key's scopes are fixed at creation and are **not**
re-derived from its creator's current role. Demoting a member does not narrow
their keys, and removing a member's membership does not revoke them: an
organisation's automation keeps running when its people change.

**The cost of 6 is real and is accepted rather than argued away:** an offboarded
admin's key keeps working until somebody revokes it. Three things reduce it, and
none of them makes it zero.

- Removing a membership shows the keys that member created, with their last-used
  time, and offers to revoke them in the same action. The default in that dialog
  is to revoke.
- The key list shows creator and last-used, and flags keys whose creator is no
  longer a member.
- Offboarding documentation names key revocation as a step, and the audit log
  records creation, revocation and the membership change, so the gap is
  reconstructable afterwards.

**7. Deleting an organisation is two steps.** An instance admin first
**deactivates** it: checks stop, public status pages and share links go dark,
and all data is retained. A separate **purge**, permitted only after a grace
period, deletes the rows and the organisation's report artifacts from disk. Both
steps are audited, and the default organisation cannot be deleted at all. This
extends `pending_purges` to a second entity type, which means a table rebuild
for its CHECK — cheap, and the right place for the mechanism, since
monitor purging already works this way.

**8. Nothing about the authorisation path changes.** `require(scope, …)` stays
at every route, denials stay `403 insufficient-scope` naming the missing scope,
and the only change is where the scope set comes from: the membership's role,
built-in or custom, resolved through one function.

## Consequences

**What this makes easy.**

- **The load gate is unaffected by authorisation.** No query gains a predicate,
  so the 5,000-monitor promise is not re-litigated by this decision. That was
  the single strongest argument for organisation-wide roles.
- **Roles are explainable.** "What can an editor do?" has one answer, the same
  on every install, printable as a table and testable as one.
- **Enforcement is already written.** Scopes, route guards, implication and
  containment all exist. This phase changes a lookup, not a mechanism.
- **Custom roles are cheap and contained.** They cannot exceed their creator,
  cannot be shared between organisations, and cannot be orphaned while in use.
- **Automation survives reorganisation.** Keys keep working across promotions,
  demotions and departures, which is what a team running production needs.

**What this makes hard, or forecloses.**

- **No "team A edits only team A's monitors".** A 50-person organisation where
  one team must not touch another's monitors cannot express that in Phase 3.
  The answer is two organisations, which costs them a single dashboard
  (ADR-010's accepted cost). This is the most likely feature request to follow
  the release, and adding it later means a predicate on every list query plus a
  load-gate re-run — not a schema problem, a performance one.
- **An offboarded member's key keeps working** until revoked. Stated in full
  above. It is the one decision here that trades security for continuity, and a
  reviewer should satisfy themselves that the three mitigations are enough for
  their threat model.
- **An install cannot tighten a built-in role.** An organisation that wants a
  narrower `editor` must create a custom role and move its members onto it.
- **Deactivated organisations are a state the UI must render** — listed, inert,
  purgeable, and clearly not running — rather than a row nobody thought about.

**What becomes expensive to reverse later.**

- **Organisation-wide scope, from the first Phase 3 release.** Not because of
  the schema, but because narrowing later changes the performance
  characteristics of every list endpoint at once.
- **Custom roles as data, from the first release that creates one.** Installs
  will have rows.
- `billing` being reserved is free to revisit: giving it grants later is
  additive. Key ownership is cheap to tighten later (intersecting scopes on
  demotion is a lookup change), and tightening is the direction a later ADR
  would move, which is the right way round.

## Alternatives considered

Recorded as they were put to the maintainer.

**Optional team scoping.** A membership narrowed to one or more teams, with
monitors carrying an owning team; unscoped memberships behave as today. Clearly
more powerful for a large organisation. It lost on cost and risk: every list,
count, search and report query gains a team predicate, the load gate has to
prove it at 5,000 monitors, and it would land in the same phase as the tenancy
refactor — two sets of new predicates on the same queries at once.

**Tag-based scoping.** A membership narrowed by tag selector, reusing the tag
system and matching how report templates already select monitors. It lost
because tags are mutable by editors, so the permission boundary would be
editable by the people it constrains.

**All roles in a table, built-ins seeded and editable.** Uniform, and lets an
organisation tighten `editor` in place. It lost because a built-in role name
would then mean something different on every install, which weakens the
documentation, the support position and the role tests — and the role tests are
the thing standing between this phase and a privilege-escalation bug.

**Built-ins only, no custom roles.** The six roles cover the exit condition, and
the plan already names custom roles as cuttable. It lost narrowly: a
"viewer who can acknowledge" is a real and common request, and custom roles are
cheap given scopes already exist. It remains the first thing to cut if the phase
runs long, per the plan's cut line.

**`billing` as read-only on reports and usage.** Gives it a real meaning now. It
lost as a role invented to fill a schema gap rather than to meet a request.
Reserved costs nothing and keeps the option.

**`billing` as an alias for viewer.** Harmless and selectable. It lost because
two names for one thing is exactly what nobody can explain in a year.

**API keys revoked on membership end and intersected on demotion.** The
strongest security position: an offboarded person leaves no working credential,
and a demotion cannot be bypassed by a key minted earlier. It lost because it
makes a routine personnel change silently narrow or break an organisation's
automation, and because a key's effective scopes would then depend on state
elsewhere, which is hard to display honestly in a key listing.

**API keys revoked on both.** Simplest to explain and safest. It lost for the
same reason, more sharply: a demotion would break automation outright.

**Immediate organisation deletion with a typed confirmation.** No half-live
organisations, and nothing to sweep. It lost because there is no recovery from a
mistake, and because the artifacts on disk would have to be removed
synchronously inside a request.

**No organisation deletion in Phase 3.** Smallest surface; removal would be a
documented manual database procedure. It lost because an operator with a
departed client needs a supported way to remove them, and because a manual
procedure around a reports directory and a dozen tables is exactly where someone
deletes the wrong rows.

## Compliance with the product principles

- [x] Sixty seconds to first monitor is preserved. The first account is an owner
      with every scope; nothing new is asked of it.
- [x] Nothing is paywalled in the open source build. RBAC and custom roles ship
      in the open source build.
- [x] API-first — no privileged endpoints the dashboard uses and users cannot.
      Roles, custom roles, key revocation and organisation lifecycle are all API
      operations, specified before any handler.
- [x] Progressive disclosure. A single-member organisation sees no role picker,
      no custom roles screen and no organisation lifecycle controls.
- [x] The client is never sent full state; the UI stays fast at 5,000 monitors.
      **This decision adds no predicate to any query**, which is the main reason
      organisation-wide scope was chosen.
- [x] Solo mode keeps zero required external dependencies.
- [x] Dependency surface stays minimal. Nothing new is introduced.

## References

- [ADR-010](010-tenancy-isolation-and-membership.md): memberships, the role's
  home, the instance-admin flag, and flat organisations as the isolation
  boundary this ADR relies on.
- [ADR-013](013-incident-automation-and-alert-routing.md): acknowledgement, which
  is the one new grant `responder` receives.
- `internal/auth/scope.go`: the 33 scopes, `ScopesFor`, write-implies-read, and
  `Covers` — the containment rule reused for custom roles.
- [`0002_identity.sql`](../../migrations/sqlite/0002_identity.sql): the role
  CHECK and the `api_keys` table, including `created_by` and the soft
  `revoked_at`.
- [`0003_alerting_and_pages.sql`](../../migrations/sqlite/0003_alerting_and_pages.sql):
  `pending_purges`, whose `entity_type` CHECK this ADR extends.
- [PHASE-2-PLAN.md](../plans/PHASE-2-PLAN.md) §1: "report authoring by
  non-owners (Phase 3 RBAC)", the deferral decision 5 closes.
- [PHASE-3-PLAN.md](../plans/PHASE-3-PLAN.md) §3.2 and §9: the questions as
  posed, and the cut line that keeps custom roles cuttable.
