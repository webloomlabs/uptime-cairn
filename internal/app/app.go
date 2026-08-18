// Package app is the composition root: the one place that knows how every other
// package fits together.
//
// The import rule that keeps the seams honest, stated here because this is the
// package that would break it first (see docs/development/repo-layout.md):
//
//   - Only app constructs concrete implementations. Nothing else imports
//     internal/store/sqlite.
//   - internal/probe never imports internal/store or internal/controlplane. It
//     reaches the control plane over gRPC and nothing else, in solo mode as much
//     as in Phase 4 (ADR-001). The day the scheduler reaches around that
//     interface is the day the design stops being true, and it will not announce
//     itself.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/webloomlabs/uptime-cairn/internal/config"
	"github.com/webloomlabs/uptime-cairn/internal/version"
)

// ErrNotImplemented is what every entry point returns until Phase 1 builds it.
// A skeleton that starts and does nothing is worse than one that says so: the
// first is discovered by a user, the second by the developer who wired it.
var ErrNotImplemented = errors.New("Phase 1 has not been built yet — see docs/plans/PHASE-1-PLAN.md")

// Run starts the process and blocks until ctx is cancelled.
//
// The wiring order Phase 1 Month 1 fills in, recorded now because the order is
// itself a decision:
//
//  1. Open the database and run migrations to head, failing hard on a checksum
//     mismatch (data model §8). Migrations run automatically on start.
//  2. Build the stores behind their interfaces (ADR-002).
//  3. Start the control plane's probe-facing gRPC service.
//  4. Start the embedded probe over bufconn, and let it enrol into nothing —
//     solo mode has no credentials (ADR-005 decision 14).
//  5. Start the ingest, state-transition, notification, and rollup workers.
//  6. Serve /api/v1 and the embedded UI on cfg.ListenAddr.
//  7. On ctx cancellation, stop accepting checks, flush buffered results, close
//     the database.
func Run(ctx context.Context, cfg config.Config, out io.Writer) error {
	fmt.Fprintf(out, "%s\n", version.String())
	fmt.Fprintf(out, "mode=%s data-dir=%s listen=%s\n", cfg.Mode, cfg.DataDir, cfg.ListenAddr)
	return ErrNotImplemented
}
