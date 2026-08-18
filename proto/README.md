# Protocol definitions

Wire formats live here, versioned by package. Today that is one:

| Package | Files | Semantics |
|---|---|---|
| `cairn.probe.v1` | [`cairn/probe/v1/`](cairn/probe/v1/) | [docs/probe/protocol.md](../docs/probe/protocol.md) |

**The `.proto` files are the smaller half of the deliverable.** Ordering,
idempotency, retry, backoff, buffering, and the rules about what a probe must
never do cannot be expressed in a message definition, and an implementation
written from these files alone will be wrong in ways that only surface during an
outage. Read the semantics document.

## Layout

```
proto/cairn/probe/v1/
  probe_service.proto   service, enrolment, credentials, registration, capabilities
  assignment.proto      assignment set, deltas, reconciliation
  result.proto          outcomes, results, acknowledgement, probe health
```

The directory path mirrors the proto package, so an import path is the file path
— `import "cairn/probe/v1/result.proto";` — with `proto/` as the only include
root a generator needs.

## Rules

**Additive only.** Field numbers are never reused, never renumbered, and a
field's meaning never changes. Anything breaking is a new package (`v2`), and the
old one keeps working for the deprecation window in
[docs/api/README.md](../docs/api/README.md). Within a version, feature
differences are handled by capability negotiation rather than version gating —
see [ADR-005](../docs/adr/005-probe-architecture.md) decision 7.

**The REST surface is not here.** `/api/v1` is specified in
[docs/api/openapi.yaml](../docs/api/openapi.yaml) and the two are deliberately
separate contracts: gRPC faces probes, REST faces every other client.

## Code generation

Not wired up yet, and deliberately so — the choice between `protoc` with
`protoc-gen-go`/`protoc-gen-go-grpc` and `buf` is a dependency decision
([AGENTS.md](../AGENTS.md) §5) plus a CI change, and neither belongs in the
document that first defines the messages. Generated code is not committed today
and no build depends on these files.

For reference, generating with `protoc` from the repository root would be:

```sh
protoc -I proto \
  --go_out=. --go_opt=module=github.com/webloomlabs/uptime-cairn \
  --go-grpc_out=. --go-grpc_opt=module=github.com/webloomlabs/uptime-cairn \
  proto/cairn/probe/v1/*.proto
```

`option go_package` currently places generated code beside the definitions;
moving it under `internal/` later is a one-line change per file.

Whichever tool is chosen, the additive-only promise above wants a
**breaking-change check in CI** rather than a review convention — `buf breaking`
against the previous tag is the usual shape.
