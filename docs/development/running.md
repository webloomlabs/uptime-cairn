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
level=INFO msg="migration applied" version=3 name=alerting_and_pages
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

## History and retention

Raw heartbeats are kept for seven days. Everything longer — a status page's
90-day uptime bar, the year Phase 2's reports quote — is made of rollup rows, and
a background pass builds them every minute:

```
heartbeats  →  heartbeat_1m  →  heartbeat_5m  →  heartbeat_1h  →  heartbeat_1d
```

Each tier is computed **from the tier below**, not from raw. That only gives the
right answer because the columns store a sum and a count rather than an average:
an average cannot be re-weighted into a coarser bucket, a sum and a count can.
Buckets are UTC and epoch-aligned, start inclusive and end exclusive, so a
heartbeat lands in exactly one bucket at every tier.

After a few minutes of a live install, with one healthy monitor, one pointed at a
closed port, and one ping:

```
heartbeat_1m: 24 buckets
  04:42:00 Up     up=3 down=0 sum=187.5 n=3 min=57.5 max=67.5 p95=67.5
  04:42:00 Down   up=0 down=3 sum=1.4   n=3 min=0.4  max=0.5  p95=0.5

heartbeat_5m: 3 buckets
  04:35:00 Up     up=13 down=0 sum=907.2 n=13 min=55.6 max=152.7 p95=152.7
  04:35:00 Down   up=0 down=13 sum=7.4   n=13 min=0.3  max=1.2   p95=1.2

monitor_uptime_cache:
  Up   24h ratio=1.0000 total=13 down=0  downtime=0s
  Down 24h ratio=0.0000 total=13 down=13 downtime=300s
```

Three rules the numbers above are obeying, each of which is a way a monitoring
tool can lie:

- **`uptime_ratio` is not stored.** It is `up / (up + down)` at read time, so the
  API's three-way maintenance choice stays implementable. Storing it would bake
  one policy into the data.
- **`unknown` and `skipped` are counted and never in the denominator.** A probe
  that could not look is a gap in observation, not an outage.
- **A bucket with no checks has no row; a bucket of nothing but `unknown` has
  one, with `up + down = 0`.** Both read as a null ratio, and the second carries
  the reason the observation is missing.

`p95` is real only at the 1m tier, computed from raw by nearest rank. Coarser
tiers carry the largest sub-bucket p95 — an approximation, deliberately the
conservative direction, and one that has to be labelled wherever it surfaces.

### Retention, and the disk it gives back

Defaults are the ones in `Settings.retention`: 7 days raw, 30 days of 1m, 90 of
5m, 365 of 1h, and 1d indefinitely. The validator enforces the rule that makes
the chain coherent — a coarser tier must be kept at least as long as a finer one,
or history develops a hole where detail was deleted before the summary that
replaced it existed. The audit log is never touched: deleting an audit log
defeats its purpose.

Deleting rows from SQLite does not shrink the file. This is the trap that decides
whether a Pi with a 32GB card keeps working, and it needs `auto_vacuum` set to
`INCREMENTAL` **before the first table exists**; changing it afterwards means a
full `VACUUM` that rewrites the whole file and wants free space equal to its
size. Measured on 20,000 heartbeats:

```
6,385,664 bytes with data
6,385,664 bytes after deleting every one of them   ← the delete alone reclaims nothing
  622,592 bytes after PRAGMA incremental_vacuum
```

### Reading it back

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:3000/api/v1/monitors/<id>/history" | jq
```

```json
{
  "monitor_id": "01a0184d-a657-7118-9b70-3eeeeb666be4",
  "resolution": "5m",
  "from": "2026-08-18T05:06:02Z",
  "to": "2026-08-19T05:06:02Z",
  "data": [
    { "bucket_start": "2026-08-19T04:35:00Z", "up_count": 13, "down_count": 0,
      "uptime_ratio": 1, "response_time_avg_ms": 69.8, "response_time_p95_ms": 152.7 }
  ]
}
```

`resolution=auto` picks the coarsest tier that still gives a useful number of
points — 5m for a day, 1h for a week, 1d for a quarter. Ask for something finer
than the range can carry and the answer is **coarser than requested rather than
refused**, which the spec allows and the response reports:

```sh
"…/history?resolution=1m&from=2026-01-01T00:00:00Z"   →  "resolution": "1d"
```

`resolution=raw` means one bucket per check, so its width is the monitor's own
interval.

Two things worth knowing about where the numbers come from:

- **Raw wins whenever it covers the range**, even though the tiers exist. A tier
  lags by its bucket width plus the pipeline's grace period, and that lag lands
  on the right-hand edge of the chart — the part someone watching an incident is
  looking at. In the example above the last bucket is the *current* five minutes,
  which the tier does not have yet.
- **`response_time_p95_ms` is null when it would be an approximation.** A real
  percentile is computed from raw; the coarse tiers store the largest sub-bucket
  p95, and the response schema has no field in which to say so. A p95 quoted to
  an auditor without its method is worse than no p95, so it is reported absent.

```sh
curl -s -H "Authorization: Bearer $KEY" \
  "localhost:3000/api/v1/monitors/<id>/uptime?window=1h&window=24h" | jq
```

```json
{
  "maintenance_handling": "exclude",
  "windows": {
    "1h": { "uptime_ratio": 0, "total_checks": 30, "down_checks": 30,
            "downtime_seconds": 600, "response_time_p95_ms": 1.206 }
  }
}
```

`maintenance` takes `exclude` (the default), `count_as_up`, or `count_as_down`,
and the answer says which it used. One set of buckets, three defensible numbers —
which is the entire reason `uptime_ratio` is computed at read time and never
stored. A ratio quoted without saying what it did with maintenance is not a
figure anyone can defend.

`incident_count` is **absent**, not zero: incidents are not implemented, and "no
incidents" is a different claim from "we do not track incidents".

A monitor that has never been checked answers `null`, not `0`:

```json
{"windows": {"24h": {"uptime_ratio": null, "total_checks": 0, "down_checks": 0}}}
```

### Deleting a monitor

`DELETE /api/v1/monitors/{id}` removes the configuration row and returns `204`
immediately. The history is enqueued in `pending_purges` and deleted by the same
background pass, in bounded batches. The time series deliberately has no foreign
key back to monitors, because a cascade over a week of heartbeats and a year of
buckets cannot run inside a request without making that `204` a lie about how
long the work takes. Orphaned rows are invisible to every API query — they all
filter through a live monitor — so a purge that lags costs disk and never
correctness.

## Monitor credentials

Four of the nine monitor types take a credential: HTTP basic and bearer auth,
Docker client TLS material, and gRPC request metadata. None of them is stored in
the clear.

Create one and read it back:

```sh
curl -sS -X POST localhost:3000/api/v1/monitors \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{
    "name": "Authenticated endpoint",
    "type": "http",
    "config": {
      "url": "https://api.example.com/health",
      "auth": {"type": "basic", "username": "cairn", "password": "s3cret"}
    }
  }'
