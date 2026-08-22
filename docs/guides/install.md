# Installing Uptime Cairn

Four ways in, in the order most people want them. All four produce the same
thing: one process, one database file, and a dashboard on port 3000.

There is no database server to install, no Redis, no message broker, and no
separate web server. If you are looking for those, they are not missing — they
were never required.

> **Before you start.** Two files on disk cannot be recreated: `cairn.db` and
> `cairn.key`. The key is 45 bytes and without it every stored credential in the
> database is unreadable. Read
> [operations/backup-restore.md](../operations/backup-restore.md) before you
> write a backup script, and especially before you copy `cairn.db` on its own.

---

## Docker

```sh
docker run -d \
  --name uptime-cairn \
  --restart unless-stopped \
  -p 127.0.0.1:3000:3000 \
  -v uptime-cairn:/data \
  ghcr.io/webloomlabs/uptime-cairn:latest
```

Open <http://localhost:3000> and create the administrator account.

Three things about that command are deliberate.

**`127.0.0.1:3000`, not `0.0.0.0:3000`.** The binary speaks plain HTTP and has
no TLS flags, by design — TLS is a reverse proxy's job and always will be.
Publishing the port on every interface puts session cookies and API keys on the
wire in the clear. Put [Caddy, nginx, or Traefik](../operations/reverse-proxy.md)
in front of it before anything reaches it from outside the host.

**A named volume, not a bind mount to nowhere.** `/data` holds the database and
the encryption key. A container started without a volume runs perfectly and
loses everything the first time it is replaced.

**No `--base-url`.** You will want one — it is what turns an alert into
something clickable, and it is what a status page subscription link is built
from — but it is set in Settings rather than on the command line, so that
changing it does not mean recreating the container. Set it before you create
your first notification channel.

### ICMP monitors need one more thing

Ping is the only monitor type that needs a capability the container does not get
by default. Uptime Cairn tries the unprivileged ICMP datagram socket first, so
the fix is a sysctl rather than `CAP_NET_RAW`:

```sh
docker run -d \
  --sysctl net.ipv4.ping_group_range="10001 10001" \
  ...
```

The image runs as UID/GID 10001, and that range has to reach it.

Without it, an ICMP monitor reports **`unknown`** rather than `down` — the
distinction is deliberate and it is worth knowing before you see it. `unknown`
means "this probe could not perform the check", which is a statement about the
probe. Reporting `down` would be a statement about the target, and it would be
false: the host is fine, the container simply cannot open a socket. A monitoring
tool that blames the target for its own missing permission is a monitoring tool
that pages the wrong person.

If the sysctl is unavailable in your environment — some managed platforms
disallow it — set `fallback_to_tcp` on the monitor and it will check a TCP port
instead. That is a different check and the monitor says so.

---

## Docker Compose

The reference file is [`docker-compose.yml`](../../docker-compose.yml) in the
repository root. It is the Docker command above with the sysctl and the base URL
already in it:

```sh
curl -O https://raw.githubusercontent.com/webloomlabs/uptime-cairn/main/docker-compose.yml
# edit --base-url, then:
docker compose up -d
```

To build from source instead of pulling an image — which is what a
platform-as-a-service deploying from your repository does —
[`docker-compose.dev.yml`](../../docker-compose.dev.yml) builds the Dockerfile
in place.

### Upgrading

```sh
docker compose pull && docker compose up -d
```

Migrations run automatically on start and are logged. There is no separate
migration step and no maintenance mode. Read
[operations/upgrading.md](../operations/upgrading.md) for what happens if one
fails, and for the one thing to know about rolling back (there are no down
migrations; restore from backup is the path).

---

## Binary

Every release publishes static binaries with checksums for five targets:

| Target | For |
|---|---|
| `linux/amd64` | ordinary servers |
| `linux/arm64` | Graviton, Ampere, Raspberry Pi 4/5 on a 64-bit OS |
| `linux/armv7` | Raspberry Pi 2/3, and Pi 4s still running 32-bit Raspberry Pi OS |
| `darwin/arm64` | Apple silicon, for development |
| `darwin/amd64` | Intel Macs, for development |

