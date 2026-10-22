# ADR-012: External Identity — OIDC and SAML, Invite-Only with Verified-Email Linking, and a Named Break-Glass Account

- **Status:** Access
- **Date:** 2026-10-21
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** **Builds on [ADR-010](010-tenancy-isolation-and-membership.md)** (instance-wide accounts, memberships, the instance-admin flag) and **[ADR-011](011-authorisation-roles-and-grants.md)** (what a role grants). Neither is modified.

> ADRs are **immutable once accepted** — if we later change our minds, write a
> new ADR that supersedes this one and update the Status line above.

> **How this ADR was written, and why it needs more review than the others.**
> [AGENTS.md](../../AGENTS.md) rule 3 (no agent-written ADRs) was waived by the
> maintainer on 2026-09-22; the decisions below are the maintainer's, put to
> them as questions, and the prose is an agent's.
>
> **[AGENTS.md](../../AGENTS.md) rule 8 — "Security work is human-led. Do not
> generate authentication, session, crypto, or access-control code" — is not
> waived by that, and this document does not assume it is.** This is a design
> record for the authentication path itself. Phase 2 waived rule 8 for share
> links and an S3 signer and recorded honestly that the resulting code worked,
> had tests, and had been read by nobody. The login path does not get that
> treatment. **This design should be reviewed against a current reference by a
> human before any of it is implemented**, in the same spirit as data model
> §11.6, and the implementation itself is human-led work.

## Context

Phase 3's roadmap line is "Local auth + OIDC + SAML + LDAP". Local
authentication is already complete and good: argon2id passwords
(`internal/auth/password.go`), sessions whose tokens are stored only as hashes
with a CSRF companion, TOTP with an encrypted secret and ten single-use recovery
codes, and in-memory login rate limiting. There is **no OIDC, SAML or LDAP code
or stub anywhere** in the repository.

Four constraints shape the decision before preference enters it.

**The dependency policy is the crux.** [CONTRIBUTING.md](../../CONTRIBUTING.md)
requires human justification for every third-party package, and the project has
held a deliberately small surface. The three protocols are not equally cheap:

- **OIDC** is feasible on the standard library plus a JOSE/JWT verification
  dependency. Discovery, the authorisation-code flow and PKCE are HTTP and JSON.
- **SAML** requires XML canonicalisation (c14n) and XML-DSig signature
  verification, which the standard library does not provide. This is the part
  with a long public history of signature-wrapping and comment-splicing
  vulnerabilities in hand-rolled implementations. It needs a maintained library,
  or it must not ship.
- **LDAP** requires an LDAP client.

**ADR-010 makes one account per person.** Email is unique instance-wide, and
membership is a separate row. That removes the hardest question an SSO design
otherwise faces — whether an IdP identity maps to one account or several — and
is why this ADR can be about linking rather than about identity shape.

**The instance is self-hosted and often small.** The same binary serves a
freelancer and a 50-person organisation. An SSO design that can lock an operator
out of their own server is worse than one that is slightly less strict.

**Sessions are fixed at 30 days** (`sessionLifetime` in
[`internal/api/auth.go`](../../internal/api/auth.go)) with no IdP-side
revocation signal, so deprovisioning at the IdP does not currently end a Cairn
session.

## Decision

**1. OIDC and SAML are in scope. LDAP is cut from Phase 3** and moves on the
roadmap, as the plan's cut line anticipated. The reason is not effort but
coverage: directories that justify LDAP (Active Directory, FreeIPA, OpenLDAP) are
nearly always frontable by Keycloak, Authentik or Entra, which speak OIDC. LDAP
is reconsidered when someone reports a directory they cannot front.

**2. Both protocols are library-backed, and the SAML library is a gate on the
feature.** OIDC takes a JOSE verification dependency; SAML takes a maintained
SAML service-provider library. Each is justified in its own PR under the
dependency policy, naming what it does, why the standard library will not, and
its maintenance status. **If no SAML library passes that review, SAML is cut
and the cut is recorded** — it is not written in-tree. No part of XML-DSig
verification is implemented by this project.

**3. Connections are configured per organisation.** An organisation may have at
most one OIDC connection and at most one SAML connection in this phase. Client
secrets and signing material are sealed at rest under the existing envelope
scheme (the `SMTPSettings.PasswordSealed` precedent), and never returned by the
API.

**4. Provisioning is invite-only. An IdP login never creates an account.** The
flow is:

