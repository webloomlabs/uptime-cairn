# Uptime Cairn — Probe Protocol v1

**Status: draft, not reviewed.** This and `proto/cairn/probe/v1/` are the
[PHASE-0-PLAN.md](../plans/PHASE-0-PLAN.md) §3.5 deliverable, whose bar is *"documented
well enough that a third party could implement a probe."* Phase 0 exits when it has been
reviewed and versioned, not when it has been written.

The binding documents, in precedence order. Where this document disagrees with one of
them, it is wrong and should be fixed here:

| | |
|---|---|
| [ADR-001](../adr/001-probe-and-control-plane-split.md) | the split: stateless probes, outbound-only, gRPC, in-process in solo mode |
| [ADR-005](../adr/005-probe-architecture.md) | the sixteen decisions this protocol implements |
| [data model](../data-model/README.md) | `heartbeats`, `probes`, `monitors`, and what a result becomes once stored |
| [probe-plan.md](probe-plan.md) | the design exploration behind ADR-005 — the options that lost |

`.proto` files define the messages. This document defines everything a message
definition cannot: ordering, idempotency, retry, backoff, what each field means when it
is absent, and what a probe must never do. **An implementation written from the `.proto`
files alone will be wrong**, and wrong in ways that surface during an outage rather than
in testing.

Requirement keywords — MUST, MUST NOT, SHOULD, MAY — are used in the RFC 2119 sense.

---

## 1. The shape in one page

```
probe host                                          control plane
┌────────────────────────┐
│ cairn --mode=probe     │  1. Enrol ─────────────▶  token → credential   (once, ever)
│                        │  2. IssueToken ────────▶  credential → access token
│  no listening socket   │  3. Register ──────────▶  capabilities, capacity, versions
│                        │  4. WatchAssignments ──▶  ◀── set, deltas, reconcile  (stream)
│                        │  5. StreamResults ─────▶  batches ▶ ◀ acks           (stream)
└────────────────────────┘
     egress: 443/tcp outbound only        ingress: none, on the probe side
```

One process holds **N sessions**, one per control plane, sharing a scheduler and a
worker pool and nothing else that matters (ADR-005 decision 3). Everything below is
per-session unless it says otherwise. **No control plane learns that the others exist**
(decision 4).

The division of labour, which explains most of what follows:

> The probe evaluates everything that requires the response payload. The control plane
> evaluates everything that requires knowledge of another check, another monitor, or
> another probe.

So the probe runs checks, applies assertions, applies `upside_down`, and runs retries.
It does not evaluate `consecutive_failures`, the up/down transition, `resend_after`,
dependency suppression, maintenance windows, incidents, notifications, or rollups.

## 2. Conventions

**UUIDs are 16 raw bytes**, never a string, never dash-formatted. UUIDv7 throughout,
matching the data model's `BLOB(16)` storage. A 36-byte string on a 250-message/second
path is 20 bytes of waste per result and a parse on both ends.

**Time is `int64` microseconds since the Unix epoch, UTC.** Microseconds because
`heartbeats.time` is microseconds; a protocol that carried milliseconds would force the
control plane to invent three digits and would collide two checks that genuinely ran
0.4 ms apart.

**Durations are `uint32` seconds** unless the field name ends in `_millis`. 0 means
"unset, use your default" for every server-supplied tuning value, and there is no field
where 0 seconds is a meaningful setting.

**Strings are UTF-8 and user-facing.** Every string that can reach a log, an error, or
the UI MUST be redacted of credentials before it is sent (§12).

**Unset scalars mean absent, not zero.** `response_time_ms` is the case that matters:
0.0 is a measurement, and a probe with nothing to measure MUST leave the field unset so
ingest stores NULL rather than a phantom instant response.

**Unknown enum values MUST be tolerated**, matching the API's compatibility promise in
[docs/api/README.md](../api/README.md). A probe that receives an `Assignment.type` it
does not know rejects that assignment; it does not close the stream. A control plane
that receives an `ErrorClass` it does not know stores the result and ignores the class.

