# Operations

Running Uptime Cairn somewhere that matters. For running the development build,
see [development/running.md](../development/running.md).

- **[backup-restore.md](backup-restore.md)** — the two files that matter, the
  online backup path, and a verified restore. Read the part about not copying
  `cairn.db` on its own before you write a backup script.
- **[reverse-proxy.md](reverse-proxy.md)** — Caddy, nginx, and Traefik, TLS
  termination, and custom-domain status pages. Contains one thing that is not
  optional: `/metrics` must be denied at the proxy.
- **[upgrading.md](upgrading.md)** — how an upgrade runs, why there is no
  rollback, and what an older binary does against a newer database.
- **[self-monitoring.md](self-monitoring.md)** — the health endpoints, the five
  metrics worth alerting on, and what they cost your Prometheus.

## Deployment artefacts

| File | What it is |
|---|---|
| [`Dockerfile`](../../Dockerfile) | Multi-arch image; frontend, then binary, then a small runtime |
| [`docker-compose.yml`](../../docker-compose.yml) | Reference solo install: one container, one volume |
| [`deploy/systemd/uptime-cairn.service`](../../deploy/systemd/uptime-cairn.service) | Unit for a binary install, hardened |

## The shape of a solo install

One process. It is the control plane, the probe, the API, the dashboard, and
SQLite, and there is no second service to run — no Redis, no worker, no separate
web server. Two things on disk cannot be recreated:

```
/data/cairn.db     everything the install knows
/data/cairn.key    45 bytes; without it, every stored secret is unreadable
```

Back them up to different places, and read
[backup-restore.md](backup-restore.md) for why the second sentence is literal.

## Configuration

Flags, not environment variables. There are six, every one has a working
default, and a solo install needs none of them — progressive disclosure applies
to operators too.

| Flag | Default | Notes |
|---|---|---|
| `--mode` | `solo` | `probe` is Phase 4 and is rejected today |
| `--data-dir` | `/data` | The database and, by default, the key |
| `--listen` | `:3000` | HTTP API and dashboard on the same port |
| `--base-url` | *(empty)* | The one worth setting: what alert links point at |
| `--instance-name` | `Uptime Cairn` | Shown in authenticator apps and on status pages |
| `--encryption-key-file` | *(data dir)* | Also `CAIRN_ENCRYPTION_KEY_FILE`, `CAIRN_ENCRYPTION_KEY` |

`--base-url` is empty by default and empty in the alert envelope when unset,
because guessing it from the listen address would put `http://0.0.0.0:3000` in
somebody's pager.

The two encryption variables are the only environment variables the process
reads. Everything else is a flag, which matters when a platform's UI offers you
an environment panel and no way to set arguments: setting `BASE_URL` there does
nothing.