- The assertion's email is matched against existing accounts.
- A match is accepted **only if** the IdP asserts the email as verified **and**
  the email's domain is on the connection's allowed-domain list. The account is
  then linked to that IdP subject (the stable `sub`/`NameID`, stored alongside
  the account) and the login proceeds.
- **No match, or an unverified email, or a domain not on the list: the login is
  refused** with an explanation that an invitation is required. Nothing is
  created and nothing is linked.
- After the first successful link, subsequent logins match on the stored IdP
  subject rather than the email, so an email change at the IdP does not silently
  re-link or orphan the account.

This makes the invitation, not the IdP's email claim, the thing that grants
access. The failure mode of a misconfigured connection is then "nobody can log
in", not "anybody can".

**5. Group-to-role mapping is optional and, when configured, authoritative.** A
connection may map IdP group claims to roles (built-in or custom, per
[ADR-011](011-authorisation-roles-and-grants.md)). When a mapping exists, the
member's role is **re-applied on every login**, so a demotion at the IdP takes
effect here, and every change is written to the audit log. Groups that match no
mapping change nothing. With no mapping configured, roles are managed in Cairn
and logins never alter them. Mapping can never grant the instance-admin flag,
which is set only by an instance admin.

**6. An organisation may require SSO; instance admins always keep local
login.** `require_sso` on an organisation means its members authenticate through
the connection. **Instance admins retain password-plus-TOTP login
unconditionally**, and that is the documented break-glass path when an IdP is
unreachable or a connection is misconfigured. The escape hatch is a named,
auditable account — not a hidden flag, an environment variable or a recovery
mode. The documentation states plainly that an instance admin's local password
and TOTP are therefore part of the install's security boundary.

**7. Sessions and deprovisioning.** An SSO login creates an ordinary Cairn
session, so everything that already protects sessions keeps protecting them. Two
additions: a session records whether it was created by SSO and through which
connection, and **deactivating a member or ending their membership revokes their
sessions immediately** (ADR-011 already requires this for the membership case).
Back-channel logout and session-lifetime-from-the-IdP are **not** in this phase.
The consequence is stated rather than hidden: a user deprovisioned at the IdP
keeps a valid Cairn session until it expires or somebody deactivates them, and
that is the strongest argument for group mapping, which re-reads authority on
each login.

**8. Every authentication event is audited**, per the Phase 3 plan: IdP login
success and failure with the reason, a refused link and why, a link established,
a role changed by mapping, and local break-glass logins. A refused SSO login
that nobody can see is an outage with no diagnosis.

## Consequences

**What this makes easy.**

- **Most self-hosted IdPs work on day one** through OIDC, with discovery doing
  the configuration.
- **A misconfigured connection fails closed.** Because nothing is provisioned
  from an assertion, the worst outcome of a bad connection is that logins are
  refused — and an instance admin can still get in and fix it.
- **The IdP can stay authoritative for roles** without a second place to manage
  them, and the audit log shows exactly when a mapping changed somebody's
  access.
- **Nothing about local authentication is disturbed**, so the solo user's
  experience and the existing TOTP and rate-limiting behaviour are untouched.

**What this makes hard, or forecloses.**

- **No self-service onboarding.** A 50-person organisation adopting Cairn must
  send 50 invitations. That is real friction, accepted in exchange for the IdP's
  email claim not being the gate. An "invite the whole allowed domain" bulk
  action is the obvious later mitigation and is not in this phase.
- **No LDAP.** Named as a cut, not an omission.
- **SAML may not ship**, if no library passes dependency review. The roadmap
  line would then be partly unmet, and that must be said in the release notes
  rather than left for someone to discover.
- **A deprovisioned user keeps a session for up to 30 days** unless deactivated
  in Cairn. The offboarding documentation must name both steps, exactly as
  ADR-011's documentation names key revocation.
- **Instance admins are a permanent local-login hole** for an organisation that
  wants every authentication through its IdP. This is deliberate, and an
  organisation that cannot accept it should not share an instance with others.
- **Sealed connection secrets join the backup story.** Losing the root key
  loses the connection configuration with everything else it already loses.

**What becomes expensive to reverse later.**

- **The linking rule, from the first release that links an account.** Loosening
  it later (to JIT) is additive and safe; tightening it after installs have
  JIT-provisioned accounts would strand users. The strict direction is therefore
  the right one to ship first — this is the one place where the cautious choice
  is also the cheaper choice to change.
- **Storing the IdP subject per account, from the first link.** Changing the
  match key afterwards means re-linking everybody.
- **A SAML dependency, once shipped.** It will sit on the authentication path,
  and replacing it is a migration of trust, not of code.