## 3. Session lifecycle

A session is one control plane: its own endpoint, credentials, assignment set, result
buffer, reconnect backoff, protocol version, and capability negotiation. Monitor
identity inside the probe is `(session, monitor_id)` everywhere — never `monitor_id`
alone, because two control planes may legitimately issue the same UUID.

```
                    ┌──────────────┐
  operator runs     │  UNENROLLED  │
  --enrol ─────────▶└──────┬───────┘
                           │ Enrol            (once per control plane, ever)
                           ▼
                    ┌──────────────┐
                    │ CREDENTIALED │◀────────────────────┐
                    └──────┬───────┘                     │
                           │ IssueToken                  │ credential still valid
                           ▼                             │
                    ┌──────────────┐                     │
                    │ AUTHENTICATED│                     │
                    └──────┬───────┘                     │
                           │ Register                    │
                           ▼                             │
                    ┌──────────────┐   stream error      │
                    │   RUNNING    │────────────────────▶│  backoff, then IssueToken
                    │ Watch+Stream │                     │
                    └──────┬───────┘                     │
                           │ PERMISSION_DENIED           │
                           ▼                             │
                    ┌──────────────┐                     │
                    │   REVOKED    │─── long backoff ───▶┘
                    └──────────────┘
```

Rules that are easy to get wrong:

1. **`Register` MUST precede `WatchAssignments` and `StreamResults`** on each new
   connection. Both fail `FAILED_PRECONDITION` otherwise. Registration is per
   connection, not per process: after any reconnect the probe re-registers, because
   capabilities can change across a restart and the control plane's view of a probe is
   built from the current connection.
2. **The two streams are independent.** Losing one does not require closing the other,
   and a probe MUST keep executing its assignment set while `StreamResults` is down —
   that buffering is ADR-001's entire HA claim.
3. **Checks keep running when both streams are down.** The probe holds its last known
   assignment set for as long as the process lives (§5.5).
4. **A restart while the control plane is unreachable comes back empty and idle.**
   Assignments are never persisted (decision 9). This is an accepted limitation, not a
   defect, and operator documentation MUST say so in these words.

## 4. Enrolment, credentials, and authentication

### 4.1 Where a credential goes

A credential *being exchanged for another* is a message field, because it is the subject
of the call. A credential *authorising* a call is request metadata:

```
authorization: Bearer <access_token>
```

So enrolment tokens and long-lived credentials appear in `EnrolRequest` and
`IssueTokenRequest`; access tokens appear only in metadata, on every other RPC including
re-registration.

### 4.2 Enrolment

The operator creates a probe in the control plane and receives a **single-use,
short-TTL enrolment token** scoped to one org and one probe row, whose hash goes in
`probes.token_hash`. On the probe host:

```
cairn --mode=probe --enrol cairn_enrol_7f3a…@cp.acme.example:443
                           └── credential ──┘ └─── endpoint ───┘
```

One copy-pasteable string carrying both who-you-are and where, because an operator
cannot get half of one string right and half of it wrong.

`Enrol` is the only unauthenticated RPC. It is called **once per control plane, ever**;
three control planes means three enrolments, three credentials, three sessions, and none
of them learns the others exist. A probe that has a credential MUST NOT call `Enrol`
again — losing the credential file costs a re-enrolment by the operator, which is the
intended failure mode.

Failures are terminal and interactive: a used, expired, or unknown token returns
`NOT_FOUND` or `FAILED_PRECONDITION`, and the probe exits non-zero with the control
plane's message rather than retrying. Retrying a single-use token is never going to work
and a fleet of processes retrying one is a denial of service against enrolment.

**The list of control planes is operator-owned, on the probe host. There is no
discovery, and a control plane can never add itself to a probe** (decision 8). Trust
flows one way because a probe receives every monitor credential in its assignment set: a
control plane able to enlist probes would be a control plane able to harvest another
operator's secrets by recruiting their agent.

### 4.3 Steady state

