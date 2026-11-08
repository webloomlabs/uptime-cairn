# API compatibility and the spec change process

This is the document [openapi.yaml](openapi.yaml) means when it says the spec is
"immutable except through the documented change process", and the one
[GOVERNANCE.md](../../GOVERNANCE.md) §4 means by "an explicit deprecation policy".
Four other places promise it too — [PHASE-0-PLAN.md](../plans/PHASE-0-PLAN.md) §5.6
and its risk table, and [PHASE-1-PLAN.md](../plans/PHASE-1-PLAN.md) §4.2. Until now
none of them was it.

It answers one question: **the spec is frozen — what do I do when I need to change
it?**

---

## 1. The freeze binds shipped surface, not the file

This is the rule everything else depends on, and it is the one most easily got
wrong.

`openapi.yaml` is a single file containing three different kinds of thing:

| | What it is | Can it change? |
|---|---|---|
| Operations a stable release implements | A contract somebody's script depends on | **Frozen.** This document governs it. |
| Operations tagged `x-cairn-phase: 2` or `3` that no stable release implements | A design draft that happens to live in the spec file | **Editable.** Nobody can depend on it. |
| `/healthz`, `/readyz`, `/metrics` | Operational endpoints outside `/api/v1` | Versioned separately; see §7 |

Every operation already carries `x-cairn-phase`, and CI already selects on it —
Phase 1 contract tests run against `x-cairn-phase: 1`. **That extension plus the
release that implemented the operation is what decides whether a change is
expensive.**

An operation becomes frozen the moment a stable release implements it. Not when it
is written, not when the file is declared frozen.

**A pre-release does not freeze.** The freeze attaches at the first stable tag:
`1.1.0` binds, `1.1.0-beta.1` and `1.1.0-rc.1` do not. A pre-release exists to
collect the feedback that changes a surface, and one that froze on contact would
make that feedback unusable — the answer to every report about the shape of an
endpoint would be `/api/v2`, which is precisely the conversation a beta is
published to avoid having. The cost is carried by the person who takes the
pre-release, and the tag is what tells them they are taking it: an operation
reached from a `-beta` or `-rc` build is a draft, and may move before it binds.

> **Worked example.** The Phase 2 reporting merge of 2026-08-27 removed
> `ReportRun.share_url`, replaced `ReportTemplate.branding` with `brand_profile_id`,
> tightened `sla_target` from `maximum: 100` to `exclusiveMaximum: 100`, and added
> members to two response enums. Against shipped surface, every one of those is a
> `/api/v2` conversation. All of them were free, because every affected operation is
> `x-cairn-phase: 2` and no release implements it. There was no client to break.

If the whole file were frozen equally, that reconciliation would have been
impossible and the Phase 2 surface would have shipped carrying contradictions with
three accepted ADRs. Freezing the file rather than the contract makes the spec
harder to improve without making it safer to depend on.

---

## 2. Classify every spec change

Every pull request touching `openapi.yaml` states its class in the description.
There are three, and only the third is expensive.

### Additive — safe under `/api/v1`, always

- A new endpoint.
- A new **optional** request field, query parameter, or header.
- A new response field.
- A new member of a **request** enum. Widening what a client may send breaks
  nobody, because no existing client sends the new value.
- A new member of a **response** enum **only where the schema has pre-declared
  tolerance** — see §3.
- A relaxed constraint: a wider range, a longer `maxLength`, a field becoming
  optional.
- A new optional `include`, filter, or sort value.

### Breaking-but-unshipped — free, but say so

A change that would be breaking if the operation had shipped, made to an operation
no release implements. Free to make. Still classified, so a reviewer can check the
"unshipped" claim rather than take it on trust, and so the class is not confused
with genuinely additive change later.

### Breaking-and-shipped — needs `/api/v2`

Per [GOVERNANCE.md](../../GOVERNANCE.md) §4, breaking the `/api/v1` contract
"requires a major version and a migration path".

- Removing or renaming an endpoint, field, parameter, or enum member.
- Making an optional field required.
- Tightening validation: a narrower range, a shorter `maxLength`, a stricter
  pattern, a new `required` entry.
- Changing a type, a default that alters behaviour, or the meaning of an existing
  value.
- Changing an error status code or the `type` URI of a problem document.
- Adding a member to a **response** enum that carries no tolerance note — a
  generated client that exhaustively switches on it will fail to compile or will
  fall through.

---

## 3. The enum tolerance rule

An enum in a response is a promise about what a client will receive. Growing one is
breaking unless the schema said in advance that it might grow.

The spec already does this correctly in one place, and it is the model:

```yaml
ApiKeyScope:
  description: |
    A permission grant. Scopes are `<resource>:<read|write>`; `write` implies `read`
    on the same resource. Clients must tolerate scopes added in later releases.
```

That last sentence is why adding `brand_profiles:read` and `brand_profiles:write`
was additive rather than breaking. **Any response enum expected to grow must carry
an equivalent sentence before it ships.** Adding the sentence later does not help:
by then the clients exist.

Enums that should carry it: scopes, monitor types, notification channel types,
report formats, error `type` URIs. Enums that should **not** — because a client
genuinely must handle every case — are small closed sets like monitor status, where
silent growth would produce a client that renders an unknown state as nothing.

---

## 4. Deprecation

A breaking change becomes a scheduled change through deprecation. The spec has a
first-class field for it and currently uses it zero times.

