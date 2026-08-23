# Security policy

## Reporting a vulnerability

**Email [security@uptimecairn.dev](mailto:security@uptimecairn.dev).** Not a
GitHub issue, not a discussion, not a pull request — those are public the moment
they are filed, and every self-hosted install stays vulnerable for as long as it
takes somebody to notice and upgrade.

If you want the report encrypted, say so in a first message with no detail in it
and a key will come back.

### What helps

- What you did, and what happened.
- The version — `cairn --version`, or the image tag.
- Whether the install was solo mode or behind a reverse proxy, and which one.
- A proof of concept, if you have one. A curl command is ideal.

### What to expect

| | |
|---|---|
| Acknowledgement | Within 3 working days. |
| An assessment | Within 10 working days: whether it is a vulnerability, how severe, and roughly when a fix lands. |
| A fix | Critical and high severity: as fast as it can be tested. Medium and low: the next release. |
| Credit | In the release notes and the advisory, under whatever name you want, unless you would rather not be named. |

We will tell you before we publish, and we will not publish before a fixed
release exists.

This is an Apache 2.0 project with no bug bounty. What we can offer is that a
report will be read by somebody who understands the code, and answered by a
person.

### Scope

In scope: this repository, the published container images, and the released
binaries.

Out of scope, and please do not spend your time on them:

- Anything requiring an attacker who already has the database *and* the
  encryption key. That is game over by construction and is documented as such.
- Missing hardening headers on an install with no reverse proxy in front of it.
  The binary speaks plain HTTP by design and TLS termination is the proxy's job;
  [the recipes](docs/operations/reverse-proxy.md) set the headers.
- Denial of service by asking a monitor to check something enormous. A
  monitoring tool makes outbound requests to hosts you name; that is the
  feature.
- Reports from an automated scanner with no working proof of concept.

---

## What this product does about security, so you know where to look

The full reasoning lives in the ADRs and the data model. This is the map.

### Credentials at rest

Every credential the product stores is encrypted with AES-256-GCM under a
per-row envelope, with the AAD binding the envelope to its table, column, and
row — so a blob moved from one row to another fails to open rather than
decrypting into the wrong place.

That covers: monitor HTTP basic and bearer auth, Docker client TLS material,
gRPC metadata, every notification channel's secrets, outbound webhook signing
secrets and headers, the instance SMTP password, TOTP secrets, and status page
subscriber addresses.

The rule the code follows is **hash what you verify, encrypt what you replay**.
A status page password is hashed, because it is only ever compared against. A
subscriber's address is encrypted, because a notification has to send to it.

The root key is 32 bytes in `cairn.key`, or supplied by
`--encryption-key-file` / `CAIRN_ENCRYPTION_KEY_FILE` / `CAIRN_ENCRYPTION_KEY`.
**Without it the database is unreadable**, which is the point and is also a
backup obligation — see
[operations/backup-restore.md](docs/operations/backup-restore.md).

### Authentication

- Passwords: argon2id.
- Sessions: HTTP-only cookies with CSRF tokens on every write. The CSRF token is
  issued once, at login, and `GET /auth/session` deliberately does not reissue
  it — an endpoint that did would let anything able to make a `GET` obtain the
  token that authorises a write.
- Login is rate-limited per IP and email.
- TOTP two-factor with single-use recovery codes.
- API keys are scoped, expiring, revocable, and stored hashed. **A key cannot be
  granted a scope its creator does not hold**, which is the property that stops
  scoped keys being a privilege-escalation path.

### The unauthenticated surface

Three paths take no credential, each on purpose, and each carrying its
credential in the path itself:

- `GET|POST /api/v1/push/{token}` — the dead-man's-switch ingest. Callers are
  cron jobs and shell scripts, and `curl <url>` with no flags has to work. The
  token is looked up **by hash through a unique index**, so guessing costs one
  index probe rather than a scan, and a stolen database yields no working
  tokens. A missing token and a malformed one both answer 404.
- `/api/v1/public/...` — the status page read path. A separate projection rather
  than a filtered monitor read, because a field cannot leak through a shape with
  no place to put it — and this is the one endpoint where a leak reaches
  strangers.
- `/subscriptions/confirm/{token}` and `/subscriptions/unsubscribe/{token}` —
  the two links subscriber mail carries. Tokens are stored hashed *and*
  encrypted, because one is verified when somebody follows it and the other is
  replayed at the foot of every message.

Unsubscribing waits for a button press rather than acting on load, and
`List-Unsubscribe` does not advertise RFC 8058 one-click. Mail clients prefetch
and security appliances follow every link in a message; acting on load would
quietly remove people who never clicked.

### `/metrics`

Requires `metrics:read` **except from loopback**, so a Prometheus on the same
host needs no credential.

That exemption had a defect worth naming, because the shape is a common one: a
reverse proxy on the same host also connects from `127.0.0.1`, so every request
it forwarded — from anywhere — inherited an exemption meant for a local scraper.
What leaks is the whole monitor inventory, since `cairn_monitor_status` carries
every monitor's id, name, and type.

The current behaviour:

- Loopback peer, no `X-Forwarded-For` → exempt. A local scraper connects
  directly and sends no such header.
- Loopback peer **with** `X-Forwarded-For`, from a peer not declared as a proxy →
  **not** exempt. All three published proxy recipes set that header, so a
  proxied request no longer passes as a local one.
- Peer declared with `--trusted-proxy` → the header is read, right to left,
  skipping our own hops, and the exemption is applied to the real client.

`X-Forwarded-For` is believed nowhere else in the process, and `--trusted-proxy`
is the only thing that makes it believed anywhere.

The reverse-proxy recipes still deny `/metrics` at the edge, and that remains the
recommendation.

### The probe boundary

The control plane and the probe are separate by construction
([ADR-001](docs/adr/001-probe-and-control-plane-split.md)), and the boundary is
enforced as an import restriction a test asserts mechanically: the control plane
does not link the checkers. In solo mode they are one process talking over an
in-memory gRPC connection with real serialisation, which means every install
exercises the same code path a remote probe will.

A monitor whose credentials cannot be decrypted is **withheld from the probe**
rather than sent with half a configuration. An HTTP monitor missing its bearer
token would authenticate as nobody and report the target down, which is a lie
about the target; leaving it unassigned is at least visibly wrong.

### Supply chain

- Pure Go, no cgo. `modernc.org/sqlite` is the reason, and it is also why the
  binary cross-compiles to five targets without a toolchain per architecture.
- A short dependency list, and adding to it requires justification in the pull
  request: what it does, why the standard library will not, and its maintenance
  status.
- Releases publish checksums and an SBOM.
- `BUILD_DATE` comes from the commit rather than the clock, so re-running a
  release on the same tag produces the same bytes.
- Dependency and container scanning run in CI on every pull request.

### Reporting security-relevant behaviour that is not a vulnerability

If something is confusing rather than exploitable — a default that surprised
you, a message that made you do the wrong thing — open an issue. Confusing
security behaviour is how vulnerabilities get introduced by the people
configuring the software, and it is worth fixing.
