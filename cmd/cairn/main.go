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
	"strings"
	"syscall"

	"github.com/webloomlabs/uptime-cairn/internal/app"
	"github.com/webloomlabs/uptime-cairn/internal/config"
	"github.com/webloomlabs/uptime-cairn/internal/importer/kuma"
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
	// Subcommands are matched before the flag set, because `cairn import kuma
	// a.db b.db` has positional arguments and the flag package stops at the
	// first one. There is exactly one subcommand and it is a verb, so this stays
	// a switch rather than becoming a command framework.
	if len(args) > 0 && args[0] == "import" {
		return runImport(args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("cairn", flag.ContinueOnError)
	fs.SetOutput(stderr)

	cfg := config.Default()
	fs.StringVar(&cfg.Mode, "mode", cfg.Mode, "solo (control plane + embedded probe) or probe (agent only)")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "directory holding the database and any credential file")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "address for the HTTP API and UI")
	fs.StringVar(&cfg.EncryptionKeyFile, "encryption-key-file", cfg.EncryptionKeyFile,
		"root key for encryption at rest: 32 bytes, raw or base64 (default: generated into the data dir)")
	fs.StringVar(&cfg.InstanceName, "instance-name", cfg.InstanceName,
		"name shown in authenticator apps and on status pages")
	fs.StringVar(&cfg.BaseURL, "base-url", cfg.BaseURL,
		"public URL of this install, used in alert links (default: none, and alerts carry no link)")
	var trustedProxies string
	fs.StringVar(&trustedProxies, "trusted-proxy", "",
		"comma-separated IPs or CIDRs allowed to set X-Forwarded-For; repeat or list to name several (default: none, and the header is never believed)")
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
	for _, value := range strings.Split(trustedProxies, ",") {
		if value = strings.TrimSpace(value); value != "" {
			cfg.TrustedProxies = append(cfg.TrustedProxies, value)
		}
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

// runImport is `cairn import kuma <path>...`.
//
// It runs against a stopped install, which the help text says because the
// alternative is somebody discovering it: SQLite takes one writer, and an
// import writing while the server holds the lock spends its life waiting on
// busy_timeout. A running install imports through the dashboard, which is the
// same importer through the same seam.
func runImport(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "kuma" {
		fmt.Fprintln(stderr, "usage: cairn import kuma [flags] <kuma.db>...")
		fmt.Fprintln(stderr, "\nUptime Kuma is the only source this build imports from.")
		return errors.New("unknown import source")
	}

	fs := flag.NewFlagSet("cairn import kuma", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: cairn import kuma [flags] <kuma.db>...")
		fmt.Fprintln(stderr, "\nReproduces monitors, tags, notification channels, and status pages from one or")
		fmt.Fprintln(stderr, "more Uptime Kuma databases. Naming several performs the multi-instance merge.")
		fmt.Fprintln(stderr, "\nStop cairn before running this: SQLite takes one writer, and an import against")
		fmt.Fprintln(stderr, "a running install waits on the lock. A running install imports from the")
		fmt.Fprintln(stderr, "dashboard instead, through the same importer.")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}

	cfg := config.Default()
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "directory holding the database this imports into")
	fs.StringVar(&cfg.EncryptionKeyFile, "encryption-key-file", cfg.EncryptionKeyFile,
		"root key for encryption at rest (default: the one in the data dir)")

	opts := kuma.DefaultOptions()
	fs.BoolVar(&opts.DryRun, "dry-run", false, "produce the full report without writing anything")
	fs.StringVar(&opts.ConflictStrategy, "on-conflict", opts.ConflictStrategy,
		"what to do when a name collides: skip, rename, or replace (rename is the only one that cannot lose data)")
	fs.StringVar(&opts.NamePrefix, "name-prefix", "",
		"prefix for imported monitor names, to keep several merged instances distinguishable")
	fs.BoolVar(&opts.ImportMonitors, "monitors", opts.ImportMonitors, "import monitors and groups")
	fs.BoolVar(&opts.ImportTags, "tags", opts.ImportTags, "import tags")
	fs.BoolVar(&opts.ImportNotifications, "notifications", opts.ImportNotifications, "import notification channels")
	fs.BoolVar(&opts.ImportStatusPages, "status-pages", opts.ImportStatusPages, "import status pages")
	fs.BoolVar(&opts.ImportHistory, "history", opts.ImportHistory,
		"import historical heartbeats as well as configuration; slower, and much larger")
	fs.BoolVar(&opts.EnableAfterImport, "resume", opts.EnableAfterImport,
		"start checking immediately, rather than importing paused for review")

	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("name at least one kuma.db to import")
	}
	switch opts.ConflictStrategy {
	case "skip", "rename", "replace":
	default:
		return fmt.Errorf("--on-conflict %q: want skip, rename, or replace", opts.ConflictStrategy)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return app.ImportKuma(ctx, cfg, fs.Args(), opts, stdout)
}
