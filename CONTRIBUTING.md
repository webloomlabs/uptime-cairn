# Contributing to Uptime Cairn

Thanks for being here. This project's stated goal is to be genuinely sufficient
for everyone from a freelancer with three sites to an enterprise with auditors,
and that is not something one person can get right alone. Outside perspective is
the mechanism, not a nicety.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

---

## Where the project is right now

**Phase 0 — Foundations.** We are writing specifications, not features. The
OpenAPI spec is being written and frozen *before* the code, and the
architectural decisions that cannot be retrofitted are being made now.

That makes this an unusually high-leverage moment to contribute, because the
most valuable contributions today are not code:

1. **Review the API spec.** If you have ever automated a monitoring tool, tell us
   where our spec would frustrate you. The whole project exists partly because
   the incumbent's API was an afterthought and the community wrappers built
   around it are now abandoned. We would rather find our mistakes in a spec
   review than in a v2.
2. **Review the ADRs** in `docs/adr/`. Especially the probe/control-plane split,
   the storage strategy, and the UI state-synchronisation model.
3. **Tell us what breaks at your scale.** If you run 300+ monitors, or shard
   several Uptime Kuma instances across hosts, or produce client uptime reports
   by hand every month — that experience is the most useful thing you can give
   us. Open an issue.
4. **Tell us what is in your Uptime Kuma install.** We are mapping Kuma's schema
   onto our data model on paper, and the thing that breaks a mapping is always
   the case nobody thought of — an exotic monitor type, a notification provider
   with no obvious equivalent, a status page arranged unusually. Describe the
   shape; please do not send us a `kuma.db`, since it holds your URLs and
   credentials. The importer itself ships in Phase 1 and there will be plenty to
   test then.

Code contributions open up properly in Phase 1. See the [roadmap](ROADMAP.md).

## Ways to contribute

- **Bug reports** — what you expected, what happened, version, deployment shape
  (Docker/binary/Pi), and how many monitors you run. Monitor count matters more
  than you would think; several classes of bug only appear at scale.
- **Feature requests** — describe the problem before the solution. "I need to
  prove 99.9% to a client every month" is more useful than "add a PDF button."
- **Documentation** — including the small stuff. A wrong flag in the install
  guide costs more goodwill than a missing feature.
- **Translations** — i18n scaffolding lands in Phase 1; the translation workflow
  will be documented then.
- **Reviewing pull requests** — reviewing other people's work is how people
  become committers here (see [GOVERNANCE.md](GOVERNANCE.md)).

## Contributor License Agreement

Uptime Cairn is licensed under [AGPLv3](LICENSE), and contributions require
signing a CLA. A bot will prompt you on your first pull request.

The CLA lets the project relicense or dual-license in future if its needs
change. Because that asks real trust of you, governance bounds it: relicensing
needs a supermajority plus 30 days' public notice, and **it can never be used to
paywall a feature in the open build**. You keep full copyright in your work.
See [GOVERNANCE.md §6](GOVERNANCE.md).

## Development setup

> Phase 0 note: the application does not exist yet. This section describes the
> intended workflow and will be filled in with exact commands as the code lands
> in Phase 1.

**Prerequisites**

- Go (the version pinned in `go.mod`) — backend and probe
- Node.js LTS — frontend build only
- Docker — for running checks against local test targets
- `make` — the task entrypoint

**Common tasks**

```bash
make setup      # install tooling and hooks
make dev        # run the binary with a local SQLite database
make test       # unit + integration tests
make lint       # linters and formatters
make contract   # verify the API against the frozen OpenAPI spec
make loadtest   # 5,000-monitor synthetic workload + UI benchmark
```

Everything runs locally with no external services. If a change makes that
untrue, it needs an ADR — a hard dependency on Redis or similar is a documented
adoption barrier we have committed not to repeat.

## The rules that are not negotiable

These come straight from the product principles. A PR that violates one will not
be merged, no matter how good it is otherwise:

1. **Sixty seconds to first monitor.** `docker run` → open browser → monitor
   running. If your change adds a step to that path, it needs a very good reason.
2. **No feature is paywalled in the open source build.** Ever.
3. **API-first, literally.** The dashboard is the first API client, not a
   privileged one. No private endpoints that the UI uses and users cannot.
