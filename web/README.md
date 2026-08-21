# Frontend

The dashboard, the public status pages, and the setup flow.

**Stack:** SvelteKit 2 + Svelte 5 (runes) + Tailwind 4, per
[PHASE-1-PLAN.md](../docs/plans/PHASE-1-PLAN.md) §2. It compiles to static
assets; there is no Node process in production.

## Building it

```sh
cd web
npm install
npm run build      # writes ../internal/ui/dist
cd ..
go build ./cmd/cairn
```

The order matters: `internal/ui` embeds `dist/` at compile time, so the Go build
picks up whatever the last frontend build left there.

For development, run the binary and the Vite dev server side by side:

```sh
go run ./cmd/cairn -data-dir /tmp/cairn-data
cd web && npm run dev          # http://localhost:5173
```

`npm run dev` proxies `/api` to `http://localhost:3000`, which is `cairn`'s own
default listen address, so neither side needs a flag. They are the same
origin in development exactly as they are in production — which is what keeps
cookie authentication and CSRF behaving the same in both. Point it elsewhere with
`CAIRN_URL=http://host:port npm run dev`.

Other commands: `npm run check` (svelte-check, and it is expected to report
zero), `npm run format`, `npm run lint`.

## The build contract

```
web/            →  static output  →  internal/ui/dist/  →  embedded in the binary
```

`internal/ui/dist/` is generated in full and gitignored apart from `.gitkeep`,
which exists only so `//go:embed` has a non-empty directory to find — embedding a
missing or empty directory is a compile error. A clean checkout therefore builds
the server without ever running the frontend toolchain; the binary then serves a
page saying the dashboard is not in this build, and the API is unaffected.

That placeholder is committed in two places on purpose. `web/static/.gitkeep` is
copied into the build output verbatim by SvelteKit, so `npm run build` reproduces
the committed `internal/ui/dist/.gitkeep` byte for byte rather than deleting it —
which is what keeps `git status` clean after a build. Keep the two identical.

Two things about that embed are easy to get wrong and are held by tests:

- **The pattern must be `all:dist`.** `//go:embed` skips any path beginning with
  `_` or `.`, and SvelteKit puts every hashed asset under `_app/`. A bare pattern
  compiles, starts, and serves an `index.html` referencing a bundle that is not in
  the binary — blank in the browser, silent in the log.
  `internal/ui/embed_test.go` asserts that everything `index.html` references is
  actually embedded.
- **The server needs an SPA fallback.** Every route is resolved in the browser, so
  an unknown document path has to return `index.html`. A missing _asset_ still
  returns 404, because serving HTML where a script was requested turns a broken
  build into a MIME error three layers from the cause. `internal/api/ui_test.go`
  covers both directions.

## The two rules that are not style preferences

**The dashboard is an ordinary API client.** It consumes `/api/v1` and nothing
else — no privileged endpoint, no server-rendered page that reaches into the
database, no field the UI can set that a scoped API key cannot. The incumbent's
API being an afterthought is half of why this project exists. In practice this is
enforced by `src/lib/api.ts` being the only file in the frontend that calls
`fetch`.

**The client is never sent full state**
([ADR-004](../docs/adr/004-ui-state-synchronisation.md)). Server-side pagination,
filtering, and search; live updates subscribed to the monitor IDs currently on
screen and no others. Sending every monitor to every browser on every heartbeat is
the exact mechanism behind Uptime Kuma's 300–600 monitor wall.

`src/lib/monitorlist.svelte.ts` is where that lives, and it implements the half of
ADR-004 that the server currently supports:

- Every load is a cursor-paginated, server-filtered query. There is no
  "small install" shortcut that fetches everything when the count happens to be
  low today — the shortcut is the bug, because nothing tells you the day it stops
  being small.
- `/monitors/membership` is polled every 5 seconds for a version and a count
  scoped to the active filter. A change raises a _stale_ banner rather than
  silently reordering rows under the pointer.
- Refreshing re-reads the rows currently held, never the collection. That is
  ADR-004's second load-test invariant — client cost bounded by viewport, not by
  monitor count — and it is the one a frontend can break on its own.

**Not yet built: the real-time scoped diffs.** ADR-004 also specifies push over
NATS (or an in-process bus in solo mode) for the monitors on screen. This build
has no browser-facing channel for that, so reconciliation is the whole mechanism
and the 5-second interval is what bounds staleness. The controller is written
against "subscribe to visible IDs, reconcile on interval" as its model, which the
ADR notes is the thing that has to be true from the start for the upgrade to be
additive rather than a rewrite.

## Dependencies

