// Package api serves /api/v1 and the embedded UI.
//
// API-first, literally: the dashboard consumes this and only this. There are no
// privileged internal endpoints, no "the UI can do it because it is the UI"
// shortcuts, and no field the browser can set that a scoped API key cannot
// (PHASE-1-PLAN.md §2). Two abandoned community wrappers around the incumbent's
// API are the reason that rule exists.
//
// The contract is docs/api/openapi.yaml, frozen before this package is written,
// and contract tests in CI verify the server against it. When they disagree, the
// spec is right — a deviation is a spec change with a deprecation window, not a
// handler edit.
//
// Invariants this package must hold, all of them from ADR-004 and all of them
// load-tested every release:
//
//   - Every list is paginated on an opaque cursor keyed on (updated_at, id).
//     There is no small-install exception where the full set is sent because it
//     happens to fit today.
//   - Filtering, sorting, and search are server-side. The client is never sent
//     full state.
//   - Live updates are scoped to the monitor IDs on screen, never broadcast.
//     This is the exact mechanism behind the incumbent's 300–600 monitor wall.
package api
