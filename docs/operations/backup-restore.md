# Backup and restore

Everything below has been run against the current build; the numbers and
messages are real output, not illustrations. The one exception is
[Report artifacts](#report-artifacts), which is marked as such: it describes a
directory this release does not yet create.

Two files matter, and they have different failure modes:

| File | What it is | If you lose it |
|---|---|---|
| `cairn.db` | Every monitor, heartbeat, incident, and status page | You are restoring from an older copy |
| `cairn.key` | 45 bytes; the root key wrapping every stored secret | Every credential in the database is permanently unreadable |
| `reports/` | Generated report artifacts, once reporting ships | The reports you sent clients are gone, and they cannot be reproduced — see [Report artifacts](#report-artifacts) |

Back the key up **somewhere the database backup is not**. A backup that puts the
key beside the database it protects has encrypted nothing against the threat
that actually happens, which is somebody walking off with the backup. The
reports directory is under no such constraint and may sit beside the database:
it holds no credentials, and it is a rendering of data already in the file next
to it.

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

## Report artifacts

Generated reports are files under `<data-dir>/reports/<yyyy>/<mm>/`, named by
artifact id, with the database holding the index and not the bytes. **Backing up
`cairn.db` alone therefore leaves you with rows describing files you do not
have.** That install starts, runs, lists every report it ever produced, and
answers `410 Gone` on the download — naming the missing directory, because that
is the one thing you can act on. It is precisely why the omission is not noticed
until somebody asks for a file.

**They cannot be regenerated, and this is the part worth reading before deciding
to skip them.** A report over last March re-run in 2028 reads whatever rollup
tier has survived retention and returns daily figures where it once returned
hourly ones. A corrected incident timeline changes every post-mortem drawn from
it. And the artifact is what was *sent to a client*: if an uptime claim is ever
disputed, evidence that regenerates differently is not evidence.

Copying them is ordinary, because artifacts are immutable once written — there
is no equivalent of the WAL hazard above, and a plain recursive copy of a
running install is sound:

```sh
sqlite3 /data/cairn.db "VACUUM INTO '/backups/cairn-$(date +%F-%H%M).db'"
rsync -a --delete /data/reports/ /backups/reports/
```

**Take the database snapshot first, then the directory** — in that order, for a
reason. The engine writes and fsyncs an artifact file before committing the row
that points at it, so every row in a snapshot taken at the earlier instant
already has its file on disk. Reversed, the window is a row without bytes.
Neither ordering can corrupt anything; one of them can produce a download that
404s.

In Docker, the directory comes out the same way the database does:

```sh
docker cp uptime-cairn:/data/reports ./reports
```

Permissions are `0750` on the directories and `0600` on the files — the same
`0600` the root key is written with, because a report is a client's operational
data and there is no second user on the box who needs to read it. `rsync -a` and `docker cp` both preserve
that; `cp` without `-p` does not.

### If the S3 mirror is enabled

Where `settings.report_storage.mirror_enabled` is on, every artifact is copied
offsite as it is written. That is a real reason to enable it beyond durability —
it covers the half of this procedure most likely to be forgotten.

**It does not let you skip the local `rsync` above, and you should not treat it
as though it does.** An upload that fails is recorded on the artifact row and
does not fail the run, deliberately — a bucket outage must not take reporting
down — and **nothing retries it**. So a mirror that has been quietly failing for
a fortnight looks exactly like one that is working, from everywhere except the
artifact rows. If you intend to rely on the mirror instead of the local copy,
alert on it:

```
GET /api/v1/report-runs?limit=50
```

and check `artifacts[].mirror.state` for anything that is not `uploaded`. Until
you have that, the mirror is a second copy rather than a replacement for the
first.

Two constraints come with it, and neither is optional:

- **The bucket must not be public.** Artifacts contain client names, monitor
  names, and uptime figures, and a share link is supposed to be the only
  unauthenticated way to reach one.
- **`cairn.key` does not go in that bucket.** Artifacts and a database backup may
  share one; the key requires a different trust boundary, for the reason at the
  top of this page. The bucket exists now, which is what makes the mistake
  convenient rather than hypothetical.

The mirror is a durability copy, not a read path: Cairn always serves artifacts
from local disk, so a restore has to put the directory back rather than relying
on the bucket. Remote backup of the database and key is a separate,
not-yet-built thing (roadmap Phase 4); this is not it.

### Retention

Artifacts expire on `settings.retention.report_artifact_days` — 365 by default,
zero meaning keep forever — independently of the rollup tiers, because an
artifact is expected to outlive the data it was computed from. An expired
artifact leaves its row behind as a tombstone, so a bookmarked link reports that
the report existed and is gone rather than that it never existed. **Your backup
retention should be at least as long as that setting**, or you will be pruning
copies of reports the product still lists.

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
6|monitor_probe_pinning
7|import_vocabulary
8|reporting
9|artifact_mirror
```

`foreign_key_check` printing nothing is the pass. The migration list is the
check that matters for restores: it tells you which build can open this file,
which is the question you will be asking under pressure. What you are reading
off it is the **highest number**, not the count.

Where the install holds report artifacts, one more check is worth the seconds it
costs, because it is the one that catches the failure this page exists to
prevent — a database and a reports directory that do not agree:

```sh
sqlite3 backup.db "SELECT path FROM report_artifacts WHERE state = 'rendered';" |
  while read -r p; do [ -f "/backups/reports/$p" ] || echo "MISSING: $p"; done
```

Printing nothing is the pass. Anything it names is a row whose file is not in
the backup, which is a report you will not be able to produce if the figure in
it is ever disputed.

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

Where reports exist, the directory goes back in the same step, before the
process starts:

```sh
rsync -a /backups/reports/ /var/lib/uptime-cairn/reports/
chown -R uptime-cairn:uptime-cairn /var/lib/uptime-cairn/reports
```

Restoring the database without it is not corruption and does not fail: the
install runs, the report history lists, and only the downloads are missing. It
is the quietest half of a half-done restore, which is the argument for checking
it deliberately rather than waiting to be told.

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

Now that reporting has shipped, that cron job is **two** commands rather than
one, and nothing in the product checks that you run the second. That is the
known cost of holding artifacts as files rather than as blobs in the database,
recorded in [ADR-008](../adr/008-report-artifact-storage.md) rather than
discovered later — along with the honest statement that documentation, the S3
mirror, and the consistency check above all reduce the risk and none of them
makes it zero.

**The S3 mirror does not close it either.** A failed upload is recorded on the
artifact and never retried, so a mirror that has been quietly failing looks
exactly like one that is working from everywhere except the artifact rows. It is
a second copy, not a substitute for the first.
