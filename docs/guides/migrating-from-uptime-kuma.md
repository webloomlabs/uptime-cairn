# Migrating from Uptime Kuma

`cairn import kuma` reads a `kuma.db` and reproduces the monitors, tags,
notification channels, and status pages. Several databases at once merges several
Kuma instances into one install — which is the path out for everyone currently
sharding Kuma by hand across hosts because one instance stopped coping.

Two things about it before anything else.

**Nothing is dropped silently.** Every source entity appears exactly once in the
report, with what happened to it. An import that maps 900 of 1,000 monitors and
says which 100 it could not is something you can finish by hand; one that reports
success is something you discover is wrong during an outage.

**Do a dry run first.** It produces the entire report and writes nothing. It
takes as long as the real thing minus the writes, and it is the difference
between reading the surprises now and finding them afterwards.

---

## Find your kuma.db

Inside the container, at `/app/data/kuma.db`.

```sh
docker cp uptime-kuma:/app/data/kuma.db ./kuma.db
```

Copying it out of a running Kuma is fine — SQLite's WAL means you may miss the
last few heartbeats, and nothing you are importing changes minute to minute. If
you want it exact, stop Kuma first.

The file is opened **read-only**. The importer cannot write to your live
monitoring database, and that is enforced by the connection mode rather than by
care.

---

## The dashboard

**Import** in the sidebar. Choose the file, leave **Dry run** ticked, and press
start. Read the report. Untick and run it again.

That is the whole flow, and it is the same importer as the command line —
literally, through the same seam — so the two cannot disagree about what a
`keyword` monitor becomes.

## The command line

```sh
# Stop cairn first: SQLite takes one writer.
sudo systemctl stop uptime-cairn

cairn import kuma --data-dir /var/lib/uptime-cairn --dry-run ./kuma.db
cairn import kuma --data-dir /var/lib/uptime-cairn ./kuma.db

sudo systemctl start uptime-cairn
```

A running install imports from the dashboard instead, which does not need to
stop anything.

### Flags

| Flag | |
|---|---|
| `--dry-run` | The whole report, nothing written. |
| `--on-conflict` | `rename` (default), `skip`, `replace`. |
| `--name-prefix` | Prefixes imported monitor names. |
| `--monitors`, `--tags`, `--notifications`, `--status-pages` | On by default; turn off what you do not want. |
| `--history` | Also import historical heartbeats. Slower, and much larger. |
| `--resume` | Start checking immediately. Off by default. |

**`replace` behaves as `skip`, and the report says so.** Honouring it literally
would mean deleting an existing monitor and its whole history to make room for
one named the same — during a migration, which is exactly when you are least able
to notice. A real replace needs a decision about history that nobody has made.

---

## Merging several instances

```sh
cairn import kuma --name-prefix "acme / "  ./acme-kuma.db
cairn import kuma --name-prefix "globex / " ./globex-kuma.db
```

Or all at once:

```sh
cairn import kuma ./acme-kuma.db ./globex-kuma.db ./initech-kuma.db
```

Identity is per file. Two Kuma instances both have a monitor with id 1, so
nothing is keyed on the source id across files.

Names collide constantly — every Kuma install has a "Checkout" — which is what
`--on-conflict` and `--name-prefix` are for. Renaming is the default because it
is the only one of the three that cannot lose one of them.

---

## What comes across

| Kuma | Becomes |
|---|---|
| `http`, `keyword`, `json-query` | `http`, with the keyword or JSON assertion |
| `port` | `tcp` |
| `ping` | `icmp` |
| `dns` | `dns` |
| `docker` | `docker` |
| `push` | `push` |
| `grpc-keyword` | `grpc` — see below |
| `certificate-expiry` | `tls_expiry` |
| a monitor of type `group` | a real **group** |
| `tag` + `monitor_tag` | tags — see below |
| `notification` | a notification channel, for thirteen of Kuma's providers |
| `status_page` + `group` + `monitor_group` | a status page with ordered sections |
| `heartbeat` | history, with `--history` |

Uptime Kuma's intervals below 20 seconds are raised to 20, which is this
product's floor. A Kuma `timeout` of 0 — its default, which it interprets
internally as a fraction of the interval — becomes half the interval, because
the schema requires a timeout below the interval and a zero would be refused for
a value you never set.

---

## What does not, and what the report says about it

These are the honest edges. All four are named per entity in the report rather
than mentioned once here.