`IssueToken` exchanges the long-lived credential for a short-lived access token. The
probe calls it on connect and again at **50% of the token's remaining lifetime**, never
waiting for expiry — a refresh that only happens on failure turns every token expiry
into a reconnect.

| | Default | Why |
|---|---|---|
| Enrolment token TTL | 15 min, single use | Small target; an operator pastes it within minutes or generates another |
| Access token TTL | 15 min | Revocation takes effect within one lifetime (decision 8) |
| Credential lifetime | 90 days | Long enough that a control-plane outage cannot lock out a fleet |
| Credential rotation | at 50% of lifetime | Rotation is opportunistic; a probe that cannot reach its plane keeps using a valid credential |
| Previous credential grace | 24 h after rotation | A rotation that races a crash must not strand the probe |

The failure mode to design against, stated once: **an outage longer than a credential
lifetime must not lock out the fleet that is buffering data for it.** That is why the
credential lifetime is measured in months and the access token in minutes, and why
rotation is opportunistic rather than scheduled.

Probe credentials authorise exactly two things — pull assignments for this probe, push
results for this probe — scoped to one org, in a namespace separate from `cairn_` API
keys, with **no REST API access whatsoever**.

### 4.4 Transport security

- TLS is required. The probe MUST support **pinning** the control plane's certificate or
  SPKI hash, because self-signed control planes are the common case in self-hosted.
- **`insecure_skip_verify` is not offered.** A pin is the same amount of operator effort
  and does not silently disable authentication.
- **`HTTPS_PROXY` / HTTP CONNECT MUST be supported.** Corporate egress through a proxy is
  the environment ADR-001 named as the reason for the split; a probe that cannot dial
  through one cannot be deployed there.

## 5. Assignment synchronisation

### 5.1 The exchange

```
→ WatchAssignments{ probe_id, known_set_version, known_assignment_digest }
← AssignmentUpdate{ set:   { set_version, assignments[], final } }   ← one or more chunks
← AssignmentUpdate{ delta: { set_version, added[], updated[], removed_monitor_ids[] } }
← AssignmentUpdate{ reconcile: { set_version, assignment_digest } }  ← ~15 min
```

On connect the control plane either sends a full set, or — if `known_set_version` and
`known_assignment_digest` both match what it believes the probe holds — resumes with
deltas alone. When in doubt it sends a full set; a needless full set costs one transfer,
and a wrongly skipped one costs silently wrong monitoring.

### 5.2 Chunking and atomicity

A full set at 5,000 monitors with ~1 KB of config each is ~5 MB, and gRPC's default
receive limit is 4 MiB. **The full set MUST be chunked**: multiple `AssignmentSet`
messages sharing one `set_version`, the last carrying `final = true`. A control plane
SHOULD chunk at 500 assignments.

The probe accumulates chunks into a staging table and **swaps atomically on
`final = true`**. A stream that fails mid-set is discarded entirely — a half-applied set
is a monitoring gap that nothing will ever correct, because neither side knows it
happened.

`set_version` is opaque, monotonic per session, and compared by equality only. The probe
MUST NOT parse it or order it.

### 5.3 The reconciliation digest

Both sides compute it identically:

1. For each assignment, form `monitor_id` (16 bytes) ‖ `0x00` ‖ `config_version` (UTF-8)
   ‖ `0x00`.
2. Sort those byte strings in unsigned lexicographic order.
3. SHA-256 over the concatenation.
4. Lowercase hex.

An empty set digests as SHA-256 of the empty string. `config_version` is the only config
input, which is why it MUST change whenever `config` or any scheduling field changes —
a control plane that edits a timeout without changing `config_version` has made
reconciliation blind.

On `Reconcile`, the probe compares. Equal digests: nothing happens, and this is the
common case. Different: the probe **closes the stream and reopens it with an empty
`known_set_version`**, which forces a full set. It cannot ask on the stream itself —
`WatchAssignments` is server-streaming, so the reopen *is* the request.

### 5.4 Validation, and rejection instead of lying

