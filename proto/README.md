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

## Tooling: buf

Decided: **buf**, configured in [buf.yaml](../buf.yaml) and
[buf.gen.yaml](../buf.gen.yaml), checked in CI by
[.github/workflows/proto.yml](../.github/workflows/proto.yml).

The reason is the additive-only promise above. `protoc` compiles; it has no
opinion about whether field 7 meant something else last release. `buf breaking`
does, and mechanically — which is the only kind of architectural rule that
survives a deadline.

```sh
buf lint                 # DEFAULT rules, minus the two exceptions buf.yaml argues for
buf format -w            # the formatter is the style guide; CI checks it
buf build                # parse and compile everything
buf generate             # Go + gRPC stubs, beside the definitions
buf breaking --against ".git#tag=$(git describe --tags --abbrev=0)"
```

Installing it: `brew install bufbuild/buf/buf`, or the release binaries at
<https://github.com/bufbuild/buf/releases>. CI pins a version; a local buf that
is newer will occasionally disagree about formatting, and the pinned one wins.

**The breaking check compares against the last release tag, not against `main`.**
Before the first tag it explains itself and passes: the protocol is still a Phase
0 draft under review, nothing is deployed, and treating an unreviewed draft as
frozen would mean writing a superseding version before anyone has implemented the
first one.

## Code generation

Generation needs two local plugins, which buf invokes:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
buf generate
```

Local rather than remote plugins on purpose: a build that reaches a registry for
a code generator is a build that fails when the registry does, and reproducible
builds are a Phase 0 §3.6 deliverable. When Phase 1 makes generation part of the
build, those two versions belong in the root module as tool dependencies so they
are pinned in `go.sum` rather than in this file.

Output lands beside the definitions (`proto/cairn/probe/v1/*.pb.go`), matching
each file's `option go_package`, so an import path and a directory path stay the
same thing.

**Generated code is not committed today**, because nothing imports it yet.
Committing it once Phase 1 does is the recommendation — it keeps buf and the two
plugins off the critical path for a contributor who only wants to build the
server — but that is a call to make with the first consumer, in the PR that adds
it, not now.
