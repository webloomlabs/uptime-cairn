# Upgrading, and the rollback stance

## How an upgrade works

Replace the binary or the image tag and restart. Migrations are numbered,
embedded in the binary, and applied automatically on start; there is nothing to
run by hand and no maintenance mode to enter.

```console
level=INFO msg="migration applied" version=4 name=monitor_config_secrets
level=INFO msg="migration applied" version=5 name=subscriber_delivery
```

A migration that has already been applied is skipped, and its checksum is
verified against the file the new binary carries. A mismatch is fatal on start
rather than a warning, because the alternative is two installs claiming the same
schema version with different schemas:

```
migration 0004_monitor_config_secrets was applied with checksum a1b2… but the
file now hashes to c3d4…: a released migration was edited, which two installs
will disagree about — restore the file and write a new migration instead
```

If you see that against an official release, you are running a modified build.

### Docker

```sh
docker compose pull
docker compose up -d
```

### Binary

```sh
systemctl stop uptime-cairn
install -m 755 ./cairn /usr/local/bin/cairn
systemctl start uptime-cairn
cairn --version
```

Stop before replacing. The running process holds the database in WAL mode, and a
clean stop is what checkpoints and truncates it.

## Take a backup first, and mean it

Not ceremony. The migrations are forward-only — there are no down migrations,
and there will not be. A rollback path that is never exercised is a rollback
path that does not work, so the project does not pretend to have one. **The
documented recovery stance for a bad upgrade is restore-from-backup**, and that
is the path that is tested.

So the backup is not a precaution against the upgrade failing loudly. It is the
only way back if the upgrade succeeds and you dislike the result.

```sh
sqlite3 /var/lib/uptime-cairn/cairn.db "VACUUM INTO '/backups/pre-upgrade.db'"
```

See [backup-restore.md](backup-restore.md); note in particular that copying
`cairn.db` on its own does not work while the process is running.

## Downgrading

There is nothing stopping you, and that is the problem.

An older binary starting against a database written by a newer one **does not
refuse and does not warn**. The runner iterates the migrations the binary
carries, finds each already applied with a matching checksum, and proceeds —
schema versions beyond the ones it knows about are simply not examined. Verified
against the current build: a database carrying an extra `schema_migrations` row
started an older binary cleanly, with nothing in the log about it.

What happens next depends entirely on what the newer migration did. A migration
that only added a table or a nullable column is harmless, and the older binary
never touches it. One that added a `NOT NULL` column, tightened a constraint, or
moved data between columns will produce write failures against tables the older
build thinks it understands — at write time, per row, scattered through the log,
rather than at startup where you would see them.

The stance, stated plainly:

- **Restore the pre-upgrade backup.** That is the supported way back, it takes
  the schema with it, and it is the only path anyone tests.
- Running an old binary against a new database is not supported, is not
  detected, and is not safe to assume harmless because it started.

A startup guard that refuses an unknown-higher schema version is worth adding
and is not built. Until it is, the burden is on the operator, which is why it is
written down here rather than left to be discovered.

## What an upgrade will not break

`/api/v1` carries an explicit compatibility promise: fields and enum values may
be added, nothing is removed or retyped, existing endpoints do not change
semantics, and anything breaking goes to `/api/v2`. Deprecations are announced,
carry `Deprecation` and `Sunset` headers, and keep working for no less than two
minor releases or six months, whichever is longer. The full statement is in
[docs/api/README.md](../api/README.md#compatibility-promise).

That promise is what makes an unattended `docker compose pull` reasonable for
the API clients you have written. It says nothing about the schema, which is
private, and nothing about the downgrade above.

## After the upgrade

```sh
curl -s localhost:3000/healthz          # {"status":"ok","version":"v1.0.1"}
```

`version` there is the binary's own build identity, so it is also the check that
the new image is actually the one running. Then confirm heartbeats are still
landing — `cairn_heartbeats_written_total` should keep climbing, and
`cairn_results_rejected_total` should not start. See
[self-monitoring.md](self-monitoring.md).