```

```json
{
  "config": {
    "url": "https://api.example.com/health",
    "auth": {"type": "basic", "username": "cairn", "password": "__redacted__"}
  }
}
```

The username survives and the password does not. A username is not a credential
on its own, and it is the half worth being able to read back when working out
which account a monitor is using.

What is in the row:

```sql
sqlite> SELECT config, length(config_secrets) FROM monitors WHERE name = 'Authenticated endpoint';
{"auth":{"type":"basic","username":"cairn"},"url":"https://api.example.com/health"}|68
```

The credential is not in `config` to be redacted — it was moved into
`config_secrets` on the way in, sealed with AES-256-GCM and bound to
`(org_id, 'monitors', 'config', id)` so a blob relocated onto another monitor's
row fails to open rather than being read against the wrong monitor. The rest of
the config stays as queryable JSON, which is why the `json_valid` constraint and
every future query over monitor settings still work.

It goes back together in exactly one place: the control plane, when it hands the
monitor to a probe. That is what "decrypted only at delivery" means — at rest an
opaque blob, in flight protected by the transport the probe dialled.

### Which fields are secret

Declared by the checker that owns the config schema, because the checker is the
only thing that knows a bearer token lives at `auth.token`:

| Type | Encrypted |
|---|---|
| `http` | `auth.password`, `auth.token` |
| `docker` | `tls.ca_cert`, `tls.client_cert`, `tls.client_key` |
| `grpc` | `metadata` — keys are returned, values are not |
| everything else | nothing; they check anonymously |

That table is not maintained by hand against the spec. A test reads
`docs/api/openapi.yaml`, finds every `writeOnly` property of each monitor type's
config schema, and fails if a checker's declaration disagrees. The failure it
exists to catch is silent: a monitor type gains a credential in the spec, nobody
adds it here, and the result is not an error — it is a password in the clear.

### Sending a redaction back

A create carrying `"__redacted__"` is refused:

```json
{"pointer": "/config/auth/password", "code": "redacted",
 "message": "auth.password came back from a read with its value hidden; supply the real credential, or omit it"}
```

Accepting it would produce a monitor that looks configured and authenticates as
nobody, and the failure would arrive hours later as a 401 attributed to the
target.

### Upgrading a database that predates this

Migration `0004` adds the column; it cannot encrypt anything, because SQL has no
key. So the process does it on start, before anything reads a config:

```
level=INFO msg="migration applied" version=4 name=monitor_config_secrets
level=INFO msg="moved monitor credentials out of plaintext configuration" monitors=1
```

It covers disabled and paused monitors too — a monitor nobody is checking today
still has its password in the database — and it is a no-op on every start
afterwards. `updated_at` is deliberately not touched: the probe's config version
is derived from it, and re-sealing changes where a credential is stored rather
than what the monitor checks.

**It does not scrub history, and nothing can.** The bytes may still sit in
unallocated space in the database file until those pages are reused, and they
were in every backup taken before the upgrade. The connection sets
`secure_delete=FAST`, which zeroes freed content inside pages SQLite is rewriting
anyway — cheap, and enough to stop a rotated credential lingering in the slack
space of its own page — but it applies from that connection onward. A credential
that was ever stored in plaintext should be rotated. [Data model §12.7](../data-model/README.md)
is the list of things encryption at rest does not do, and this belongs on it.

## Groups and tags

Two organisational primitives, and they are deliberately not the same idea. A
monitor belongs to at most one **group**, which is where it lives; it carries any
number of **tags**, which is what it is. Collapsing them into one mechanism costs
you the ability to ask both questions — "show me the EU stack" and "show me
everything customer-facing" are different queries over the same monitors.

```sh
curl -sS -X POST localhost:3000/api/v1/groups \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name": "Production"}'

curl -sS -X POST localhost:3000/api/v1/tags \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name": "Edge / Customer Facing", "color": "#c026d3"}'
```

Then hang monitors off them with `group_id` and `tag_ids` on the monitor write.

### A group reports what is worst underneath it

```
Production / EU      monitors=1  status=pending  parent=yes
Production           monitors=2  status=down     parent=-
```

`Production` holds one monitor directly and one through its child, and its status
is the worse of the two. A parent group showing green while the child underneath
it is down would be a dashboard that goes clear during an outage, which is the
single worst thing a monitoring tool can do — so both the count and the status
reach into children.

An empty group reports `"status": null`. Null is a different statement from `up`,
and rendering it green would be the dashboard inventing health.

Groups nest **one level** in this release, enforced by two rules that also make a
cycle impossible: a parent must itself have no parent, and a group that already
has children cannot be given one.

```json
{"code": "too_deep",
 "message": "\"Production / EU\" is already nested, and groups nest one level deep in this release"}
```

Deleting a group ungroups its monitors and promotes its children to the top
level. Deleting a container never deletes what it contained.

### A tag's slug is derived, not supplied

```json
{"name": "Edge / Customer Facing", "slug": "edge-customer-facing", "color": "#c026d3"}
```

The slug is lower-case ASCII letters, digits, and single hyphens, so it never
needs percent-encoding in a URL. Everything else becomes a separator, which means
`Edge / Customer Facing` and `edge  customer facing` are the same tag — and that
is the point, because two tags that render identically in a list are two tags
nobody can tell apart. The second one is a `409`:

```json
{"status": 409, "title": "Tag name already in use",
 "detail": "another tag already uses the slug \"edge-customer-facing\"; tag names must be distinguishable once reduced to a URL-safe form"}
```

A `409` rather than a `422` because the request is well-formed and the current
state is the problem: the caller resolves it by choosing another name, not by
correcting a field.

A name written entirely in another script leaves nothing behind, and that is a
`422` saying so rather than an identifier the user did not choose. `color`
defaults to a neutral grey, so a tag created without one does not claim a meaning
it was not given.

### Filtering

```
(none)                       3 monitors
?group_id=<production>       2 monitors   ← reaches the child group
?group_id=<production/eu>    1 monitor
?tag_id=<database>           1 monitor
?tag_id=<database>&tag_id=<edge>   2 monitors   ← OR within the parameter
?group_id=<production/eu>&tag_id=<edge>  1 monitor   ← AND across them
```

Repeated values combine with OR within a parameter and AND across parameters,
per the spec. A group filter reaches its children for the same reason the count
does: filtering to a parent and getting nothing back while the child holds every
monitor is a filter nobody trusts twice.

`status`, `type`, `enabled`, and `search` are specified and **not implemented**.

### What this unblocks

A maintenance window can now target a tag rather than a list of monitor ids:

```json
{"targets": {"tag_ids": ["<edge>"]}}
```

Live, that window put both tagged monitors into maintenance and left the untagged
one alone — and a monitor created **after** the window opened, carrying the tag,
was swept in on the next pass. That is why targets resolve by query at evaluation
time and are never snapshotted into a list of ids at creation.

## Maintenance windows

Planned downtime that pages somebody is not planned downtime.

```sh
curl -sS -X POST localhost:3000/api/v1/maintenance-windows \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{
    "title": "Sunday patching",
    "strategy": "recurring_weekly",
    "timezone": "Australia/Sydney",
    "starts_at": "2026-08-23T02:00:00+10:00",
    "duration_minutes": 120,
    "recurrence": {"weekdays": [0]},
    "targets": {"monitor_ids": ["<id>"]}
  }'
