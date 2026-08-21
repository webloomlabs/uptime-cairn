# Guides

Getting Uptime Cairn running and using it. For running it somewhere that
matters — backups, upgrades, reverse proxies, what to alert on — see
[operations/](../operations/).

| | |
|---|---|
| **[install.md](install.md)** | Docker, Compose, binary, and Raspberry Pi. Start here. |
| **[quickstart.md](quickstart.md)** | Account, first monitor, first alert, and how to prove it fires. Sixty seconds. |
| **[monitor-types.md](monitor-types.md)** | The nine types, what each actually checks, and the fields where the obvious reading is wrong. |
| **[alerting.md](alerting.md)** | Every channel field by field, webhook templating, and maintenance windows. |
| **[migrating-from-uptime-kuma.md](migrating-from-uptime-kuma.md)** | `cairn import kuma`, the multi-instance merge, and exactly what does not come across. |

## The API

The contract is [openapi.yaml](../api/openapi.yaml). Two pages sit beside it:

- **[api/README.md](../api/README.md)** — the conventions the whole surface
  follows: authentication, scopes, pagination, error shape, `include=`.
- **[api/reference.md](../api/reference.md)** — every operation, its scope, and
  its phase. Generated from the spec, so it cannot drift from it.

The dashboard is an ordinary client of that API. There is no privileged endpoint
it uses and you cannot, and no field it can set that a scoped key cannot.