- OIDC's JOSE dependency is replaceable. The flow is standard and the
  verification surface is small.

## Alternatives considered

Recorded as they were put to the maintainer.

**Include LDAP.** Direct bind against an existing directory, and the only option
for a site with no OIDC-capable front end. It lost on coverage: the directories
that would use it are nearly always frontable by an OIDC provider, and it is the
plan's own first cut.

**JIT provisioning within allowed domains.** A first login from an allowed
domain creates the account and a membership with a default role — the obvious
convenience for a 50-person rollout. It lost because the IdP's email claim
becomes the authority for who gets in, so a misconfigured connection, a provider
that does not actually verify email, or an allowed domain with open
self-registration becomes an open door into every organisation using that
connection.

**JIT with admin approval before first access.** A first login creates a pending
account an admin approves. Safer than open JIT and much less work than 50
invitations. It lost narrowly: it adds a "pending user" state to every screen
that lists members, and the invitation flow already exists for the same purpose.
It is the natural mitigation if invite-only proves too much friction in practice.

**SSO required for everyone, including instance admins.** The strongest
guarantee that the IdP is the single front door, which is what a security
reviewer at a large organisation will ask for. It lost because an IdP outage or
a bad connection would lock every account out of a self-hosted box, recoverable
only through the CLI or the database — a failure mode that would be hit by real
operators, and one where the recovery procedure is more dangerous than the hole
it closes.

**Local login always available alongside SSO, with no require-SSO option.**
Impossible to lock yourself out. It lost because an organisation that must route
all access through its IdP (and that IdP's MFA policy) could not express it, and
that is a legitimate requirement this phase exists to serve.

**Group mapping applied only at first login.** Avoids an IdP group rename
cascading into a mass demotion. It lost because the two systems then diverge
silently, which is worse than a rename being visible and audited.

**Roles always managed in Cairn, ignoring group claims.** Most predictable, no
mapping to misconfigure. It lost because a 50-person organisation would maintain
roles twice, and because re-applying on login is the only mechanism in this
phase that reacts to a demotion at the IdP at all.

## Compliance with the product principles

- [x] Sixty seconds to first monitor is preserved. SSO is invisible until
      configured; first-run setup is unchanged.
- [x] Nothing is paywalled in the open source build. OIDC and SAML ship in the
      open source build, as the roadmap states explicitly.
- [x] API-first. Connection configuration, linking and the callbacks are
      specified in the Phase 3 spec draft before any handler exists.
- [x] Progressive disclosure. The SSO screen appears under organisation
      settings and nowhere else; a solo install never meets it.
- [x] The client is never sent full state; the UI stays fast at 5,000 monitors.
      Not affected: authentication is per request, and resolution adds no
      per-monitor work.
- [x] Solo mode keeps zero required external dependencies. An IdP is optional;
      nothing external is required to run.
- [ ] Dependency surface stays minimal. **Two new dependencies are proposed** —
      JOSE verification for OIDC, and a SAML service-provider library. Each is
      justified in its own PR, and SAML does not ship if its library fails
      review. This is an exception argued on the ground that hand-rolling
      either one is strictly worse.

## References

- [ADR-010](010-tenancy-isolation-and-membership.md): instance-wide accounts and
  memberships, which make linking rather than identity-shape the question.
- [ADR-011](011-authorisation-roles-and-grants.md): the roles a group mapping may
  assign, and the containment rule that stops a mapping exceeding its creator.
- [CONTRIBUTING.md](../../CONTRIBUTING.md) § Dependency policy: the review each
  library must pass.
- [AGENTS.md](../../AGENTS.md) rule 8, and
  [data model §11.6](../data-model/README.md#116-secrets-at-rest): why this
  document is a design to be reviewed rather than a licence to generate the
  code.
- `internal/auth/password.go`, `internal/auth/token.go`, `internal/auth/totp.go`
  and [`internal/api/auth.go`](../../internal/api/auth.go): the local
  authentication this ADR extends and does not alter, including the 30-day
  session lifetime that bounds deprovisioning.
- Data model §12 (secrets at rest): the envelope the connection secrets use.
- [PHASE-3-PLAN.md](../plans/PHASE-3-PLAN.md) §3.3 and §9: the questions as
  posed, and the cut line that put LDAP first and SAML second.
- Deferred to a later phase, named so they are refused consistently: SCIM
  provisioning, back-channel logout, IdP-driven session lifetime, more than one
  connection per protocol per organisation, and WebAuthn.