```

Five strategies: `single`, `recurring_daily`, `recurring_weekly`,
`recurring_monthly`, and `cron` for the schedules the named ones cannot express.

**The timezone is an IANA name, and that is the whole point.** The schedule is
evaluated in local time, so "02:00 every Sunday" is still 02:00 after a
daylight-saving transition — an offset could not express that, and a window that
fires an hour late pages the engineer during the change they scheduled it
around. The zone database is embedded in the binary rather than read from the
host, because a `FROM scratch` image has no `/usr/share/zoneinfo` and
`Australia/Sydney` silently becoming UTC is exactly the failure this avoids.

A day past the end of a short month is **skipped, not clamped**: a monthly window
on the 31st does not fire in February. "The 31st" meaning the 28th is a guess
about intent, and a maintenance window is a poor place to guess.

### What it does to a check

A monitor inside an active window still gets checked. Its heartbeat records the
result — message, code, response time, all of it — but the verdict is recorded
as `maintenance` rather than as up or down:

```
07:41:11  maintenance  suppressed=True  reason=maintenance
          Get "http://10.0.0.1/": dial tcp 10.0.0.1: connect: connection refused
```

That is what makes `/uptime`'s three-way maintenance choice mean anything. The
same window, three defensible numbers:

```
exclude        ratio=0       total=20  down=20  maintenance_seconds=160
count_as_up    ratio=0.2857  total=20  down=20  maintenance_seconds=160
count_as_down  ratio=0       total=20  down=20  maintenance_seconds=160
```

The failure count is frozen for the duration rather than reset — a window says
nothing about the target either way, so counting resumes where it left off. On
the way out the monitor returns to `pending` rather than to whatever it was
before: the last real observation predates the window, and presenting it as
current would be the dashboard lying about how fresh it is. The next check
settles it within one interval.

### state is derived, not stored

```json
{"state": "active", "next_occurrence_at": "2026-08-23T16:00:00Z"}
```

`state` is computed from the schedule and the clock on every read. Storing it
would make it wrong between the moment a window starts and the moment something
notices, which is exactly the interval anybody asks about. `next_occurrence_at`
*is* stored, for the opposite reason: it is what lets the sweep find due windows
with an index seek instead of evaluating every cron expression on every tick,
forever, for windows scheduled months out.

The sweep runs every 15 seconds and is woken immediately by any write, so a
window created to start now suppresses the check that is about to run rather
than the one after it.

### suppress_notifications off means annotation, not suppression

A window with `suppress_notifications: false` still has a schedule and still
appears as scheduled maintenance, but it does not mark heartbeats or silence
anything. Marking the period as maintenance while still paging would exclude it
from uptime *and* wake somebody — the worst of both answers.

### What is refused

A schedule that will never fire is a 422, not a saved window:

| Body | Refused because |
|---|---|
| no `targets` | a window covering nothing suppresses nothing |
| `recurring_weekly` with no weekdays | it produces no occurrence, ever |
| `recurring_daily` with no `duration_minutes` | an occurrence needs a length |
| `"timezone": "+11:00"` | an offset is not an IANA zone |
| `"cron": "0 2 * *"` | four fields, not five |
| `recurrence.until` already past | the recurrence has already stopped |

Every write runs the same evaluator the sweep runs, so a window that parses but
never occurs is caught on the form rather than discovered by its silence.

## Dependency suppression

Set `parent_monitor_id` on a monitor and it stops paging while the thing it
depends on is unavailable. The router going down should page once, not forty
times.

```sh
curl -sS -X POST localhost:3000/api/v1/monitors \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"name": "API", "type": "http", "config": {"url": "https://api.example.com"},
       "parent_monitor_id": "<the router>"}'
```

Live, with the parent down:

```
parent  07:40:11  down  suppressed=False
child   07:40:07  down  suppressed=True  reason=dependency
```

Three things about that child heartbeat are deliberate:

- **It is recorded as `down`, not `maintenance`.** The service really was
  unavailable, so it counts against the child's uptime. Only the page is
  withheld.
- **It is still marked important.** Suppression withholds the alert, not the
  history — `important_only` and the activity feed still show the change.
- **The reason is recorded**, so "why was I not told?" is a query rather than an
  argument.

Suppression is **transitive** — a router, a switch behind it, and the services
behind that is the shape this exists for — and a parent under **maintenance**
suppresses its children too. Taking the router down for a firmware upgrade is
the most known problem there is; the alternative is making the operator target
every descendant by hand, which is a list that goes stale the first time
somebody adds a service.

A dangling parent suppresses nothing. Failing open is the right direction here:
the cost is an alert that should have been silent, and the alternative is
silence that should have been an alert.

Chains are limited to ten levels and cycles are rejected at write time. A cycle
cannot form through the create endpoint today — a parent must already exist — so
the check is there for the first `PATCH` that lets a parent change.

**One bounded gap, stated because it is easier to find here than in the code.**
When a parent leaves a maintenance window it returns to `pending` for one
interval, and `pending` is not `down`, so a child that happens to transition
inside that interval will page. It is at most one interval wide and only occurs
on the way out of a window; widening suppression to cover `pending` would mean a
monitor whose parent is merely flapping through its retry budget never alerts at
all, which is worse.

## Alerting

A monitoring tool that detects an outage and tells nobody is a logging tool. This
is the part that tells somebody.

Create a channel. Thirteen types exist — `email`, `webhook`, `slack`, `discord`,
`telegram`, `matrix`, `gotify`, `ntfy`, `msteams`, `pagerduty`, `opsgenie`,
`twilio`, and `apprise` as the meta-provider for everything else:

```sh
curl -sS -X POST localhost:3000/api/v1/notification-channels \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{
    "name": "Ops Slack",
    "type": "slack",
    "is_default": true,
    "config": {"webhook_url": "https://hooks.slack.com/services/T/B/XXXX"}
  }'
```

Read it back and the webhook URL is gone:

```json
{
  "name": "Ops Slack",
  "type": "slack",
  "config": {"webhook_url": "__redacted__"},
  "monitor_count": 0,
  "last_used_at": null,
  "last_error": null
}
```

That is not the serialiser being careful. The secret fields are split out of
`config` before the row is written and stored in a separate encrypted column, so
the read path has nothing to redact — it is assembling a marker, not hiding a
value. `grep` the database file and the token is not in it. The marker also
round-trips: a form that `GET`s a channel, changes one field and `PATCH`es the
whole object back sends `"__redacted__"` for the secret, and the server treats
that as "leave it alone" rather than overwriting a bot token with asterisks.

**Test it before you need it.** Every channel has this, because a channel that
fails silently at 3am is worse than no channel at all:

```sh
curl -sS -X POST localhost:3000/api/v1/notification-channels/$ID/test \
  -H "Authorization: Bearer $KEY"
