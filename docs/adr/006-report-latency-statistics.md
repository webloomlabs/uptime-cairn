# ADR-006: Report Latency Statistics — Exact Aggregates Only, with a Real Percentile Confined to Raw Retention

- **Status:** Accepted
- **Date:** 2026-08-27
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** **Extends** [ADR-002](002-storage-engine.md). The rollup tiers and the repository interface it fixed are unchanged by this decision; nothing here alters the schema.

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.

**Recorded for the repository's own history:** [AGENTS.md](../../AGENTS.md) §3 places
ADRs with a human and describes that restriction as absolute. It was explicitly
waived for this document by the project maintainer on 2026-08-27, in the same
manner and for the same kind of reason as the waiver recorded at
[data model §11.6](../data-model/README.md#116-secrets-at-rest). The decision
below is the maintainer's; the drafting is not. It is offered for review as a
draft to be argued with, and the reasoning is written out in full precisely so
that it *can* be argued with rather than accepted on the strength of looking
finished.

## Context

[Data model §11.5](../data-model/README.md#115-percentile-strategy) settled the
percentile strategy for Phase 1 and left an explicit instruction for this phase:

> **Phase 2 must not build an SLO or error-budget calculation on coarse-tier
> percentiles** without revisiting this. Error budgets computed from an
> approximation are an approximation, and the report will not say so unless
> someone makes it.

This ADR is that revisiting. Phase 2 makes reporting a first-class subsystem, and
a client-facing report is exactly the artifact §11.5 was worried about: a number
in a PDF sent to somebody who will not be in a position to ask how it was
computed.

**What the storage layer can and cannot answer today.** Three layers, and they do
not agree about what a percentile is:

- **Raw heartbeats.** [`UptimeFromRaw`](../../internal/store/sqlite/history.go)
  computes a genuine nearest-rank p95 over the whole requested window in one pass.
  This is the only true window-level percentile in the system. Raw is retained
  seven days by default ([`DefaultRetention`](../../internal/rollup/rollup.go)).
- **The 1m tier.** [`RollUpRaw`](../../internal/store/sqlite/rollup.go) computes a
  real per-minute p95 from raw, at rank `(n*95+99)/100` in integer arithmetic so
  the rank is reproducible on both backends. Retained thirty days.
- **The 5m, 1h and 1d tiers.** [`RollUpTier`](../../internal/store/sqlite/rollup.go)
  stores `MAX(response_time_p95)` of the sub-buckets. The column is populated, and
  [`HistoryFromTier`](../../internal/store/sqlite/history.go) then substitutes the
  literal `NULL` for it on read, because the frozen API schema has no field in
  which to say "this one is an estimate."

So the accurate statement of the problem is narrower and worse than "no percentile
beyond seven days": **there is no true percentile over any window longer than a
single one-minute bucket, except inside raw retention.** A true p95 for last March
does not exist at any resolution and never did.

**Why it cannot be assembled from what is stored.** `sum` and `count` merge
because addition is associative — which is precisely why the tiers store a sum
and a count rather than a mean, as the comment in
[`rollup.go`](../../internal/rollup/rollup.go) says. `min` and `max` merge because
they are idempotent. A quantile is a *rank* statistic: its value depends on the
ordering of the whole combined population, and the quantiles of the parts
constrain the quantile of the union only very loosely.

A worked case, two buckets of a hundred samples each:

- Bucket A: all 100 samples at 10 ms → p95 = 10 ms.
- Bucket B: 90 samples at 10 ms, 10 at 1000 ms → rank 95 falls in the tail →
  p95 = 1000 ms.
- `MAX` of the two is **1000 ms**. The true p95 of the 200 combined samples sits
  at rank 190, which is still **10 ms**.

A hundredfold overstatement from two buckets. In fairness to the stored value,
`MAX`-of-p95 is a *provable upper bound* rather than a guess: each bucket has at
least ⌈0.95·nᵢ⌉ samples at or below its own p95, hence at or below the maximum M,
so the union has at least ⌈0.95·N⌉ samples at or below M and its p95 cannot exceed
it. The bound is sound. It is also arbitrarily loose, which is why serving it
would mislead.

**What the reports actually need.** The exit condition for Phase 2 is an agency
sending fifty branded client reports on the first of the month. The figures that
have to be defensible in that document are uptime against a target, an error
budget, an incident narrative, and a response-time story the client recognises.
Only the last of those was blocked, and only in its tail.

## Decision

**Phase 2 reports are computed exclusively from statistics that are exact over an
arbitrary window from the stored rollup tiers. The monthly percentile is not
provided, not approximated, and not implied.**

Concretely, five figures, and no others in the latency block:

1. **Average over the report window.** Computed as `SUM(response_time_sum) /
   SUM(response_time_count)` across the buckets in range. Exact, not an
   approximation of an average — sum and count are additive at every tier, so the
   value equals what a pass over raw heartbeats would have produced, at any window
   length and at any tier.
2. **The daily average series** — one point per day over the window, read from the
   1d tier. Thirty-one numbers for a month, three hundred and sixty-six for a
   year. This is the report's primary latency exhibit.
3. **Best and worst day, by daily average**, taken from that series.
4. **Days over target** — the count of days whose daily average exceeded the
   report's response-time target, and the dates of those days. Present only when a
   target is configured.
5. **A real p95 over the trailing seven days**, computed by `UptimeFromRaw`,
   guarded by [`RawCovers`](../../internal/store/sqlite/history.go), labelled with
   its own window, and rendered in a block visually separate from the
   window-length figures.

**Window-level minimum and maximum are not reported.** Day-level extremes replace
them.

**The stored coarse-tier `response_time_p95` remains unserved.** `HistoryFromTier`
continues to substitute `NULL` beyond the 1m tier. No API field is added to label
the approximation, and no report surfaces it.

**Error budgets and SLO calculations are computed from uptime counts, never from
percentiles.** `up_count` and `down_count` are additive at every tier, so an error
budget over any window is exact. This is the direct answer to the instruction
§11.5 left for this phase: the calculation it warned against is not built, rather
than built and disclaimed.

**The mergeable latency histogram is deferred, not rejected.** See *Alternatives*
below for what it was and *Consequences* for the trigger that should reopen it.

### A note on latency targets that is easy to get wrong

A response-time threshold breach already marks a check **down** —
[`http.go`](../../internal/probe/check/http.go) sets `StatusDown` with
`ClassAssertion` when the elapsed time exceeds `ResponseTimeThresholdMs`. So for
any monitor carrying a threshold, `up_count / (up_count + down_count)` is already
a latency service level indicator: exact, additive, and available over any window
for as long as the tiers are retained. Reports may use it.

They must phrase it carefully. `Class` is **not persisted** — it is dropped at the
[observation boundary](../../internal/observation/observation.go), which renders
`protocol.Check` from `check.Observation` and carries `Code` and `Message` but not
`Class`. Stored history therefore cannot distinguish "too slow" from "did not
answer". The figure is honestly described as *"met the response-time target"* and
is dishonestly described as *"was slow"*, because the denominator includes genuine
outages. That phrasing constraint is part of this decision, not a detail of
implementation.

## Consequences

**What this makes easy.**

No schema change, no migration, no backfill, and no data discontinuity at upgrade.
Every figure above is a `SUM`, `MIN` or `MAX` over columns that exist and are
populated for all history the install has ever retained. A report run on the day
of upgrade covers the same range as one run a year later.

The [ADR-002](002-storage-engine.md) repository contract is untouched. All four
window statistics are pure aggregations, so SQLite computing them here and
Timescale computing them as continuous aggregates produce identical numbers by
construction rather than by careful matching — which is the property
[`rollup.go`](../../internal/rollup/rollup.go) says the interface most needs.

**Group- and tag-level averages become available**, because sum and count merge
across monitors exactly as they merge across time. An agency report can state a
client's whole-estate average response time. It cannot state that estate's p95,
at any window, for the same reason it cannot state a monthly one.

Month 4 of the Phase 2 plan is unblocked. The hardest of the three decisions that
gated implementation resolves without new storage design.

The daily series is a better exhibit than the statistics it replaces, not merely a
cheaper one. A worst-*day* average is stable — it takes a sustained degradation to
move it — where a worst-*check* maximum moves on a single garbage-collection pause.

**What this makes hard, or forecloses.**

*No tail statistic over the report window.* The question "what did the slowest five
per cent of checks look like last quarter" has no answer and will not acquire one
under this decision. An average conceals exactly the behaviour a client complains
about: a service at 100 ms with two per cent of checks at 5 s averages about
198 ms, which describes nobody's experience.

*A contractual p95 SLO cannot be reported on beyond seven days.* This is the
sharpest limitation and the one most likely to arrive as a bug report. If a client
contract specifies "p95 under 400 ms", Uptime Cairn can report compliance for the
trailing week and not for the month the contract is billed on. The honest response
is that the histogram exists as a design and this decision deferred it; the
dishonest response is a number derived from `MAX`-of-p95.

*Comparative reporting compares means, not tails.* Period-over-period and
monitor-against-monitor comparisons will move on shifts in the body of the
distribution and stay flat on shifts in the tail. Degradation usually shows in the
tail first, so the comparison is a lagging one.

*The daily series inherits 1d retention.* It is unbounded by default —
`Rollup1dDays` is zero, indefinite, described in
[`rollup.go`](../../internal/rollup/rollup.go) as the long history the reporting
engine sells — but an operator who sets a finite value truncates their own report
history. The report must state the range it actually covered rather than the range
requested.

*The weekly p95 depends on a setting the operator controls.* `RawDays` is
configurable with a floor of one. `RawCovers` must gate the figure and the report
must omit it, labelled, when coverage is short. Printing a p95 over three days
under a seven-day heading is precisely the failure this ADR exists to prevent.

*The weekly p95 is the most expensive query in the report path.* Roughly ten
thousand raw rows per monitor at a sixty-second interval, as a bounded index range
scan per monitor, multiplied by every monitor in the report and concentrated in
the 09:00 burst on the first of the month. It belongs in the month-7 load gate as
a measured figure and not as an assumption.

**What becomes expensive to reverse later.**

Unusually little, and that is a substantive part of the argument for deferring.
Adding the histogram later is purely additive: new columns, populated forward from
the release that introduces them. Nothing written under this decision has to be
unwritten.

The genuinely irreversible commitment in the histogram design is the **choice of
bucket boundaries** — once installs hold data, changing them invalidates the
history they hold. By not choosing them now, under Phase 2 schedule pressure and
without measured storage figures from a real 5,000-monitor install, we decline to
make a permanent decision on a provisional basis. The point of no return belongs
to the histogram's own ADR, at the first release that ships boundaries.

The real cost of deferral is retrospective and should be stated plainly: **history
accumulated between now and the histogram's introduction will never acquire
percentiles.** An operator who upgrades in 2027 gets tail statistics from 2027
onward and never for 2026. That is the price, it is paid by early adopters, and it
is the strongest argument the other way.

**The trigger to reopen this.** Any of: a paying or prospective user whose contract
specifies a percentile SLO over a monthly or quarterly window; more than a small
number of reports of the tail-blindness above; or the Phase 4 Timescale path
arriving, at which point `uddsketch` exists natively on one backend and the
question becomes whether SQLite can be made to agree with it rather than whether
percentiles are worth storing.

## Alternatives considered

**Log-spaced latency histogram columns in the rollup tiers.** Store counts per
latency band; histograms are additive, so they merge across tiers, across arbitrary
windows, and across monitors. Percentiles come back as a band with stated bounds —
"between 250 and 500 ms" — which answers §11.5's actual objection, since its
complaint is against a percentile *without its method* and a histogram has a
method with a bounded error. Columns rather than a packed blob, so the
`INSERT…SELECT` with `SUM()` in `RollUpTier` survives and the merge stays in SQL.
Computed from raw at the 1h tier and summed upward, to avoid the row-count
explosion at 1m (roughly 216 million rows at thirty-day retention for 5,000
monitors).

It lost on three specifics, none of them permanent. The band boundaries are a
permanent commitment and would have been chosen under schedule pressure: with
log-spacing the relative error is (γ−1)/(γ+1), so γ = 2 gives about seventeen bands
at ±33 %, γ = 1.5 gives about twenty-eight at ±20 %, and γ = 1.3 gives about
forty-two at ±13 % — and ±33 % on a figure shown to an auditor is not defensible,
while forty-two columns wants justifying with measurements nobody has taken. The
storage cost is genuinely unknown: SQLite encodes integer 0 and 1 as zero-payload
serial types and most buckets will have counts in only two or three bands, so the
true cost is plausibly tens of bytes per row rather than hundreds — but *plausibly*
is not a number, and this ADR declines to commit permanent schema on an estimate.
And it is not required by the reports agencies actually send, which is the test
Phase 2 is being built against. **Deferred, with the trigger named above; this is
the alternative most likely to become the successor ADR.**

**Serving the stored `MAX`-of-p95, labelled as an upper bound.** Zero storage work,
since the column is already populated, and provably sound as shown in *Context*. It
lost because the bound is arbitrarily loose — the worked example overstates by a
factor of a hundred — so the labelled figure would be true and useless
simultaneously. It also requires a field in the frozen API schema to carry the
label, which is a spec change bought for a number nobody should act on. A blank
with an explanation is better than a number that can be a hundredfold wrong.

**Window-level minimum and maximum instead of a percentile.** Exact, additive, free,
and the obvious substitute. It lost on what the statistics mean over a long window:
a maximum over roughly forty-three thousand samples is the single slowest
successful check in the month, which will sit near the timeout for most monitors
most months and reads as alarming on a client report while carrying no signal; a
minimum is the single fastest check, which reports cache warmth. Both are
extreme-value statistics presented where a distributional one is wanted, which is
the same error as quoting an unlabelled percentile, made in the other direction.
Day-level extremes give the same shape of information from a stable statistic.

**Raising `RawDays` to thirty-one so that monthly reports can use `UptimeFromRaw`.**
Legitimate for a small install — around 600 MB of raw for a hundred monitors over
thirty-one days. It lost at the scale this phase targets: 5,000 monitors at a
sixty-second interval is roughly 7.2 million heartbeats a day, on the order of a
gigabyte a day once the unique index is counted, so approximately 30 GB to make one
figure computable. It also makes report correctness depend on a retention setting
the operator can lower without being told what it will break.

**Computing a real percentile at every tier directly from raw.** Every 5m, 1h and
1d bucket closes well inside the seven-day raw window, so each tier's p95 could be
computed from raw rather than merged upward, yielding a genuine per-bucket
percentile — a real daily p95, useful for charts. It lost because it does not solve
the stated problem: a month is not a stored bucket, and thirty real daily
percentiles merge no better than thirty approximate ones. It buys chart quality at
the cost of an extra raw scan per tier per run, and this ADR is about reports.

**Sketches — t-digest, DDSketch, HDR histogram.** Already rejected at
[§11.5](../data-model/README.md#115-percentile-strategy) on storage cost and the
absence of SQLite-native support; restated here because the reasoning has hardened.
A sketch needs a blob and an application-side merge, which removes the merge from
SQL and therefore from the one place both backends share. Timescale has native
`uddsketch` and SQLite has nothing, so the two backends would produce *different*
numbers — the exact divergence ADR-002's repository interface exists to prevent. The
useful observation is that DDSketch with a fixed γ *is* a log-spaced histogram, so
the deferred alternative above already carries this one's accuracy guarantee in a
form that keeps the SQL merge.

## Compliance with the product principles

- [x] **Sixty seconds to first monitor is preserved.** No new configuration, no new
      startup requirement, no new prompt in the first-run path.
- [x] **Nothing is paywalled in the open source build.** Every figure above ships in
      the same build for every user.
- [x] **API-first.** The report figures are read through the same `/api/v1` history
      surface the dashboard uses; no privileged path is introduced. The additions
      the spec needs are extensions to that surface and belong in the spec PR that
      precedes implementation, per the Phase 0 freeze rule.
- [x] **Progressive disclosure.** The solo user sees an average and a chart. "Days
      over target" appears only once a target exists; the weekly p95 block appears
      only where raw coverage supports it. No new concept is imposed on anyone who
      has not asked for it.
- [x] **The client is never sent full state; the UI stays fast at 5,000 monitors.**
      The daily series is at most 366 rows per monitor from the 1d tier. The
      trailing-seven-day p95 is a bounded index range scan per monitor and is the
      one figure here with a real cost at scale — it is named in *Consequences* as a
      month-7 load-gate item rather than left to be discovered.
- [x] **Solo mode keeps zero required external dependencies.** Unchanged.
- [x] **Dependency surface stays minimal.** Nothing is added. This decision's main
      effect on the dependency policy is to decline a sketch library.

## References

- [Data model §11.5 — percentile strategy](../data-model/README.md#115-percentile-strategy)
  — the Phase 1 decision this ADR was instructed to revisit, and the source of the
  rule that an unlabelled percentile is worse than none.
- [Data model §11.6](../data-model/README.md#116-secrets-at-rest) — the precedent for
  how a maintainer waiver of an AGENTS.md restriction is recorded in the document it
  applies to.
- [ADR-002](002-storage-engine.md) — the rollup tiers and the two-backend repository
  contract this decision is constrained by and does not alter.
- [ADR-005](005-probe-architecture.md) — `unknown` and `skipped` are not failures,
  which is why the response-time columns count successful checks only and why a
  bucket can carry a non-zero `down_count` with a null `response_time_min`.
- [`internal/rollup/rollup.go`](../../internal/rollup/rollup.go) — tier chain,
  reprocess windows, `DefaultRetention`, and the sum-and-count rationale.
- [`internal/store/sqlite/rollup.go`](../../internal/store/sqlite/rollup.go) —
  `RollUpRaw`'s real nearest-rank p95 and `RollUpTier`'s `MAX`-of-p95 approximation.
- [`internal/store/sqlite/history.go`](../../internal/store/sqlite/history.go) —
  `UptimeFromRaw`, `RawCovers`, and the `NULL` substitution in `HistoryFromTier`.
- [`internal/probe/check/http.go`](../../internal/probe/check/http.go) — the
  response-time threshold assertion that marks a check down.
- [`internal/observation/observation.go`](../../internal/observation/observation.go)
  — the boundary at which `Class` is dropped, which is why stored history cannot
  distinguish slow from broken.
- [PHASE-2-PLAN.md §3.1](../plans/PHASE-2-PLAN.md) — the problem statement this ADR
  closes, and the milestone it unblocks.
- Open follow-up: the histogram alternative should be reopened on any of the triggers
  named in *Consequences*, as a superseding ADR rather than an amendment to this one.