1. **Mark it.** `deprecated: true` on the operation, parameter, or schema, with a
   description saying what replaces it and from which release.
2. **Announce it.** A `### Deprecated` entry in [CHANGELOG.md](../../CHANGELOG.md)
   for the release that marks it, written for the person deciding whether to
   upgrade.
3. **Serve it.** The deprecated surface keeps working, unchanged, for the whole
   window.
4. **Signal it at runtime.** Responses from a deprecated operation carry
   `Deprecation: true` and a `Sunset` header with the date it stops working, so an
   integration finds out from its own logs rather than from an outage.
5. **Remove it** no sooner than **two minor releases and ninety days** after the
   marking release, whichever is later — and removal is still a breaking change,
   so it happens in `/api/v2`, not in a `v1` minor.

Deprecation does not convert a breaking change into an additive one. It converts a
surprise into a schedule.

---

## 5. What is not promised

Stating these plainly is what makes the rest of the promise affordable.

- **Cursor contents.** Cursors are opaque base64 and the API says so. Their internal
  shape is not a contract, and it changes — [ADR-009](../adr/009-pagination-sort-key.md)
  changes it to carry its sort field. A client that decodes a cursor has taken a
  dependency the API never offered. **A cursor issued by an older build may stop
  parsing across an upgrade**; the only commitment is that a cursor the server
  cannot read is rejected explicitly rather than silently reset to page one.
- **Ordering not established by an explicit `sort`.** Where no order is documented,
  none is promised.
- **Absolute performance.** The load-test gate is a project commitment, not an API
  contract term.
- **Field ordering in JSON objects**, and the exact prose of `detail` strings in
  problem documents. The `type` URI and the status code are the contract; the
  sentence is for a human.
- **The probe protocol.** gRPC and Protobuf under [ADR-001](../adr/001-probe-and-control-plane-split.md),
  with its own compatibility rules. It is not part of `/api/v1`.
- **Unshipped future-phase surface**, per §1.

---

## 6. The `/api/v2` bar

`/api/v2` is not where mistakes go to be tidied. It is expensive for everyone who
integrated, and [PHASE-0-PLAN.md](../plans/PHASE-0-PLAN.md)'s own risk table frames
the freeze as being affordable *because* "perfection is what `/api/v2` is for" —
which only works if v2 is rare.

A major version is justified when the accumulated deprecations make the surface
incoherent, or when a change is required that no deprecation window can soften. When
it comes:

- Both versions serve concurrently for at least one release cycle.
- A written migration path, per resource, in the docs — not only a changelog entry.
- `/api/v1` gets its own `Sunset` date, announced at the same time v2 ships.

---

## 7. Endpoints outside the versioned surface

`/healthz`, `/readyz` and `/metrics` sit outside `/api/v1` by design. They are
operational, consumed by orchestrators and scrapers rather than by integrations, and
their compatibility expectation is different: the *existence* and the status-code
semantics of the health endpoints are stable, and the metric names follow Prometheus
convention with renames announced in the changelog. They are not covered by §2.

---

## 8. Pull request checklist

For any change to `openapi.yaml`:

- [ ] The class is stated: **additive**, **breaking-but-unshipped**, or
      **breaking-and-shipped**.
- [ ] For breaking-but-unshipped: the `x-cairn-phase` tag is named, and the claim
      that no release implements it is checkable.
- [ ] For breaking-and-shipped: there is a `/api/v2` plan and a migration path, or
      the change does not go in.
- [ ] Any new response enum expected to grow carries the tolerance sentence from §3.
- [ ] `npx @redocly/cli@2 lint docs/api/openapi.yaml` passes with no new warnings.
- [ ] `go run ./tools/apidoc` has been run and `docs/api/reference.md` committed —
      CI checks this rather than doing it for you, so that the diff a reviewer reads
      is the diff the author wrote.
- [ ] Both generated clients still generate (CI proves this on every pull request).
- [ ] Contract tests pass against the implemented surface.
- [ ] Deprecations, if any, carry `deprecated: true`, a changelog entry, and a
      sunset date.

---

## 9. Worked examples

| Change | Class | Why |
|---|---|---|
| Adding `/api/v1/brand-profiles` | Additive | New endpoint; nothing existing moves. |
| Adding `brand_profiles:read` to `ApiKeyScope` | Additive | The schema pre-declared that scopes grow (§3). |
| Widening `sort` to `[name, -name, …]` per ADR-009 | Additive | A **request** enum. No client sends a value it did not send before. |
| Adding `slo_target_percent` to `Monitor` | Additive | New optional field on request and response alike. |
| `ReportRun.share_url` → a `share` object | Breaking-but-unshipped | A removal — free only because `x-cairn-phase: 2` is unimplemented. |
| `sla_target`: `maximum: 100` → `exclusiveMaximum: 100` | Breaking-but-unshipped | Tightened validation; a client sending exactly `100` starts failing. |
| `ReportRunState` gaining `partial` | Breaking-but-unshipped | Response enum growth with no tolerance note. Add the note before it ships. |
| The ADR-009 cursor re-encoding | Not a contract change | Cursor contents are explicitly not promised (§5) — but the one-release grace window is still owed to anyone mid-pagination. |
| Removing a monitor type from the `type` enum | Breaking-and-shipped | `/api/v2`, with a migration path for anyone running that type. |
