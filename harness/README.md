# Load-test harness

The 5,000-monitor gate, built before the product it measures — the Phase 0 §3.6
deliverable.

> **5,000 monitors on one install, and the UI stays fast.**

That claim is the project's central promise. This harness is what makes it a
measurement rather than an assertion, and it runs in CI from before there is any
product code to measure.

## Two targets, measuring two different things

**`-target sqlite`** builds the real schema from `migrations/sqlite/`, seeds a
synthetic install, and times the access patterns
[ADR-004](../docs/adr/004-ui-state-synchronisation.md) depends on. It answers
*is the data model right* — which is what the data model's
[§13](../docs/data-model/README.md) says needs evidence:

| Hypothesis | Scenario here |
|---|---|
| §4.2 — status belongs in `monitor_state`, not on `monitors` | `list: filter status=down (join)` |
| §6.2 — the status-filter join stays cheap at 5,000 monitors | same |
| §6.5 — membership signal option 3 is affordable | `membership: *` |
| §5.1 — batched heartbeat writes sustain 250/sec | `heartbeat write rate` |

**`-target http`** starts a real `cairn`, creates the workload through the real
API against an endpoint this process serves, and then mostly watches. It answers
*does the product hold up*. Only a running engine has a scheduler that can fall
behind, a worker pool that can shed, a result buffer that can fill, and an
alerting queue that can drop — and every one of those failures is invisible from
the database, because the symptom is a row that was never written.

### The write measurement means opposite things on the two targets

This is the distinction the report goes out of its way to label, because two
numbers printed in one column without it would mislead nobody deliberately and
everybody accidentally.

- **SQLite drives.** It pushes batches as fast as the write path will take them,
  and the number is a *ceiling*: this is what storage can absorb. The assertion
  is that it clears the floor 5,000 monitors need.
- **HTTP observes.** It reads the engine's own counter over a window and the
  number is bounded by *arithmetic*: N monitors on an I-second interval produce
  N/I results a second and cannot produce more, because there is nothing else to
  check. The assertion is that the engine *achieves* what the schedule implies.

Applying the ceiling test to an observed rate would fail every install smaller
than 5,000 monitors. Applying the achievement test to a driven rate would pass an
engine writing as fast as it could while ten minutes behind schedule.

A rate *above* the expectation is not headroom, it is a backlog draining, and the
gate says so rather than congratulating the engine.

### The partition phase

The HTTP target serves the endpoint every monitor watches, so it can fail all of
them at once. That is the burst the delivery queues were sized against, and until
something counted the deliveries on the other end the size was an argument in a
comment. Measured at 5,000 monitors:

```
  detected   5000/5000 down in 20.633s
  recovered  4841/5000 up in 20.923s
  alerts     9682 published, 0 shed
  webhooks   9682 delivered, 0 shed
  probe      0 results shed, 0 checks skipped
```

Detection is polled through `/monitors/membership`, which is exactly what that
endpoint is for — a cheap count for a filter, asked repeatedly. Counting through
the monitor listing would page 5,000 rows to find a number.

### What the harness cannot fake

Every figure except one comes from the engine's own counters, and a counter that
is wrong reports a healthy system. The exception is the request count on the
checked endpoint, tallied by this process on the other side of the network. A
gap between it and the heartbeat count means results were produced and never
stored, and nothing inside the engine would say so.

## How the gate decides

Absolute millisecond thresholds on a shared CI runner either flake or assert
nothing. What ADR-004 actually claims is a *scaling* property, so the gate is
built around comparison:

1. **Row counts must not grow with scale.** A page returns 25 rows at 500
   monitors and 25 at 5,000. This is ADR-004's second invariant stated literally.
2. **p95 must not grow more than its declared factor** between the smallest and
   largest scale. A 10× increase in monitors with a >3× increase in page-fetch
   latency means the index is not doing its job.

   Only when the two runs did the same work, though. Two p95s are a growth ratio
   if the row counts match — a page is 25 rows at every scale — or if both are
   large enough that one row either way cannot be the signal. Otherwise the
   figures are printed and no verdict is given, because a ratio between a query
   that returned one row and one that returned two is measuring which query ran.
3. **Absolute ceilings** are a generous backstop for order-of-magnitude
   regressions only.
