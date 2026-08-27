# ADR-009: Pagination Key — Generalise the Cursor to `(sort_field, id)`, with Sortable Fields Restricted to Stable Stored Columns

- **Status:** Proposed
- **Date:** 2026-08-27
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** **Supersedes [ADR-004](004-ui-state-synchronisation.md) in one part only** — the pagination key. ADR-004's three other decisions (ID-scoped live subscriptions, membership reconciliation by polling, and the two load-test invariants) remain in force, unmodified, and this decision is constrained by them.

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

**Recorded for the repository's own history:** [AGENTS.md](../../AGENTS.md) §3 places
ADRs with a human and describes that restriction as absolute. It was explicitly
waived for this document by the project maintainer on 2026-08-27, on the same
terms as ADR-006, ADR-007 and ADR-008, following the precedent at
[data model §11.6](../data-model/README.md#116-secrets-at-rest). The decision below
is the maintainer's; the drafting is not.

## Context

[ADR-004](004-ui-state-synchronisation.md) fixed cursor pagination on
`(updated_at, id)`, and it was right to. Its stated reason still holds: offset
pagination gets into reordering problems when rows change status in real time, and
a keyset cursor does not.

What was not costed at the time is the consequence, which
[docs/api/README.md](../api/README.md) open question 8 recorded rather than quietly
worked around: **a keyset cursor can only paginate the ordering it is keyed on.**
So `sort` on `listMonitors` collapsed from four options — name, status,
`last_check_at`, `uptime_24h` — to one axis, and the spec today reads:

```yaml
schema:
  type: string
  default: -updated_at
  enum: [updated_at, -updated_at]
```

**A dashboard listing 5,000 monitors cannot be sorted alphabetically.** That is not
a cosmetic loss. Filter and search are different tools for a different job: search
answers "find the monitor called acme-api", and sorting answers "show me this
client's forty monitors in an order a human can scan and return to". The persona
Phase 1 exists to win — the agency with a thousand monitors — is precisely the one
that feels it, and "most recently changed first" is a poor primary ordering for a
list somebody reads down.

The store encodes the current key literally: `Cursor` in
[`internal/store/store.go`](../../internal/store/store.go) is a struct of
`UpdatedAt` and `ID`, base64-encoded as `<millis>.<id-hex>` with no field name in
it, and the monitors table carries four `(org_id, …, updated_at DESC, id DESC)`
indexes shaped for exactly that one ordering.

## Decision

**The cursor generalises from `(updated_at, id)` to `(sort_field, id)`.** Keyset
pagination is retained in full; only the choice of leading column becomes a
parameter.

1. **Sortable fields are a closed, enumerated set of stable, stored, `NOT NULL`
   columns.** For monitors: `name`, `updated_at`, `created_at`. Each is available
   ascending or descending. Nothing else is sortable, and the set grows only by
   decision.

2. **`status` and `uptime_24h` are refused, and permanently.** This is the
   substantive narrowing against what open question 8 implied, and the reasoning
   is the same one that licenses sorting by name in the first place. The argument
   for name is that *names do not change in real time*, so ordering by one does not
   reintroduce the churn ADR-004 avoided. That argument does not extend to status:
   a monitor going down while somebody paginates jumps position, and the reader
   then sees it twice or never — which is the exact defect ADR-004 chose keyset
   pagination to eliminate. `uptime_24h` fails on a second ground as well: it is
   computed at read time, not stored, so there is no column to key a cursor on and
   no index to seek. Offering either would trade a real invariant for a menu item.

3. **The cursor is self-describing and validated against the request.** It encodes
   which field and direction it was issued for. A request whose `sort` disagrees
   with its `cursor` is `400`, never a silent reset to the first page — the same
   reasoning `DecodeCursor` already gives for a malformed cursor, that a silent
   reset "would loop forever".

4. **Every sortable field carries `(org_id, <field>, id)` in the matching
   direction**, and this is why the set stays small: each member is a permanent
   index on the busiest table in the schema, and the data model's index budget is
   already strained.

5. **Collation is pinned, in the index and in the query, and asserted across
   backends.** SQLite's default `BINARY` collation sorts `Zebra` before `apple`,
   which users report as a bug; the schema currently specifies no collation
   anywhere. Name ordering therefore uses a case-insensitive collation, declared
   identically on the index and in the `ORDER BY` — an index built under one
   collation and queried under another is simply not used, which turns a
   correctness decision into a silent performance cliff. Because
   [ADR-002](002-storage-engine.md)'s repository interface only holds if both
   backends produce the same answers, **a contract test asserts that SQLite and
   Postgres return an identical order for a fixed mixed-case fixture.** Ordering of
   non-ASCII names is not guaranteed identical across backends and is out of scope,
   consistent with i18n being scaffolded rather than delivered.

6. **This is opt-in per endpoint, and monitors is the only one that opts in now.**
   ADR-004 applied one cursor uniformly across seventeen collections and that
   uniformity of *mechanism* is preserved — every list still paginates by keyset,
   with no small-install exception. What varies is the sortable set per resource,
   which is empty for every collection except monitors until somebody demonstrates
   a need.

7. **ADR-004's load-test invariants govern this decision.** Client payload bounded
   by viewport, server cost flat as total monitor count grows. A sort that cannot
   hold them is not shipped.

## Consequences

**What this makes easy.**

A five-thousand-monitor list becomes readable: alphabetical order, stable across
pages, with the cursor doing the same job it did before. The agency persona gets
the ordering it actually asked for without the client ever receiving full state.

Nothing about live updates, membership reconciliation, or the summary channel
changes. Those are ADR-004's hard half and this decision does not touch them.

Widening the `sort` enum is an **additive** API change — no existing client sends a
value it did not previously send, and none breaks. It is therefore safe under
`/api/v1` even after the spec freezes, which is a useful worked example for
[COMPATIBILITY.md](../api/COMPATIBILITY.md).

**What this makes hard, or forecloses.**

*The index budget grows on the hottest table.* Two new indexes on `monitors`, and
they are permanent: an index supporting a published sort option cannot be dropped
without removing the option, which is a breaking change.

*Filtered-and-sorted is the real risk to the 5,000 gate.* Filter × sort
combinations cannot each have an index without a combinatorial explosion. The base
sort index serves unfiltered sorts; a filtered sort seeks that index and discards
non-matching rows. A selective filter is fine. An **unselective** one — `enabled=true`
matching 4,900 of 5,000 while sorting by name — degrades toward a scan. The load
gate must cover filter-plus-sort at full size, and if it regresses the answer is to
**narrow the sortable set, not to keep adding indexes**.

*Existing cursors become invalid.* The encoding changes to carry its field, so a
cursor issued by an earlier build no longer parses. In-flight pagination breaks
across an upgrade. The old two-part form should be accepted for one release and read
as `updated_at`-keyed, after which it is refused — this is a real compatibility
event on a token the API documents as opaque, and it is the reason cursor opacity is
worth stating as a non-promise rather than assumed.

*Duplicate names paginate in id order.* Two monitors called `api` appear in an order
the user cannot predict. Correct, stable, and occasionally surprising; the `id`
tiebreaker is what makes the cursor work at all.

*Collation is a permanent cross-backend commitment.* Changing it later reorders
every list and invalidates the index built under the old one.

**What becomes expensive to reverse.**

The sortable set is a public enum on a request parameter. **Adding a member is
additive and cheap; removing one is breaking and needs `/api/v2`.** So the set
should start at the three fields above and grow only on evidence — the asymmetry is
the whole reason to be conservative at the outset.

The cursor encoding change has its point of no return at the release that ships it,
and the one-release grace window above is the only opportunity to soften it.

**What this requires that is not in this ADR.** A migration adding the new indexes
with their collation, the store-layer cursor rework, and the widened `sort` enum in
the spec. Those are implementation and belong in their own pull requests, driven by
a human, per [AGENTS.md](../../AGENTS.md) §1 and §2.

## Alternatives considered

**Accept the restriction — filter and search instead of sorting.** Open question
8's first option, and defensible on its face: "most recently changed first" is
arguably what a monitoring dashboard wants. It lost because the two tools do
different jobs. Search finds a monitor you can already name; sorting makes a list of
forty scannable by somebody who cannot. The install this project exists to serve
that Uptime Kuma cannot — the agency at a thousand monitors — is exactly the one for
whom an unsortable list is a daily cost, and accepting the restriction would have
been accepting it on their behalf to avoid amending an ADR.

**Sort a page client-side.** Rejected in open question 8 and rejected again here: it
orders twenty-five rows out of five thousand and presents as a bug, because it looks
like sorting and is not.

**Offset pagination for sorted views only.** Would give every sort option for free.
It loses on the entire premise of ADR-004 — offset pagination reorders under
concurrent writes, and a monitoring list is the most write-active list in the
product. Reintroducing it for the sorted path would mean the defect appears only
when a user sorts, which is worse than uniform behaviour because it is harder to
diagnose.

**A materialised rank or sort-key column.** Would make any ordering indexable
uniformly. It lost on write cost: the rank of every row after an insert changes, so
a monitor rename would touch an unbounded number of rows on the hottest table.

**Allowing `status` sorting behind a snapshot.** Freeze the ordering at first page
by materialising the result set, then paginate the snapshot. It genuinely works and
is how some products do it. It lost on server state — a per-client, per-view
temporary result set is precisely the kind of server-side session state ADR-004's
design avoids, and it would need eviction, sizing, and a story for what a stale
snapshot shows. Not worth it for one sort option that a status *filter* serves
better anyway.

## Compliance with the product principles

- [x] **Sixty seconds to first monitor is preserved.** No new configuration; sorting
      is a query parameter with an unchanged default.
- [x] **Nothing is paywalled in the open source build.** Sorting ships to everyone.
- [x] **API-first.** `sort` is a documented parameter on the public list endpoint;
      the dashboard uses exactly what a client does.
- [x] **Progressive disclosure.** The default ordering does not change, so a user who
      never touches sorting sees what they saw before.
- [x] **The client is never sent full state; the UI stays fast at 5,000 monitors.**
      The constraint this decision is most exposed to. Keyset pagination is retained
      in full, page size stays capped, and the filtered-and-sorted path is named
      above as the case the load gate must prove rather than assume.
- [x] **Solo mode keeps zero required external dependencies.** Unchanged.
- [x] **Dependency surface stays minimal.** Nothing added; two indexes and a wider
      enum.

## References

- [ADR-004](004-ui-state-synchronisation.md) — the pagination key this supersedes,
  and the three decisions it does not touch.
- [ADR-002](002-storage-engine.md) — the two-backend repository contract that makes
  the collation assertion in decision 5 necessary.
- [docs/api/README.md](../api/README.md) open question 8 — where the regression was
  recorded rather than worked around, and the three options weighed here.
- [`internal/store/store.go`](../../internal/store/store.go) — `Cursor`,
  `DecodeCursor`, and the "silent reset would loop forever" reasoning decision 3
  reuses.
- [`migrations/sqlite/0001_initial.sql`](../../migrations/sqlite/0001_initial.sql) —
  the four existing `(org_id, …, updated_at DESC, id DESC)` monitor indexes, and
  `name TEXT NOT NULL` with no collation declared.
- [docs/api/COMPATIBILITY.md](../api/COMPATIBILITY.md) — where the additive-versus-
  breaking classification this ADR's changes are examples of is written down.
- Open follow-up: revisit the sortable set only on evidence — a user request naming
  a field, not a guess — given that adding is cheap and removing is not.
