package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/webloomlabs/uptime-cairn/internal/config"
	"github.com/webloomlabs/uptime-cairn/internal/importer/kuma"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/store/sqlite"
)

// ImportKuma runs `cairn import kuma <path>...` against a stopped install.
//
// Against a *stopped* install, and that is worth stating in the command's own
// help rather than only here: SQLite takes one writer, and an import writing
// through a second process while the server holds the write lock would spend
// its life waiting on busy_timeout. The guided flow in the dashboard is what a
// running install uses, and it is the same importer through the same seam.
//
// It shares Run's composition: the same store, the same key hierarchy, the same
// checker registry. An importer with its own idea of any of those would produce
// rows the server then could not read, which is the failure mode a second write
// path always has.
func ImportKuma(ctx context.Context, cfg config.Config, paths []string, opts kuma.Options, out io.Writer) error {
	if len(paths) == 0 {
		return fmt.Errorf("name at least one kuma.db to import")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	store, err := sqlite.Open(ctx, filepath.Join(cfg.DataDir, "cairn.db"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if _, err := store.Migrate(ctx); err != nil {
		return err
	}
	keeper, err := openKeeper(ctx, store, cfg, log)
	if err != nil {
		return err
	}

	registry := check.NewRegistry()
	registry.Register(check.NewHTTP())
	registry.Register(check.NewTCP())
	registry.Register(check.NewICMP())
	registry.Register(check.NewDNS())
	registry.Register(check.NewTLSExpiry())
	registry.Register(check.NewDomainExpiry())
	registry.Register(check.NewDocker())
	registry.Register(check.NewGRPC())

	files := make([]kuma.File, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		files = append(files, kuma.File{Path: path, Name: filepath.Base(path)})
	}

	target := kuma.NewTarget(store, registry, keeper, model.SentinelOrgID)
	job, entries, runErr := kuma.New(target, model.SentinelOrgID, log).Run(ctx, files, opts)

	// The report is written before the error is returned. A run that died
	// halfway still has a report explaining how far it got, and that is more
	// use than the error on its own.
	if !opts.DryRun {
		if err := store.CreateImportJob(ctx, job); err == nil {
			_ = store.AddImportEntries(ctx, entries)
		}
	}

	WriteImportReport(out, job, entries, opts)
	return runErr
}

// WriteImportReport prints the report a migrating user reads.
//
// The order is deliberate: what did not come across, first and in full, then
// the tally, then the count of what did. An import report that leads with "1,204
// monitors imported" and buries thirty unsupported types at the bottom is a
// report that gets skimmed, and skimming it is how somebody discovers during an
// outage that a monitor they thought they had was never created.
func WriteImportReport(out io.Writer, job model.ImportJob, entries []model.ImportEntry, opts kuma.Options) {
	if opts.DryRun {
		fmt.Fprintf(out, "Dry run — nothing was written.\n\n")
	}

	for _, src := range job.Sources {
		fmt.Fprintf(out, "%s", src.Filename)
		if src.KumaVersion != "" {
			fmt.Fprintf(out, " (Uptime Kuma %s)", src.KumaVersion)
		}
		fmt.Fprintln(out)
		for _, entity := range sortedCountKeys(src.DetectedEntities) {
			fmt.Fprintf(out, "    %-14s %d\n", entity, src.DetectedEntities[entity])
		}
	}
	fmt.Fprintln(out)

	var problems []model.ImportEntry
	for _, e := range entries {
		switch e.Result {
		case model.ImportResultImported:
			if e.Detail == "" {
				continue
			}
			problems = append(problems, e)
		default:
			problems = append(problems, e)
		}
	}

	if len(problems) > 0 {
		fmt.Fprintf(out, "Needs your attention (%d):\n\n", len(problems))
		for _, e := range problems {
			name := e.SourceName
			if name == "" {
				name = e.SourceID
			}
			fmt.Fprintf(out, "  [%s] %s %q\n", e.Result, e.EntityType, name)
			for _, line := range wrap(e.Detail, 74) {
				fmt.Fprintf(out, "      %s\n", line)
			}
			fmt.Fprintln(out)
		}
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "entity\timported\trenamed\tskipped\tfailed\tunsupported")
	summary := model.Tally(entries)
	for _, entity := range sortedSummaryKeys(summary) {
		s := summary[entity]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\n",
			entity, s.Imported, s.Renamed, s.Skipped, s.Failed, s.Unsupported)
	}
	_ = tw.Flush()

	fmt.Fprintf(out, "\n%s", job.State)
	if job.Error != "" {
		fmt.Fprintf(out, ": %s", job.Error)
	}
	fmt.Fprintln(out)

	if job.State == model.ImportPartial {
		fmt.Fprintln(out, "\nPartial rather than failed: some entities came across and some did not. "+
			"Everything above is what did not.")
	}
	if !opts.DryRun && !opts.EnableAfterImport {
		fmt.Fprintln(out, "\nImported monitors are paused. Review them, then resume them — "+
			"in bulk from the monitor list, or with PATCH /api/v1/monitors/bulk.")
	}
}

func sortedCountKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSummaryKeys(m map[string]model.ImportSummary) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// wrap breaks a detail onto terminal-width lines. Hand-rolled because it is
// eight lines and the alternative is a dependency for text layout.
func wrap(s string, width int) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var lines []string
	var line strings.Builder
	for _, word := range strings.Fields(s) {
		if line.Len() > 0 && line.Len()+1+len(word) > width {
			lines = append(lines, line.String())
			line.Reset()
		}
		if line.Len() > 0 {
			line.WriteByte(' ')
		}
		line.WriteString(word)
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	return lines
}