`Validate(config)` runs **at assignment time, not check time** (decision 11). A monitor
whose config the probe cannot parse produces one `AssignmentRejection` on the next
`ResultBatch` and **no results at all** — it is not scheduled, and it does not emit
`unknown` 250 times an hour to say the same thing.

| Reason | When |
|---|---|
| `REASON_UNSUPPORTED_TYPE` | The probe has no checker for `type` |
| `REASON_INVALID_CONFIG` | Config does not parse or fails the checker's validation |
| `REASON_CAPABILITY_UNAVAILABLE` | Checker exists but is unavailable here — `icmp` without `CAP_NET_RAW` |
| `REASON_AT_CAPACITY` | Accepting would exceed `max_concurrent_checks` |
| `REASON_INTERVAL_BELOW_MINIMUM` | Below the probe's per-type floor — `domain_expiry` against rate-limited RDAP |

A rejection names the `config_version` it applies to, so the control plane can tell a
stale rejection from a current one. The control plane surfaces it against that monitor
as a configuration error; the monitor MUST NOT sit in `pending` forever, which is the
failure this message exists to prevent.

A control plane SHOULD NOT assign a type the probe did not advertise as available, and a
probe MUST reject it anyway if it does. Belt and braces: capability state can change
across a restart in between.

### 5.5 Outage behaviour

The probe keeps running its last known assignment set while the control plane is
unreachable, indefinitely. Assignments are **never written to disk** — they contain
every secret belonging to every monitor in them, and the probe is by design the least
trusted host in the system (decision 15).

## 6. Executing checks

### 6.1 Scheduling

| Rule | Value | Why |
|---|---|---|
| One min-heap keyed on next-due | — | 5,000 timers is survivable in Go and the wrong shape at 50,000 |
| Deterministic dispersal | phase offset = `hash(monitor_id) mod interval` | Without it, 5,000 monitors imported on a 60 s interval fire in the same second — a 5,000-wide burst instead of 83/s. Cheapest line in the design and the easiest to forget |
| Bounded worker pool | default 2,000 concurrent | ~75 concurrent at steady state; ~1,250 on a day when everything times out |
| Lateness budget | 50% of interval | Past it, the check is `SKIPPED`, never queued |
| Global rate cap | `max_checks_per_second` | Politeness and self-protection |
| Per-destination-host concurrency cap | operator-configurable | 500 monitors against one host must not look like an attack |
| Fair share across sessions | deficit round-robin on `share` | 20,000 monitors in one session must not starve 40 in another |

**Overload sheds; it never queues.** An unbounded queue turns overload into a memory
leak and then emits results timestamped from the past, which is worse than no data
because it is data that looks fine.

**Connection reuse.** Fresh connection per check by default: a pooled connection can
report `up` against a socket the far end has half-closed, and it measures none of DNS,
TCP, or TLS. The cost is recovered in a **strictly TTL-honouring** DNS cache, shared
across sessions because a DNS answer is a public network fact. A stale entry is never
served — if the operator's DNS is broken, the honest answer is a failure. The `dns`
monitor type never touches this cache.

### 6.2 Outcomes

| Outcome | Meaning | Pages anyone? | `heartbeats.status` |
|---|---|---|---|
| `OUTCOME_UP` | Ran, all assertions passed | no | 1 |
| `OUTCOME_DOWN` | Ran, target failed or an assertion failed | **yes** | 0 |
| `OUTCOME_UNKNOWN` | Could not perform the check | no — alert on probe health | 4 |
| `OUTCOME_SKIPPED` | Never started: shed or past its lateness budget | no — capacity signal | 5 |

**The wire enum and the stored encoding do not share numbers** — proto3 requires 0 to be
`UNSPECIFIED` and the schema uses 0 for `down`. The mapping above is the only place they
are related, and an implementation that casts one to the other silently inverts up and
down.

`pending` (2) and `maintenance` (3) exist in storage and are never sent by a probe:
`pending` is a threshold the control plane counts, `maintenance` a window it holds.

`unknown` and `skipped` are **excluded from uptime ratios**, exactly as an absent bucket
is (data model §5.3): `uptime_ratio` is null whenever `up_count + down_count = 0`. A
status page that renders "our probe fell over" as customer downtime is lying, and Phase
2's SLA reports would inherit the lie.