```

```json
{
  "delivered": false,
  "status_code": 403,
  "error": "403 Forbidden: {\"error\":\"invalid_token\"}",
  "duration_ms": 41.2,
  "rendered_payload": "{\"text\":\"[DOWN] Sample monitor\", ...}"
}
```

`200`, not `502`: the request succeeded and the delivery did not, and those are
different facts. The provider's own words are passed through rather than
summarised — `invalid_token` is something an operator can act on and "delivery
failed" is not. The same string lands on the channel's `last_error`, so a broken
channel is visible in a list without anybody reading a log, and it is cleared the
next time a delivery works.

### Which monitors alert where

A channel marked `is_default` attaches to monitors created afterwards, which is
what makes setting alerting up once work. Existing monitors are left alone — a
box ticked today must not silently start alerting on five thousand monitors.

Per monitor, `notification_channel_ids` has three distinct states, and the
distinction matters:

| Field | Meaning |
|---|---|
| absent | attach the default channels |
| `[]` | no alerts for this monitor, deliberately |
| `["<id>", ...]` | exactly these |

### What fires, and what deliberately does not

| Transition | Event |
|---|---|
| up → down | `monitor.down` |
| down → up | `monitor.up`, unless `notify_on_recovery` is false |
| up → pending | `monitor.pending` — only to channels that ask for it by name |
| pending → up | nothing: it never went down, so it did not recover |
| anything → unknown or skipped | nothing |

That last row is the whole reason those two outcomes exist. A probe whose egress
dies reports `unknown`, and `unknown` pages nobody — otherwise one broken probe
becomes five thousand pages about targets that are perfectly healthy.

`monitor.pending` is excluded from the default subscription on purpose. Pending
precedes every down transition, so including it would send two messages for one
outage, which is how people learn to filter the whole channel.

`resend_after` re-notifies after that many further consecutive failures while
still down; `0` disables it, so an ongoing outage alerts once. It is counted from
the failure that produced the verdict rather than from the first one, so it means
a period of continued downtime rather than "retries plus `resend_after`". It is
derived from `consecutive_failures`, which the schema already records — a
`last_notified_at` column would be a second source of truth for a question the
first one already answers.

PagerDuty and Opsgenie model this properly: the failure opens an incident keyed
by monitor and the recovery closes *that* incident rather than opening a second
one. Turn `auto_resolve` off and the recovery is recorded as a **suppressed**
delivery rather than a successful one — "we chose not to" and "it worked" are
different answers to the only question anybody asks after an incident.

### Webhook payload templating

The generic webhook sends the event envelope by default. Give it a
`body_template` and it sends whatever you write, with `{{variable}}`
interpolation in the body, in the headers, and nowhere else:

```json
{
  "name": "Ops",
  "type": "webhook",
  "config": {
    "url": "https://example.com/hook",
    "headers": {"X-Monitor": "{{monitor.name}}"},
    "body_template": "{\"text\": \"{{monitor.name}} is {{status}}\", \"detail\": \"{{message}}\"}"
  }
}
```

Values are escaped for the declared `content_type`, so a monitor named
`He said "hi"` produces a payload the receiver accepts rather than one it
rejects — the failure that otherwise shows up during the outage rather than
during setup. `{{x | raw}}` opts out and `{{x | json}}` opts in.

The available variables are an endpoint, not a document:

```sh
curl -sS localhost:3000/api/v1/notification-channels/template-variables \
  -H "Authorization: Bearer $KEY"
```

It is the same list the renderer resolves against, so the UI's autocomplete
cannot drift from what actually works. Preview before saving:

```sh
curl -sS -X POST localhost:3000/api/v1/notification-channels/preview \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"template": "line one\nand {{monitor.nmae}} here"}'
```

```json
{
  "ok": false,
  "error": {
    "message": "unknown variable \"monitor.nmae\" — GET /api/v1/notification-channels/template-variables lists the ones that exist",
    "line": 2,
    "column": 5
  }
}
```

`200` with `ok: false`: a broken template is your typo, shown inline with its
position, not a server fault. The same check runs when the channel is saved, so a
misspelling is a `422` on a form you are looking at rather than a missed alert
during an outage. Pass `monitor_id` to render against a real monitor's current
values instead of the sample.

### When delivery fails

Every attempt writes a row to `notification_deliveries`, successful or not, and
the outcome is one of `succeeded`, `failed`, or `suppressed`. A failure is
retried up to three times — but only when a retry could plausibly work. A `401`
or a `403` is not retried, because sending the same wrong credential three times
produces three identical failures and delays the moment you are told which it
was; `408`, `429`, and every 5xx are.

Publishing an event never blocks ingest. The queue is bounded and fire-and-forget:
the moment alerting is under strain is the moment heartbeats matter most, and an
alerting backlog must not become heartbeat backpressure. If the queue does fill,
the count is logged — a silently dropped alert is the one failure this whole
subsystem exists to prevent.

### Two channel types with conditions attached

**Email** needs its own SMTP settings. `use_instance_smtp` defaults to `true`,
instance-wide settings have nowhere to live until `/settings` exists, and a
channel that asks for them is refused at save time with the alternative spelled
out — rather than accepted, listed as configured, and delivering nothing:

```json
{"pointer": "/config/use_instance_smtp", "code": "unsupported",
 "message": "instance-wide SMTP settings are not implemented in this build; set use_instance_smtp to false and give this channel its own smtp_host, smtp_port and from_address"}
```

**Apprise** needs the binary. `pipx install apprise` and restart; without it the
channel type is refused at creation rather than offered and failing on first use.
Its URLs are written to a mode-0600 file rather than passed as arguments, because
an Apprise URL embeds its own credentials and an argument vector is readable
through `ps`.

## Editing a monitor

`PATCH` is a partial update. Omitted fields are left alone, an explicit `null`
clears a nullable one, and `type` is not a field at all — changing what a monitor
checks would make its history a record of two different things.

```sh
curl -sX PATCH localhost:3000/api/v1/monitors/$MONITOR \
  -b jar -H "X-Cairn-CSRF-Token: $CSRF" -H 'content-type: application/json' \
  -d '{"interval_seconds": 120}'
