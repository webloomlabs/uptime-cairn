# Frontend

The dashboard, the status pages, and the setup flow. Nothing lives here yet —
the UI is [Phase 1](../docs/plans/PHASE-1-PLAN.md) Month 3 work, and this
directory exists so the build contract is written down before it is built.

**Stack:** SvelteKit + Tailwind + shadcn-svelte, per
[PHASE-1-PLAN.md](../docs/plans/PHASE-1-PLAN.md) §2. It compiles to static
assets; there is no Node process in production.

## The build contract

```
web/            →  static output  →  internal/ui/dist/  →  embedded in the binary
```

`internal/ui/dist/` is generated and gitignored apart from its placeholder, so a
clean checkout still builds the server without ever running the frontend
toolchain.

## The two rules that are not style preferences

**The dashboard is an ordinary API client.** It consumes `/api/v1` and nothing
else — no privileged endpoint, no server-rendered page that reaches into the
database, no field the UI can set that a scoped API key cannot. The incumbent's
API being an afterthought is half of why this project exists.

**The client is never sent full state** ([ADR-004](../docs/adr/004-ui-state-synchronisation.md)).
Server-side pagination, filtering, and search; live updates subscribed to the
monitor IDs currently on screen and no others. Sending every monitor to every
browser on every heartbeat is the exact mechanism behind Uptime Kuma's 300–600
monitor wall, and a UI benchmark in CI holds this side of it honest from Month 3.
