# Running the development build

What exists today is the [Phase 1](../plans/PHASE-1-PLAN.md) Month 1 checkpoint:

> a monitor can be created via `curl`, checked on schedule, and its history
> queried via the API — no UI yet.

HTTP/HTTPS monitors only. Everything below has been run; the numbers in the
output are real.

## Start it

```sh
go run ./cmd/cairn -data-dir /tmp/cairn-data -listen 127.0.0.1:3000 -insecure-no-auth
```

`--insecure-no-auth` is not optional yet, and it is deliberately uncomfortable to
type. Authentication — first-run setup, sessions, TOTP, scoped API keys — is
specified in [openapi.yaml](../api/openapi.yaml) and is the next thing to build.
Until it exists the API answers `401` with an explanation rather than quietly
accepting everything, and this flag is how an operator says "I know, run anyway".
Do not expose the port.

On the first start it creates `cairn.db`, runs migration `0001`, and registers
the embedded probe:

```
level=INFO msg="migration applied" version=1 name=initial
level=INFO msg="probe capability unavailable" type=icmp reason="not implemented in this build"
level=INFO msg="probe registered" name=embedded capabilities=1 unavailable=7
level=INFO msg="registered with control plane" probe=00000000-0000-7000-8000-000000000002
```

Those `capability unavailable` lines are the protocol working as designed: the
probe declares every type the product defines and says which ones it can
actually run here, so "no probe can run this monitor" is a fact the control plane
holds before a single check runs ([protocol.md §4](../probe/protocol.md)).

## Create a monitor

```sh
curl -s -X POST localhost:3000/api/v1/monitors \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Example dot com",
    "type": "http",
    "interval_seconds": 20,
    "timeout_seconds": 10,
    "config": {
      "url": "https://example.com",
      "accepted_status_codes": ["200-299"],
      "keyword": {"value": "Example Domain", "mode": "contains"}
    }
  }'
```

`201`, with a `Location` header and the monitor in `pending` — it has not earned
a verdict yet, and the first heartbeat lands on the next scheduler tick rather
than synchronously with the call.

## Watch it check

```sh
curl -s "localhost:3000/api/v1/monitors/<id>/heartbeats" | jq '.data[]'
```

```json
{
  "monitor_id": "01a015e0-5b99-73fa-a960-d3a78ab7528b",
  "time": "2026-08-18T17:17:21.952330Z",
  "status": "up",
  "response_time_ms": 68.462,
  "code": "200",
  "attempt": 1,
  "important": false,
  "probe_id": "00000000-0000-7000-8000-000000000002"
}
```

`important` is true only on the heartbeat that changed the verdict — the events,
not the ticks.

## Retries and the pending state

```sh
curl -s -X POST localhost:3000/api/v1/monitors -H 'Content-Type: application/json' \
  -d '{"name":"Refused","type":"http","interval_seconds":20,"timeout_seconds":5,
       "retries":1,"retry_interval_seconds":20,"config":{"url":"http://127.0.0.1:1/"}}'
```

With `retries: 1` the first failure is `pending`, not `down`; the second attempt
20 seconds later is what earns the verdict, and that heartbeat is the one marked
`important`:

```
17:17:58 down attempt=1 important=false | dial tcp 127.0.0.1:1: connect: connection refused
17:18:18 down attempt=2 important=true  | dial tcp 127.0.0.1:1: connect: connection refused
```

The probe ran the retries; the control plane decided what they meant. That split
is [ADR-005](../adr/005-probe-architecture.md) decision 1 and it is not
negotiable in either direction.

## What the API answers

| | |
|---|---|
| `GET /healthz`, `GET /readyz` | Outside `/api/v1` and outside auth |
| `GET /api/v1/monitors` | Cursor-paginated, `limit` clamped to 100 |
| `POST /api/v1/monitors` | 422 with one entry per invalid field |
| `GET /api/v1/monitors/{id}` | |
| `DELETE /api/v1/monitors/{id}` | Unassigns the monitor from the probe immediately |
| `GET /api/v1/monitors/{id}/heartbeats` | `important_only=true` for events alone |
| Anything else under `/api/v1/` | `501`, naming the spec — the endpoint exists in the contract, not in this build |

Errors are RFC 9457 problem documents, and clients branch on `type`:

```json
{
  "type": "https://uptimecairn.dev/errors/validation-failed",
  "title": "Validation failed",
  "status": 422,
  "errors": [
    {"pointer": "/interval_seconds", "code": "below_minimum",
     "message": "interval_seconds must be at least 20"}
  ]
}
```

## Tests

```sh
go test ./...
go test -race ./...
```

The suite covers the parts where a bug would be silent rather than loud: the
HTTP checker's assertion and failure classification, the control plane's
transition table, the result buffer's shedding order, migration checksums and
rollback, and heartbeat write idempotency.

## What is deliberately missing

- **Authentication.** See above. Next.
- **The other eight monitor types.** The registry takes one file each; the API
  refuses them at creation today rather than accepting a monitor that would sit
  pending forever, and the probe advertises them as unavailable.
- **`json_path` assertions.** Rejected at validation rather than ignored: a
  monitor reporting `up` without evaluating the assertion the user asked for is
  worse than one that refuses to start.
- **Notifications, rollups, status pages, the UI, the importer.** Phase 1
  Months 2–4.
- **The load-test harness against the real engine.** Its `http` target still
  refuses to run, which is the honest state until someone points it here.
