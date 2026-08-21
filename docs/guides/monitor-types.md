# Monitor types

Nine types. This page is what each one actually checks, and the fields where the
obvious reading is wrong.

The full field-by-field schema is in the
[OpenAPI spec](../api/openapi.yaml) under `HttpConfig`, `TcpConfig`, and so on —
that is the contract, and this page is the part of it a spec cannot say.

## The vocabulary, first

Every heartbeat carries one of six statuses, and two of them are about the probe
rather than the target. This distinction runs through the whole product and it is
the first thing worth understanding:

| Status | Means |
|---|---|
| `up` | The check ran and passed. |
| `down` | The check ran and the target failed it. |
| `pending` | Nothing has been checked yet. Not a verdict. |
| `maintenance` | Suppressed by a maintenance window. |
| `unknown` | **The probe could not perform the check.** |
| `skipped` | The probe shed the check under load. |

`unknown` is not a soft `down`. A DNS lookup failing because the target's record
is gone is `down` — a statement about the target. The same lookup failing because
*this host's* resolver is unreachable is `unknown` — a statement about the probe.
Collapsing the two would mean one broken probe paging an entire on-call rotation
about services that were never affected.

Where a checker cannot tell the difference, it reports `unknown`. That is a rule
rather than a preference.

---

## `http` — HTTP and HTTPS

The workhorse. Four assertions, evaluated in order, and the first failure is what
the heartbeat message reports — so the message names the actual cause rather than
"check failed".

1. **Status code.** `accepted_status_codes` takes codes and inclusive ranges:
   `["200-299"]` by default, `["200-299", "404"]` if a 404 is a legitimate answer
   from the endpoint you are checking.
2. **Keyword.** `contains`, `not_contains`, `regex`, `not_regex`, with
   `case_sensitive` defaulting to false. `not_contains` on a word your error page
   prints is often a better check than a status code, because a great many
   applications return 200 with an apology in the body.
3. **JSON path.** `{"path": "$.status", "operator": "eq", "expected": "ok"}`.
   The path grammar is deliberately small — root, dotted field names, array
   indices, `$["quoted names"]` — and anything outside it is rejected at save
   time rather than ignored at check time. A monitor whose assertion is a program
   is a monitor nobody can read in a list.
4. **Response time.** `response_time_threshold_ms` fails a check that succeeded
   but took too long. This is how "the site is up but unusable" becomes a page.

### Things worth knowing

**`verify_tls: false` is offered and is a bad idea outside a private CA.** It
disables certificate verification entirely, which means a monitor that would not
notice a man-in-the-middle. The UI surfaces it as a warning. For an internal
target with a private CA, prefer adding the CA to the host's trust store.

**Credentials are encrypted.** `auth.password`, `auth.token`, and every value in
`headers` are split out of the config at the storage boundary and sealed. They
are never returned by a read — a `GET` shows a redaction marker instead — and a
`PATCH` that sends the marker back is understood as "leave it alone". That is
what makes a form which round-trips its own `GET` unable to destroy a password it
was never shown.

**`follow_redirects` and `max_redirects` are separate.** Setting
`max_redirects: 0` with `follow_redirects: true` means "do not follow", which is
also what `follow_redirects: false` means. Prefer the flag; the count is there
for a redirect chain you want to bound rather than forbid.

---

## `tcp` — a port is open

A completed TCP handshake within the timeout. Nothing is sent and nothing is
read.

That makes it the right check for a database, a message broker, or anything else
whose protocol you do not want to speak — and the wrong check for anything where
the port being open does not mean the service works. A Postgres accepting
connections while its disk is full passes this check.

---

## `icmp` — ping

The type with the most environment-specific behaviour, so it has the most
explicit handling.

It tries the **unprivileged ICMP datagram socket** first
(`net.ipv4.ping_group_range`), then a **raw socket** (`CAP_NET_RAW`). When
neither opens, the monitor reports **`unknown`, not `down`**, with a message
saying why. The target is fine; this host cannot open a socket, and blaming the
target for that would be a lie.

`fallback_to_tcp` turns that case into a TCP connect check against
`fallback_tcp_port`, and each heartbeat records that it fell back. It is a
different check and the monitor says so rather than quietly substituting one for
the other.

In Docker, the sysctl is the fix:

```
--sysctl net.ipv4.ping_group_range="10001 10001"
```

