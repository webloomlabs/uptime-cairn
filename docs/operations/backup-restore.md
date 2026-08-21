# Backup and restore

Everything below has been run against the current build; the numbers and
messages are real output, not illustrations.

Two files matter, and they have different failure modes:

| File | What it is | If you lose it |
|---|---|---|
| `cairn.db` | Every monitor, heartbeat, incident, and status page | You are restoring from an older copy |
| `cairn.key` | 45 bytes; the root key wrapping every stored secret | Every credential in the database is permanently unreadable |

Back them up to **different places**. A backup that puts the key beside the
database it protects has encrypted nothing against the threat that actually
happens, which is somebody walking off with the backup.

## Do not copy `cairn.db` on its own

The database runs in WAL mode, which means recent writes are in `cairn.db-wal`
and not yet in `cairn.db`. Copying the one file while the process is running
gets you a valid database that is silently missing data. On a running install
with two monitors:

```console
$ cp /data/cairn.db naive.db
$ sqlite3 naive.db "SELECT count(*) FROM monitors;"
1
$ sqlite3 /data/cairn.db "SELECT count(*) FROM monitors;"
2
```

That is the whole hazard: `integrity_check` passes on `naive.db`, it opens
cleanly, and it is wrong. Immediately after a fresh start the main file is 4 KB
against an 848 KB WAL — a copy taken then contains essentially nothing while
looking like a database.

`cp -r` of the whole directory has a subtler version of the same problem: the
three files are read at three different instants and need not agree.

## Online backup

`VACUUM INTO` takes a consistent snapshot through SQLite's own reader, which
means it sees the WAL, does not block writers, and produces a defragmented file
with no `-wal` beside it:

```sh
sqlite3 /data/cairn.db "VACUUM INTO '/backups/cairn-$(date +%F-%H%M).db'"
```

It ran in 28 ms against the drill database. It is proportional to data rather
than to uptime, so budget for it growing with the install.

In Docker, run it beside the volume rather than through the application:

```sh
docker exec uptime-cairn \
  sqlite3 /data/cairn.db "VACUUM INTO '/data/backup.db'"
docker cp uptime-cairn:/data/backup.db ./cairn-$(date +%F).db
docker exec uptime-cairn rm /data/backup.db
```

The image ships the `sqlite` CLI for exactly this. The backup lands inside
`/data` first because that is the only writable path in the container.

The key is a separate, one-time copy — it does not change:

```sh
docker cp uptime-cairn:/data/cairn.key ./cairn.key   # then store it elsewhere
```

## Verify the backup, every time

A backup nobody has restored is a hypothesis. These two are cheap enough to run
on every backup, in the script that takes it:

```console
$ sqlite3 backup.db "PRAGMA integrity_check;"
ok
$ sqlite3 backup.db "PRAGMA foreign_key_check;"
$ sqlite3 backup.db "SELECT version, name FROM schema_migrations ORDER BY version;"
1|initial
2|identity
3|alerting_and_pages
4|monitor_config_secrets
5|subscriber_delivery
```

`foreign_key_check` printing nothing is the pass. The migration list is the
check that matters for restores: it tells you which build can open this file,
which is the question you will be asking under pressure.

## Restore

Stop the process, put both files in the data directory, start it.

```sh
systemctl stop uptime-cairn          # or: docker compose down
install -m 600 backup.db  /var/lib/uptime-cairn/cairn.db
install -m 600 cairn.key  /var/lib/uptime-cairn/cairn.key
rm -f /var/lib/uptime-cairn/cairn.db-wal /var/lib/uptime-cairn/cairn.db-shm
systemctl start uptime-cairn
```

Delete any stale `-wal` and `-shm`: they belong to the database you just
replaced, and SQLite will try to recover the new file with the old file's WAL.

A verified restore of the drill backup returned the monitor through the API with
its stored credential decrypting correctly, and reported setup as already
complete — which together prove the database, the key, and the encryption
envelope all survived:

```console
$ curl -s localhost:3000/api/v1/monitors -H "Authorization: Bearer $KEY" | jq '.data[0]'
{
  "id": "01a022d0-680b-703d-8eab-c5ab83c17a58",
  "name": "Drill target",
  "type": "http",
  "config": {
    "url": "https://example.com/",
    "auth": {"type": "basic", "username": "cairn", "password": "__redacted__"}
  }
}
$ curl -s localhost:3000/api/v1/setup/status
{"setup_required":false}
```

`__redacted__` is the read path refusing to serialise a secret, not a decryption
failure — the credential is there. A key mismatch does not look like this; it
looks like the next section.

### Restoring without the key

The process refuses to start rather than generating a replacement:

```console
$ cairn --data-dir ./restored
cairn: encryption key not found at ./restored/cairn.key, but this database holds
encrypted data. Refusing to start: generating a new key would make every stored
secret permanently unreadable while appearing to work. Restore the key file from
backup, or pass --encryption-key-file
```

This is the intended behaviour and it is worth understanding before you meet it
at 3am. A start that "worked" here would give you a running install whose
monitors all fail authentication for reasons nothing reports, and the damage
would be discovered days later. There is no recovery path other than the key: if
it is gone, the encrypted columns are gone, and the install has to be rebuilt
with credentials re-entered.

If you keep the key somewhere other than the data directory — a secrets manager,
a mounted file — point at it explicitly and the data directory stays
recoverable on its own:

```sh
cairn --data-dir /var/lib/uptime-cairn --encryption-key-file /run/secrets/cairn.key
```

`CAIRN_ENCRYPTION_KEY_FILE` and `CAIRN_ENCRYPTION_KEY` do the same thing from
the environment, in that order of precedence, ahead of the default path.

## Cold backup

If you would rather not depend on `VACUUM INTO`, a clean stop is genuinely
sufficient: shutdown checkpoints and truncates the WAL, leaving one file.

```console
$ ls -la /var/lib/uptime-cairn     # after systemctl stop
-rw-r--r--  626688  cairn.db
-rw-------      45  cairn.key
```

No `-wal`, no `-shm`. Copy those two and you have everything. The cost is the
downtime, and it is the reason the online path exists.

## What is not automated yet

There is no `cairn backup` subcommand and no scheduled backup inside the
product. Both are worth having and neither is built; today this is a cron job
you own. Schedule the `VACUUM INTO` above, keep the retention policy outside the
data directory, and test a restore on a cadence you actually keep to.
