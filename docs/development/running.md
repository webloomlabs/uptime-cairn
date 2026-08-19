# Running the development build

What exists today is the [Phase 1](../plans/PHASE-1-PLAN.md) Month 1 checkpoint:

> a monitor can be created via `curl`, checked on schedule, and its history
> queried via the API — no UI yet.

HTTP/HTTPS monitors only. Everything below has been run; the numbers in the
output are real.

## Start it

```sh
go run ./cmd/cairn -data-dir /tmp/cairn-data -listen 127.0.0.1:3000
```

On the first start it creates `cairn.db`, runs the migrations, generates an
encryption key, and registers the embedded probe:

```
level=INFO msg="migration applied" version=1 name=initial
level=INFO msg="migration applied" version=2 name=identity
level=WARN msg="generated a new encryption key — back it up separately from the database,
                because without it every stored secret is unrecoverable" path=/tmp/cairn-data/cairn.key
level=INFO msg="first-run setup is required: POST /api/v1/setup to create the administrator account"
level=INFO msg="probe capability unavailable" type=icmp reason="not implemented in this build"
level=INFO msg="probe registered" name=embedded capabilities=1 unavailable=7
```

Those `capability unavailable` lines are the protocol working as designed: the
probe declares every type the product defines and says which ones it can
actually run here, so "no probe can run this monitor" is a fact the control plane
holds before a single check runs ([protocol.md §4](../probe/protocol.md)).

The encryption key protects secrets at rest — today the TOTP secret, and every
monitor and notification credential as those land. It is generated so that
`docker run` needs no key management to work, and it can come from
`--encryption-key-file`, `CAIRN_ENCRYPTION_KEY_FILE`, or `CAIRN_ENCRYPTION_KEY`
instead ([data model §12.3](../data-model/README.md)). **Back it up separately
from the database.** Losing it while encrypted data exists is fatal on purpose:
the process refuses to start rather than generating a replacement that would
render every stored secret unreadable while appearing to work.

## Create the administrator

Nothing under `/api/v1` answers without a credential. The one exception is
first-run setup, and it closes permanently the moment an account exists.

```sh
curl -s -c jar.txt -X POST localhost:3000/api/v1/setup \
  -H 'Content-Type: application/json' \
  -d '{"email": "you@example.com", "password": "correct-horse-battery", "instance_name": "Cairn Dev"}'
```

`201`, a session cookie in `jar.txt`, and a body carrying the user, the scopes
the `owner` role holds, and a `csrf_token`. Passwords are hashed with argon2id;
a second call to `/api/v1/setup` gets `409` for good.

**Cookie-authenticated writes must echo that token** in `X-Cairn-CSRF-Token`.
Reads do not. A cookie is ambient — the browser attaches it to any request,
including one a hostile page triggered — so a write has to prove the caller could
read the login response. Bearer tokens are not ambient and need no such proof.

```sh
CSRF=$(curl -s -c jar.txt -X POST localhost:3000/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email": "you@example.com", "password": "correct-horse-battery"}' | jq -r .csrf_token)
```

Failed sign-ins are rate-limited: five per address per fifteen minutes, after
which even the right password waits, because a limiter that lets the correct
guess through is an oracle for which guess was correct.

## Create an API key

```sh
curl -s -b jar.txt -H "X-Cairn-CSRF-Token: $CSRF" \
  -X POST localhost:3000/api/v1/api-keys \
  -H 'Content-Type: application/json' \
  -d '{"name": "ci", "scopes": ["monitors:read", "monitors:write", "heartbeats:read"]}'
```

The plaintext key is in that response and nowhere else — the server stores a
hash. Afterwards a listing shows only the prefix (`cairn_7Kq2`), which is enough
to recognise a key and not enough to use one.

Two rules worth knowing before you script against it:

- **`write` implies `read` on the same resource, and nothing else implies
  anything.** A hierarchy where one scope quietly grants another is one nobody
  can audit.
- **A key cannot be granted a scope its creator does not hold.** Otherwise the
  weakest key in an install can mint the strongest one.

Revocation is immediate — authentication checks `revoked_at` on every request —
and the row survives it so audit entries stay resolvable.

## Optional: two-factor

```sh
curl -s -b jar.txt -H "X-Cairn-CSRF-Token: $CSRF" -X POST localhost:3000/api/v1/auth/totp
# → {"secret": "...", "provisioning_uri": "otpauth://totp/Cairn%20Dev:you@example.com?..."}
curl -s -b jar.txt -H "X-Cairn-CSRF-Token: $CSRF" -X POST localhost:3000/api/v1/auth/totp/confirm \
  -H 'Content-Type: application/json' -d '{"totp_code": "123456"}'
# → {"recovery_codes": ["b6nm-svra-478p", ...]}   shown once, never again
```

Enrolment does not take effect until it is confirmed, so an enrolment abandoned
halfway cannot lock anyone out. After that, a password-only login returns `401`
with a `type` ending `/totp-required`, and the client resubmits with `totp_code`
or a single-use `recovery_code`. The secret itself is encrypted at rest and bound
to its own row: moved onto another user, the ciphertext fails to open.

## Create a monitor

Either credential works. With the key:

```sh
curl -s -H "Authorization: Bearer $KEY" \
  -X POST localhost:3000/api/v1/monitors \
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
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:3000/api/v1/monitors/<id>/heartbeats" | jq '.data[]'
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
curl -s -H "Authorization: Bearer $KEY" -X POST localhost:3000/api/v1/monitors \
  -H 'Content-Type: application/json' \
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

| | Scope |
|---|---|
| `GET /healthz`, `GET /readyz` | None — outside `/api/v1` and outside auth |
| `GET /api/v1/setup`, `POST /api/v1/setup` | None — open only until an administrator exists |
| `POST /api/v1/auth/login` | None — this is how a caller obtains a credential |
| `POST /api/v1/auth/logout`, `GET /api/v1/auth/session` | Any authenticated caller |
| `POST/DELETE /api/v1/auth/totp`, `POST .../totp/confirm` | User session only — not API keys |
| `GET /api/v1/api-keys`, `GET /api/v1/api-keys/{id}` | `api_keys:read` |
| `POST/PATCH/DELETE /api/v1/api-keys...` | `api_keys:write` |
| `GET /api/v1/monitors`, `GET /api/v1/monitors/{id}` | `monitors:read` |
| `POST`, `DELETE` `/api/v1/monitors...` | `monitors:write` |
| `GET /api/v1/monitors/{id}/heartbeats` | `heartbeats:read` |
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
rollback, heartbeat write idempotency, and — since they are the ones that fail
open — scope enforcement, CSRF, rate limiting, key revocation, privilege
escalation, recovery-code single use, and ciphertext relocation.

## What is deliberately missing

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