69 packages, all of them build tooling: SvelteKit, Vite, Tailwind, TypeScript,
svelte-check, Prettier. **Nothing ships in the bundle except Svelte itself.**

The plan names shadcn/ui. Its Svelte port is not used, and that is a deliberate
deviation worth a reviewer's attention: `shadcn-svelte` copies components into the
repository — which is a model this project agrees with — but the components it
copies depend on `bits-ui`, plus `tailwind-variants`, `clsx`, `tailwind-merge`,
and an icon package. That is a runtime dependency tree for a UI whose entire
component surface is a button, a badge, a field wrapper, a spinner, and two
charts. Those are hand-written in `src/lib/components/`, in the spirit of
CONTRIBUTING.md's "not a package when a hundred lines of our own code will do".

If richer primitives are later needed — a real combobox, a focus-managed dialog,
a virtualised list — that is the moment to take the dependency, with the
justification the policy asks for.

## Internationalisation

English is complete and is the source catalogue. The runtime is
`src/lib/i18n/index.svelte.ts`, ninety lines, and the point of it is that
adding a language never touches a component.

**Adding a language:**

1. Copy `src/lib/i18n/en.json` to `src/lib/i18n/<tag>.json`, where `<tag>` is a
   BCP 47 language subtag (`de`, `pt-br`).
2. Translate the values. Leave the keys alone — they are identifiers, not text.
3. Add one line to `LOCALES` in `src/lib/i18n/index.svelte.ts`:
   ```ts
   de: () => import('./de.json');
   ```
   Everything but English is loaded on demand, so a language nobody selects costs
   nothing.

**The three rules the catalogue follows**, each of which is a mistake somebody has
to make once:

- **Keys are dotted identifiers, never the English text.** A catalogue keyed by
  its own source string breaks every translation the moment somebody fixes a typo
  in English.
- **A missing key renders as the key**, visibly, rather than falling back to
  English. A half-translated UI that looks finished is how a language ships with a
  third of its strings missing for two years.
- **Interpolation is `{name}`**, resolved against an object.
- **Plurals go through `Intl.PluralRules`.** A key with a `count` value gets its
  category appended — `overview.usingMonitors.one`,
  `overview.usingMonitors.other` — and the browser decides which category a number
  falls into for the active locale. English needs two forms and Polish needs four;
  writing the English rule inline as `n === 1` is how a language ships permanently
  broken. A key with no plural variants is used as-is, so most strings pay nothing
  for this.

Locale is negotiated from `navigator.languages` by language subtag, so a browser
asking for `en-AU` gets `en` rather than nothing.

There is no translation platform wired up. When there is one, it consumes
`src/lib/i18n/*.json` directly — flat JSON with stable keys is what every one of
them expects.

## A Tailwind 4 trap worth knowing about

`@theme` variables are emitted **only where Tailwind can see a utility class using
them**. Every status colour in this UI is composed at runtime —
`var(--color-{tone})`, where `tone` is a status word — which a source scanner
cannot see. Declaring them in `@theme` silently dropped four of the six from the
built stylesheet, and the symptom is a status marker that is present in the DOM,
correctly sized, and completely transparent.

They are declared on `:root` in `app.css` instead. The rule: a custom property
referenced only through a runtime-built `var()` belongs on `:root`, not in
`@theme`.

## Accessibility and theming

Dark mode is a class on `<html>` with three states — light, dark, and follow the
system — applied before first paint by an inline script in `app.html`, because
reading the preference from a module leaves one frame of the wrong theme on every
load. The palette is defined once as semantic tokens in `app.css`; status colours
carry a separate soft variant for use behind text, because the same green that
reads well as a 12px dot fails contrast as body text.

Status is never communicated by colour alone. Every badge carries its label, and
the dot-only variant carries it in `title` and to assistive technology.

## Layout

```
src/
  app.html               shell; the pre-paint theme script lives here
  app.css                semantic colour tokens, light and dark
  lib/
    api.ts               the only fetch in the frontend: CSRF, RFC 9457 errors
    types.ts             wire shapes, mirroring internal/api/dto.go
    session.svelte.ts    who is signed in, and what this build can do
    theme.svelte.ts      dark mode
    format.ts            durations, relative time, uptime — null is never zero
    monitorlist.svelte.ts   ADR-004 pagination and reconciliation
    monitortypes.ts      per-type monitor config fields
    channeltypes.ts      per-type notification channel config fields
    i18n/                runtime and catalogues
    components/          buttons, fields, badges, charts
  routes/
    (app)/               the authenticated dashboard
    login/  setup/       unauthenticated entry
    status/[slug]/       the public status page
    subscriptions/       the two links subscriber mail carries
```
