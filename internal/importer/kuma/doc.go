// Package kuma imports an Uptime Kuma installation.
//
// The migration path is a P0 deliverable and the reason a Kuma user can leave
// without hand-rebuilding 300 monitors: cairn import kuma, as a CLI and a guided
// UI flow, including merging several Kuma instances into one install
// (PHASE-1-PLAN.md §3.7).
//
// The field-by-field schema mapping is written down in data model §10, along
// with three gaps that have no home yet — Kuma's ~40 monitor types against our
// nine, monitor_tag.value, and per-monitor proxies. An import that silently
// drops any of them is worse than one that refuses: the import report names
// everything it could not carry, and the user decides.
//
// Two rules this package does not get to bend:
//
//   - It writes through the repository layer, never around it, so credentials
//     read as plaintext out of kuma.db are encrypted on the way in (data model
//     §12.6).
//   - An uploaded kuma.db is a file full of someone's URLs and credentials. It
//     is processed and deleted, never retained, never logged.
package kuma