```

`config` merges shallowly against what is stored. The interesting case is the
one a form produces: read the monitor, change one field, submit the whole object
back. The read never showed you the password —

```json
"auth": {"type": "basic", "username": "cairn", "password": "__redacted__"}
```

— so a naive write path would either store the literal string `__redacted__` as
the password or drop the credential entirely. Both fail hours later as a `401`
attributed to the target. Instead the marker is resolved back to the stored
value before the merge, verified live: after patching only the username, the
probe's next check sent

```
Authorization: Basic Y2Fpcm4tMjpzM2NyZXQtbGl2ZQ==   → cairn-2:s3cret-live
```

and `strings cairn.db | grep s3cret-live` still finds nothing.

Reparenting is the first endpoint that can genuinely close a dependency loop — a
newly created monitor is nobody's ancestor, an existing one can be moved under
its own descendant — so the cycle walk that has always been in `resolveParent`
finally earns its keep:

```
422 | cycle | that parent would make the dependency chain a loop
```

## Pause, resume, and check now

```sh
curl -sX POST localhost:3000/api/v1/monitors/$MONITOR/pause  -b jar -H "X-Cairn-CSRF-Token: $CSRF"
curl -sX POST localhost:3000/api/v1/monitors/$MONITOR/resume -b jar -H "X-Cairn-CSRF-Token: $CSRF"
```

Pausing writes `status: paused` and clears `next_check_at`. Resuming writes
`pending` rather than restoring whatever the monitor was before: it has not been
checked since, and reporting a stale verdict as current is how a monitor that
broke while paused stays green.

`POST .../check` runs one check outside the schedule and returns the heartbeat.
The check itself runs in the API process, because the control plane must not
import `probe/check` — that is the ADR-001 seam. The *result* goes back through
the control plane's ingest, so a manual check counts, transitions, and alerts
exactly like a scheduled one. A "test" that took a different path would be
testing the test.

It is rate-limited per monitor, not per caller, because the thing being protected
is somebody else's server:

```
429 | This monitor was checked manually within the last 10s.
      The target is somebody's server; try again in 10s.
```

A push monitor is refused: it is a deadline, not a check, and running one would
write a heartbeat the target did not send.

## Filtering, membership, and includes

`status`, `type`, `enabled`, and `search` join `tag_id` and `group_id`. Repeated
values OR within a parameter and AND across them. `search` matches the name *or*
the target, because the question that brings somebody to the search box is
usually "what else points at this host?" and the answer lives in a field they
never named.

An unrecognised value is a `400`, not an empty page — silently returning nothing
for a typo is how somebody concludes their monitors have been deleted.

`GET /api/v1/monitors/membership` takes the same filters and returns a version
and a count. It is ADR-004's reconciliation half: live updates only reach the
monitors a client has on screen, so a monitor that changes status *off* screen
and would now match an active filter has no subscription telling the server
anyone cares. Both numbers are needed — a monitor leaving a `status=down` view as
another enters keeps the count identical while the view is now wrong. Live, the
listing and the signal agree across every filter:

```
  (none)                         list=2 membership=2
  ?status=paused                 list=1 membership=1
  ?enabled=true                  list=1 membership=1
  ?search=Checkout               list=1 membership=1
```

The version moves on a configuration edit too, not only on a check result:
renaming a monitor bumps `state_version`, so an open list view learns about the
new name instead of showing the old one until something fails.

`include=last_heartbeat,uptime,tags,group` embeds related data, one query per
embed for the whole page rather than one per row. It is opt-in because it costs
per-row work: the dashboard's list view wants it and an export does not. Without
`include=`, the response is byte-for-byte what it was before the embeds existed.

`uptime` reads the precomputed cache and reports `null` for a monitor it has not
computed yet — zero would be a claim of total downtime made by a table that
simply had not run.

## Bulk operations

Up to a thousand monitors, one operation, per-identifier outcomes:

```sh
curl -sX POST localhost:3000/api/v1/monitors/bulk \
  -b jar -H "X-Cairn-CSRF-Token: $CSRF" -H 'content-type: application/json' \
  -d '{"monitor_ids": ["...", "...", "..."], "operation": "disable"}'
```

```json
{"succeeded": ["...", "..."], "failed": [{"id": "...", "code": "not_found", ...}]}
```

Partial success is the contract, not a fallback. One monitor deleted five minutes
ago must not fail the other nine hundred and ninety-nine — an endpoint that
refuses the batch is useless at exactly the size it exists for. `add_tags` is
idempotent, so a caller retrying a partial batch does not accumulate duplicates.

## Incidents

An incident is the human narrative laid over the machine's observations. A
monitor going down is a fact; an incident is what somebody decided that fact
meant, and it is the thing customers read.

```sh
curl -sX POST localhost:3000/api/v1/incidents \
  -b jar -H "X-Cairn-CSRF-Token: $CSRF" -H 'content-type: application/json' \
  -d '{"title":"Checkout is failing","impact":"major",
       "monitor_ids":["'"$MONITOR"'"],
       "body":"Investigating elevated errors."}'
```

**State changes do not go through `PATCH`.** Advancing an incident is done by
posting a timeline entry that carries the new state, so every state change
arrives with the sentence explaining it:

```sh
curl -sX POST localhost:3000/api/v1/incidents/$INCIDENT/updates \
  -b jar -H "X-Cairn-CSRF-Token: $CSRF" -H 'content-type: application/json' \
  -d '{"state":"resolved","body":"Rollback complete."}'
```

An incident whose history reads `investigating → identified → resolved` with no
words attached answers no question anybody will actually ask afterwards, and a
`PATCH` that could set `state` would make that the path of least resistance. Live:

```
   state resolved | resolved_at 2026-08-19T11:41:42.289Z
   MTTR  77 s | MTTA None
     investigating Investigating elevated errors.
     identified    A bad deploy. Rolling back.
     resolved      Rollback complete.
```

The three MTT\* figures are derived from the timestamps at read time and are
deliberately not stored: a stored metric drifts from the timeline it was computed
from, and the timeline is the thing anyone will argue about. MTTA is `null`
because acknowledgement is Phase 3, rather than zero, which would be a claim.

Resolving stamps `resolved_at`; reopening clears it, so the column always agrees
with the state rather than recording the first time somebody thought it was over.

## Status pages

```sh
curl -sX POST localhost:3000/api/v1/status-pages \
  -b jar -H "X-Cairn-CSRF-Token: $CSRF" -H 'content-type: application/json' \
  -d '{"slug":"acme","title":"Acme Status","published":true,
       "sections":[{"name":"Core","monitor_ids":["'"$MONITOR"'"]}]}'
```

Then, with **no credential at all**:

```sh
curl -s localhost:3000/api/v1/public/status-pages/acme
```

The public view is assembled from its own store query rather than filtered down
from the monitor read path. That costs a second query and buys the property that
matters: a field cannot leak through a shape that has no place to put it. This is
the one endpoint in the system where a leak reaches people who are not customers
of the operator. Live, a visitor sees

```
     overall_status  : major_outage
      Checkout API v2  down | keys: [description, id, name, response_time_ms, status, uptime_percentage]
     leaks config?   : no — no url, no target, no credential
