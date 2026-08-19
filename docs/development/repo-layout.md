# Repository layout

The skeleton [Phase 1](../plans/PHASE-1-PLAN.md) fills in. Every directory below
exists; almost all of the Go in them is package documentation and stubs that
return "not implemented", because the point of writing the structure first is to
fix the seams before there is code with an opinion about them.

```
cmd/cairn/            the binary: flags, signals, exit codes, and nothing else
internal/
  app/                composition root — the only package that wires the others
  config/             flags and defaults
  version/            build identity, set by -ldflags
  model/              domain types; imports nothing from this repository
  auth/               password hashing, tokens, TOTP, scopes
  secrets/            encryption at rest: envelopes, data keys, the root key
  store/              the persistence seam (ADR-002)
    sqlite/           embedded SQLite: the solo default, the one that runs on a Pi
    migrate/          our own forward-only migration runner
  probe/              check execution behind the ADR-001 gRPC seam
    check/            the monitor-type registry and the Checker interface
  controlplane/       assignment, ingest, state transitions, dispatch, rollups
  api/                /api/v1 and the embedded UI
  notify/             alert channels and webhook templating
  status/             public status pages
  importer/kuma/      cairn import kuma
  telemetry/          logs, metrics, health endpoints
  ui/                 //go:embed of the built frontend
migrations/           numbered SQL, embedded; sqlite/ now, postgres/ in Phase 4
proto/                the probe protocol (docs/probe/protocol.md is its semantics)
web/                  SvelteKit frontend; builds into internal/ui/dist
harness/              the 5,000-monitor load gate — its own Go module, built first
docs/                 ADRs, plans, API spec, data model, probe protocol
```

## Import rules

Three, and they are the reason the layout is worth writing down. Each is
mechanically checkable, which is the only kind of architectural rule that
survives a deadline.

**1. `internal/probe` imports neither `internal/store` nor
`internal/controlplane`.** The probe reaches the control plane over gRPC and
nothing else — in solo mode over an in-memory bufconn with real serialisation,
exactly as remote probes do ([ADR-001](../adr/001-probe-and-control-plane-split.md),
[ADR-005](../adr/005-probe-architecture.md) decision 14). This is the seam that
cannot be retrofitted. If the scheduler ever reaches around it — one direct call,
one shared struct, one assumption that a result is available synchronously —
that is the day the design stops being true, and it will not announce itself.

**2. Only `internal/app` constructs concrete implementations.** Nothing else
imports `internal/store/sqlite`. Everything else takes an interface from
`internal/store`, which is what keeps the SQLite install and the
Postgres/Timescale install the same product with different wiring rather than
two codebases wearing one name.

**3. `internal/model` imports nothing from this repository.** A domain type that
knows about storage, HTTP, or gRPC drags one of them into every package that
touches it — and this is the package everything touches.

Beyond those: `cmd` imports `app` and little else, and no package imports `cmd`.

## Two modules, on purpose

`harness/` is its own Go module. It was built before the product it measures and
must stay buildable without the product's dependency tree — a gate that cannot
run until the thing it gates compiles is not a gate. The root module currently
has **no third-party dependencies at all**; the first ones arrive with real code
(`modernc.org/sqlite`, `grpc-go`, `protobuf`), each needing the justification
[AGENTS.md](../../AGENTS.md) §5 asks for.

## Building

```sh
go build ./...            # server skeleton; runs, prints its config, exits 1
go vet ./...
go run ./cmd/cairn -version
cd harness && go mod tidy && go build .   # the load gate, separate module
```

The wire formats have their own toolchain, needed only when editing `proto/`:

```sh
buf lint
buf format -w
buf generate              # Go + gRPC stubs; not committed, nothing imports them yet
```

`go run ./cmd/cairn` exits non-zero with "Phase 1 has not been built yet". That
is deliberate: a skeleton that starts and silently does nothing gets discovered
by a user, and one that says so gets discovered by the developer who wired it.

The frontend build (`web/` into `internal/ui/dist/`) is not wired up yet;
`internal/ui/dist/index.html` is a committed placeholder so `//go:embed` has a
directory to find in a clean checkout.

## What is deliberately not here yet

- **No generated protobuf code**, though the tooling for it is now wired:
  `buf lint`, `buf format`, and `buf breaking` run in CI, and `buf generate`
  produces `proto/cairn/probe/v1/*.pb.go` when someone needs them. Nothing
  imports them yet, so nothing is committed ([proto/README.md](../../proto/README.md)).
- **No Dockerfile, Makefile, or release workflow.** Deployment artefacts are
  [PHASE-1-PLAN.md](../plans/PHASE-1-PLAN.md) §4.2, and a Dockerfile written
  before the build it packages is a Dockerfile that will be rewritten.
- **No notifications, rollups, status pages, UI, or importer**, and eight of the
  nine monitor types. Phase 1 Months 2–4.
- **No user management beyond the first account.** One owner, created at setup.
  Additional users, roles, and teams are Phase 3, though the role column and the
  scope table already carry them.