The image runs as UID/GID 10001. See [install.md](install.md#docker).

---

## `dns` — a record resolves, and to what

All ten record types: A, AAAA, CNAME, MX, NS, TXT, SRV, CAA, SOA, PTR.

`expected_values` with `match_mode`:

- `any` — at least one expected value is present. The usual choice.
- `all` — every expected value is present, extras allowed.
- `exact` — the answer set matches exactly, nothing extra. This is the one that
  catches a record somebody added.

Empty `expected_values` asserts only that the record resolves at all.

### The resolver

`resolver: null` walks **every** nameserver in `resolv.conf` in file order, not
just the first. That file is a fallback list, and a host whose primary nameserver
is unreachable could otherwise never run a DNS monitor at all. Every candidate
shares one timeout, so three dead nameservers cannot cost three times the timeout
you configured.

An unreachable resolver reports `unknown`, not `down`. Watch for this: a monitor
sitting on `pending` forever with no recorded failures is the symptom, and it
means nothing is being monitored.

The response code is recorded on the heartbeat — `NXDOMAIN`, `SERVFAIL` — spelled
the way DNS spells it, so grepping the history a year later finds what you expect.
Truncated responses are retried over TCP automatically.

CAA is queried by number, because it postdates most DNS libraries. A wrong CAA
record is invisible until an issuance fails, which is exactly the kind of thing
worth a monitor.

---

## `tls_expiry` — a certificate is still valid

The handshake is made **unverified** and the chain is then checked by hand. That
ordering matters: an expired certificate reported through ordinary verification
comes back as a generic TLS error, and this reports it as expiry, which is the
thing you set the monitor up to hear about.

- `days_remaining_threshold` (default 14) is when it starts failing.
- `verify_chain` (default true) also fails on an untrusted or incomplete chain —
  a missing intermediate is the classic one, because it works in every browser
  that has cached the intermediate and fails for everyone else.
- `server_name` sets SNI when it differs from `hostname`.

What was observed on the wire is readable from
`GET /monitors/{id}/certificate` — subject, issuer, serial, SANs, fingerprint,
and `observed_at`.

**`observed_at` means "last confirmed on the wire", to within an hour.** The
certificate rides the result frame when it changes and once an hour otherwise,
because it is several hundred bytes against a hundred for the result carrying it,
and sending it on every check would cut the probe's outage buffer to a fifth. A
renewal lands on the next check, not on the next hour. Pressing **Check now**
refreshes it immediately.

An `http` monitor also records the certificate it was presented with, and
deliberately does not alert on expiry: it has no threshold to alert against. If
you want the page, add a `tls_expiry` monitor — that is the type that asks for
the line.

---

## `domain_expiry` — the registration has not lapsed

RDAP first (RFC 9224 bootstrap), falling back to WHOIS where the registry offers
no RDAP endpoint.

**Checked once a day per domain regardless of the monitor's interval.** Registries
rate-limit, the data changes once a year, and a 20-second interval against a
registry is a way to be blocked. The interval you set governs how often the
monitor's *state* is evaluated; the lookup itself is throttled underneath.

`days_remaining_threshold` defaults to 30, which is longer than the TLS default
for a reason: a lapsed domain is not recoverable in an afternoon.

---

## `push` — a dead-man's switch

Backwards from every other type. Nothing is checked; something calls *you*.

```
GET|POST /api/v1/push/{push_token}
```

Up while that arrives at least once per `expected_interval_seconds`. Down once
that plus `grace_period_seconds` passes in silence.

This is how you monitor a cron job, a backup, or a batch that runs somewhere with
no inbound port. The pusher may report its own verdict:

```sh
curl "$PUSH_URL?status=down&msg=disk+full&ping=12.5"
```

Three things about it:

- **The token is shown once, at creation.** It is stored as a hash, so it cannot
  be recovered afterwards. Rotating it means recreating the monitor.
- **It is a credential.** Anyone holding it can report the monitor healthy, which
  is the one thing a dead-man's switch must not let a stranger do.
- **It is evaluated by the control plane, never by a probe.** There is nothing to
  execute, and pretending otherwise would write a heartbeat the target did not
  send. **Check now** on a push monitor is refused, with that explanation.

---

## `docker` — a container is running

Through the Docker API, over the socket at `docker_host` (default
`unix:///var/run/docker.sock`). In solo mode that means mounting the socket into
the container:

```yaml
volumes:
  - /var/run/docker.sock:/var/run/docker.sock:ro
```

`require_healthy: false` (the default) passes on *running*. Set it true and a
running container with a failing `HEALTHCHECK` counts as down — which is usually
what you meant, if the image defines one.

`tls` carries client certificate material for a remote daemon. All three fields
are encrypted at rest and redacted on read.

### It is pinned to one probe

This is the only type whose placement matters. Every other check answers the same
question from anywhere with the right egress — an `http` monitor checked from
Sydney and from Frankfurt is two opinions about one target, which is the whole
basis of multi-region probing. "Is this container running" is a question about one
host's daemon, and there is no second opinion to be had.

So a `docker` monitor carries a `probe_id`. On an install with exactly one probe
— which is every solo install — the server fills it in and you will never see the
field. On an install with more, the write is refused with an error naming
`/probe_id`, because guessing which host you meant would produce a monitor
reporting a container missing that was never meant to be there.

---

## `grpc` — a server declares itself healthy

The standard `grpc.health.v1.Health/Check` protocol.

- `service_name: null` asks about the server overall.
- `accepted_statuses` defaults to `["SERVING"]`. `NOT_SERVING` from a server that
  answered is a healthy *server* reporting an unhealthy *service*, and the
  distinction is occasionally what you want.
- `metadata` is request metadata — a bearer token usually — and is encrypted at
  rest.

It does not call an arbitrary method and grep the response. If you are coming
from Uptime Kuma's `grpc-keyword`, that is a different check against the same
server, and the import report says so on every one it converts.

---

## Settings every type shares

| Field | Notes |
|---|---|
| `interval_seconds` | Floor is 20. Every capacity figure in this project is derived from 5,000 monitors at 20 seconds = 250 checks/sec. |
| `timeout_seconds` | Must be less than the interval. The schema enforces it. |
| `retries` / `retry_interval_seconds` | Retries run probe-side, and each attempt is its own heartbeat with its own `attempt` number. |
| `resend_after` | Re-alert after this many consecutive failures. 0 means alert once per outage. |
| `upside_down` | A passing check is `down` and vice versa. For things that are supposed to be unreachable. |
| `notify_on_recovery` | On by default. Turning it off means you hear about the outage and not the fix. |
| `parent_monitor_id` | Dependency suppression — see below. |
| `group_id`, `tag_ids` | Organisation. A group is exclusive and hierarchical; a tag is many-to-many and flat. |

### Dependency suppression

`parent_monitor_id` is what stops the router going down from paging you forty
times.

It is transitive up the chain, and a parent **under maintenance** suppresses its
children exactly as a parent that is down does — taking the router down for a
firmware upgrade is the most known problem there is.

The child's own heartbeat still records the real outage. Only the page is
withheld, so the child's uptime figure is unaffected and the history tells the
truth about what happened.