```

Other decisions worth knowing:

- **An unpublished page is `404`, not `403`.** An operator building a page before
  launch should not have its existence confirmed by the error code.
- **A real outage during a maintenance window is still an outage.** `maintenance`
  only wins the overall status when nothing is actually down; reporting a genuine
  outage as scheduled work is the sort of thing that ends up on a screenshot.
- **A day with no data renders as a gap, never as downtime.** The uptime bar
  carries `null` for those days. It is the single most common way a status page
  lies.
- **`custom_css` is refused rather than sanitised** when it contains markup or a
  `javascript:` URL. Stripping tags is whack-a-mole against an attacker who only
  has to win once, on a page served to the operator's customers.
- **A slug collision is `409`**, naming what is taken. A hostname in
  `custom_domain` is unique across every organisation, because a request arrives
  with nothing but a `Host` header to route on.

Subscriptions are double opt-in and the address is encrypted at rest, because a
notification replays it:

```
   first request : pending_confirmation
   repeat request: pending_confirmation   ← the identical answer
   stored        : al…@example.com | confirmed False
   plaintext in the database? no
```

The repeat gets the same answer as the first because telling a stranger that an
address is already subscribed turns the endpoint into a membership oracle. The
operator's own subscriber list is masked too: it is an export of somebody else's
customers, and the reason to open it is "did my subscriber confirm", which a mask
answers.

## Outbound webhooks

A notification channel is aimed at a person — it renders a sentence and picks a
colour. A webhook is aimed at a program, so it posts the event envelope verbatim,
signs it, and logs every attempt.

```sh
curl -sX POST localhost:3000/api/v1/webhooks \
  -b jar -H "X-Cairn-CSRF-Token: $CSRF" -H 'content-type: application/json' \
  -d '{"name":"Ops bus","url":"https://hooks.example.com/cairn",
       "events":["monitor.down","monitor.up","incident.opened"]}'
```

The response carries `secret` exactly once. It is **encrypted** at rest rather
than hashed, because every delivery recomputes an HMAC with it — the distinction
in data model §12.1, and the one that only bites at the first delivery if it is
got wrong. Every later read carries `secret_prefix` and nothing else.

Verify a delivery by recomputing over the raw body you read:

```python
want = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
hmac.compare_digest(request.headers["X-Cairn-Signature"], want)
```

Live, against a receiver that checks it:

```
{"event": "incident.opened", "monitor": null,      "signature_valid": true}
{"event": "monitor.down",    "monitor": "Checkout", "signature_valid": true}
```

`X-Cairn-Event-Id` is stable across retries and across a manual redelivery, so a
receiver that processed a delivery whose response was lost can recognise the
repeat. Redelivery resends the *exact original body* — the reason to press the
button is that the receiver was broken and has been fixed, and a payload
regenerated from current state would describe a world the original event did not.

Configured headers go on the request first and the reserved ones after, so an
operator can add a header but never replace the signature or the event identity.
A receiver whose deduplication key can be changed by a typo in a settings field
has no deduplication.

A subscription that fails ten times in a row disables itself and records when, so
"why did this stop" has an answer in the row rather than in the logs. Re-enabling
it clears the counter.

## Settings

`GET`/`PATCH /api/v1/settings`. Every section is optional on update, and a
section that is present is a statement about that section as a whole; within it,
an absent field leaves the stored value alone.

Which sections this build actually **consults**, rather than merely stores:

| Section | Effect |
|---|---|
| `general.instance_name` | The issuer in an authenticator app and the name on every alert, applied on save |
| `general.base_url` | What a push URL is built from |
| `retention` | Handed to the rollup runner on save — the next sweep, not the next restart |
| `smtp` | What makes an email channel's `use_instance_smtp` mean something |
| `monitoring` | The defaults a newly created monitor inherits |
| `appearance`, `security`, `telemetry` | **Stored and not yet consulted** — no dashboard, limits compiled in, no exporter |

That table is the honest list. Settings nothing reads would be a checklist that
flatters itself.

The `smtp` section closes the gap the alerting work left open. Before it, an
email channel asking for the instance relay was refused at save time; now:

```
   before settings: 422 - this instance has no SMTP relay configured; set one under /api/v1/settings…
   after settings : Ops mail created
   smtp password in the database? no
   read back      : smtp keys [encryption, from_address, from_name, host, port, username]
```

The password is encrypted with the same envelope every other credential uses,
bound by AAD to its row, and lives inside the section's JSON — which is why
adding it needed no migration and why the read shape has nowhere to put it.

Retention is refused when it would leave a hole in history:

```
422 | rollup_1m_days (7) must be at least raw_days (30): a coarser tier kept for
      less time than a finer one leaves a hole in history
```

## Prometheus metrics

```sh
curl -s localhost:3000/metrics
```

Unauthenticated from loopback, because the overwhelmingly common deployment is a
Prometheus on the same host and a metrics endpoint that needs a credential is one
somebody turns off. From anywhere else it needs an API key holding
`metrics:read`.

Hand-written, no client library. The exposition format is a dozen lines of text;
the library brings a registry, a collector abstraction, and a dependency tree
onto a binary whose whole pitch is that it is one static file you drop on a Pi.
When this needs histograms it will need the library, and that is the moment to
take the dependency.

```
cairn_monitor_status{monitor_id="…",monitor="Checkout",type="http"} 0
cairn_monitor_response_time_seconds{monitor_id="…",monitor="Checkout"} 0.000478
cairn_monitors{status="down"} 1
cairn_webhook_events_dropped_total 0
```

`cairn_monitor_status` keeps the full status vocabulary rather than collapsing to
zero-or-one. Flattening it would make a Prometheus alert fire during a
maintenance window the operator declared. A monitor with nothing measured is
absent from the response-time series rather than reported as zero — zero is a
measurement of zero, which is a different claim.

## The load test, against the real engine

```sh
go build -o /tmp/cairn ./cmd/cairn
cd harness && go build -o harness .
./harness -target http -cairn /tmp/cairn -scales 500,5000
```

The harness starts the binary itself, creates the monitors through the real API
pointed at an endpoint it serves, and then mostly watches. See
[harness/README.md](../../harness/README.md) for what it asserts and why.

Two results worth knowing before reading the numbers.

**The write measurement means opposite things on the two targets.** The SQLite
target *drives* batches as fast as storage will take them — a ceiling. The HTTP
target *observes* the engine's own counter — a rate bounded by arithmetic, since
N monitors on an I-second interval produce N/I results a second and cannot
produce more. So the assertion differs: clear the floor on one, achieve the
schedule on the other. At 5,000 monitors on the 20-second floor:

```
5000 monitors: 248.2 heartbeats/sec against 250.0/sec implied by the schedule,
               1262 requests seen by the checked endpoint over the 5-second window
```

That last figure is the one the engine cannot fake — it is counted by the harness
on the other side of the network. A gap between it and the heartbeat count would
mean results were produced and never stored, and no engine counter would say so.

**A total partition is the burst the delivery queues were sized against**, and
until now that size was an argument in a comment:

```
5000 monitors, total partition:
  detected   5000/5000 down in 21.136s
  recovered  4841/5000 up in 20.857s
  alerts     9685 published, 0 shed
  webhooks   9685 delivered, 0 shed
  probe      0 results shed, 0 checks skipped