4. **Sustained write rate.** On the SQLite target, clear 250 heartbeats/sec —
   what 5,000 monitors on the 20-second floor require. On the HTTP target, come
   within 15% of the rate the schedule implies, and shed nothing while doing it.
5. **A total partition** must mark every monitor down inside two intervals, bring
   them all back, and lose no alerts on the way. "All" means back to the
   pre-partition baseline, which is sampled twice an interval apart and only
   accepted once it stops moving — a monitor is `pending` until it has been
   checked, and a baseline read mid-sweep sets a recovery target that can never
   be reached.

The engine target also reports **how much of a slow path was queueing**, read
from `cairn_db_pool_wait_total{pool="writer"}` either side of the creation phase.
A rate that falls while that counter stays flat is work getting harder; the same
rate with it climbing is a queue behind somebody else's write. Not asserted on —
it is a diagnostic for the numbers that are.

The membership scenarios deliberately have **no growth bound**. `COUNT(*)` over
an index is inherently O(n), so it is expected to grow; the number is reported
because it is what decides whether §6.5 option 3 survives many concurrent
viewers. Its cost scales with connected clients, a dimension the 5,000-monitor
gate does not otherwise exercise — see "Known gaps".

## Running it

```bash
cd harness
go build -o harness .

# The schema gate.
./harness -scales 500,5000
./harness -scales 100 -iterations 50 -write-seconds 1   # quick local run

# The engine gate. The harness starts and stops the binary itself, because a
# gate that depends on somebody having remembered to start a server measures
# whatever that server happened to be doing.
go build -o /tmp/cairn ../cmd/cairn
./harness -target http -cairn /tmp/cairn -scales 500,5000 -json results.json

# Or point it at something already running.
./harness -target http -base-url http://localhost:3000 -scales 500
```

The engine run takes about eight minutes at `-scales 500,5000`: two minutes of it
is creating 5,000 monitors through the real write path, which is itself one of
the measurements.

Useful flags: `-target sqlite|http`, `-cairn`, `-engine-dir`, `-partition=false`,
`-migrations`, `-page-size`, `-rollup-hours`, `-min-write-rate`, `-seed`, `-v`.

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
main.go          flags, orchestration, the partition phase, reporting, exit code
target.go        the Target interface, the optional Disruptor, shared types
sqlite.go        SQLite target: applies migrations, seeds, runs the queries
http.go          HTTP target: starts an engine, drives /api/v1, reads /metrics
workload.go      deterministic synthetic install generator
scenario.go      the scenarios, their thresholds, and the verdict
harness_test.go  the parsing and verdict logic, which is where a silent mistake
                 would produce a passing gate over a meaningless measurement
