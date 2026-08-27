# Governance

This document describes how Uptime Cairn is run: who decides what, how someone
gains (or loses) authority, and what happens to the project if the people
currently holding it walk away.

That last question is the reason this document exists. The project plan names
**single-maintainer burnout as a critical, existential risk**. Both of the
community wrapper projects that Uptime Cairn exists to make unnecessary are
dying of exactly that, and the incumbent this project succeeds is famously one
person. Contributors and adopters are entitled to evaluate our continuity story
*before* they invest, so it is written down here rather than assumed.

---

## 1. Mission and constraints

Uptime Cairn is **one** open source uptime monitoring and reporting tool that
serves the whole spectrum of users — from a freelancer with three client sites
to an enterprise with SOC 2 auditors — without artificial feature gating.

Three commitments constrain every governance decision. They are not up for
casual revision:

1. **No feature is paywalled in the open source build.** RBAC, SSO, multi-region
   probing, PDF reports, and audit logs all ship in the open source build. The
   project earns money from hosting and support, never by crippling the
   software.
2. **One codebase, one binary, progressive disclosure.** Complexity is revealed
   on demand, never imposed by default. We do not fork into "community" and
   "enterprise" editions.
3. **Your data is yours.** Full export in open formats, no lock-in, no
   phone-home, telemetry opt-in only.

A change to any of the three requires the supermajority process in §7.

## 2. Roles

### Users
Anyone running Uptime Cairn. Users are not a formal role, but bug reports,
feature requests, and "this is how it broke for me" reports are treated as
first-class contributions.

### Contributors
Anyone who opens an issue or pull request. No formal process, no invitation
needed. Contributors retain copyright in their work and license it to the
project under the CLA (§6).

### Committers
Contributors with merge rights on the main repository. Committers may:

- review and merge pull requests, including their own after review by another
  committer,
- triage, label, and close issues,
- cut releases,
- vote on decisions escalated under §7.

**Becoming a committer.** There is no fixed contribution count. The bar is
demonstrated judgment over time — roughly:

- a track record of merged, non-trivial contributions,
- reviews of *other people's* work that catch real problems,
- evident understanding of the mission constraints in §1 (in particular, why we
  refuse to paywall and why the UI must stay fast at 5,000 monitors),
- reliability: says what they will do, does it or says they cannot.

Any committer may nominate a contributor by opening a private discussion among
existing committers. Nomination passes on lazy consensus after 7 days with no
sustained objection. New committers are announced publicly.

**Target: at least 3 committers with merge rights before v1.0.** This is a
release-blocking deliverable, not a background hope. Recruiting them is
explicitly part of the maintainers' job, and progress against it is reported in
the monthly notes (§8).

### Maintainers
Committers who additionally hold administrative control: repository and
organisation ownership, domains, registry namespaces (Docker Hub, npm scope),
signing keys, release infrastructure, and the security contact.

Maintainers are responsible for the things that cannot be delegated by a pull
request: security response, licence and trademark decisions, infrastructure, and
the continuity plan in §5.

**There must be at least two maintainers at all times once v1.0 ships.** A
single maintainer is a single point of failure, and this project's entire
argument for existing is that single points of failure are unacceptable in
monitoring.

### Emeritus
Committers and maintainers who step back keep the credit and lose the keys.
Moving to emeritus is a normal, honourable outcome — it is explicitly *not*
failure, and we would rather someone step back cleanly than burn out silently.
Access is revoked on the way out (§5.3); returning is a nomination away.

## 3. How decisions get made

Most decisions need no ceremony. In rough order of weight:

**Lazy consensus (the default).** Open a PR or issue, give people time to object,
merge if nobody with standing objects. Small fixes need no discussion at all.

**Architecture Decision Records.** Anything that changes a structural
commitment — the probe/control-plane split, the storage strategy, the tenancy
model, the UI state-synchronisation model, the API contract — requires an ADR in
`docs/adr/`, discussed publicly before acceptance. ADRs are immutable once
accepted; a reversal is a new ADR that supersedes the old one, so the reasoning
trail survives.

**Committer vote.** If lazy consensus fails, any committer may call a vote.
Simple majority of active committers, 7-day window, public unless the subject is
a security or conduct matter.

**Supermajority.** Changes to the §1 mission constraints, licence changes, and
this governance document require two-thirds of active committers *and* the
consent of all maintainers.

"Active" means having reviewed, merged, or voted in the preceding 90 days.

**When there is genuine disagreement**, the tiebreaker is the project plan's
priority order: does this serve the *whole* spectrum in one install, does it keep
onboarding under 60 seconds, and does it hold the line on scale? A feature that
serves the enterprise by making the freelancer's first run harder is the wrong
trade, and so is the reverse.

## 4. Releases

- Semantic versioning. The `/api/v1` contract carries an explicit deprecation
  policy — [docs/api/COMPATIBILITY.md](docs/api/COMPATIBILITY.md) — and breaking it
  requires a major version and a migration path.
- No more than two consecutive weeks without a release, as a health target.
- Any committer may cut a release; releases are reproducible and signed, and
  ship with an SBOM.