The distinction that carries the whole taxonomy: **a DNS failure resolving the target is
`down`; a DNS failure because the probe's own resolver is broken is `unknown`.** A
checker that cannot tell them apart MUST report `unknown`. Over-reporting `down` is how
one broken probe pages an entire on-call rotation.

### 6.3 Retries, assertions, inversion

- Attempts run probe-side at `retry_interval_seconds`, up to `retries + 1` attempts.
  **Each attempt is one `Result`**, with `attempt` 1-based — which is what
  `heartbeats.attempt` anticipates.
- The probe does not decide what a run of failures means. `consecutive_failures`, the
  transition, and the notification are control-plane work, and where both sides hold a
  number (`retries` is the example) **the control plane is authoritative**.
- Assertions — status code, keyword, JSON path, regex, response-time threshold — are
  applied by the checker, because they require the response payload.
- `upside_down` inverts `up` and `down` **after** assertions. It MUST NOT invert
  `unknown` or `skipped`: those are statements about the probe, and an operator asking
  to invert a monitor did not ask to invert their probe's health.
- A timeout is a target failure: `DOWN` with `ERROR_CLASS_TIMEOUT`.

### 6.4 Where each type runs

| Type | Executed by | Constraint |
|---|---|---|
| `http`, `tcp`, `tls_expiry`, `grpc` | probe | outbound TCP |
| `icmp` | probe | `CAP_NET_RAW` or unprivileged ICMP sockets — **declared as a capability**, with TCP fallback offered |
| `dns` | probe | outbound 53; never uses the shared resolver cache |
| `domain_expiry` | probe | RDAP/WHOIS egress; needs a per-type minimum interval far above the 20 s floor, enforced probe-side as well as at the API |
| `docker` | probe | Docker socket on that host — intrinsically probe-local, so the control plane must be able to pin the monitor to a **named probe**, not merely a region |
| `push` | **control plane** | Never assigned to a probe at all: nothing to execute, and the dead-man's-switch evaluation is control-plane work |

## 7. Results

### 7.1 Batching

Results go up `StreamResults` in batches, because the storage layer's primary operation
is already a batch write (data model §5.1) — a received frame should hand almost
straight to `HeartbeatStore.WriteBatch`.

Flush on **1 second or 500 results, whichever comes first**, both overridable by
`RegisterResponse`. A batch MUST carry `ProbeHealth` at least every
`health_interval_seconds` (default 30), including on an otherwise empty batch: a probe
with nothing to report is exactly when its health matters.

### 7.2 Idempotency and acknowledgement

- Every result carries a `result_id` (UUIDv7), generated **once, when the check
  completes**. A probe that mints a new id on replay has defeated deduplication
  entirely.
- `result_id`s MUST increase monotonically within a session, and batches MUST be sent in
  that order, because the ack is a high-water mark over exactly this ordering.