```

`Target` is the seam, and the scenarios did not change when the HTTP target
landed — which is the whole reason they talk to an interface rather than to a
database handle. Two things had to give, and both are honest rather than
incidental:

- **Cursors.** The SQLite target seeks on `(updated_at, id)` because that is the
  index; the HTTP target only ever sees the opaque token the API hands back. A
  harness that reconstructed the token from the pair would be asserting an
  encoding the API deliberately does not promise, so `Setup` fills in a
  `DeepCursor` and the scenario reads it without caring which kind it got.
- **Identifiers.** The HTTP target creates monitors through the API and the
  server assigns their ids, so `Setup` rewrites the workload's. Skipping that
  would leave the tag and group filters querying things that do not exist — and
  returning nothing, and passing.

`Disruptor` is optional, and the SQLite target does not implement it: with no
engine underneath, a partition would be the harness writing rows that say "down"
and reading them back. The run says so rather than skipping the phase quietly.

The harness applies the **canonical** migrations from `migrations/sqlite/` rather
than keeping its own copy. A harness with its own schema validates something the
product does not use, which is the one way this exercise could quietly become
worthless.

## Known gaps

Listed because a gate whose limits are undocumented reads as more coverage than
it has:

- **No concurrent-viewer dimension.** The membership signal's cost scales with
  connected clients, not monitor count. Until the harness simulates N
  simultaneous filtered views, §6.5 option 3 is only half-tested — and the
  engine run measured 6.2ms per membership poll at 5,000 monitors, which makes
  the arithmetic for a hundred open dashboards worth doing before the UI lands.
- **Every monitor on the HTTP target is an HTTP check.** The workload's type mix
  is right for the schema target, where type only affects a column. Against a
  real engine a `dns` monitor would resolve a name that does not exist and
  measure the resolver's timeout, so the mix is flattened and the other eight
  checkers are not exercised under load.
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
- **The engine gate is not in CI.** The workflow still runs the SQLite target
  only. Wiring the engine run in is a change to `.github/workflows/load-test.yml`,
  and CI configuration is not edited without being asked
  ([AGENTS.md](../AGENTS.md) rule 7). The command is in "Running it" above and
  takes about eight minutes.

## What it found

The engine target earned its keep on the first run, which is the argument for
building this kind of thing rather than reasoning about the numbers:

- **Creating monitors was quadratic.** Every write woke the assignment publisher,
  which reloaded and re-diffed the whole set: 2,116 full recomputations for 5,000
  creations, and the run never finished. The publisher now settles for a second
  before recomputing, which is invisible against a 20-second floor.
- **Monitor creation was queued behind the store's one connection.** The reload
  holds it while it scans every assignable monitor, and every write queues
  behind that. Three back-to-back pairs on the same machine, because the absolute
  figures move with whatever else the machine is doing and only the pair means
  anything: at 5,000 monitors, 73, 36 and 60 creations/sec on one connection
  against 105, 142 and 99 with a reader pool alongside it. Roughly double, every
  run.

  Creation still slows as the install grows — 1,861/sec at 500 against 173/sec at
  5,000 — but the queue is no longer where the time goes, and the harness now
  says so rather than leaving it to be argued:

  ```
  created 5000 monitors through the API in 28.683s (174/sec)
    2676 statements queued for the write connection, 1.301s in total, 486µs each
  ```

  1.3 seconds of queueing across eight workers inside 28.7 seconds of creation.
  What remains is the O(N) reload itself, now merely running somewhere it blocks
  nothing. Reported as a finding rather than a failure: there is no product
  commitment about creation speed, and inventing one here would be the harness
  making policy.
- **A gate threshold was wrong, and had never run.** `list: filter status=down`
  capped growth at 4.0x and measured 4.8x at 500→5,000 — scales it had never
  reached, because the harness had no committed `go.sum` and CI refused it first.
  The query plan turned out identical at both scales (covering status index,
  primary-key probe, temp b-tree for the ordering); what grew was the matched set,
  from 13 rows to 159, because the workload keeps a fixed *proportion* down. 4.8x
  latency against 12x rows is sub-linear — the §6.2 hypothesis holding, not
  failing. The cap is now 6.0 with that reasoning recorded next to it, which is a
  correction rather than a loosening: the old bound compared a latency ratio
  against a scale ratio while ignoring that the work had grown too.
- **The first version of this harness reported 499 heartbeats/sec against a
  schedule implying 250**, and it was the harness that was wrong. The engine was
  draining the backlog it had built while seeding saturated the writer: rows
  counted by check time said 250/sec, rows counted by write time said 500, and
  both were true. The warm-up now waits for the observed rate to settle rather
  than sleeping a fixed interval and hoping.
- **Two of its own assertions were coin flips**, found while measuring the reader
  pool and fixed before its result could be read at all — a gate that goes red at
  random cannot tell you whether a change helped.

  *Recovery* compared against a down-count sampled once, before the partition.
  But a monitor is `pending` until it has actually been checked, and at 5,000
  monitors the first sweep is still running when the write window ends: the probe
  reported 4,923 checks started against 5,000 monitors. A baseline one too low
  makes the recovery target one too high — a number that can never be reached, a
  wait that runs to its deadline, and a report that the engine failed to recover
  a monitor which was never up. The baseline is now two agreeing samples an
  interval apart.

  *Growth* compared two p95s without asking whether the two runs had done the
  same work. `history` reads whatever the engine produced during warm-up, which
  lands on nought, one or two one-minute buckets depending on where the clock
  fell; the same build produced 257µs and 1.618ms minutes apart, and the ratio
  between two such runs is a measurement of which query ran rather than of scale.
  A ratio is now only computed when both scales returned the same number of rows,
  or enough rows that one either way cannot be the signal. Otherwise both figures
  are printed and no verdict is given.