```

Every monitor marked down inside one interval, every one back afterwards, and
nothing dropped on the way. The 159 that did not recover are the ones the
workload keeps permanently failing, so that `status=down` filters something real
rather than an empty set.

### What it found

- **Creating monitors was quadratic.** Every write woke the assignment publisher,
  which reloaded and re-diffed the whole set — 2,116 full recomputations for
  5,000 creations, and the run never finished. The publisher now settles for a
  second first, which is invisible against a 20-second interval.
- **Creation was queued behind the store's one connection.** The reload holds it
  while it scans every assignable monitor, and every write queued behind that.
  Three back-to-back pairs on the same machine, because the absolute numbers move
  with whatever else the machine is doing and only the pair means anything: at
  5,000 monitors, 73, 36 and 60 creations/sec on one connection against 105, 142
  and 99 with the reader pool. Roughly double, every run. See
  [the reader pool](#the-reader-pool) below.
- **The harness's own first answer was wrong**, which is worth saying because the
  fix is the interesting part. It reported 499 heartbeats/sec against 250
  implied, and the engine was fine: it was draining the backlog built while
  seeding saturated the writer. Rows counted by check time said 250/sec; rows
  counted by write time said 500. Both were true. The warm-up now waits for the
  observed rate to settle instead of sleeping a fixed interval.
- **Two of its own assertions were coin flips**, and both had to be fixed before
  the reader pool's result could be read at all, because a gate that goes red at
  random cannot tell you whether a change helped.

  The recovery check compared against a down-count sampled once, before the
  partition. But a monitor is `pending` until it has been checked, and at 5,000
  monitors the first sweep is still running when the measurement window ends —
  the probe reported 4,923 checks started against 5,000 monitors. A baseline one
  too low makes the recovery target one too high: a number that can never be
  reached, a wait that runs to its deadline, and a report that the engine failed
  to recover a monitor that was never up. The baseline is now two agreeing
  samples an interval apart.

  The growth check compared the p95 of two runs without asking whether they had
  done the same work. `history` reads whatever the engine produced during warm-up,
  which lands on nought, one or two one-minute buckets depending on where the
  clock fell; the same build produced 257µs and 1.618ms minutes apart. A ratio
  is now only computed when the two scales returned the same number of rows, or
  enough rows that one either way cannot be the signal. Otherwise both figures
  are printed and no verdict is given.

### The reader pool

SQLite takes one write lock per database, so the write pool is one connection and
stays that way. WAL's other half is that readers run against a committed snapshot
without taking that lock at all — so reads get their own pool, opened `mode=ro`,
and a scan of every assignable monitor no longer sits in front of the writes
queued behind it.

What makes the split safe is the writer staying at one. Every check-then-act in
the store does its check inside a transaction on the write connection — a tag
slug already taken, a status-page subscriber that already exists — and those are
exact because there is one write connection and therefore one such transaction at
a time. A check moved to the read pool would stop being exact and start being a
race that produces a duplicate row once a month. Reads that return go on the read
pool; reads a write depends on stay with the writer. Two of them do, and both say
why in the code.

`mode=ro` is enforcement rather than convention. Routing is decided per call site
by hand, and the operating system refusing the write turns a mistake into a
failure the first time it runs instead of a rare lock error under load.

The gate now reports the queue directly, which is what makes the claim checkable
rather than a story about why a number moved:

```
created 5000 monitors through the API in 28.683s (174/sec)
  2676 statements queued for the write connection, 1.301s in total, 486µs each