### Monitor types with no equivalent

Kuma has roughly forty types; this build has nine. MQTT, Kafka, RADIUS, SNMP,
Steam and the other game-server checks, the database checks, `real-browser`, and
several more have no equivalent.

They are recorded as **`unsupported`** with their name and type, and **nothing is
written**. Both of the alternatives are wrong:

- *Skipping silently* loses the name, the interval, the tags and the notification
  attachments you spent an afternoon setting up, for a monitor you have to
  rebuild anyway.
- *Importing as the nearest type* invents a check. An MQTT monitor imported as a
  TCP check on port 1883 is green while the broker rejects every publish, which
  is a monitoring tool lying — and the entire product is on the other side of
  that line.

So you get a list, in the order your own install had them, to work from.

### Per-monitor tag values

Kuma lets one tag carry a different value per monitor: `env` attached as
`production` here and `staging` there. This build's tags are a flat join.

The importer **splits it into one tag per value** — `env: production` and
`env: staging` — and attaches each monitor to the one it actually had. The
alternative was a nullable `value` column, which is cheap in the schema and
expensive everywhere else: it would appear in the tag filter, the bulk tag
operation, the status page tag display, and the API's tag resource, and every one
of those would have to decide what a value means. The split produces something
the existing filters and bulk operations already handle, and you can see what
happened from the name.

### Per-monitor proxies

Kuma supports an HTTP proxy per monitor. Nothing here has a proxy concept, and
inventing API surface from inside a migration tool is not something a migration
tool gets to do.

A proxied monitor imports as the check it is, **without** the proxy, and the
report says the check will now be made from this host directly — which may fail
if the target is only reachable through that proxy. That is a statement you can
act on, and it is materially different from the monitor quietly going red.

### gRPC

Kuma's `grpc-keyword` calls an arbitrary method and greps the response. This
build's `grpc` monitor speaks the standard health protocol. That is a different
check against the same server, so the monitor is imported and the difference is
named on it rather than being imported as though the two were the same.

### Other things the report will tell you

- **Push tokens come across unchanged, but the path does not.** Kuma's
  `/api/push/<token>` against this build's `/api/v1/push/<token>`. Repoint
  whatever sends the heartbeat; the token itself is the one you already have.
- **A status page password is not imported.** Kuma stores it in a form this
  build cannot verify against. The page imports public; set a password if it
  needs one.
- **OAuth2, mTLS, and NTLM HTTP authentication are not imported.** Basic and
  bearer are.
- **A Kuma webhook's custom body template is not translated.** Kuma's templates
  are Liquid and this build's are its own; the payload is left as the default
  event envelope rather than being half-translated into something that renders
  differently.
- **Imported heartbeat timestamps may be offset.** Kuma writes them without a
  time zone in 1.x, so they are read as UTC and may be out by the source host's
  own offset.

---

## Reading the report

Both the dashboard and the CLI lead with **what needs your attention** — every
entity that did not come across, plus every one that did with something lost
along the way — and put the tally after it.

That order is deliberate. A summary that leads with "1,204 monitors imported" and
buries thirty unsupported types below the fold is a summary that gets skimmed, and
skimming it is how somebody finds out during an outage that a monitor they thought
they had was never created.

The job's overall state is one of three:

- **succeeded** — everything came across.
- **partial** — some things did not. What did not is listed above the tally.
- **failed** — nothing usable came across, and the error says why.

`partial` exists because collapsing it into either of the others is how somebody
concludes their migration finished when thirty monitors are missing.

---

## After the import

**Imported monitors arrive paused.** That is the default and it is worth keeping:
resuming five thousand monitors at once means five thousand checks firing at
once, against production, in the first minute of a migration.

Review them, then resume in bulk from the monitor list — select all, **Resume** —
or:

```sh
curl -X PATCH /api/v1/monitors/bulk \
  -d '{"monitor_ids": [...], "operation": "resume"}'
```

Then the three things worth doing before you decommission Kuma:

1. **Check the notification channels.** Test-fire each one. A Slack webhook that
   came across correctly and was revoked six months ago looks identical to one
   that works until you press the button.
2. **Repoint your push jobs.** The path changed.
3. **Compare the monitor counts.** The report's tally against what Kuma showed
   you. They should differ by exactly the unsupported ones, and if they do not,
   the report says which.

Running both side by side for a week costs nothing and is the cheapest possible
confidence.
