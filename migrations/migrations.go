// Package migrations embeds the versioned SQL migrations into the binary.
//
// Embedded rather than read from disk so that a single static binary can
// migrate its own database with nothing else installed — the same reason the UI
// is embedded. The files stay on disk as ordinary SQL because they are read by
// people far more often than by the runner.
//
// The numbering is shared across backends: 0007 means the same logical change in
// both sqlite/ and postgres/, so drift between them is visible in a directory
// listing (data model §8).
package migrations

import "embed"

// SQLite holds migrations/sqlite/*.sql.
//
// Postgres has no directory yet; ADR-002 defers the scaled path to Phase 4, and
// an empty embed pattern is a compile error rather than an empty filesystem, so
// its var appears when its first migration does.
//
//go:embed sqlite/*.sql
var SQLite embed.FS
