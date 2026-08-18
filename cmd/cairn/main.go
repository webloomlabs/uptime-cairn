// Command cairn is the Uptime Cairn binary.
//
// One artefact, two modes. In solo mode it is the control plane, the probe, the
// UI, and SQLite in a single process; in probe mode (Phase 4) the same binary
// runs nothing but the agent. There is no separate cairn-probe build, no
// edition, and no feature behind a licence check — see README.md and
// PHASE-1-PLAN.md §2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/webloomlabs/uptime-cairn/internal/app"
	"github.com/webloomlabs/uptime-cairn/internal/config"
	"github.com/webloomlabs/uptime-cairn/internal/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "cairn: %v\n", err)
		os.Exit(1)
	}
}

// run holds everything main does, so a test can exercise it without exiting the
// process. main itself stays three lines on purpose.
func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("cairn", flag.ContinueOnError)
	fs.SetOutput(stderr)

	cfg := config.Default()
	fs.StringVar(&cfg.Mode, "mode", cfg.Mode, "solo (control plane + embedded probe) or probe (agent only)")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "directory holding the database and any credential file")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "address for the HTTP API and UI")
	fs.BoolVar(&cfg.InsecureNoAuth, "insecure-no-auth", cfg.InsecureNoAuth,
		"serve the API with no authentication (this build has none yet); do not expose the port")
	showVersion := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		// -h is a request, not a failure. Anything else has already been
		// reported to stderr by the flag package.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Fprintln(stdout, version.String())
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// SIGINT and SIGTERM cancel the context rather than killing the process, so
	// the scheduler can finish its in-flight checks and flush what it holds.
	// "Never lose a heartbeat" (PHASE-1-PLAN.md §4.4) starts with shutting down
	// properly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return app.Run(ctx, cfg, stdout)
}
