# Running the development build

What exists today is the [Phase 1](../plans/PHASE-1-PLAN.md) Month 1 checkpoint:

> a monitor can be created via `curl`, checked on schedule, and its history
> queried via the API — no UI yet.

All nine monitor types the spec defines run. Everything below has been run; the
numbers in the output are real.

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
level=INFO msg="probe registered" name=embedded capabilities=8 unavailable=0
```

Eight, not nine: `push` is absent from that list on purpose. It is evaluated by
the control plane against the clock and is never assigned to a probe at all
([ADR-005](../adr/005-probe-architecture.md) decision 6).

The count is the protocol working as designed — the probe declares every type the
product defines and says which ones it can actually run *here*, so "no probe can
run this monitor" is a fact the control plane holds before a single check runs
([protocol.md §4](../probe/protocol.md)). On a host that refuses raw sockets the
ICMP line carries a reason saying so; see [Ping and restricted
containers](#ping-and-restricted-containers).

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

## The other eight types

Same call, different `type` and `config`. Each of these has been run against a
live target; the messages are what the heartbeat actually carried.

```sh
# TCP — a completed handshake and nothing more. No probe byte is sent, because
# half the services worth watching close the connection on unexpected input.
'{"name":"TCP","type":"tcp","config":{"hostname":"one.one.one.one","port":443}}'
#   up   response_time_ms=106.456

# ICMP — unprivileged datagram socket first, raw second.
'{"name":"Ping","type":"icmp","config":{"hostname":"1.1.1.1","packet_count":2}}'
#   up   response_time_ms=12.701

# DNS — a named resolver, and the response code recorded as the heartbeat code.
'{"name":"DNS","type":"dns","config":{"hostname":"google.com","record_type":"A","resolver":"1.1.1.1"}}'
#   up   code=NOERROR   142.251.222.206

# TLS expiry — the code is the days remaining, on success and on failure alike.
'{"name":"TLS","type":"tls_expiry","config":{"hostname":"github.com"}}'
#   up   code=42   certificate valid for 42 days, until 2026-09-30T23:59:59Z

# Domain expiry — RDAP with a WHOIS fallback, one registry lookup a day.
'{"name":"Domain","type":"domain_expiry","config":{"domain":"google.com"}}'
#   up   code=756  google.com registered until 2028-09-14T04:00:00Z, 756 days away (per RDAP)

# gRPC — the standard grpc.health.v1.Health/Check protocol.
'{"name":"gRPC","type":"grpc","config":{"address":"127.0.0.1:1","use_tls":false}}'
#   down code=Unavailable   unavailable: connection error: ... connection refused

# Docker — one GET against the daemon socket, no client library.
'{"name":"Docker","type":"docker","config":{"container":"web","require_healthy":true}}'
#   down code=no_such_container   no container named "web" on this daemon
```

HTTP gained `json_path` alongside them:

```sh
'{"name":"API","type":"http","config":{
   "url":"https://api.github.com/",
   "json_path":{"path":"$.current_user_url","operator":"contains","expected":"github.com"}}}'
#   up   code=200
```

The path syntax is a deliberately small subset — a root, field names, and array
indices (`$.a.b[0]`, `$["a b"]`). Filters, wildcards, and recursive descent are
**rejected at validation**, not ignored at check time, because an assertion that
silently does not run reports `up` for a monitor that is asserting nothing.

### Ping and restricted containers

Raw sockets are unavailable in most container runtimes. When neither the
unprivileged nor the raw socket opens, the heartbeat is `unknown` — never
`down`:

```
ICMP unavailable on this probe: raw and unprivileged ICMP sockets both refused
(no CAP_NET_RAW, and this process's group is outside net.ipv4.ping_group_range).
Grant CAP_NET_RAW, widen net.ipv4.ping_group_range, or set fallback_to_tcp on this monitor
```

The target may be perfectly healthy; this probe cannot ask. Paging an on-call
rotation about a container permission is the specific failure the `unknown`
outcome exists to prevent.

With `fallback_to_tcp` the monitor checks the named port instead, and says so on
every heartbeat with `code=tcp_fallback` — a monitor that quietly changed what it
measures is worse than one that failed:

```sh
'{"name":"Ping","type":"icmp","config":{
   "hostname":"db.internal","fallback_to_tcp":true,"fallback_tcp_port":5432}}'
#   up   code=tcp_fallback   ICMP unavailable (...); checked TCP 5432 instead
```

## Push monitors

A push monitor is the one type no probe ever runs: it measures silence, which
only the side holding the clock and the last heartbeat can see. The control plane
sweeps them itself.

```sh
curl -s -H "Authorization: Bearer $KEY" -X POST localhost:3000/api/v1/monitors \
  -H 'Content-Type: application/json' \
  -d '{"name":"Nightly backup","type":"push",
       "config":{"expected_interval_seconds":3600,"grace_period_seconds":60}}'
```

The response carries `config.push_token` and `config.push_url` **once**. The
server stores only a hash, so a later read of the monitor does not show them and
there is no way to recover one — rotate by recreating the monitor.

```sh
curl -s http://localhost:3000/api/v1/push/<token>
# → {"ok":true}
```

No credential, no flags, `GET`: the callers are cron jobs, and anything more
elaborate does not get used. The token *is* the credential, so treat it as a
secret. A job can report its own failure, with a message and its own timing:

```sh
curl -s "http://localhost:3000/api/v1/push/<token>?status=down&msg=backup+failed&ping=87.5"
```

Miss the window and the sweep records it, honouring `retries` exactly as every
other type does:

```
down important=true  | no push received since creation, 23s ago
up   important=true  | (after one curl)
down important=true  | backup failed          response_time_ms=87.5
```

An unissued or malformed token gets `404` — the same answer for both, so the
endpoint cannot be used to discover which tokens exist.

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
| `GET/POST /api/v1/push/{token}` | None — the token is the credential |
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

The suite covers the parts where a bug would be silent rather than loud: every
checker's assertion and failure classification, the control plane's transition
table, the push sweep's deadlines and retries, the result buffer's shedding
order, migration checksums and rollback, heartbeat write idempotency, and — since
they are the ones that fail open — scope enforcement, CSRF, rate limiting, key
revocation, privilege escalation, recovery-code single use, and ciphertext
relocation.

The checker tests run against servers started in-process — a DNS resolver built
on `dnsmessage`, a TLS listener presenting a certificate generated with the
window under test, a real gRPC health service, a fake Docker daemon, an RDAP
registry. Nothing in the suite needs the internet, because a test that fails on
an aeroplane is a test that gets deleted.

## What is deliberately missing

- **Monitor-to-named-probe pinning for Docker.** The checker works and is correct
  in solo mode, which is the only mode this build has. In a multi-probe install
  "is this container running" is only answerable by the probe on that host, and
  nothing yet makes the assignment land there.
- **Encryption of monitor credentials.** Bearer tokens, basic-auth passwords,
  Docker client keys, and gRPC metadata all reach `monitors.config` in plaintext.
  The layer that would fix it exists and already carries the TOTP secret; nothing
  routes monitor configs through it yet.
- **Notifications, rollups, status pages, the UI, the importer.** Phase 1
  Months 2–4.
- **The load-test harness against the real engine.** Its `http` target still
  refuses to run, which is the honest state until someone points it here.