```sh
VERSION=v1.0.1
TARGET=linux_amd64        # or linux_arm64, linux_armv7, darwin_arm64, darwin_amd64
BASE=https://github.com/webloomlabs/uptime-cairn/releases/download/$VERSION

curl -LO $BASE/cairn_${VERSION}_${TARGET}.tar.gz
curl -LO $BASE/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS

tar xzf cairn_${VERSION}_${TARGET}.tar.gz
sudo install -m 0755 cairn_${VERSION}_${TARGET}/cairn /usr/local/bin/cairn
```

The archive unpacks into a directory of its own name carrying the binary,
`README.md`, `LICENSE`, and `SECURITY.md` — hence the path in the last line. The
names come from the release workflow rather than from convention, so if a target
above ever disagrees with what is attached to a release, the release is right.

Verify the checksum. It is two commands and it is the difference between running
what we published and running whatever a mirror handed you.

Then run it:

```sh
sudo mkdir -p /var/lib/uptime-cairn
sudo cairn --data-dir /var/lib/uptime-cairn --listen 127.0.0.1:3000
```

### As a service

[`deploy/systemd/uptime-cairn.service`](../../deploy/systemd/uptime-cairn.service)
is a hardened unit — `ProtectSystem=strict` with `ReadWritePaths` naming only
the data directory, which `systemd-analyze security` will grade for you.

```sh
sudo useradd --system --home /var/lib/uptime-cairn --shell /usr/sbin/nologin cairn
sudo mkdir -p /var/lib/uptime-cairn && sudo chown cairn: /var/lib/uptime-cairn

sudo cp deploy/systemd/uptime-cairn.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now uptime-cairn
sudo journalctl -u uptime-cairn -f
```

For ICMP monitors on a binary install, grant the capability to the binary rather
than to the whole service:

```sh
sudo setcap cap_net_raw+ep /usr/local/bin/cairn
```

Note that `setcap` is lost when the binary is replaced, so it has to be reapplied
on every upgrade. The unit file has the alternative in a comment —
`AmbientCapabilities=CAP_NET_RAW` — which survives upgrades and grants slightly
more.

---

## Raspberry Pi

A Pi is a first-class target rather than a curiosity: it is where a great many
self-hosted monitoring installs actually live, and the binary is pure Go with no
cgo, so there is nothing to compile and nothing to link.

**Which binary.** `uname -m` decides:

| `uname -m` | Download |
|---|---|
| `aarch64` | `linux-arm64` |
| `armv7l` | `linux-armv7` |
| `armv6l` | Not published. A Pi Zero or original Pi is below what this can usefully run. |

A Pi 4 or 5 running 32-bit Raspberry Pi OS reports `armv7l` even though the
hardware is 64-bit. Take it at its word — the 32-bit build runs, and the 64-bit
one will not start.

**Put the database on something other than the SD card if you can.** Not for
speed. An SD card under a sustained small-write load has a finite and
uncomfortably short life, and this is a sustained small-write load by
definition. A USB SSD, or an NFS mount with the WAL on local disk, both work:

```sh
sudo cairn --data-dir /mnt/ssd/uptime-cairn
```

**Turn retention down.** The default keeps seven days of raw heartbeats, which
at 50 monitors on a 60-second interval is about 600,000 rows. That is fine. At
500 monitors on 20 seconds it is 15 million, and a Pi's card will notice. Settings
→ Retention; the rollup tiers above raw are what every history range beyond a
week is made of, so shortening raw costs you nothing except per-check detail
older than the window.

**Expect ICMP to need the sysctl.** Add to `/etc/sysctl.d/99-cairn.conf`:

```
net.ipv4.ping_group_range = 0 2147483647
```

---

## After it starts

Whichever route you took, the first three things worth doing:

1. **Create the administrator account.** Open the dashboard; it will ask. The
   password is hashed with argon2id and there is no recovery path other than the
   database, so use a password manager.
2. **Set the base URL** in Settings → General. Alert links, push URLs, and
   subscriber links are all built from it, and every one of them is wrong in a
   way nobody notices until it is clicked from outside the network.
3. **Back up the key.** `cairn.key` sits next to `cairn.db` by default, which is
   convenient and is also both eggs in one basket. Copy it somewhere else now,
   while there is nothing to lose.

Then: [First monitor in sixty seconds](quickstart.md).

## Coming from Uptime Kuma?

Do not rebuild your monitors by hand. `cairn import kuma` reads a `kuma.db` and
reproduces the monitors, tags, notification channels, and status pages — several
Kuma instances at once, if you have been sharding by hand.

See [migrating-from-uptime-kuma.md](migrating-from-uptime-kuma.md).