- Delivery is **at-least-once**. Ingest deduplicates with
  `INSERT … ON CONFLICT DO NOTHING` against `(org_id, monitor_id, time, probe_id)` —
  `result_id` is **not persisted per heartbeat**, and
  [data model §11.8](../data-model/README.md#118-heartbeat-idempotency-and-the-unknown-outcome)
  records why, along with the maintainer ruling that is still outstanding on it. Its job
  on the wire is the acknowledged high-water mark, which is what makes a resumed stream
  resume in the right place.
- `ResultAck.acknowledged_through_result_id` means every result at or below it, in
  unsigned byte order, is durable. **The probe frees buffer only on ack, never on send.**
- `duplicate` counts are expected after a reconnect and healthy. A persistently non-zero
  count without reconnects means the probe is regenerating ids.
- `rejected` results are dropped, never resent. Resending something the control plane
  has already refused is an infinite loop with extra steps.

### 7.3 Buffering and shedding

| | Default |
|---|---|
| Buffer bound | 64 MB **or** 250,000 results, whichever binds first |
| Coverage at 5,000 monitors on the 20 s floor | order of 40 minutes — *estimated, not measured* ([probe-plan §10](probe-plan.md#10-how-this-gets-validated)) |
| Overflow policy | shed the **oldest results with `outcome_changed` unset** first |
| Disk spill | off, and out of scope for Phase 1 — it reintroduces state and puts customer data on the probe host |

Shedding is honest by construction: a bucket with no checks has no row, and absence
means "no data", not downtime. **Every shed result increments `shed_results_total`**,
which the control plane surfaces — a probe quietly dropping data must be visible in the
UI.

`outcome_changed` is a **shedding priority hint**, not `heartbeats.important`. The
control plane computes the stored column itself, from state the probe does not have, and
may disagree with the flag. (This is a deliberate narrowing of the field ADR-005 §4.8
called `important`; the storage column is unchanged and its meaning is unchanged. Flagged
here rather than assumed, because the name in the ADR sketch implies an authority the
probe does not have.)

## 8. Probe health and clock skew

The probe has no inbound port, so self-metrics ride the result stream and the control
plane republishes them on its own `/metrics`, labelled by probe. `--metrics-addr` exists,
is **off by default**, and is documented as opening a listening socket.

**Clocks.** `Result.time_unix_micros` is the probe's clock and the control plane MUST NOT
rewrite it: a corrected timestamp changes the natural key of a row that may already
exist, turning a deduplicated replay into a second row. Skew is reported instead —
`RegisterResponse.server_time_unix_micros` lets the probe compute an offset it reports in
`ProbeHealth.clock_offset_micros`.

| Observed skew | Behaviour |
|---|---|
| ≤ 5 s | Normal; recorded |
| > 5 s | Control plane surfaces a probe warning |
| > 300 s | Control plane MAY refuse registration with `FAILED_PRECONDITION` |

The last row is a judgement call worth stating: timestamps five minutes wrong poison
rollups and produce heartbeats in the future, and a probe with a broken clock is more
useful stopped and visible than running and subtly wrong.

## 9. Transport, timeouts, and reconnection

gRPC only, per ADR-001 and decision 10. NATS stays between control-plane components and
browsers and does not extend to the probe path.

| | Default | Notes |
|---|---|---|
| Keepalive ping interval | 30 s | Corporate firewalls and NATs silently drop idle connections |
| Keepalive timeout | 20 s | |
| `permit_without_stream` | true | The connection matters even when both streams are idle |
| **Server enforcement policy** | `MinTime` 25 s, `PermitWithoutStream` true | **The trap:** grpc-go's server default `MinTime` is 5 minutes, so a probe pinging every 30 s gets `GOAWAY: too_many_pings` and reconnects forever. A control plane MUST relax this to match |
| Unary deadline | 30 s | `Enrol`, `IssueToken`, `Register` |
| Stream deadline | none | Long-lived by design |
| Reconnect backoff | 1 s base, ×1.6, ±20% jitter, 60 s cap | Reset after 60 s of a healthy stream |
| Max receive size | 4 MiB (default) | Hence chunked assignment sets (§5.2) |

Every control-plane→probe action is *"takes effect on next connect"*, never synchronous:

| Action | Probe connected | Probe disconnected |
|---|---|---|
| Assign or update a monitor | milliseconds, on the open stream | delivered on reconnect |
| Revoke the probe | immediate | within one access-token lifetime |
| Probe restarts during an outage | — | comes back empty and idle |

## 10. Errors

| Status | Meaning | Probe MUST |
|---|---|---|
| `UNAUTHENTICATED` | Missing, expired, or invalid access token | Refresh via `IssueToken` once, retry once; then treat as transient |
| `PERMISSION_DENIED` | Probe disabled, revoked, or credential not valid for this probe | Stop checks for **this session only**, retain the buffer, retry every 5 min. Do not exit: the other sessions are unaffected, and revocations are reversed |
| `FAILED_PRECONDITION` | Major protocol mismatch, unacceptable clock skew, or streaming before `Register` | Log the control plane's message verbatim — it names both versions and what to do — and back off long. A retry loop will not fix a version mismatch |
| `RESOURCE_EXHAUSTED` | Batch too large, or the control plane is overloaded | Halve the batch size, back off. Prefer `ResultAck.pause_millis`, which costs no reconnect |
| `INVALID_ARGUMENT` | Malformed request | A bug. Log loudly, do not retry the same message |
| `NOT_FOUND` / `ALREADY_EXISTS` | Enrolment token unknown or already used | Exit non-zero with the message. Enrolment is interactive; retrying never helps |
| `UNAVAILABLE` | Transport failure, restart, network | Reconnect with backoff. The expected error, and the one that must never lose buffered results |

A control plane MUST NOT return `INTERNAL` where one of the above fits: the probe's
behaviour is chosen from the code, and a generic code produces a generic reaction.

## 11. Versioning and compatibility

- Package `cairn.probe.v1`, **additive only**: field numbers are never reused, never
  renumbered, and a field's meaning never changes. Anything breaking is
  `cairn.probe.v2`, and v1 keeps working for the deprecation window in
  [docs/api/README.md](../api/README.md).
- `RegisterRequest` carries `protocol_version` and `agent_version`. The control plane
  refuses **only on a major mismatch**, with an error naming both versions and the
  remedy.
- **Within v1, any v1 probe works against any v1 control plane.** Feature differences are
  handled by capability negotiation, never by version gating — this is what Phase 0
  §3.5's compatibility requirement actually asks for. A v1.0 probe simply does not
  advertise a check type introduced later, and the control plane does not assign it any.
- **No self-update.** A binary that can replace itself is a remote code execution path
  with extra steps. Upgrades are the operator's package manager or container.

## 12. Security model

**A probe is a credential holder.** It holds, in memory, every secret belonging to every
monitor assigned to it — HTTP basic and bearer credentials, gRPC metadata, Docker client
TLS keys. This is stated plainly here and MUST be stated in the operator documentation,
because it is the first thing a security-conscious enterprise will ask about.

Consequences, each of them a requirement:

- An assignment carries **only that probe's** secrets, never the org's full set.
- Secrets are never written to disk, never logged, and **redacted from every error path
  including timeouts** — a URL with embedded credentials escapes through error strings
  more often than through anything else.
- **Per-probe revocation** exists and is the documented response to suspected compromise.
- A private probe in a customer's VPC is by design the least trusted host in the system
  and holds the most sensitive assignment set. That tension is inherent to ADR-001.

**The isolation invariant, verbatim from ADR-005 decision 3, because it is a security
boundary and not internal hygiene:**

> The only state shared between sessions is derived from public network facts (DNS
> answers) and from resource accounting (rate limiters, the worker pool, aggregate
> metrics). Credentials, cookies, TLS session tickets, HTTP connection pools, response
> bodies, and results are never shared across sessions and never keyed by anything less
> than the session.

An HTTP connection pool keyed only by host would let one customer's session reuse a
connection another customer's session authenticated. That is the specific defect this
invariant exists to prevent, and it is written by accident, not by malice.

**What a probe MUST NOT do**, collected in one list because each of these is a plausible
mistake rather than a straw man:

1. Listen on a socket, except `--metrics-addr` when the operator explicitly asks.
2. Persist assignments, results, or monitor secrets.
3. Send notifications, or evaluate state transitions, thresholds, dependencies, or
   maintenance windows.
4. Emit `pending` or `maintenance`.
5. Report `down` when it cannot tell whether the target or the probe failed.
6. Regenerate `result_id` on replay, or free buffer on send.
7. Accept a control plane it was not configured with, or let one control plane learn of
   another.
8. Log a secret, a full config blob, or an unredacted URL.

## 13. Solo mode

Solo mode runs the probe **in-process over an in-memory gRPC connection (`bufconn`), with
real serialisation** (decision 14). No enrolment, no credentials, no TLS, no port; an
`embedded`-mode row in `probes` is the identity, so `Enrol` and `IssueToken` are never
called and `Register` is where `probe_id` comes from.

One correction to ADR-005 decision 14's wording, since an implementer will go looking:
migration `0001_initial.sql` defines `probes.mode` with `embedded` as a value but **seeds
no row**. Creating it is Phase 1 bootstrap work, alongside the default organisation.

The cost is a marshal/unmarshal per message, estimated at 1–2 µs — under a millisecond
of CPU per second at 250 results/s, and unverified until the harness measures it. The
return is that **every solo install continuously exercises the identical code path remote
probes use**: solo mode is the protocol's integration test, run by every user, every day.

The alternative — direct interface calls, skipping serialisation — is the shortcut
ADR-001 exists to forbid. The first field that works in-process and not on the wire would
be found by a Phase 4 user rather than by us.

## 14. Conformance checklist

A third-party probe is conforming when all of the following hold. This list is the
`.proto` files' companion and is what a reviewer checks against.

- [ ] Dials outbound only; opens no listening socket by default.
- [ ] Enrols once per control plane; never retries a single-use token.
- [ ] Refreshes the access token at 50% of remaining lifetime.
- [ ] Registers before opening either stream, and re-registers after every reconnect.
- [ ] Declares every check type it implements, with `available = false` and a
      user-facing `reason` for those it cannot run here.
- [ ] Applies a full set atomically only on `final = true`; discards partial sets.
- [ ] Computes the digest exactly as §5.3 specifies and forces a full set on mismatch.
- [ ] Validates config at assignment time and reports `AssignmentRejection` rather than
      emitting results for a monitor it cannot run.
- [ ] Disperses start times by `hash(monitor_id) mod interval`.
- [ ] Sheds rather than queues, and reports shed work as `SKIPPED`, never `DOWN`.
- [ ] Reports `UNKNOWN` — never `DOWN` — when the probe itself is the failure.
- [ ] Applies `upside_down` to `up`/`down` only.
- [ ] Emits one result per attempt, with `attempt` 1-based.
- [ ] Generates monotonic `result_id`s once per result, and never regenerates on replay.
- [ ] Frees buffer only at or below the acknowledged high-water mark.
- [ ] Sheds oldest non-`outcome_changed` results first, and counts every one.
- [ ] Sends `ProbeHealth` at least every `health_interval_seconds`, empty batch or not.
- [ ] Keeps checking through a control-plane outage, and comes back empty after a restart
      during one.
- [ ] Never writes an assignment or a secret to disk, and redacts secrets from every
      error path.
- [ ] Keeps sessions isolated per §12 when serving more than one control plane.

## 15. What this document does not decide

Named so the gaps are visible rather than discovered:

- **Whether generated code is committed.** The tooling is settled — buf, with
  `buf lint`, `buf format`, and `buf breaking` in CI ([proto/README.md](../../proto/README.md))
  — and the additive-only promise in §11 is checked mechanically against the last release
  tag rather than by review. What is still open is whether the generated Go is committed
  alongside the definitions, which is a call for the PR that adds the first consumer.
- **Every default in this document is reasoned, not measured.** Buffer coverage, batch
  sizes, the worker ceiling, `bufconn` overhead — [probe-plan §10](probe-plan.md#10-how-this-gets-validated)
  lists what would test each one, and the harness is where that happens.
- **N-of-M consensus** (Phase 4). The protocol carries what it will need — per-probe
  results, `unknown` distinct from `down`, probe identity on every heartbeat — and
  nothing more. Note that consensus is meaningless for host-local types: a `docker` check
  runs where the daemon is and there is no second opinion to be had.
- **Shared join tokens** for autoscaling fleets. Pre-registration is decided (decision 8);
  join tokens are an explicit opt-in if fleet size ever demands it, and they would change
  `EnrolRequest`.
- **The alerting gap.** A control-plane outage stops alerting even though data collection
  survives ([probe-plan §8.3](probe-plan.md#83-adr-001-claims-ha-what-it-delivers-is-data-ha)).
  That is a documentation obligation on the ops docs, not something this protocol can fix.