```

Read that with `cairn_db_pool_wait_total`: a rate that falls while the wait
counter stays flat is work getting harder, and the same rate with the counter
climbing is a queue. They want opposite fixes, and until this pool existed the
question could not be asked — every statement queued on the same connection, so
the answer was always "both".

**Creation still degrades with size** — 1,861/sec at 500 monitors against 173/sec
at 5,000 — but the queue is no longer where the time goes. Eight workers spent a
combined 1.3 seconds waiting for the write connection out of the 28.7
seconds the creation took, so what remains is the reload itself: the same O(N) scan, now merely running
somewhere it does not block anything. Reported and not failed, because there is
no product commitment about how fast monitors can be created and inventing one in
the harness would be the gate making policy.

One new failure mode came with the pool and is worth knowing about. An unclosed
result set now holds a read snapshot, which stops WAL checkpointing and grows the
`-wal` file quietly; against a single shared connection the same mistake
deadlocked the next statement, which is unpleasant but impossible to miss.
`cairn_db_pool_in_use_connections{pool="reader"}` above zero on an idle instance
is what that looks like.

## Self-metrics

`/metrics` now carries what the process knows about itself, which is what the
load test reads and what an operator would alert on:

```
cairn_heartbeats_written_total 45626
cairn_results_ingested_total 45626
cairn_probe_shed_results_total{probe_id="…",probe="embedded"} 0
cairn_probe_skipped_checks_total{probe_id="…",probe="embedded"} 0
cairn_probe_due_queue_depth{probe_id="…",probe="embedded"} 12
cairn_alerts_dropped_total 0
```

Two pairs are worth explaining.

**Written against ingested.** They differ exactly when a probe redelivers: the
natural key absorbs the repeat, so results are offered and no rows result. That
is correct behaviour and still work being done twice. One counter for both would
make "the probe is resending" and "the system is doing twice the work"
indistinguishable — which is precisely the confusion that produced the 499/sec
figure above.

**The probe's numbers arrive on the result stream.** A probe has no inbound port;
it dials out and never listens. So its self-report rides the result batches and
the control plane republishes it here, labelled by probe. In solo mode the probe
is in this process and the path is identical, which is the point: what an
operator reads is produced by the same code a remote probe will run. The gauges
lag by up to one health interval, thirty seconds.

`cairn_probe_shed_results_total` and `cairn_probe_skipped_checks_total` matter
most. A probe under overload sheds rather than queueing, and shedding is
invisible from the monitor's side *by design* — the whole outcome taxonomy exists
so that probe overload never looks like target downtime, which means it has to
look like something here instead.

**The connection pools**, labelled by pool, because they answer a question no
latency number can:

```
cairn_db_pool_max_connections{pool="writer"} 1
cairn_db_pool_max_connections{pool="reader"} 8
cairn_db_pool_wait_total{pool="writer"} 2676
cairn_db_pool_wait_seconds_total{pool="writer"} 1.301
cairn_db_pool_in_use_connections{pool="reader"} 0
```

A slow endpoint whose wait counter is flat is slow because its query is slow; the
same endpoint with the counter climbing is behind somebody else's write. Those
want opposite fixes and look identical from outside. The reader pool is reported
too, where the number to watch is a floor above zero on an idle instance — that
is a result set nobody closed, holding a snapshot that stops the WAL being
checkpointed.

These are asked for rather than required. The store arrives at the API as the
consumer-defined interface ADR-002 asks for, which describes what the API needs
and says nothing about connections; a backend with a pool reports one, a backend
without stays silent, and neither has to change to accommodate the other.

Scraping is unauthenticated from loopback and needs an API key holding
`metrics:read` from anywhere else. A Prometheus on the same host is the
overwhelmingly common deployment, and a metrics endpoint that needs a credential
is one somebody turns off.

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
| `GET /api/v1/system/info` | Any authenticated caller |
| `GET /api/v1/overview` | `monitors:read` |
| `GET /api/v1/monitors`, `GET /api/v1/monitors/{id}`, `.../membership`, `.../{id}/certificate` | `monitors:read` |
| `POST`, `PATCH`, `DELETE` `/api/v1/monitors...`, `.../{id}/pause`, `.../resume`, `.../check`, `.../bulk` | `monitors:write` |
| `GET /api/v1/monitors/{id}/heartbeats` | `heartbeats:read` |
| `GET /api/v1/monitors/{id}/history`, `.../uptime` | `heartbeats:read` |
| `GET /api/v1/notification-channels`, `.../{id}`, `.../preview`, `.../template-variables` | `notifications:read` |
| `POST/PATCH/DELETE /api/v1/notification-channels...`, `.../{id}/test` | `notifications:write` |
| `GET /api/v1/maintenance-windows`, `.../{id}` | `maintenance:read` |
| `POST/PATCH/DELETE /api/v1/maintenance-windows...` | `maintenance:write` |
| `GET /api/v1/groups`, `.../{id}` | `groups:read` |
| `POST/PATCH/DELETE /api/v1/groups...` | `groups:write` |
| `GET /api/v1/tags`, `.../{id}` | `tags:read` |
| `POST/PATCH/DELETE /api/v1/tags...` | `tags:write` |
| `GET /api/v1/incidents`, `.../{id}`, `.../{id}/updates` | `incidents:read` |
| `POST/PATCH/DELETE /api/v1/incidents...`, `.../{id}/updates` | `incidents:write` |
| `GET /api/v1/status-pages`, `.../{id}`, `.../{id}/subscribers` | `status_pages:read` |
| `POST/PATCH/DELETE /api/v1/status-pages...` | `status_pages:write` |
| `GET /api/v1/webhooks`, `.../{id}`, `.../{id}/deliveries` | `webhooks:read` |
| `POST/PATCH/DELETE /api/v1/webhooks...`, `.../redeliver` | `webhooks:write` |
| `GET /api/v1/settings` | `settings:read` |
| `PATCH /api/v1/settings` | `settings:write` |
| `GET /api/v1/users`, `.../{id}` | `users:read` |
| `GET/PATCH /api/v1/users/me` | User session only — an API key is not an account |
| `GET/POST /api/v1/push/{token}` | None — the token is the credential |
| `GET /api/v1/public/status-pages/{slug}` and the rest of `/public/` | None — a status page whose audience needs a credential is not a status page |
| `GET /metrics` | None from loopback; `metrics:read` from anywhere else |
| — | Engine counters, plus each probe's self-report republished from the result stream |
| `POST /api/v1/imports/kuma`, `GET /api/v1/imports/{id}` | `501` — the endpoint is in the contract and the importer is a separate deliverable |

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

The credential tests assert the property rather than the response: that the
password is absent from the stored `config`, that the sealed column does not
contain it either, that all three read paths redact it, and that a monitor whose
credentials cannot be decrypted is withheld from the probe rather than sent with
half a config — an HTTP monitor missing its bearer token would authenticate as
nobody and report the target down, which is a lie about the target. The
re-sealing pass has its own tests, because a migration path nobody runs twice is
a migration path nobody has tested.

The group and tag tests are mostly about the two places these can be quietly
wrong. One is the slug: a table of names that must collapse to the same
identifier, because a lookalike pair that both save is a pair nobody can tell
apart afterwards. The other is the status rollup, where the failure is a parent
group reading green during an outage in the child underneath it — the assertion
is that the count and the status both reach into children, and that an empty
group reports null rather than up.

The maintenance tests are mostly about the schedule, because a recurrence rule
is where being approximately right is worst: a window that fires an hour late
pages somebody during the change it was scheduled around. They cover the DST
transition in both directions, the short-month skip, cron's union rule for its
two day fields, and the four-year lookahead a February 29th expression needs.
The suppression tests cover the difference between the two kinds — that a window
records `maintenance` and freezes the failure count, and that a dependency
records a real `down` and withholds only the page — because collapsing the two
would either hide a real outage from the SLA figure or count planned downtime
against it.

The alerting tests cover the two things that fail invisibly. One is the secret
boundary: that a token is not in the serialised config, does not appear in any
read response, and survives a form round-tripping its own `GET`. The other is the
transition table — which changes raise an event and which deliberately do not,
because getting it wrong in one direction is a missed outage and in the other is
the alert fatigue that makes people filter the channel, which is a missed outage
with extra steps. All thirteen providers are driven through one harness that
redirects even the hard-coded hosts, so the assertions are about where the
credential goes and whether the payload is well-formed.

The history tests cover the two decisions a reader can get quietly wrong: which
source answers a range, and when a percentile is real. Both fail invisibly — the
response is well-formed either way, just less accurate than it looks.

The rollup tests assert the arithmetic directly, because a rollup bug is the
quietest kind there is: nothing errors, the numbers are just wrong, and they are
wrong in history that raw heartbeats no longer exist to contradict. They cover
the bucket contract, the totals surviving all three tier hops, idempotent
re-runs, late heartbeats being folded in, the null-versus-absent distinction, and
the file actually shrinking.

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
- **The observations behind `/monitors/{id}/certificate`.** The endpoint is real
  and answers `404` for every monitor, which is the honest answer to "what
  certificate was observed" when none has been. The TLS and HTTP checkers *see*
  the certificate and report only an expiry verdict; carrying the observation to
  the control plane means a new field on the probe protocol's result frame, which
  is a protocol change rather than API work.
- **The Kuma importer.** `POST /api/v1/imports/kuma` still answers `501`. The
  endpoint without the importer behind it would accept a file and report success
  for an import that never happened, which is worse than saying no.
- **The UI.** Phase 1 Month 3. Every surface it needs now exists, which was the
  point of finishing the API first.
- **OpenTelemetry export.** `/metrics` is Prometheus text, hand-written, no new
  dependency. OTel means the SDK's dependency tree on a binary whose pitch is
  that it is one static file, and that is a decision worth taking on purpose.
- **Generated Go and TypeScript clients.** Codegen belongs in CI, and CI is not
  changed without being asked.
- **The engine load test in CI.** The harness drives the real engine now and the
  5,000-monitor gate passes, but the workflow still runs the SQLite target only.
  Wiring the engine run in is a change to `.github/workflows/load-test.yml`, and
  CI configuration is not edited without being asked (AGENTS.md rule 7).
- **A concurrent-viewer dimension in the load test.** Membership polling costs
  6.2ms at 5,000 monitors, and its cost scales with connected clients rather than
  with monitor count — the one dimension the gate does not exercise.
