# AGENTS.md

Instructions for AI coding agents (Claude Code, Cursor, Copilot, Codex, Aider,
and anything similar) working in this repository, and for the humans directing
them.

**If you are an AI agent: read this file before writing anything. The limits in
[§ What agents may not do](#what-agents-may-not-do) are hard limits, not
preferences.**

---

## The project in brief

**Uptime Cairn** is an all-in-one open source uptime monitoring and reporting
platform, Apache 2.0, designed to serve a freelancer with three sites and an
enterprise with SOC 2 auditors from the same install — no paywalled features, no
separate editions.

- **Status:** Phase 0 — Foundations. There is **no application code yet.** The
  repository currently holds specifications, ADRs, governance, and phase plans.
  Code contributions open properly in Phase 1.
- **Core idea:** one codebase, one binary, progressive disclosure. Solo mode is a
  single static binary with an embedded probe, embedded UI, and SQLite. Scaled
  mode is the same binary with different flags — control plane, remote probes,
  Postgres + Timescale. The upgrade is a config change and a migration, never a
  reinstall.
- **The decisive split:** control plane / probe, over gRPC. It cannot be
  retrofitted, so it was decided in week one — see
  [docs/adr/001-probe-and-control-plane-split.md](docs/adr/001-probe-and-control-plane-split.md).
- **Headline requirement:** 5,000 monitors on one install with the UI staying
  fast, enforced by an automated load test in CI from the first commit.
- **Stack:** Go (backend + probe), SvelteKit + Tailwind (frontend), SQLite or
  Postgres + Timescale, gRPC + Protobuf, Playwright sidecar for browser checks,
  Typst for PDF reports.

Read [README.md](README.md), [ROADMAP.md](ROADMAP.md),
[CONTRIBUTING.md](CONTRIBUTING.md), and [docs/adr/](docs/adr/) before proposing
anything. Everything in CONTRIBUTING.md applies to agent-assisted work
unchanged — the rules that are not negotiable, the dependency policy, the ADR
requirement, Conventional Commits, tests, docs in the same PR.

---

## Why this file exists

The objective is simple and it is not about code quality in the abstract:

> **Every contributor must genuinely understand the code they put their name on.**

This project intends to be maintained for years, by people who have not met each
other, under a governance model with an explicit succession plan. A monitoring
tool is trusted precisely because someone understands its failure paths. Code
that entered the repository without a human who can explain it — line by line,
under review, at 3am during an incident — is a liability no matter how correct
it looks today.

Generated features are the specific hazard. They arrive plausible, complete, and
unowned. They pass review because they look finished, and then nobody can
maintain them, extend them, or reason about what they do when the network
partitions. That is how a project accumulates code with no living understanding
behind it, and it is the failure mode this file exists to prevent.

So: **agents are a tool for the contributor's thinking, never a substitute for
it.**

---

## What agents may not do

These are prohibited. A pull request found to be in breach will be closed
regardless of its quality.

1. **No whole features.** Do not generate a feature end to end — a monitor type,
   an alert channel, an API resource and its handlers, a status page, a
   reporting pipeline, a UI screen and its data layer. Features are designed and
   written by a human who understands them.
2. **No multi-file scaffolding runs.** Do not create or rewrite a set of source
   files in one sweep. If a code change spans several files, the human drives it
   file by file. (Documentation is exempt — see
   [What agents are for](#what-agents-are-for).)
3. **No architectural decisions, and no agent-written ADRs.** The probe
   protocol, storage engine, tenancy model, API contract, UI state model, or
   anything expensive to reverse requires a human-authored ADR, discussed in its
   own PR. An ADR is a record of human reasoning and the trail someone will read
   years from now to understand why we chose what we chose — a generated one
   records nothing. An agent may pressure-test a draft you have written, argue
   the opposing case, or point out an alternative you did not consider; it may
   not draft the ADR or decide its content. This is the one documentation
   carve-out, and it is absolute.
4. **No API surface invention.** The OpenAPI spec is frozen before the code.
   Agents do not add, rename, or reshape endpoints, fields, or error semantics.
5. **No new dependencies.** Adding a third-party package requires human
   justification in the PR description (what it does, why the standard library
   will not, its maintenance status). Agents must not introduce one, and must
   not reach for a package when a hundred lines of our own code will do.
6. **No unattended commits, PRs, or reviews.** No agent-authored PR descriptions
   passed off as the contributor's, no autonomous merges, no bot approvals
   standing in for human review.
7. **No generated code the contributor cannot explain.** This is the catch-all,
   and the one that actually matters. If you could not defend a line in review
   without re-reading it, it does not go in.

---

## What agents are for

Encouraged, and genuinely useful:

- **Small, bounded functions.** A parser, a comparator, a retry helper, a
  formatting routine — something you have already specified and could have
  written yourself, where the agent saves typing rather than thinking. You read
  every line, you adjust it to fit the surrounding code, you own it.
- **Planning and design exploration.** Sketching approaches, listing trade-offs,
  finding the failure paths you have not considered, poking holes in your own
  design before a reviewer does.
- **Debugging.** Reading stack traces, forming hypotheses, narrowing down where a
  heartbeat went missing, explaining why a query is slow.
- **Understanding the codebase.** Asking what something does, tracing a call
  path, orienting yourself in an unfamiliar package.
- **Tests.** Enumerating edge cases and failure paths — with your judgement over
  what is worth asserting. This is a monitoring tool; the failure paths *are*
  the product, and they deserve tests you understand.
- **Review preparation.** Catching typos, unclear naming, missing doc updates,
  Conventional Commit slips before a human reviewer spends time on them.
- **Documentation — including writing it outright.** Prose is the one place the
  restrictions above do not apply. Agents may draft install guides, how-tos,
  reference pages, changelog entries, READMEs, and clarity passes over existing
  docs, whole files at a time. Documentation that lags the code is how
  self-hosted projects lose people, and lowering the cost of writing it is a
  straightforwardly good use of these tools.

  Two conditions. **A human verifies every factual claim** — a wrong flag in an
  install guide costs more goodwill than a missing feature, and a confidently
  invented flag is exactly what these tools produce. And **ADRs are excluded**;
  see below.

The line, stated once: **an agent may help you write code you understand; it may
not write code for you. Docs are the exception — write those freely, then check
that every claim in them is true.**

---

## Rules for agents operating in this repository

If you are an agent reading this:

1. **Ask before you write code.** Confirm scope with the human before creating
   or substantially editing a source file. When the request is larger than a
   small function, say so and propose the smaller step instead. Documentation
   needs no such throat-clearing — if you are asked for a doc, write it.
2. **Prefer explaining to producing — in code.** Where a well-aimed explanation
   would leave the contributor able to write it themselves, give the
   explanation. Prose is the opposite: a finished draft they can edit beats a
   description of what the draft should say.
3. **Refuse gracefully.** If asked to build a whole feature, decline in one
   sentence, cite this file, and offer the parts you may legitimately do —
   planning, a specific helper, tests, review, documentation.
4. **Stay inside the current phase.** Phase 0 is specification work. Do not
   scaffold an application that the project has deliberately not started.
   Writing or improving docs is in scope in any phase.
5. **Do not silently widen scope.** No opportunistic refactors, no reformatting
   untouched files, no "while I was in here" changes.
6. **Surface uncertainty.** Say plainly when you are unsure, when something
   conflicts with an ADR, or when you are guessing at intent. Confident wrong
   output is the expensive failure here.
7. **Never touch these without explicit human instruction:** [LICENSE](LICENSE),
   [GOVERNANCE.md](GOVERNANCE.md), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md),
   [MAINTAINERS.md](MAINTAINERS.md), accepted ADRs (immutable once accepted — a
   change means a new superseding ADR), the frozen OpenAPI spec, and CI
   configuration including the load-test gate.
8. **Security work is human-led.** Do not generate authentication, session,
   crypto, or access-control code. Vulnerabilities go to
   **security@uptimecairn.dev**, never into a public issue.

---

## Rules for contributors using agents

- **You are the author.** The CLA you sign covers what you submit; you are
  representing that you understand and have the right to contribute it. Agent
  assistance does not change that, and it does not dilute your copyright.
- **Disclose meaningfully.** If an agent contributed materially to a PR, say so
  in the description — one line, no ceremony. This is context for reviewers, not
  a confession.
- **Review is on you first.** Read the generated code as adversarially as a
  reviewer will. Delete what you do not need. Rewrite what does not match the
  surrounding style.
- **Generated docs still need a fact-check.** Drafting prose with an agent is
  fine and expected; shipping a command, flag, path, or version number nobody
  ran is not. Verify the specifics, then submit it as your own writing.
- **Expect to be asked.** Reviewers may ask you to explain any line. "That is
  what the model produced" is not an answer, and a PR that stalls on that
  question will be closed rather than debugged.
- **Slower is fine.** This project is optimised for being maintainable in five
  years, not for shipping a feature this weekend. Volume of code is not the
  metric anyone here is graded on.

---

## Questions

If a task feels like it sits on the line, it probably does — open a GitHub
Discussion or an issue and ask. Getting that wrong in public costs the project
far less than getting it wrong quietly in a merge.
