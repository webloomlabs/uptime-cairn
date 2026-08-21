# Backup, upgrade, and day-to-day operations

The short version of the three pages this one indexes, plus the things that only
matter when you are running it rather than reading about it.

If you have five minutes, read [the two files](#the-two-files-that-matter)
section and nothing else.

---

## The two files that matter

```
/data/cairn.db     everything the install knows
/data/cairn.key    45 bytes; without it, every stored secret is unreadable
```

Everything else in the data directory is reconstructible. These two are not.

**Do not copy `cairn.db` on its own while the process is running.** This is
measured rather than theorised: with cairn up, `cp cairn.db backup.db` produced
a database that passed `integrity_check` and was missing a monitor — one row
against the live database's two, because the writes were still in an 848 KB WAL
beside a 4 KB main file. The copy is not corrupt. It is silently old, which is
worse.

The correct snapshot is a single command and took 28 ms on that same database,
without blocking writers:

```sh
sqlite3 /data/cairn.db "VACUUM INTO '/backup/cairn-$(date +%F).db'"
cp /data/cairn.key /backup/
```

**Back the key up somewhere the database is not.** It sits beside the database by
default because `docker run` has to work with no key management, and that
convenience is also both eggs in one basket. A backup containing only the
database restores an install where every monitor's password, every bot token,
and every subscriber address is unrecoverable.

Full detail, including a restore verified end to end through the API:
**[backup-restore.md](backup-restore.md)**.

---

## Upgrading

```sh
docker compose pull && docker compose up -d
```

Migrations run on start, are logged, and are forward-only. There is no
maintenance mode and no separate migration step.

**There are no down migrations and there will not be.** A rollback path nobody
exercises is a rollback path that does not work. The documented recovery is
restore-from-backup, which is exercised.

An older binary opened against a newer database now **refuses to start**, naming
the migration it does not know about. That was found by testing the rollback
stance rather than by reasoning about it: before the guard, the older binary
started cleanly and silently, iterating only the migrations it carried and never
looking past them. Whether that is harmless depends entirely on what the newer
migration did, and the failure it produces when it is not — write errors, per
row, scattered through the log hours later — is the expensive kind.

Full detail: **[upgrading.md](upgrading.md)**.

---

## What to watch

`/metrics` carries thirty-odd series. Five of them are ways for this product to
be broken while looking fine, and they are the ones worth an alert:

| Series | Why |
|---|---|
| `cairn_alerts_dropped_total` | Alerts shed by a full queue. The monitoring works and nobody is told. |
| `cairn_probe_shed_results_total` | A probe's buffer filled and results were discarded. Correct behaviour, invisible from the monitor's side by design — which is why it has to be visible here. |
| `cairn_results_rejected_total` | Results that could not be attributed to a monitor. |
| `cairn_heartbeats_written_total` | Going flat is the whole product having stopped. Alert on the *rate*, against `monitors × (1 / interval)`. |
| `cairn_db_pool_wait_total` | Writer contention. It separates "this got slower" from "this is queued behind something else", and only one of those is fixed by tuning a query. |

`cairn_live_subscribers` is the one cost that scales with connected browsers
rather than with monitor count — the dimension the 5,000-monitor gate does not
exercise. Worth a graph rather than an alert.

The per-monitor series carry `monitor_id`, `monitor`, and `type`. That is 15,000
series at the install size this product is built for, with the name label
churning on every rename. [self-monitoring.md](self-monitoring.md) says how to
drop them at scrape time and what you lose by doing it — which is nothing about
Cairn's own health.

### Health endpoints

| | |
|---|---|
| `GET /healthz` | 200 while the process is running. Never authenticated. |
| `GET /readyz` | 200 once migrations are done, the store is reachable, and the scheduler has started. 503 otherwise, naming what failed. |

Use `/readyz` as a container health check and `/healthz` as a liveness probe.
Using `/readyz` for liveness would restart a container that is slow to migrate,
which is the one moment it must be left alone.

---

## Retention, and the Raspberry Pi problem

Raw heartbeats are the expensive tier. At 500 monitors on a 20-second interval
that is 2.1 million rows a day, and an SD card will notice long before a server
does.

The rollup tiers above raw — 1m, 5m, hourly, daily — are what every history range
beyond the raw window is made of, and they are two orders of magnitude smaller.
Shortening raw retention costs you per-check detail older than the window and
nothing else: the uptime bar on a status page, the 90-day figure, and Phase 2's
reports all read rollups.

Settings → Retention. Zero means keep indefinitely, which on a Pi is how the card
fills up.

Disk is genuinely reclaimed on SQLite: `auto_vacuum=INCREMENTAL` is applied in
the connection DSN, and a test asserts the file shrinks. (It was in a migration
first, where the PRAGMA is a no-op inside the runner's transaction — which is
the kind of thing that looks configured and is not.)

---

## Reverse proxies

The binary speaks plain HTTP and has no TLS flags, by design. TLS termination is
a reverse proxy's job.

Two things in the recipes are not optional:

1. **Deny `/metrics` at the edge.** It carries the full monitor inventory.
2. **Pass `Host` through** if you use custom-domain status pages. nginx does not
   do this by default.

**[reverse-proxy.md](reverse-proxy.md)** has Caddy, nginx, and Traefik, plus
custom domains and ACME.

---

## The one thing a control-plane outage does not survive

Data collection survives a control-plane outage: a probe buffers its results and
delivers them when the connection comes back, which is what
[ADR-001](../adr/001-probe-and-control-plane-split.md) buys.

**Alerting does not.** The transition decision, the notification dispatch, and
the incident timeline all live in the control plane, so an outage there means
outages elsewhere go unreported until it returns — and the history afterwards
will show them correctly, which makes it worse rather than better: the record
says you should have known.

If that matters to you, monitor Cairn from somewhere else. A free account on
anything, pointed at `/healthz`, is enough. The tool that watches everything
needs something watching it, and it cannot be itself.
