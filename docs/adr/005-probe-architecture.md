# ADR-005: Probe Architecture — A Thin, Stateless, Multi-Control-Plane Agent Behind ADR-001's gRPC Seam

- **Status:** Accepted
- **Date:** 2026-08-08
- **Deciders:** [Shakil Ilham](https://github.com/silham)
- **Relationship to prior ADRs:** **Extends** [ADR-001](001-probe-and-control-plane-split.md). It does not supersede it; ADR-001 remains in force and unmodified.

> Copy this file to `NNN-slug.md`, numbered sequentially, and open it as its own
> pull request for discussion. ADRs are **immutable once accepted** — if we later
> change our minds, write a new ADR that supersedes this one and update the
> Status line above. The reasoning trail is the point: it has to survive for
> whoever maintains this project after us.


## Context

[ADR-001](001-probe-and-control-plane-split.md) fixed the split — stateless
probes, outbound-only, gRPC, in-process in solo mode — and deliberately stopped
there. It says nothing about what crosses the wire, how a probe is identified, how
many monitors one probe carries, or how a new monitor type reaches it.
[PHASE-0-PLAN.md](../plans/PHASE-0-PLAN.md) §3.5 requires all of that as a Phase 0
deliverable, at the bar of *"documented well enough that a third party could
implement a probe."* The protobuf definitions cannot be written until the
questions this ADR answers are settled, because each of them changes the wire
format.

Five requirements were set for the design. Three sit inside ADR-001's frame; two
do not, and they are why this ADR exists rather than a bare protocol document.

**The 5,000-monitor target is a single-process target, not a fleet target.** Solo
mode is one binary with one embedded probe ([PHASE-1-PLAN.md](../plans/PHASE-1-PLAN.md)
§2), and Phase 1's exit criterion is 5,000 monitors *on one install* running on
hardware as small as a Raspberry Pi. So one probe process must carry 5,000
monitors — 250 checks per second on the 20-second floor, the same 250 writes per
second [data model §5.1](../data-model/README.md#51-sizing-because-it-sets-every-other-choice-here)
derives for storage, because they are the same events. Horizontal fan-out across
probes is a Phase 4 convenience, not how this number is met.

**One probe must serve several control planes, and ADR-001 does not contemplate
it.** Two drivers, both already in the repository and both now confirmed as real:
[ADR-003](003-tenancy-model.md) settled that managed hosting runs *one dedicated
instance per customer*, so a regional probe fleet for that offering either talks
to many control planes or is rebuilt per hosted customer per region. Separately,
an MSP running probes on their own infrastructure against several Cairn installs
is the same shape. The second driver is the consequential one: it means a single
probe process may serve control planes belonging to **different customers**, so
isolation between sessions inside that process is a security boundary, not
internal hygiene.

The full design exploration, including the options that lost and the numbers that
still need measuring, is [docs/probe/probe-plan.md](../probe/probe-plan.md). This
ADR records what was decided and why.

## Decision

**Sixteen decisions. The first is the one the rest hang off.**

**1. The probe is thin.** It executes checks, applies per-check assertions
(status code, keyword, JSON path, regex, response-time threshold), applies
`upside_down`, and runs retry attempts at `retry_interval_seconds`. The control
plane owns everything else: `consecutive_failures`, the up/down state transition,
`resend_after`, dependency suppression, maintenance windows, incidents,
notifications, and rollups. The rule, stated once so it can be applied to cases
not yet imagined:

> The probe evaluates everything that requires the response payload. The control
> plane evaluates everything that requires knowledge of another check, another
> monitor, or another probe.

Each retry attempt is one result, which is what the existing `heartbeats.attempt`
column anticipates. Where both sides hold a number — `retries` is the example —
the control plane is authoritative.

**2. This ADR extends ADR-001 rather than superseding it.** Nothing here
contradicts it. Multi-control-plane support is new scope ADR-001 did not
contemplate, and it is added, not substituted.

**3. One process, N sessions, one shared executor.** A *session* is one control
plane: its own endpoint, credentials, assignment set, result buffer, reconnect
backoff, protocol version, and capability negotiation. Monitor identity is
`(session_id, monitor_id)` throughout the probe. Sessions declare a relative
`share`; admission control is deficit round-robin over per-session due-queues, so
a control plane with 20,000 monitors cannot starve one with 40. **The isolation
invariant is a security boundary and is stated here verbatim so it survives into
code review:**

> The only state shared between sessions is derived from public network facts
> (DNS answers) and from resource accounting (rate limiters, the worker pool,
> aggregate metrics). Credentials, cookies, TLS session tickets, HTTP connection
> pools, response bodies, and results are never shared across sessions and never
> keyed by anything less than the session.

An HTTP connection pool keyed only by host would let one customer's session reuse
a connection another customer's session authenticated. That is the specific defect
this invariant exists to prevent, and it is written by accident, not by malice.

**4. No control plane learns that it shares a probe.** It cannot enumerate the
others and it cannot address them. A control plane sees one probe, its
capabilities, and its capacity.

**5. 5,000 monitors is the design ceiling for one process, and we are not
designing past it.** A single min-heap scheduler and the full assignment set held
in memory are sufficient at that number and are therefore what we build. Larger
installs scale by adding probes, which the assignment model already supports.

**6. Monitor configuration crosses the wire as opaque bytes** — the same JSON
[data model §11.1](../data-model/README.md#111-monitor-configuration-storage)
already stores in `monitors.config`, carried verbatim alongside the common
scheduling fields. **Adding a monitor type therefore never changes the `.proto`.**
It is one `Checker` implementation, one registry entry, and a config schema in the
OpenAPI spec. The registry also records an *execution locus*: push/heartbeat
monitors are evaluated by the control plane and never assigned to a probe at all,
and host-local types such as Docker can be pinned to a named probe rather than a
region.

**7. Probes declare capabilities at registration, and capability negotiation —
not version gating — is the compatibility mechanism.** A probe advertises each
check type it can actually run, with a reason when it cannot. This is what
satisfies Phase 0 §3.5's requirement that old probes keep working against newer
control planes, and it is how Phase 1 §3.1's ICMP-in-restricted-containers
requirement is met: `icmp: unavailable, reason: no CAP_NET_RAW` is a fact the
control plane holds before a single check runs, not an error message repeated
250 times a second.

**8. Enrolment is pre-registered; steady-state authentication is a short-lived
token exchange.** The operator creates a probe in the control plane and receives a
**single-use, short-TTL enrolment token** scoped to one org and one probe row,
hashed into the existing `probes.token_hash`. The probe presents it on first dial
and exchanges it for a long-lived credential, which it in turn exchanges for
short-lived access tokens. **No CA is operated.** Probe credentials authorise
exactly two things — pull assignments for this probe, push results for this probe —
in a namespace separate from `cairn_` API keys, with no REST API access. Disabling
a probe stops it within one access-token lifetime.

**The list of control planes is operator-owned, on the probe host. There is no
discovery, and a control plane can never add itself to a probe.** Trust flows one
way, because a probe receives every monitor credential in its assignment set
(decision 15) and a control plane able to enlist probes would be a control plane
able to harvest another operator's secrets.

**9. "Stateless" means the probe holds no monitoring state the control plane does
not own.** Assignments, results, and monitor secrets live in memory only. The
credential file is **identity, not state**; losing it costs a re-enrolment, not
data. **Assignments are never persisted**, and the consequence is accepted: a probe
that restarts while its control plane is unreachable comes back empty and idle
until the control plane returns. A probe that stays up keeps running its last
known assignment set throughout an outage, which is ADR-001's HA claim and costs
nothing.

**10. Transport is gRPC via `grpc-go`, and only gRPC.** NATS stays where
[ADR-004](004-ui-state-synchronisation.md) put it — between control-plane
components and browsers — and does not extend to the probe path. The probe opens
three streams, all outbound: a unary registration, a server-stream of assignments,
and a bidirectional result stream. Outbound-only constrains direction, not count:
N control planes is N outbound connections and still no listening socket. Because
the streams are long-lived, the control plane pushes by writing to a connection
the probe established. The corollary is that **every control-plane→probe action
takes effect on next connect, never synchronously**, and that reconnect-with-backoff
and transport keepalives are load-bearing rather than polish. `HTTPS_PROXY`/CONNECT
is supported, because corporate egress is the environment ADR-001 named. Control
plane certificates may be **pinned**; `insecure_skip_verify` is not offered.

**11. Assignments synchronise as a full set, then deltas, then periodic
reconciliation against a version.** This is deliberately the same
diff-plus-reconcile pattern ADR-004 chose for the browser — one idea in the
codebase, not two. Config is validated at assignment time, not check time.

**12. Results stream in batches, are idempotent, and are acknowledged.** Every
result carries a `result_id` (UUIDv7); delivery is at-least-once and the control
plane deduplicates. The control plane returns an acknowledged high-water mark and
the probe frees buffer only on ack, never on send. The buffer is bounded by bytes
and count; **on overflow the oldest non-`important` results are shed first**,
preserving state changes, on which alerting and the incident timeline depend.
Shedding produces gaps, and [data model §5.3](../data-model/README.md#53-rollups)
already renders a gap as "no data" rather than downtime, so the degradation is
honest by construction. Every shed result is counted and reported.

**13. Overload sheds; it never queues.** A check that cannot start within a
bounded lateness budget is recorded as `skipped`. The invariant:

> **Probe overload must never look like target downtime.**

Which requires a fourth and fifth outcome the schema does not currently have —
see decision 16.

**14. Solo mode runs the probe in-process over an in-memory gRPC connection
(`bufconn`), with real serialisation**, exactly as ADR-001 requires. The cost is
microseconds per result. The return is that **every solo install continuously
exercises the identical code path remote probes use** — solo mode becomes the
protocol's integration test, run by every user, every day. Solo mode performs no
enrolment and holds no credentials; the `embedded` probe row from migration 0001
is its identity.

**15. A probe is a credential holder, and this is stated plainly in the operator
documentation.** It holds, in memory, every secret belonging to every monitor
assigned to it. An assignment therefore carries only that probe's secrets, never
the org's full set; secrets are never written to disk, never logged, and redacted
from every error path including timeouts; and per-probe revocation is the
documented response to suspected compromise.

**16. Two data-model amendments are made now, before the schema freezes**, both
consequences of this ADR that would otherwise become migrations plus ambiguous
backfills:

- **Heartbeat writes must be idempotent.** [Data model §5.2](../data-model/README.md#52-heartbeats)'s
  claim that the scheduler prevents duplicate `(monitor_id, time)` holds for one
  in-process scheduler and survives neither at-least-once replay nor multi-region
  probes, both of which are ADR-001's own stated payoffs. Ingest deduplicates on
  `result_id`, and the heartbeat key accounts for `probe_id`.
- **The heartbeat status encoding gains values for "we do not know."**
  `unknown` (the probe could not perform the check) and `skipped` (shed under
  overload) are distinct from `down`. Both are **excluded from uptime ratios**,
  like an absent bucket. Without this, a probe whose egress fails reports 5,000
  monitors down and pages everyone — the precise false positive ADR-001 introduced
  N-of-M consensus to eliminate. The API side is already permitted: *"Enum values
  may be added. Clients must tolerate values they do not recognise."*

### The ten open questions, resolved

Recorded because the answers are the decision and the plan they answer is a
living document that will be edited.

| # | Question | Answer |
|---|---|---|
| 1 | Is multi-control-plane driven by hosting or MSP fleets? | **Both** — so isolation is a security boundary (decision 3) |
| 2 | Should a control plane know it shares a probe? | **No** (decision 4) |
| 3 | Fleet target beyond 5,000 per process? | **No** — 5,000 is the ceiling we design for (decision 5) |
| 4 | Thin probe, accepting the alerting gap? | **Yes**, gap accepted (decision 1, and Consequences below) |
| 5 | Steady-state credentials? | **Short-lived token exchange** — not bearer, not mTLS (decision 8) |
| 6 | Must a restarted probe work while its control plane is down? | **No** — assignments are never persisted (decision 9) |
| 7 | `grpc-go` or ConnectRPC? | **`grpc-go`** (decision 10) |
| 8 | Data-model amendments now or in Phase 1? | **Now** (decision 16) |
| 9 | Supersede or extend ADR-001? | **Extend** (decision 2) |
| 10 | Pre-registered probes or a shared join token? | **Pre-registered** (decision 8) |

## Consequences

**What this makes easy.**

A new monitor type costs one file and one registry line, and touches neither the
protocol nor the database — which is what makes ten types in Phase 1 and an open
set by Phase 5 affordable. New alerting behaviour ships without touching a probe
at all, because no probe knows what an alert is. N-of-M consensus remains
buildable in Phase 4, since the control plane sees every probe's result
independently and `unknown` is distinguishable from `down` — the two things
consensus needs and the two things a thick probe would have destroyed. Phase 4
turns remote probes on by adding enrolment and a network, not by rewriting an
engine, because solo mode has been running the identical protocol path since
Phase 1. One probe process serves any number of control planes at roughly the cost
of one, which is what makes ADR-003's per-customer hosting model affordable to
operate regionally.

**What this makes hard, or forecloses.**

**Alerting does not survive a control-plane outage.** ADR-001 lists "High
Availability (probes buffer and replay data if the control plane drops)" among its
benefits; with a thin probe that is precisely true and no more. Data collection
survives; state evaluation and notification do not, and a backlog arrives at once
when the control plane returns. This is accepted deliberately — the alternative
puts every notification credential on every probe and forks the alert path into two
implementations that must agree — but it is **not** what most readers hear in the
word "HA", and it must be stated in those words in the operator documentation
rather than discovered during an incident. Phase 4's HA control plane is the real
answer.

**A shared process is a shared blast radius.** Serving multiple customers'
control planes from one probe means the isolation invariant in decision 3 is the
only thing between them. It is enforced by code review and tests, not by the
operating system, which is a weaker guarantee than one process per control plane
would give. An operator with a customer who cannot accept that runs a dedicated
probe; the design permits it and the documentation should say so.

**The wire boundary enforces nothing about check configuration.** Opaque config
means a probe can be handed a config it cannot parse. Mitigated by validating at
assignment time and reporting it once as a visible configuration error, but the
protocol will not catch it for us.

**A pre-registered probe fleet does not autoscale.** A single-use enrolment token
cannot be baked into an image, so every probe is an operator action. Fine at five
probes, painful at fifty. Shared join tokens are the fix and were rejected for now
on blast radius: a leaked join token lets an attacker stand up a probe, receive an
assignment set, and read every credential in it. Revisit when fleet size makes the
case, as a later ADR.

**A probe that restarts during a control-plane outage does nothing** until the
control plane returns.

**What becomes expensive to reverse later.**

Nothing here is expensive to reverse *provided the seam is real*, and that is the
same conditional [ADR-002](002-storage-engine.md) attached to its repository
interface. The point of no return is not a phase boundary but a code-review
discipline: if Phase 1's scheduler ever reaches around the probe interface — one
direct call, one shared struct, one assumption that a result is available
synchronously — this design stops being true, and it will not announce itself. It
will be discovered by a Phase 4 user.

Three things genuinely are expensive to reverse. **Protobuf field numbering in
`cairn.probe.v1`**: additive only, never renumbered, never reused, semantics never
changed; anything breaking is `v2` with `v1` supported through the deprecation
window in [docs/api/README.md](../api/README.md#compatibility-promise). **The
thin/thick split**, since making the probe stateful later means designing state,
persistence, and credential distribution for a fleet already deployed. **Decision
16**, which is cheap this week and a migration plus a backfill of ambiguous
history after Phase 1 ships.

## Alternatives considered

**Thick probe — probe owns the state machine and fires alerts locally during a
control-plane outage.** The only option that closes the alerting gap, which is a
real user-visible benefit and the strongest argument against what was chosen.
Lost because it makes every probe a holder of every notification credential,
because it forks the alert path into two implementations that must agree forever,
and because it makes N-of-M consensus impossible — a probe deciding "down" alone
is exactly the false positive ADR-001 introduced consensus to remove. The gap is
closed properly by Phase 4's HA control plane, not by moving the alert path to the
edge.

**A `oneof` per monitor type on the wire.** Compile-time safety; a malformed
config could not cross the boundary. Lost because every new monitor type would
become a protocol change, a regenerated client, and a compatibility question, for
ten types now and an open set by Phase 5's plugin SDK. This is the same argument
[data model §11.1](../data-model/README.md#111-monitor-configuration-storage) had
about table-per-type, and it lost for the same reason and should lose consistently.

**One probe process per control plane.** Total isolation, enforced by the OS
rather than by our own discipline, which is a materially stronger security story
for the MSP case. Lost on resources: N× base memory, N× ops surface, N× upgrade,
against a requirement that the probe run on very small hardware. It remains
available to any operator who wants it — nothing prevents running one probe per
control plane — so the strict-isolation option is preserved as a deployment
choice rather than forced on everyone.

**Long-lived bearer tokens for steady state.** Simplest, and matches
`probes.token_hash` as the schema already stands. Lost because the credential is
replayable for its whole lifetime by anyone who reads a config file or terminates
TLS in between, and a probe credential yields an assignment set full of customer
secrets.

**mTLS with the control plane as a small CA.** Strongest of the three: the private
key never leaves the probe and there is nothing bearer to replay. Lost because we
would be operating a CA — issuance, renewal, revocation, and clock skew — in front
of a self-hosted user whose alternative product needs none of that. Kept as a
plausible opt-in for enterprise in a later phase.

**Shared join tokens for enrolment.** Much better fleet ergonomics and the only
way to autoscale probes. Lost on blast radius, as described above. A TTL and a
use-count cap narrow the exposure; they do not close it, and closing it is what a
credential guarding customer secrets has to do.

**NATS on the probe path.** ADR-001 left it open and ADR-004 already runs it, so
reusing it would not add an infrastructure component to *our* deployment. Lost
because it would add one to the *customer's*: a private probe in a VPC would need
broker credentials and broker reachability, which is a second transport to
authenticate, version, and explain, for a path gRPC already covers.

**ConnectRPC instead of `grpc-go`.** A materially smaller dependency tree, which
matters under principle 10, plus HTTP/1.1 support that behaves better through
legacy corporate proxies — both aimed squarely at the low-resource and private-probe
requirements. Lost on conventionality: `grpc-go` is the straightforward reading of
ADR-001, is what a third-party probe implementer will expect, and is the
lower-surprise choice for a protocol we are asking strangers to implement against.

**Direct in-process calls in solo mode, skipping serialisation.** Marginally
faster and the obvious optimisation. Rejected outright: it is the specific
shortcut ADR-001 exists to forbid, and the first field that works in-process and
not on the wire would be found by a Phase 4 user rather than by us.

**Persisting assignments to disk so a restarted probe survives a control-plane
outage.** Lost because it means writing customer credentials to disk on the least
trusted host in the system, and because it contradicts "stateless" in ADR-001's
own words. An encrypted assignment cache remains a defensible Phase 4 option for
private probes specifically.

## Compliance with the product principles

- [x] **Sixty seconds to first monitor is preserved.** Solo mode performs no
      enrolment, holds no credentials, reads no probe configuration, and opens no
      port. The solo user never learns a probe exists, which is what ADR-001
      promised and what decision 14 keeps true.
- [x] **Nothing is paywalled in the open source build.** Remote probes,
      multi-control-plane support, buffering and replay, and the protocol
      specification itself all ship in the open source build. There is no
      "distributed monitoring" tier.
- [x] **API-first — no privileged endpoints the dashboard uses and users cannot.**
      The probe protocol is not REST and is deliberately absent from the OpenAPI
      spec, but it is not privileged: it is published to the standard that a
      stranger could implement a conforming probe against it (Phase 0 §3.5), which
      is the same promise the REST contract makes. A first-party probe has no
      capability a third-party one lacks.
- [x] **Progressive disclosure — no new complexity imposed on the solo user.**
      Sessions, enrolment, capabilities, fair-share admission, and buffering are
      all invisible below the point where someone deliberately runs a second
      probe.
- [ ] **The client is never sent full state; the UI stays fast at 5,000 monitors —
      not yet confirmed, and this ADR is one of its inputs.** The probe is the
      source of the 250 results per second that the storage and live-update paths
      must absorb, so the batching in decision 12 and the dispersal in the plan's
      §4.4 are load-bearing for a gate this ADR cannot assert on its own. The
      harness needs a probe target before any of the resource claims here are more
      than reasoning — see References.
- [x] **Solo mode keeps zero required external dependencies.** `bufconn` is an
      in-memory connection: no port, no TLS, no broker, no second process. The
      decision to keep NATS off the probe path (decision 10) is part of holding
      this line.
- [x] **Dependency surface stays minimal.** No new dependency is introduced by
      this ADR: gRPC and Protobuf were committed by ADR-001. Recorded honestly
      nonetheless — `grpc-go`'s transitive tree is not small, ConnectRPC was the
      lighter option, and it lost on the grounds stated above rather than on
      dependency count. If the SBOM makes that trade look worse than it does today,
      it is revisitable behind the protocol, since the wire format is unchanged
      either way.

## References

- [docs/probe/probe-plan.md](../probe/probe-plan.md) — the design exploration this
  ADR decides, including the options that lost in more detail, the illustrative
  service sketch, and the proposed resource budgets. It is a living document; this
  ADR is not.
- [ADR-001](001-probe-and-control-plane-split.md) — the split this ADR extends.
  Every constraint it fixes is carried forward unmodified.
- [ADR-002](002-storage-engine.md) — the interface-discipline argument reused
  verbatim in "What becomes expensive to reverse later", and the source of the
  batched-write requirement decision 12 aligns the wire format to.
- [ADR-003](003-tenancy-model.md) — the per-customer dedicated-instance hosting
  model that makes multi-control-plane support load-bearing rather than
  speculative.
- [ADR-004](004-ui-state-synchronisation.md) — the diff-plus-reconcile pattern
  decision 11 reuses, and the NATS scope decision 10 declines to widen.
- [Data model](../data-model/README.md) §5.1 (250 writes/second), §5.2
  (heartbeats), §5.3 (a gap is not downtime), §11.1 (JSON config), §11.3 (UUIDv7),
  §12 (secrets at rest) — and §4.11, whose `probes` table this ADR fills in.
- [PHASE-0-PLAN.md](../plans/PHASE-0-PLAN.md) §3.5 — the deliverable this ADR
  unblocks. [PHASE-1-PLAN.md](../plans/PHASE-1-PLAN.md) §2, §3.1 — solo-mode
  architecture and the ten monitor types.
- [harness/README.md](../../harness/README.md) — **open follow-up:** the harness
  needs a probe target on arm64 before the resource budgets in the plan's §4.14
  are anything but reasoning. The specific hypotheses to test are listed in the
  plan's §10; the ones that could change a decision here are the TLS-handshake
  cost, the `bufconn` serialisation cost, and whether fair-share admission
  actually protects a small session.
- **Open follow-up:** decision 16 requires edits to the data model and to
  `migrations/sqlite/` before the schema is frozen. They are cheap this week and a
  migration plus an ambiguous backfill after Phase 1 ships.
- **Open follow-up:** decision 8's credential design needs a deliberate security
  review before implementation, per the waiver note at the head of this document.
- **Open follow-up:** revisit shared join tokens when probe fleet size makes the
  ergonomics case concretely — a specific operator blocked by per-probe
  enrolment — rather than on a date.
- **Open follow-up:** the N-of-M consensus algorithm is Phase 4 and gets its own
  ADR. This decision only ensures the protocol can carry what it will need:
  per-probe results, `unknown` distinct from `down`, and probe identity on every
  heartbeat.
