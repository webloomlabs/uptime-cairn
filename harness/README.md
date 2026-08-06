# Load-test harness

The 5,000-monitor gate, built before the product it measures — the Phase 0 §3.6
deliverable.

> **5,000 monitors on one install, and the UI stays fast.**

That claim is the project's central promise. This harness is what makes it a
measurement rather than an assertion, and it runs in CI from before there is any
product code to measure.

## What it measures today, and what it does not

Phase 0 has no server. So the SQLite target builds the real schema from
`migrations/sqlite/`, seeds a synthetic install, and times the access patterns
[ADR-004](../docs/adr/004-ui-state-synchronisation.md) depends on.

That is not a stand-in for the real thing — it is precisely what the data model's
[§13](../docs/data-model/README.md) says needs evidence. Three of its open
hypotheses are currently reasoning with nothing behind them:

| Hypothesis | Scenario here |
|---|---|
| §4.2 — status belongs in `monitor_state`, not on `monitors` | `list: filter status=down (join)` |
| §6.2 — the status-filter join stays cheap at 5,000 monitors | same |
| §6.5 — membership signal option 3 is affordable | `membership: *` |
| §5.1 — batched heartbeat writes sustain 250/sec | `heartbeat write rate` |

**It does not yet measure the API, the scheduler, or the UI.** The `http` target
exists and refuses to run, with an explanation, rather than passing vacuously — a
gate that goes green because it measured nothing is worse than no gate, since it
lets an exit criterion be ticked while asserting nothing.

## How the gate decides

Absolute millisecond thresholds on a shared CI runner either flake or assert
nothing. What ADR-004 actually claims is a *scaling* property, so the gate is
built around comparison:

1. **Row counts must not grow with scale.** A page returns 25 rows at 500
   monitors and 25 at 5,000. This is ADR-004's second invariant stated literally.
2. **p95 must not grow more than its declared factor** between the smallest and
   largest scale. A 10× increase in monitors with a >3× increase in page-fetch
   latency means the index is not doing its job.
3. **Absolute ceilings** are a generous backstop for order-of-magnitude
   regressions only.
4. **Sustained write rate** must clear 250 heartbeats/sec — what 5,000 monitors
   on the 20-second floor require.

The membership scenarios deliberately have **no growth bound**. `COUNT(*)` over
an index is inherently O(n), so it is expected to grow; the number is reported
because it is what decides whether §6.5 option 3 survives many concurrent
viewers. Its cost scales with connected clients, a dimension the 5,000-monitor
gate does not otherwise exercise — see "Known gaps".

## Running it

```bash
cd harness
go mod tidy          # once — see Dependencies below
go build -o harness .

./harness -scales 500,5000
./harness -scales 100 -iterations 50 -write-seconds 1   # quick local run
./harness -scales 500,5000 -json results.json
```

Useful flags: `-target sqlite|http`, `-migrations`, `-page-size`,
`-rollup-hours`, `-min-write-rate`, `-seed`.

The workload is deterministic — fixed seed, fixed base time — so two runs
produce the same shape. A gate that reshuffles its data every run cannot tell a
regression from a reshuffle.

## Dependencies

One third-party package, and it needs the justification
[AGENTS.md](../AGENTS.md) §5 asks for:

**`modernc.org/sqlite`** — a pure-Go SQLite implementation.

- *What it does*: the SQLite driver for `database/sql`. ADR-002 makes SQLite the
  solo-mode storage engine, so the project needs one regardless.
- *Why not the standard library*: Go has no SQLite driver, and reimplementing one
  is not a hundred lines of our own code.
- *Why this one over `mattn/go-sqlite3`*: mattn is the more established choice but
  requires cgo, which breaks static linking and cross-compilation to ARM. The
  single static binary that cross-compiles to a Pi is a product requirement, not
  a preference, so the pure-Go implementation is the one that fits.
- *Maintenance*: actively maintained, widely used, tracks upstream SQLite.

`go.sum` is committed and CI never runs `go mod tidy` for you — it fails with an
explanation instead. Dependencies are pinned deliberately (principle 10, and the
SBOM published with every release).

## Layout

```
main.go       flags, orchestration, reporting, exit code
target.go     the Target interface, shared types, and the HTTP stub
sqlite.go     SQLite target: applies migrations, seeds, runs the queries
workload.go   deterministic synthetic install generator
scenario.go   the scenarios, their thresholds, and the verdict
```

`Target` is the seam. Phase 1 implements `HTTPTarget` against the real `/api/v1`
and the scenarios in `scenario.go` do not change — that is the whole reason the
scenarios talk to an interface rather than to a database handle.

The harness applies the **canonical** migrations from `migrations/sqlite/` rather
than keeping its own copy. A harness with its own schema validates something the
product does not use, which is the one way this exercise could quietly become
worthless.

## Known gaps

Listed because a gate whose limits are undocumented reads as more coverage than
it has:

- **No concurrent-viewer dimension.** The membership signal's cost scales with
  connected clients, not monitor count. Until the harness simulates N
  simultaneous filtered views, §6.5 option 3 is only half-tested.
- **No UI benchmark.** Phase 0 §3.6 asks for one; there is no UI until Phase 1
  Month 3. The row-count invariant is the closest proxy available today.
- **No Postgres/Timescale target.** The data model's §13 item 1 — same fixture,
  both backends, identical rollups — needs one, and that is the test that keeps
  ADR-002's repository interface honest.
- **Rollups are seeded, not computed.** The harness inserts 1m buckets directly
  rather than deriving them from raw heartbeats, so it measures the read path but
  says nothing about whether the rollup pipeline is correct.
- **Raw heartbeat retention is not exercised.** §9.2's claim that deleting rows
  actually reclaims disk on SQLite is untested here.