- Release artifacts are built by CI from a tag. Nothing is published from a
  laptop.

## 5. Continuity — the bus factor plan

This section exists so that "what if the lead disappears?" has a written answer.

### 5.1 Nothing critical lives with one person

The following must be held by **at least two maintainers** at all times, and the
list is audited every quarter:

| Asset | Requirement |
|---|---|
| GitHub organisation | ≥2 owners |
| Domains (`uptimecairn.dev` and defensive registrations) | ≥2 accounts with registrar access, auto-renew on |
| Docker Hub / container registry namespace | ≥2 owners |
| npm `@uptimecairn` scope | ≥2 owners |
| Release signing keys | Escrowed, recoverable by ≥2 maintainers |
| Security contact mailbox | ≥2 recipients |
| CI/CD and secrets | ≥2 admins |
| Social accounts | ≥2 with credentials |

Credentials are held in a shared secret store; recovery requires two
maintainers, and no recovery path depends on a single personal email account.

### 5.2 Nothing critical lives only in someone's head

Build, release, deploy, and incident-response procedures are documented in the
repository and must be executable by a committer who did not write them. A
procedure that only one person can perform is treated as a bug with a
corresponding issue. ADRs exist partly for this reason: they preserve *why*, not
just *what*, so a future maintainer can revisit a decision without having to
excavate it.

### 5.3 Succession

**Planned departure.** A departing maintainer nominates a successor from the
committers, transfers credentials, and moves to emeritus. If they name no
successor, the remaining maintainers appoint one by the §3 vote.

**Unplanned absence.** If a maintainer is unreachable for **60 consecutive
days** with no prior notice, the remaining maintainers may reassign their
administrative access by supermajority. Access removal is not a judgement about
the person; it is hygiene. It is logged publicly and reversed on their return if
they want it back.

**Total maintainer loss.** If no maintainer remains reachable for 60 days, the
active committers may, by two-thirds vote, appoint maintainers from among
themselves and take control of the project assets. If the *committers* are also
gone, the project is considered dormant: the licence guarantees anyone may fork
and continue it, and the remaining assets should be handed to a recognised open
source foundation if one will take them.

**Archival, not abandonment.** If the project is ever wound down, maintainers
commit to announcing it explicitly, publishing a final release, marking the
repository archived with a pointer to any active fork, and *not* silently going
quiet. Silent abandonment is the specific failure mode this project was built in
reaction to; we will not reproduce it.

### 5.4 Lowering the cost of contributing

Continuity is not only a credentials problem — a project nobody can contribute
to has a bus factor of one regardless of policy. Therefore, as standing
obligations:

- ruthless CI (tests, linting, contract tests, reproducible builds) so
  contributors get fast, honest feedback without a maintainer in the loop,
- `good-first-issue` triage kept genuinely stocked and accurately scoped,
- a responsive review cadence — median first response to issues and PRs under
  48 hours,
- a public roadmap and monthly progress notes.

## 6. Licence, CLA, and relicensing

Uptime Cairn is licensed under the **Apache License, Version 2.0**
(see [LICENSE](LICENSE)). Apache 2.0 is a deliberate choice: it puts no copyleft
condition on people who run, embed, or build on Uptime Cairn, and its explicit
patent grant makes the software safe to adopt inside a company without a legal
review. The cost is real and worth stating plainly — unlike a copyleft
licence, Apache 2.0 does not compel anyone, hyperscaler included, to contribute
their changes back. What keeps this project open is therefore §1.1 and the
governance around it, not a licence obligation.

Contributions require signing a **Contributor License Agreement**. The CLA
grants the project the right to license contributions, including the option to
relicense or dual-license in future if the project's needs change.

Because a CLA asks contributors for real trust, we bound it:

- Relicensing requires the §3 supermajority *and* at least 30 days of public
  notice before it takes effect.
- Any relicensing must preserve an open source build with no paywalled features
  (§1.1). The CLA will not be used to take capability away from the community
  edition.
- Contributors retain full copyright in their work and may use it however they
  wish.

## 7. Funding and sustainability

The project may accept money from managed hosting, commercial support and SLAs,
sponsorships, and paid priority work on features that were going to be built
anyway.

It may **not** accept money in exchange for gating capability in the open build,
for a feature that violates §1, or on terms that give a funder control over the
roadmap. Sponsorship buys goodwill and prioritisation, never authority.
Significant financial relationships are disclosed publicly.

## 8. Relationship with Uptime Kuma

Cooperative, always. Uptime Kuma is an excellent project with a large amount of
accumulated goodwill, and Uptime Cairn reads as a successor in the same lineage
rather than a rival. We position as *"the tool you graduate to, when you need
teams and reporting"* — never as a replacement or a competitor. We credit it
publicly, we ship a flawless importer, and where a fix applies upstream we
contribute it upstream. Committers are expected to hold this posture in public
communication.

## 9. Amending this document

Changes are proposed by pull request and require the §3 supermajority. The
current committers and maintainers are listed in
[`MAINTAINERS.md`](MAINTAINERS.md); that file is the authoritative roster and
changes to it follow §2, not this section.