4. **Progressive disclosure.** A solo user must never see an "escalation policy"
   field. New complexity is opt-in and hidden by default.
5. **The client is never sent full state.** Pagination, filtering, and search are
   server-side. This is the specific architectural failure that ejects agencies
   from the incumbent at ~300–600 monitors, and we will not reproduce it.
6. **The UI stays fast at 5,000 monitors.** Enforced by an automated load test in
   CI. A regression here blocks the merge.
7. **Never lose a heartbeat.** Monitoring you cannot trust is worse than none.
8. **Minimal dependency surface.** See below.

## Dependency policy

Users self-hosting infrastructure monitoring are justifiably wary of transitive
supply-chain risk, and they have told us so. Therefore:

- Adding a third-party dependency requires justification in the PR description:
  what it does, why the standard library will not, and what its maintenance
  status is.
- Dependencies are pinned and vendored. Builds are reproducible. An SBOM is
  published with every release.
- Prefer a hundred lines of our own code over a package with a hundred
  transitive dependencies.

## Architecture Decision Records

Structural changes need an ADR in `docs/adr/` before implementation — the probe
protocol, storage, tenancy, the API contract, the UI state model, or anything
that would be expensive to reverse.

Copy `docs/adr/000-template.md`, number it sequentially, and open it as its own
PR for discussion. ADRs are immutable once accepted; if we later change our
minds, we write a new ADR that supersedes the old one, so the reasoning trail
survives for whoever maintains this after us.

## Pull requests

- **Branch from `main`.** Keep PRs focused; one logical change each.
- **Open an issue first** for anything non-trivial, so you do not spend a weekend
  on something that conflicts with a decision already made.
- **Draft PRs are welcome early.** Feedback at 20% is cheaper than at 100%.
- **Commit messages**: [Conventional Commits](https://www.conventionalcommits.org)
  (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`). They drive the
  changelog.
- **Tests are expected.** Bug fixes get a regression test that fails before your
  change. Features get coverage of the failure paths, not just the happy path —
  this is a monitoring tool; the failure paths *are* the product.
- **Update the docs** in the same PR. Documentation that lags the code is how
  self-hosted projects lose people.
- **CI must be green.** Lint, tests, contract tests, and the load-test gate.

### What review looks like

We aim for a **median first response under 48 hours**, and treat that as a
project health metric reported in the monthly notes. If your PR has been sitting
untouched past that, please do ping it — that is us dropping the ball, not you
being impatient.

Review is about the change, never the person. If a reviewer's comment reads as
harsh, assume text is a lossy medium and say so; we would rather fix the tone
than lose the contributor.

Not every PR gets merged. If we decline one, we will explain why, and we will try
to do it before you have sunk a lot of time in — which is why the "open an issue
first" advice above is genuinely in your interest.

## Good first issues

Issues labelled `good-first-issue` are kept genuinely stocked and honestly
scoped. If one turns out to be much larger than its label suggests, say so in
the issue — a mis-scoped starter task is a bug in our triage, and we want to fix
it.

Claim an issue by commenting on it. If life happens and you cannot finish, just
say so and un-claim it; that is completely fine and costs you nothing here.

## Security

**Do not report security vulnerabilities in public issues.** Email
**security@uptimecairn.dev**. You will get an acknowledgement, a fix timeline,
and credit in the advisory unless you would rather stay anonymous.

## A note on Uptime Kuma

Our posture toward Uptime Kuma is cooperative, always. It is an excellent
project, it earned its 88k stars, and Uptime Cairn positions as *"the tool you
graduate to, when you need teams and reporting"* — never as a replacement or a
competitor. We credit it publicly, we ship a flawless importer, and where a fix
we write applies upstream, we contribute it upstream.

Please carry that posture in issues, PRs, and anywhere you represent this
project. Comparisons should be factual and specific ("the Socket.IO state model
sets a ceiling around 300–600 monitors"), never disparaging.

## Questions

Open a GitHub Discussion, or an issue if you are not sure which it is. Asking a
question that turns out to be answered in the docs is useful signal — it means
the docs are hard to find things in.
