# Uptime Cairn — Probe Architecture Plan

**Status: decided 2026-08-08. All ten questions in [§9](#9-open-questions--these-need-your-decision)
were answered by the maintainer and are recorded in
[ADR-005](../adr/005-probe-architecture.md), which is the binding document.**

This plan remains as the design exploration behind that ADR — the options that
lost, the numbers still to be measured, the illustrative service sketch. It is a
living document and may be edited; ADR-005 is not.

This is the design layer above [PHASE-0-PLAN.md](../plans/PHASE-0-PLAN.md) §3.5,
whose actual deliverable is *"protobuf/gRPC definitions … documented well enough
that a third party could implement a probe."* Those artefacts now exist —
[`proto/cairn/probe/v1/`](../../proto/cairn/probe/v1/) and
[protocol.md](protocol.md) — and are drafted, not yet reviewed. Where they and
this plan differ, ADR-005 and protocol.md are what an implementer follows.

[§8](#8-conflicts-with-accepted-documents) lists three places where the design
collides with documents that were already accepted. Two of them are resolved by
ADR-005 decision 16, and the corresponding edits have since been made to the data
model and `migrations/sqlite/0001_initial.sql` — with one departure the data model
records in its §11.8 and refers back to the maintainer. The third is an accepted
limitation that must be documented in those words.

---

## 1. What the probe has to do

Five requirements set for this design, plus what the accepted ADRs already bind.

### 1.1 Stated requirements

| # | Requirement | Where it is answered |
|---|---|---|
| R1 | Handle 5,000+ monitors | [§4.4](#44-p4--scheduling-and-concurrency) — one probe, because solo mode has exactly one |
| R2 | Serve monitors from multiple control planes | [§4.5](#45-p5--multiple-control-planes) |
| R3 | Authenticate securely with each control plane | [§4.9](#49-p9--identity-enrolment-and-credentials) |
| R4 | Run on very low resources | [§4.4](#44-p4--scheduling-and-concurrency), [§4.14](#414-p14--resource-budgets) |
| R5 | Support current and future monitor types, small tweaks acceptable | [§4.2](#42-p2--the-check-registry-and-opaque-config), [§4.3](#43-p3--capability-negotiation) |

**R1 is not a fleet target, it is a single-process target.** Solo mode is one
binary with one embedded probe ([PHASE-1-PLAN.md](../plans/PHASE-1-PLAN.md) §2),
and the Phase 1 exit criterion is 5,000 monitors *on one install*. So a single
probe process must carry 5,000 monitors on a Raspberry Pi. Horizontal fan-out
across probes is a Phase 4 convenience, not the way R1 is met. Everything in
[§4.4](#44-p4--scheduling-and-concurrency) follows from that.

**R2 has a driver already in the repository, and it is worth naming.**
[ADR-003](../adr/003-tenancy-model.md) settled that managed hosting will run *one
dedicated instance per customer*. A regional probe fleet for that offering
therefore has to talk to many control planes, or every hosted customer needs its
own probe in every region — which is the same fleet multiplied by the customer
count. The MSP case (one agency, probes on their own infrastructure, several
Cairn installs) is the same shape. R2 is what keeps ADR-003's hosting decision
affordable.

### 1.2 Constraints already fixed by accepted ADRs

Not up for debate here; listed so the design can be checked against them.
[ADR-001](../adr/001-probe-and-control-plane-split.md) is the binding one.

- Probes are **stateless** agents that register with the control plane and pull
  assignments (ADR-001).
- Communication is **strictly outbound-only**. The probe dials out. No inbound
  ports on the probe host, ever (ADR-001).
- Transport is **gRPC + Protobuf**, with NATS available for fan-out (ADR-001).
- The probe runs **in-process in solo mode but still behind the gRPC interface**,
  and *"the scheduler must never assume same-process execution"* (ADR-001).
- **Old probes keep working against newer control planes within a major version**
  (Phase 0 §3.5).
- `heartbeats.probe_id` is `NOT NULL` from migration 0001, satisfied by a seeded
  `embedded` probe row ([data model §4.11](../data-model/README.md#411-probes)).
- Heartbeat writes are **batched** — the repository's primary operation is a
  batch write ([data model §5.1](../data-model/README.md#51-sizing-because-it-sets-every-other-choice-here)).
- Monitor configuration is **JSON**, matching the OpenAPI discriminated union,
  chosen specifically so that new monitor types need no migration
  ([data model §11.1](../data-model/README.md#111-monitor-configuration-storage)).

---

## 2. The shape in one view

```
      ┌──────────────────── probe process (one binary, one config) ───────────────────┐
      │                                                                               │
      │  session: acme          session: beta          session: solo (in-process)     │
      │  ─────────────          ─────────────          ──────────────────────────     │
      │  credentials            credentials            no credentials                 │
      │  assignment set         assignment set         assignment set                 │
      │  result buffer          result buffer          result buffer                  │
      │  backoff/reconnect      backoff/reconnect      never disconnects              │
      │        │                      │                        │                      │
      │        └──────────┬───────────┴────────────────────────┘                      │
      │                   ▼                                                           │
      │           admission control  ── weighted fair share, global ceiling            │
      │                   ▼                                                           │
      │           due-time scheduler  ── one min-heap, deterministic jitter            │
      │                   ▼                                                           │
      │           worker pool  ── bounded, goroutine per in-flight check               │
      │                   ▼                                                           │
      │           check registry  ── http │ tcp │ icmp │ dns │ tls │ rdap │ docker │ grpc
      │                   │                                                           │
      │           shared: DNS cache (TTL-honouring), rate limiters, metrics            │
      │           never shared: credentials, cookies, TLS tickets, results             │
      └───────────────────────────────────────────────────────────────────────────────┘
               │ outbound TLS, gRPC          │ outbound TLS, gRPC        │ bufconn
               ▼                             ▼                           ▼
        control plane "acme"          control plane "beta"        same process
```

Three streams per session, all opened by the probe:

| Stream | Direction | Carries |
|---|---|---|
| `Register` | unary | identity, version, capabilities, capacity → credentials scope + session id |
| `WatchAssignments` | server-stream | full set, then deltas, then periodic reconcile |
| `StreamResults` | bidirectional | batched results up; acknowledged high-water mark down |

---

## 3. The one decision everything else hangs off

**How much does the probe decide?**

| Option | For | Against |
|---|---|---|
| **Thin probe (recommended)** — executes checks, applies per-check assertions and retries, reports observations. All state, suppression, incident, and alert logic on the control plane. | New alerting behaviour ships without touching a probe. Dependency suppression and maintenance windows need cross-monitor knowledge a probe does not have. N-of-M consensus — ADR-001's stated payoff — is inherently a control-plane decision, since no probe can know what another probe saw. Keeps "stateless" true rather than aspirational. | Alerting stops during a control-plane outage. See [§8.3](#83-adr-001-claims-ha-what-it-delivers-is-data-ha). |
| Thick probe — probe owns the state machine and can fire alerts locally when the control plane is unreachable. | Survives a control-plane outage end to end. | The probe becomes a holder of every notification credential, a stateful thing to reason about, and a second implementation of the alert path that must agree with the first. Consensus becomes impossible. Rejected. |

**Recommendation: thin probe.** The split, stated as a rule that should survive
into the ADR:

> The probe evaluates everything that requires the response payload. The control
> plane evaluates everything that requires knowledge of another check, another
> monitor, or another probe.

So the probe does: DNS resolution, connection, TLS handshake, the request, and
the assertions (status code, keyword, JSON path, regex, response-time threshold),
plus retry attempts at `retry_interval_seconds`. Each attempt is one result, which
is what the `heartbeats.attempt` column already anticipates.

The control plane does: `consecutive_failures` counting, the up/down state
transition, `resend_after`, dependency suppression, maintenance windows,
incidents, notifications, and rollups.

**`upside_down` is applied by the probe**, because the stored heartbeat status
should already mean what the user asked for and the control plane should not
re-derive it per row on every read. The consequence, which belongs in the user
documentation: toggling `upside_down` does not reinterpret history.

Where probe and control plane both hold a number — `retries` is the example — the
control plane is authoritative. The probe uses it only to know how many attempts
to run before returning to the normal interval.

---

## 4. Proposed decisions

Fourteen, each with the alternative that lost and why.

### 4.1 P1 — One binary, one build

`cairn --mode=probe` rather than a separate `cairn-probe` artefact, per
[PHASE-1-PLAN.md](../plans/PHASE-1-PLAN.md) §2. The probe path initialises no
storage, no migrations, no UI, no HTTP server.

The cost is binary size: the embedded UI and the SQLite driver ship to every probe
host even though a probe uses neither, plausibly 40–80 MB on disk. That is disk,
not memory, and it buys one release artefact, one version number, and one upgrade
story. If probe-host disk turns out to matter, a `cairn-probe` build tag that
excludes the embedded assets is an additive change later, not a redesign.

### 4.2 P2 — The check registry and opaque config

**This is how R5 is met, and it is the highest-leverage choice in the document.**

The wire format carries monitor configuration as **opaque bytes** — the same JSON
the data model already stores in `monitors.config`:

```protobuf
message Assignment {
  bytes  monitor_id     = 1;   // UUIDv7, 16 raw bytes
  string type           = 2;   // "http", "tcp", "icmp", …
  bytes  config         = 3;   // the monitors.config JSON, verbatim
  uint32 interval_seconds  = 4;
  uint32 timeout_seconds   = 5;
  uint32 retries           = 6;
  uint32 retry_interval_seconds = 7;
  bool   upside_down    = 8;
  string config_version = 9;   // changes when config or schedule changes
}
```

| Option | For | Against |
|---|---|---|
| **Opaque JSON config (recommended)** | Adding a monitor type touches the probe's registry, the OpenAPI schema, and nothing else — **the `.proto` never changes**. Matches [data model §11.1](../data-model/README.md#111-monitor-configuration-storage), which chose JSON for exactly this reason, and the OpenAPI discriminated union one-to-one. No protocol version bump per monitor type, which is what keeps the compatibility promise cheap. | The wire boundary enforces nothing. A probe can be handed a config it cannot parse. Mitigated by making that a loud, reported failure ([§7](#7-failure-taxonomy)) rather than a silent one, and by the API layer being the only path that writes config. |
| `oneof` per monitor type | Compile-time safety; a malformed config cannot cross the wire. | Every new monitor type is a protocol change, a regenerated client, and a compatibility question. Ten types today, an open set by Phase 5's plugin SDK. This is the same argument [§11.1](../data-model/README.md#111-monitor-configuration-storage) already had about table-per-type, and it lost for the same reason. |

The probe-side interface, which is what "small tweaks are acceptable for a new
monitor type" should cost:

```go
type Checker interface {
    Type() string                                  // "http"
    Version() uint32                               // capability version
    Validate(config []byte) error                  // at assignment time, not check time
    Check(ctx context.Context, config []byte) Observation
}
```

A new monitor type is one file implementing that interface, one line in the
registry, and its config schema in the OpenAPI spec. Nothing else.

**Deliberately not included:** a plugin system. Go plugins are a version-lockstep
maintenance trap, and an embedded scripting engine is a sandbox we would then own.
Phase 5's plugin SDK gets its own design when it gets there — but the interface
above should not foreclose a checker that delegates to a **local sidecar over a
Unix socket**, because that is how Playwright browser checks (Phase 4) will have
to work; they cannot live inside a static Go binary.

### 4.3 P3 — Capability negotiation

At registration the probe declares what it can actually do:

```protobuf
message Capability {
  string type       = 1;   // "icmp"
  uint32 version    = 2;   // the checker's version; 0 = unavailable
  bool   available  = 3;
  string reason     = 4;   // "raw sockets unavailable: no CAP_NET_RAW"
}
```

This does more work than it looks like:

- **It is the compatibility mechanism.** ADR-001 and Phase 0 §3.5 require old
  probes to keep working against newer control planes. Capability negotiation
  handles that without version gating: a v1.0 probe simply does not advertise
  `gRPC health` and the control plane does not assign it any.
- **It turns the ICMP-in-container problem into a protocol feature.**
  [ROADMAP.md](../../ROADMAP.md) and Phase 1 §3.1 require detecting restricted
  container environments and *"fail with a clear explanatory message rather than
  breaking silently."* A probe that reports `icmp: unavailable, reason: …` at
  registration makes that a control-plane-side, user-visible fact before a single
  check runs, instead of an error message on every heartbeat.
- **It is how "no probe can run this monitor" becomes a real error** rather than a
  monitor stuck in `pending` forever.

Docker checks constrain assignment the same way: a Docker monitor can only run on
a probe that can see that daemon, so `docker` capability is per-probe and the
control plane must be able to pin a monitor to a named probe, not only to a region.

### 4.4 P4 — Scheduling and concurrency

R1 and R4 together. 5,000 monitors on the 20-second floor is **250 checks per
second** — the same number [data model §5.1](../data-model/README.md#51-sizing-because-it-sets-every-other-choice-here)
derives for the write path, and it should be, since they are the same events.

Five rules:

1. **One min-heap keyed on next-due, not a timer per monitor.** 5,000 timers is
   survivable in Go and still the wrong shape at 50,000.

2. **Deterministic dispersal.** A monitor's phase offset is `hash(monitor_id) mod
   interval`. Without this, 5,000 monitors created by an importer on a 60-second
   interval all fire in the same second — a 5,000-wide burst every minute instead
   of 83 checks per second, and the peak is what sizes the memory. This is the
   single cheapest thing in the document and the easiest to forget.

3. **Bounded worker pool, goroutine per in-flight check.** At 250 checks/s and a
   300 ms average, steady state is ~75 concurrent. A bad day where everything
   hits a 5-second timeout is ~1,250. Default ceiling 2,000, configurable — at
   roughly 10–20 KB of stack each that is 20–40 MB, which is a budget we can
   afford.

4. **Overload sheds, it never queues.** An unbounded queue turns overload into a
   memory leak and then emits results timestamped from the past. A check that
   cannot start within a bounded lateness budget (proposed: 50% of its interval)
   is recorded as **`skipped`**, which is a probe health signal and **not** a
   monitor failure.

   > **Probe overload must never look like target downtime.** If this design has
   > one invariant, it is this one. Everything in [§7](#7-failure-taxonomy) is
   > built to keep it true.

5. **Politeness limits.** A global check-start rate cap plus a per-destination-host
   concurrency cap, so a probe holding 500 monitors against one host does not look
   like an attack to that host.

**Connection reuse — the one genuine tension between R1 and correctness.**

A fresh TCP+TLS handshake per check is what the user thinks they are buying: a
pooled connection can report `up` against a socket the far end has already
half-closed, and it measures none of DNS, TCP, or TLS. But 250 TLS handshakes per
second is real CPU on a Pi — order 0.5–0.75 of a core on a Pi 4, *estimated, not
measured* ([§10](#10-how-this-gets-validated)).

Recommendation: **fresh connection per check by default**, `reuse_connection` as
an opt-in for high-frequency monitors, and recover the cost in DNS instead — a
**TTL-honouring** resolver cache, shared across sessions, which turns one lookup
per check into one lookup per TTL. Strictly TTL-honouring: a stale entry is never
served, because if the user's DNS is broken the honest answer is a failure, not a
cached success. The DNS *monitor type* never touches this cache.

### 4.5 P5 — Multiple control planes

R2. Three ways to do it:

| Option | For | Against |
|---|---|---|
| One process per control plane | Total isolation, nothing to design. | N× the base memory, N× the ops surface, N× the upgrade. Defeats R4 at any interesting N. |
| **One process, N sessions, shared executor (recommended)** | One scheduler, one worker pool, one resource budget — which is exactly what makes N control planes affordable on small hardware. | Shared blast radius, and a shared process is a shared secret store. Needs the isolation invariant below to be real, not assumed. |
| Broker in front | Decouples everything. | A broker reachable from the probe network is a second piece of infrastructure and a second thing to authenticate to. Contradicts "outbound-only, no dependencies" for private probes. |

A **session** is one control plane: its own endpoint, credentials, assignment set,
result buffer, reconnect backoff, protocol version, and capability negotiation.
Monitor identity is `(session_id, monitor_id)` everywhere inside the probe; the
two never collide even if two control planes issue the same UUID.

**Why outbound-only does not limit this.** ADR-001's rule constrains *direction*,
not *count*: nothing dials into the probe. Nothing says the probe may dial out
only once. Three control planes is three outbound connections from one process,
and the probe still has no listening socket:

```
probe host                                    control planes
┌───────────────────┐
│ cairn --mode=probe│ ──TCP SYN──▶ :443  cp.acme.example
│                   │ ──TCP SYN──▶ :443  status.beta.example
│  no listening     │ ──TCP SYN──▶ :443  cp.gamma.example
│  socket at all    │
└───────────────────┘
   egress rules: 443/tcp outbound      ingress rules: none
```

Each control plane's firewall sees an ordinary inbound TLS client, indistinguishable
from a browser. The probe host's firewall needs no ingress rule at all.

The control plane "pushes" down a connection the probe established. Once
`WatchAssignments` is open the probe blocks reading, and a new monitor is a frame
written on that existing HTTP/2 stream — no new connection, no SYN toward the
probe. That is the whole mechanism, and it is why the streams in
[§4.7](#47-p7--assignment-synchronisation) are long-lived rather than polled.

The real cost is that **every control-plane→probe action is "takes effect on next
connect", never synchronous**:

| Action | Probe connected | Probe disconnected |
|---|---|---|
| Assign or update a monitor | arrives in milliseconds on the open stream | queued; delivered on reconnect |
| Revoke the probe | immediate | within one access-token lifetime ([§4.9](#49-p9--identity-enrolment-and-credentials)) |
| Probe restarts during a control-plane outage | — | comes back empty and idle ([§4.7](#47-p7--assignment-synchronisation), [Q6](#9-open-questions--these-need-your-decision)) |

Two operational requirements follow, and both belong in the ADR: **reconnect with
backoff and jitter**, and **transport keepalive pings**. NATs and corporate
firewalls silently drop idle connections, and a probe that believes it is
connected when it is not will buffer into the void until the acknowledged
high-water mark stops moving.

**The isolation invariant, which should be stated in the ADR verbatim:**

> The only state shared between sessions is derived from public network facts
> (DNS answers) and from resource accounting (rate limiters, the worker pool,
> aggregate metrics). Credentials, cookies, TLS session tickets, HTTP connection
> pools, response bodies, and results are never shared across sessions and never
> keyed by anything less than the session.

An HTTP connection pool keyed only by host would let session A reuse a connection
session B authenticated. That is the specific bug this invariant exists to
prevent, and it is an easy one to write by accident.

**Fairness.** Sessions declare a relative `share`. Admission control is deficit
round-robin over per-session due-queues: under contention each session gets at
least `share / Σshare` of the admission slots; unused capacity is lent to whoever
wants it. One control plane assigning 20,000 monitors must not starve another
that assigned 40.

**Capacity backpressure.** The probe reports `max_concurrent_checks` and
`assigned_count` at registration and on every heartbeat. A control plane that
would exceed the probe's declared capacity **declines the assignment and surfaces
"probe at capacity" to the user** rather than assigning work that will be shed.
This is far better than discovering the ceiling as a rising `skipped` rate.

Configuration is operator-owned, on the probe host, and is written there by the
enrolment flow in [§4.9(a)](#49-p9--identity-enrolment-and-credentials) — no
control plane can add itself to this list:

```yaml
probe:
  name: syd-1
  region: ap-southeast-2
  max_concurrent_checks: 2000
  control_planes:
    - id: acme
      endpoint: cp.acme.example:443
      credential_file: /etc/cairn/acme.cred
      share: 2
    - id: beta
      endpoint: status.beta.example:443
      credential_file: /etc/cairn/beta.cred
      share: 1
```

**No control plane ever learns that the others exist.** It cannot enumerate them,
and it cannot address them. See [§9](#9-open-questions--these-need-your-decision) Q2.

### 4.6 P6 — Transport

gRPC, per ADR-001, for both control and data path. NATS stays where
[ADR-004](../adr/004-ui-state-synchronisation.md) put it — between control-plane
components and browsers — and does **not** extend to probes: a broker credential
and a broker endpoint reachable from a customer's VPC is exactly the inbound-ish
dependency ADR-001 exists to avoid, and it would be a second transport to version.

Two transport details that matter for private probes and belong in the ADR:

- **`HTTPS_PROXY` / HTTP CONNECT support.** Corporate networks route egress
  through a proxy. A probe that cannot dial through one cannot be deployed in the
  environment ADR-001 named as the reason for the split.
- **Self-signed control planes are the common case in self-hosted.** The probe
  must support pinning the control plane's certificate or SPKI hash. It must
  **not** offer `insecure_skip_verify`; a pin is the same amount of user effort
  and does not silently turn off authentication.

Worth raising once and then dropping if you disagree: **ConnectRPC** speaks the
gRPC wire protocol over HTTP/1.1 as well as HTTP/2, with a substantially smaller
dependency tree than `grpc-go` and much better behaviour through legacy corporate
proxies. It is arguably ADR-001-compliant since the wire protocol is unchanged.
It is also a dependency decision, so it is [Q7](#9-open-questions--these-need-your-decision).

### 4.7 P7 — Assignment synchronisation

The probe opens `WatchAssignments`; the control plane replies with the full set,
then streams deltas, then re-anchors periodically. **This is deliberately the same
diff-plus-reconcile pattern ADR-004 chose for the browser**, and the symmetry is
worth keeping — one idea in the codebase instead of two.

```
→ WatchAssignmentsRequest{ known_set_version }
← AssignmentSet{ set_version, assignments[] }          // full, on first connect or mismatch
← AssignmentDelta{ set_version, added[], updated[], removed_ids[] }
← AssignmentDelta{ set_version, reconcile: true }      // ~15 min; probe compares its own hash
```

`set_version` is opaque and monotonic per session. On mismatch the probe asks for
a full set — the same "if in doubt, re-fetch" rule ADR-004's membership signal
uses. A full set at 5,000 monitors and ~1 KB of config each is ~5 MB, which is
fine once on connect and unaffordable every 15 minutes; hence deltas, and hence a
hash comparison rather than a resend for the reconcile.

`Validate(config)` runs **at assignment time, not check time**. A monitor whose
config the probe cannot parse should be reported once, immediately, as a
configuration error the user can see — not discovered 250 times a second.

**The probe keeps running its last known assignment set when the control plane is
unreachable.** That is the HA claim in ADR-001 and it costs nothing: the set is
already in memory.

**Assignments are never written to disk.** ADR-001 says stateless, and assignments
contain credentials ([§4.10](#410-p10--secrets-in-assignments)). The accepted
consequence: a probe that *restarts* while its control plane is down comes back
empty and idle until the control plane returns. An encrypted on-disk assignment
cache is a defensible Phase 4 option for private probes; it is not this design.

**What "stateless" means here, precisely**, because the probe does write one file
and the word should not be read as forbidding it:

> The probe holds no monitoring state the control plane does not own. Its
> credential is **identity, not state**. Assignments, results, and monitor secrets
> live in memory only; the credential from [§4.9](#49-p9--identity-enrolment-and-credentials)
> is the sole thing on disk, and losing it costs a re-enrolment, not data.

The alternative — no credential on disk — means a fresh enrolment token on every
restart, which no fleet can operate.

### 4.8 P8 — Results, batching, buffering, replay

Results go up the bidirectional `StreamResults` in **batches**, because
[data model §5.1](../data-model/README.md#51-sizing-because-it-sets-every-other-choice-here)
already established that the storage layer's primary operation is a batch write.
Aligning the wire frame with the storage batch means the control plane can hand a
received frame almost straight to `HeartbeatStore.WriteBatch`. Proposed flush
trigger: 1 second or 500 results, whichever first.

**Every result carries a `result_id` (UUIDv7).** Delivery is at-least-once; the
control plane deduplicates. This is not optional — see
[§8.1](#81-at-least-once-replay-breaks-the-heartbeat-uniqueness-claim).

**Acknowledgement.** The control plane returns an acknowledged high-water mark on
the same stream. The probe frees buffered results at or below it. Nothing is
dropped from the buffer on send; only on ack.

**Buffering and replay** — ADR-001's HA promise, made concrete:

- Bounded by **bytes and count**, whichever binds first. Proposed defaults: 64 MB
  or 250,000 results.
- At 5,000 monitors on the 20-second floor and roughly 100 bytes per result on the
  wire, 64 MB is **on the order of 40 minutes** of outage coverage. *Estimated;
  [§10](#10-how-this-gets-validated) says how to find out.*
- **On overflow, shed the oldest non-`important` results first.** Results with
  `important = true` (a state change — the column already exists) are what
  alerting and the incident timeline depend on, and they are a tiny fraction of
  the volume. Losing raw heartbeats degrades resolution; losing state changes
  loses the event.
- The shedding is honest by construction: a bucket with no checks has no row, and
  [data model §5.3](../data-model/README.md#53-rollups) already says absence means
  "no data", surfaced as a null `uptime_ratio`. A gap is not downtime. Shedding
  produces gaps, not phantom outages.
- Disk spill is **off by default** and out of scope for Phase 1. It reintroduces
  state and puts customer data on the probe host.

Every shed result increments a counter that the control plane sees. A probe that
is quietly dropping data must be visible in the UI.

### 4.9 P9 — Identity, enrolment, and credentials

> **[AGENTS.md](../../AGENTS.md) §8 places security work with a human.** This
> section is a set of options and a recommendation for you to review, in the same
> spirit as [data model §11.6](../data-model/README.md#116-secrets-at-rest). It
> should get a deliberate security review before it becomes an ADR, not a skim.

R3. Five separable questions.

**(a) Bootstrap — how a probe learns which control planes to talk to.**

**The operator tells it, on the probe host. There is no discovery, and a control
plane can never add itself to a probe.** That direction is deliberate: a probe
receives every monitor credential in its assignment set
([§4.10](#410-p10--secrets-in-assignments)), so a control plane able to enlist
probes would be a control plane able to harvest another operator's secrets by
recruiting their agent. Trust flows one way — the operator decides who the probe
serves, and the probe dials only names it was given.

*Solo mode does none of this.* No config, no enrolment, no token: the `embedded`
probe row from migration 0001 is the identity and the session runs over `bufconn`
in the same process. Anything else would put a setup step in front of
`docker run`.

*A remote probe's first boot:*

```
1. Operator, in the control plane:   "Add probe"  →  probe row created
                                     ← one-time enrolment token, short TTL

2. Operator, on the probe host:
     cairn --mode=probe --enrol cairn_enrol_7f3a…@cp.acme.example:443
                                └── credential ──┘ └─── endpoint ───┘

3. Probe dials cp.acme.example:443 and presents the token.
   Control plane verifies against probes.token_hash, marks it used,
   returns the steady-state credential. Probe writes it to <data-dir>/acme.cred (0600).

4. Register{capabilities, capacity, versions}        → §4.3
5. WatchAssignments{known_set_version: ""}           → full set returns
6. Checks start; StreamResults opens.
```

Step 2 is one copy-pasteable string carrying both the endpoint and the credential.
That is deliberate — it collapses "where" and "who are you" into a single value an
operator cannot get half-right, which is why `tailscale up --authkey`, `k3s agent`,
and the GitHub Actions runner all take the same shape.

**Steps 1–3 repeat per control plane.** Three control planes means three
enrolments, three credentials, three independent sessions, and none of them learns
the others exist. After enrolment the list lives in the config file shown in
[§4.5](#45-p5--multiple-control-planes); `credential_file` can be populated two
ways, and they suit different infrastructure:

| | Who writes the credential | Fits |
|---|---|---|
| **Self-persisting** | The probe, after enrolment, into `--data-dir` | A VM or a Pi set up once by hand |
| **Operator-supplied** | The operator enrols separately and mounts the result read-only | Kubernetes, immutable images, GitOps — the probe's filesystem stays read-only |

**(b) Enrolment credential.** The **one-time, short-lived enrolment token** above
(proposed: single-use, 15-minute TTL, scoped to one org and one probe row). Its
hash goes in the existing `probes.token_hash` column. A bearer token that is
single-use and short-lived is a much smaller target than one that lives forever in
a config file.

This assumes **pre-registered** probes — one probe row, one token. That is fine
for five probes, miserable for fifty, and impossible for an autoscaling fleet,
since a single-use token cannot be baked into an image. The alternative, a
**shared join token**, trades blast radius for fleet ergonomics and is
[Q10](#9-open-questions--these-need-your-decision).

**(c) Steady state.**

| Option | For | Against |
|---|---|---|
| **1. Long-lived bearer token** | Simplest. Matches `probes.token_hash` as it stands today. | Replayable for its whole lifetime by anyone who reads the config file or terminates TLS in between. |
| **2. Long-lived credential exchanged for short-lived access tokens (recommended)** | Replay window is minutes, not years. No CA to run. Revocation is "stop issuing", which takes effect within one token lifetime. | Slightly more protocol. Needs a sane clock on the probe. |
| **3. mTLS, control plane as a small CA** | Strongest: the private key never leaves the probe. Nothing bearer to replay. | We would be operating a CA — issuance, renewal, revocation, CRL/OCSP or short-lived certs, and clock skew. That is a real operational burden to put in front of a self-hosted user, and it is Phase 4-shaped work. |

Recommendation: **a one-time token for enrolment, option 2 for steady state,
option 3 as an opt-in for enterprise later.** The transport already authenticates
the control plane and provides confidentiality; what is missing is proof of
*probe* identity, and a short-lived token bound to the channel supplies that
without a CA.

**(d) Scope.** Probe credentials authorise exactly two things — pull assignments
for this probe, push results for this probe — scoped to one org. They live in a
different namespace from `cairn_` API keys and can do nothing on the REST API.

**(e) Revocation and rotation.** Disabling a probe in the control plane must stop
it within one access-token lifetime. Credentials rotate on a schedule without
operator involvement, and rotation must survive a control-plane outage longer than
the credential lifetime — otherwise an outage locks out the fleet that is
buffering data for it. That last sentence is the failure mode to design against.

### 4.10 P10 — Secrets in assignments

**A probe holds, in memory, every credential belonging to every monitor assigned
to it.** HTTP basic and bearer credentials, gRPC metadata, Docker client TLS keys —
[data model §12](../data-model/README.md#12-secrets-at-rest) enumerates them, and
it is the control plane that decrypts them and puts them on the wire.

This deserves to be stated plainly in the ADR and in the operator documentation,
because it is the thing a security-conscious enterprise will ask about first:

- A probe is a credential holder. **Compromise of a probe is compromise of every
  monitor credential assigned to it.**
- Therefore: an assignment carries only the secrets for *that probe's* monitors,
  never the org's full set.
- Secrets are never written to disk ([§4.7](#47-p7--assignment-synchronisation)),
  never logged, and redacted from every error path — including timeouts, which is
  where a URL with embedded credentials usually escapes.
- Per-probe revocation exists and is documented as the response to a suspected
  compromise.
- A private probe in a customer's VPC is, by design, the least trusted host in the
  system and holds the most sensitive assignment set. That tension is inherent to
  ADR-001, not created here, but it should be written down where someone will find it.

### 4.11 P11 — Solo mode, in-process

ADR-001 accepts serialisation across the gRPC boundary even in one process, and
names it as a cost. Two ways to honour that:

| Option | For | Against |
|---|---|---|
| **In-memory gRPC over `bufconn` (recommended)** | Exactly what ADR-001 describes. No port, no TLS, no ops surface — and **every solo install continuously exercises the identical code path remote probes use.** Solo mode becomes the protocol's integration test, run by every user, every day. | A marshal/unmarshal per result. Estimated 1–2 µs for a ~100-byte message; at 250/s that is well under a millisecond of CPU per second. Confirm rather than assume ([§10](#10-how-this-gets-validated)). |
| Direct interface calls, skipping serialisation | Marginally faster. | The seam rots. The first field that works in-process and not on the wire will be found by a Phase 4 user, not by us. This is precisely the shortcut ADR-001 exists to forbid. |

Solo mode seeds no credentials and runs no enrolment; the session is trusted
because it is the same process. The `embedded` probe row from migration 0001 is
its identity.

### 4.12 P12 — Versioning and compatibility

- Proto package `cairn.probe.v1`. **Additive only.** Field numbers are never
  reused, never renumbered, and semantics never change. Anything breaking is
  `v2`, and `v1` keeps working for the deprecation window in
  [docs/api/README.md](../api/README.md#compatibility-promise).
- Registration carries `protocol_version`, `agent_version`, and capabilities. The
  control plane refuses only on a **major** protocol mismatch, with an error that
  names both versions and what to do.
- Within `v1`, **any v1 probe works against any v1 control plane.** Feature
  differences are handled by capability negotiation
  ([§4.3](#43-p3--capability-negotiation)), not by version gating. This is what
  Phase 0 §3.5's compatibility requirement actually asks for.
- **No self-update.** A binary that can replace itself is a remote code execution
  path with extra steps. Upgrades are the operator's container or package manager.

### 4.13 P13 — Observability without inbound ports

A probe cannot expose `/metrics` by default without contradicting ADR-001. So:

- The probe reports **self-metrics on the existing result stream** — queue depth,
  shed count, buffer bytes, check latency distribution, per-type error counts,
  capacity headroom, clock offset — and the control plane surfaces them on its own
  `/metrics` and in the UI, labelled by probe.
- `--metrics-addr` exists, is **off by default**, and is documented as opening a
  listening socket, for operators who run their own Prometheus and have decided
  they want one.
- Probe logs are local and structured. Nothing else is inbound.

### 4.14 P14 — Resource budgets

Targets to design against and to hold the harness to. **These are proposed
budgets, not measurements** — see [§10](#10-how-this-gets-validated).

| Scenario | RSS | CPU |
|---|---|---|
| Idle, registered, zero monitors | < 30 MB | ~0 |
| 500 monitors @ 60 s | < 60 MB | < 5% of one core (Pi 4) |
| 5,000 monitors @ 60 s | < 150 MB | < 0.5 core (Pi 4) |
| 5,000 monitors @ 20 s (the floor, worst case) | < 200 MB | < 1.5 cores (Pi 4) |

Plus:

- **`GOMEMLIMIT` set from the configured ceiling**, so the probe degrades into
  shedding predictably instead of being OOM-killed inside a container memory
  limit. A monitoring agent that dies under load is worse than useless — it is a
  monitoring agent that stops reporting exactly when things are going wrong.
- Static binary, no cgo, `linux/amd64` and `linux/arm64` at minimum. The Pi user
  is a stated persona.
- `--max-concurrent-checks`, `--max-buffer-bytes`, `--max-checks-per-second`
  configurable, with the defaults above.

---

## 5. Where each monitor type runs

Not every monitor type is probe work, and that changes the protocol.

| Type | Executed by | Capability / constraint |
|---|---|---|
| HTTP/HTTPS | probe | outbound 80/443 |
| TCP port | probe | outbound TCP |
| ICMP ping | probe | `CAP_NET_RAW`, or unprivileged ICMP sockets via `net.ipv4.ping_group_range`. **Reported as a capability**, with TCP fallback offered per Phase 1 §3.1 |
| DNS record | probe | outbound 53 UDP/TCP; never uses the shared resolver cache |
| SSL/TLS expiry | probe | outbound TCP |
| Domain expiry | probe | RDAP/WHOIS egress. Aggressively rate-limited upstream — needs a **per-type minimum interval** far above the 20-second floor, enforced probe-side as well as at the API |
| **Push / heartbeat** | **control plane** | Nothing to execute. The ingest endpoint and the dead-man's-switch evaluation are both control-plane-side |
| Docker container | probe | Docker socket on the probe host. **Intrinsically probe-local** — the control plane must be able to pin such a monitor to a named probe, not merely to a region |
| gRPC health | probe | outbound TCP |

Two consequences for the design:

- The check registry needs an **execution locus** per type. Push monitors are
  never assigned to a probe at all, and the control plane's scheduler must run
  their expiry evaluation itself.
- **N-of-M consensus is meaningless for host-local types.** A Docker check runs
  where the daemon is; there is no second opinion to be had. The consensus design
  in Phase 4 must exclude them explicitly rather than discovering it.

---

## 6. Illustrative service sketch

**Not the deliverable, and now superseded by one.** The `.proto` files are Phase 0
§3.5's actual output and have been written — see
[`proto/cairn/probe/v1/`](../../proto/cairn/probe/v1/). The sketch below is kept
because it is the shape that was argued with; it differs from the delivered
protocol in several places, and the delivered files win.

```protobuf
package cairn.probe.v1;

service ProbeService {
  rpc Enrol            (EnrolRequest)          returns (EnrolResponse);
  rpc Register         (RegisterRequest)       returns (RegisterResponse);
  rpc WatchAssignments (WatchRequest)          returns (stream AssignmentUpdate);
  rpc StreamResults    (stream ResultBatch)    returns (stream ResultAck);
}

message RegisterRequest {
  string     agent_version    = 1;
  uint32     protocol_version = 2;
  string     name             = 3;
  string     region           = 4;
  repeated Capability capabilities = 5;
  uint32     max_concurrent_checks = 6;
  uint32     max_checks_per_second = 7;
  int64      probe_time_unix_micros = 8;   // clock-skew detection, §8.2
}

message Result {
  bytes  result_id        = 1;   // UUIDv7 — idempotency key, §8.1
  bytes  monitor_id       = 2;
  int64  time_unix_micros = 3;   // when the check ran, on the probe's clock
  Outcome outcome         = 4;   // §7
  double response_time_ms = 5;
  string code             = 6;   // HTTP status, DNS rcode, gRPC health status
  string message          = 7;   // failures and state changes only
  uint32 attempt          = 8;
  bool   important        = 9;
}

message ResultBatch {
  repeated Result results = 1;
  ProbeHealth health      = 2;   // §4.13 — self-metrics ride along
}

message ResultAck {
  bytes acknowledged_through_result_id = 1;
  uint32 assignments_rejected = 2;
}
```

---

## 7. Failure taxonomy

The invariant from [§4.4](#44-p4--scheduling-and-concurrency) — *probe overload
must never look like target downtime* — needs the wire format to be able to say so.

| Outcome | Meaning | Should it page anyone? |
|---|---|---|
| `up` | Check ran, all assertions passed | no |
| `down` | Check ran, the target failed or an assertion failed | **yes** |
| `unknown` | The probe could not perform the check: capability missing, config unparseable, the probe's own DNS or egress is broken | **no** — alert on *probe health* instead |
| `skipped` | Never started: shed under overload, or past its lateness budget | **no** — a probe capacity signal |

`unknown` is the one that matters and the one that is easy to get wrong. If a
probe's egress dies and every check is reported `down`, the operator gets 5,000
pages saying the internet is broken. The honest answer is "we do not know", and
producing it is exactly why ADR-001 wanted N-of-M consensus in the first place.

`unknown` and `skipped` must be **excluded from uptime ratios**, the same way an
absent bucket is ([data model §5.3](../data-model/README.md#53-rollups)) — they are
gaps in observation, not observations of failure. A status page that renders "our
probe fell over" as customer downtime is lying, and Phase 2's SLA reports would
inherit the lie.

**This requires a change to an accepted document** — see [§8.2](#82-the-heartbeat-status-enum-has-no-value-for-we-do-not-know).

---

## 8. Conflicts with accepted documents

Three, found while writing this. Each needs resolving regardless of which way the
rest of the design goes, and the first two are cheap now and expensive after
Phase 1 ships.

### 8.1 At-least-once replay breaks the heartbeat uniqueness claim

[Data model §5.2](../data-model/README.md#52-heartbeats) states:

> Duplicate-timestamp collisions within a monitor are prevented by the scheduler,
> not by a unique constraint that would cost a write-path lookup.

That holds for one in-process scheduler. It does not survive either of ADR-001's
own payoffs:

- **Buffer and replay.** A batch sent, received, written, and then not
  acknowledged before the connection drops is resent. Same monitor, same
  timestamp, second row.
- **Multi-region probes.** Several probes check the same monitor by design.
  `(monitor_id, time)` is no longer unique even without replay.

Options: make the natural key `(monitor_id, probe_id, time)`, or deduplicate on
`result_id` at ingest, or upsert. The `result_id` in
[§4.8](#48-p8--results-batching-buffering-replay) assumes something is done; which
one is a data-model decision, not a probe-protocol one. It should be settled
before the schema is frozen, because it changes the heartbeat index.

### 8.2 The heartbeat status enum has no value for "we do not know"

[Data model §5.2](../data-model/README.md#52-heartbeats) encodes status as
`0=down, 1=up, 2=pending, 3=maintenance`. There is nowhere to put `unknown` or
`skipped` from [§7](#7-failure-taxonomy), so a probe that cannot execute a check
must currently be recorded as `down` — which pages everyone when a probe breaks,
the exact false positive ADR-001's consensus feature exists to eliminate.

The API side of this is already permitted: *"Enum values may be added. Clients
must tolerate values they do not recognise"*
([docs/api/README.md](../api/README.md#compatibility-promise)). The data-model
side is a one-line addition **if it happens before the schema freezes**, and a
migration plus a backfill of ambiguous history if it happens after.

### 8.3 ADR-001 claims HA; what it delivers is *data* HA

ADR-001 lists among its benefits *"High Availability (probes buffer and replay
data if the control plane drops)"*. With the thin probe in [§3](#3-the-one-decision-everything-else-hangs-off),
that is precisely true and no more: **data collection survives a control-plane
outage. Alerting does not.** If the control plane is down, nothing evaluates state
transitions and no notification is sent until it returns — and then a backlog
arrives at once.

Nothing about that is wrong, and the alternative (probes holding notification
credentials and running a second copy of the alert path) is worse. But it is not
what most readers will hear in the word "HA", and a monitoring product that is
vague about this in its own documentation deserves the support tickets it gets.
Phase 4's HA control plane is the real answer. Until then it should be stated
plainly in the ADR and the ops docs, in these words.

---

## 9. Open questions — these need your decision

Numbered so they can be answered in a reply rather than a rewrite.

1. **Is R2 driven by managed hosting, by MSP-owned fleets, or both?** It changes
   how strict the fairness and isolation work has to be. Hosting means all control
   planes are ours and mutual trust is high; MSP means a probe may serve control
   planes belonging to different customers, and [§4.5](#45-p5--multiple-control-planes)'s
   isolation invariant becomes a security boundary rather than a hygiene rule.

2. **Should a control plane know it is sharing a probe?** Recommendation: no —
   it cannot enumerate or address the others. But an enterprise may well require
   the opposite: a declaration that this probe is exclusive to them. Which is it?

3. **Confirm the fleet target beyond 5,000.** Single-probe 5,000 is forced by solo
   mode. Is the Phase 4 fleet target 20,000? 100,000? It sets whether the min-heap
   scheduler and the full-assignment-set-in-memory model are enough or need a
   second look now.

4. **Thin probe — do you accept the alerting gap in [§8.3](#83-adr-001-claims-ha-what-it-delivers-is-data-ha)?**
   This is the single most consequential answer in the list.

5. **Steady-state credentials: bearer, short-lived exchange, or mTLS?**
   ([§4.9](#49-p9--identity-enrolment-and-credentials).) Recommendation is the
   middle one, and per AGENTS.md §8 this whole section wants a security review
   before it becomes an ADR.

6. **Does a restarted probe need to work while its control plane is down?**
   Answering yes means persisting assignments — which means persisting secrets to
   disk on the least trusted host in the system, and contradicting "stateless".
   Recommendation is no, documented as a limitation.

7. **`grpc-go` or ConnectRPC?** ([§4.6](#46-p6--transport).) A dependency
   decision, which AGENTS.md §5 puts with you. Connect's smaller tree and HTTP/1.1
   support argue for it on R4 and on corporate-proxy grounds; `grpc-go` is the
   conventional reading of ADR-001.

8. **[§8.1](#81-at-least-once-replay-breaks-the-heartbeat-uniqueness-claim) and
   [§8.2](#82-the-heartbeat-status-enum-has-no-value-for-we-do-not-know) need
   data-model amendments.** Both are cheap before the schema freezes and expensive
   after. Do you want them folded into the data model now, or recorded as
   Phase 1 Month 1 work?

9. **Does the probe ADR supersede or extend ADR-001?** ADR-001 is accepted and
   immutable. Nothing here contradicts it, but multi-control-plane support (R2) is
   genuinely new scope that ADR-001 does not contemplate. My read is that this is
   **ADR-005, extending ADR-001 rather than superseding it** — but that is your
   call, and the reasoning trail matters more than the numbering.

10. **Pre-registered probes, or a shared join token?**
    ([§4.9(b)](#49-p9--identity-enrolment-and-credentials).) This one was settled
    by my omission rather than by an argument, which is why it is here.

    | | Ergonomics | Blast radius if the token leaks |
    |---|---|---|
    | **Pre-registered** — one probe row, one single-use token | Fine at 5 probes, painful at 50, impossible for an autoscaling fleet: a single-use token cannot be baked into an image | One probe, for 15 minutes |
    | **Shared join token** — one org-scoped token, probes self-register | A fleet scales without operator involvement | An attacker stands up a probe, receives an assignment set, and reads **every credential in it** ([§4.10](#410-p10--secrets-in-assignments)). A TTL and a use-count cap narrow this; they do not close it |

    Recommendation: pre-registered for the Phase 4 launch, join tokens as an
    explicit opt-in once fleet size demands it. Worth deciding now because it
    shapes the enrolment RPC, not just the UI.

---

## 10. How this gets validated

Every number in this document is reasoned, not measured. The harness exists
precisely so that stops being true — [harness/README.md](../../harness/README.md)
already lists the data model's unproven hypotheses in exactly this form, and these
are the probe's.

| Hypothesis | Where it is claimed | What would test it |
|---|---|---|
| One process sustains 250 checks/s at < 200 MB and < 1.5 cores on ARM | [§4.14](#414-p14--resource-budgets) | A probe target in the harness driving synthetic checks against a local null server, on arm64 |
| Deterministic dispersal flattens the burst | [§4.4](#44-p4--scheduling-and-concurrency) | Peak concurrent checks per second with and without the phase offset, 5,000 monitors on one interval |
| Fresh TLS per check costs ~0.5–0.75 core at 250/s on a Pi | [§4.4](#44-p4--scheduling-and-concurrency) | Handshake microbenchmark on the target hardware; it decides whether the reuse default is right |
| `bufconn` serialisation is negligible in solo mode | [§4.11](#411-p11--solo-mode-in-process) | Marshal/unmarshal benchmark on the result message; compare against a direct-call baseline |
| 64 MB of buffer is ~40 minutes of outage at full rate | [§4.8](#48-p8--results-batching-buffering-replay) | Measure the encoded size of a realistic result mix; the answer is a division |
| Fair-share admission does not starve a small session | [§4.5](#45-p5--multiple-control-planes) | Two sessions, 20,000 monitors and 40, assert the small one's checks stay on schedule |
| A probe restart loses at most one tick | Principle 8, Phase 1 §4.4 | Kill mid-cycle; assert the bound, not merely that it restarts |

The harness's `Target` seam is the right place for all of this — the same reason
its README gives for the HTTP target existing before there is a server to point it
at. **A probe target that refuses to run is better than one that passes
vacuously.**

---

## 11. Phase mapping

| Phase | Deliverable |
|---|---|
| **0** | This plan reviewed → **ADR-005** (yours to write) → `.proto` files + a semantics document a third party could implement against. No probe code. |
| **1** | The probe as a package, running in-process over `bufconn`. One session, no credentials, no enrolment. Nine monitor types behind the registry. The scheduler behaves as if execution were remote, because it is. |
| **4** | `--mode=probe`. Enrolment, credentials, multi-session, buffering and replay under real network partitions, capacity-aware assignment, N-of-M consensus, private probes. |

The point of doing this in Phase 0 is that Phase 4 turns on a switch instead of
rewriting an engine. If Phase 1's scheduler ever reaches around the interface —
one direct call, one shared struct, one assumption that a result is available
synchronously — that is the day this design stops being true, and it will not
announce itself. It is a code-review discipline, the same one
[ADR-002](../adr/002-storage-engine.md) names for the repository interface.

---

## 12. Deliberately absent

- **A plugin system.** [§4.2](#42-p2--the-check-registry-and-opaque-config). The
  interface does not foreclose a sidecar checker, which is what Phase 4's browser
  checks need; that is as far as this goes.
- **NATS on the probe path.** [§4.6](#46-p6--transport).
- **Probe self-update.** [§4.12](#412-p12--versioning-and-compatibility).
- **Alerting from the probe.** [§3](#3-the-one-decision-everything-else-hangs-off),
  and the honest consequence in [§8.3](#83-adr-001-claims-ha-what-it-delivers-is-data-ha).
- **Persisted assignments.** [§4.7](#47-p7--assignment-synchronisation), pending
  [Q6](#9-open-questions--these-need-your-decision).
- **The N-of-M consensus algorithm itself.** Phase 4. This design only ensures the
  protocol can carry what it will need: per-probe results, `unknown` as distinct
  from `down`, and probe identity on every heartbeat.
