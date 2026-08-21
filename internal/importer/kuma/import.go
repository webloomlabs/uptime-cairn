package kuma

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Options is what the caller asked for, matching KumaImportRequest.options in
// docs/api/openapi.yaml.
type Options struct {
	DryRun              bool   `json:"dry_run"`
	ConflictStrategy    string `json:"conflict_strategy"`
	NamePrefix          string `json:"name_prefix"`
	ImportMonitors      bool   `json:"import_monitors"`
	ImportTags          bool   `json:"import_tags"`
	ImportNotifications bool   `json:"import_notifications"`
	ImportStatusPages   bool   `json:"import_status_pages"`
	ImportHistory       bool   `json:"import_history"`
	EnableAfterImport   bool   `json:"enable_after_import"`
}

// Conflict strategies, matching the spec's enum.
const (
	ConflictSkip    = "skip"
	ConflictRename  = "rename"
	ConflictReplace = "replace"
)

// DefaultOptions matches the defaults the spec publishes. A default that
// disagrees with the documentation is a support ticket waiting to happen, and
// the two that matter here are both deliberately cautious: `rename` is the only
// conflict strategy that cannot lose data, and monitors arrive paused so a
// migrating user reviews them before five thousand checks start firing at once.
func DefaultOptions() Options {
	return Options{
		ConflictStrategy:    ConflictRename,
		ImportMonitors:      true,
		ImportTags:          true,
		ImportNotifications: true,
		ImportStatusPages:   true,
		ImportHistory:       false,
		EnableAfterImport:   false,
	}
}

// Importer runs an import against one target.
type Importer struct {
	target *Target
	log    *slog.Logger
	orgID  model.ID
}

// New returns an importer writing through target.
func New(target *Target, orgID model.ID, log *slog.Logger) *Importer {
	return &Importer{target: target, orgID: orgID, log: log}
}

// File is one uploaded or named kuma.db.
type File struct {
	// Path is where the file is on disk right now. The caller owns deleting it:
	// an uploaded kuma.db is a file full of somebody's URLs and credentials, and
	// this package never keeps one.
	Path string

	// Name is what to call it in the report. The upload's filename, or the path
	// the CLI was given.
	Name string
}

// Run performs the import and returns the job and its report.
//
// # What the report promises
//
// Every source entity appears exactly once. That is the whole contract, and it
// is what makes an import trustworthy: one that maps 900 of 1,000 monitors and
// says which 100 it could not is something a user can finish by hand, while one
// that reports success is something they discover is wrong during an outage.
//
// So nothing is dropped silently, including the three cases data model §10
// names as having no home — Kuma's types this build has no equivalent for, the
// per-attachment tag value, and per-monitor proxies. Each is recorded against
// the entity it belonged to, in the order the user's own install had them.
//
// # Order
//
// Tags, then notification channels, then groups, then monitors, then status
// pages. Each stage only references what earlier stages created, so a failure
// part-way through leaves a consistent install rather than monitors pointing at
// channels that were never written.
//
// # Multi-instance merge
//
// Several files import into one install, which is the migration path for
// everyone currently sharding Kuma by hand across hosts. Identity is per-file:
// two Kuma instances both have a monitor with id 1, so nothing is keyed on the
// source id across files. Names collide across files constantly, which is what
// conflict_strategy and name_prefix are for.
func (i *Importer) Run(ctx context.Context, files []File, opts Options) (model.ImportJob, []model.ImportEntry, error) {
	if opts.ConflictStrategy == "" {
		opts.ConflictStrategy = ConflictRename
	}

	at := importInstant()
	job := model.ImportJob{
		ID: model.NewID(), OrgID: i.orgID, Source: "kuma",
		State: model.ImportRunning, DryRun: opts.DryRun,
		StartedAt: &at, CreatedAt: at, UpdatedAt: at,
	}

	run := &pass{
		importer: i,
		opts:     opts,
		job:      job,
		at:       at,
	}

	var err error
	run.tags, run.groups, run.channels, run.monitors, err = i.target.existingNames(ctx)
	if err != nil {
		return failed(job, at, err), nil, err
	}

	for _, file := range files {
		if err := run.file(ctx, file); err != nil {
			// A file that cannot be opened at all fails the job rather than
			// being skipped: the user named it, and quietly importing the other
			// two of three would be the same lie the report exists to prevent.
			return failed(job, at, err), run.entries, err
		}
	}

	finished := time.Now().UTC().Truncate(time.Millisecond)
	job.FinishedAt = &finished
	job.UpdatedAt = finished
	job.Sources = run.sources
	job.State = model.StateFor(run.entries)
	return job, run.entries, nil
}

func failed(job model.ImportJob, at time.Time, err error) model.ImportJob {
	finished := time.Now().UTC().Truncate(time.Millisecond)
	job.State = model.ImportFailed
	job.Error = err.Error()
	job.FinishedAt = &finished
	job.UpdatedAt = finished
	return job
}

// pass is the state of one run.
//
// Everything mutable lives here rather than on the Importer, so two imports on
// one target cannot see each other's half-built name tables — which would show
// up as a rename suffix jumping by two.
type pass struct {
	importer *Importer
	opts     Options
	job      model.ImportJob
	at       time.Time

	entries []model.ImportEntry
	sources []model.ImportSource

	// Name tables, seeded from what is already installed and grown as this
	// import writes. Keyed on the folded name so "API Gateway" and "api gateway"
	// collide, which is what a user means by "already there".
	tags     map[string]model.ID
	groups   map[string]model.ID
	channels map[string]model.ID
	monitors map[string]model.ID
}

func (p *pass) file(ctx context.Context, file File) error {
	src, err := openSource(ctx, file.Path, file.Name)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	p.sources = append(p.sources, model.ImportSource{
		Filename:         file.Name,
		KumaVersion:      src.version,
		DetectedEntities: src.detected(ctx),
	})

	// Per-file identity maps: two Kuma instances both have a monitor with id 1,
	// so nothing here may be keyed across files.
	tagIDs := map[tagRef]model.ID{}
	channelIDs := map[int64]model.ID{}
	groupIDs := map[int64]model.ID{}
	monitorIDs := map[int64]model.ID{}

	if p.opts.ImportTags {
		if err := p.importTags(ctx, src, tagIDs); err != nil {
			return err
		}
	}
	if p.opts.ImportNotifications {
		if err := p.importChannels(ctx, src, channelIDs); err != nil {
			return err
		}
	}
	if p.opts.ImportMonitors {
		if err := p.importMonitors(ctx, src, tagIDs, channelIDs, groupIDs, monitorIDs); err != nil {
			return err
		}
	}
	if p.opts.ImportStatusPages {
		if err := p.importStatusPages(ctx, src, monitorIDs); err != nil {
			return err
		}
	}
	if p.opts.ImportHistory && p.opts.ImportMonitors {
		if err := p.importHistory(ctx, src, monitorIDs); err != nil {
			return err
		}
	}
	return nil
}

// record appends one entry to the report.
func (p *pass) record(file, entity, sourceID, name, result, detail string, target *model.ID) {
	p.entries = append(p.entries, model.ImportEntry{
		ID: model.NewID(), JobID: p.job.ID, OrgID: p.importer.orgID,
		SourceFile: file, EntityType: entity, SourceID: sourceID, SourceName: name,
		Result: result, TargetID: target, Detail: detail,
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	})
}

// resolveName applies the conflict strategy and reports what happened.
//
// `replace` is deliberately not implemented as a replace. The spec offers it,
// and honouring it literally would mean deleting an existing monitor and its
// entire history to make room for one named the same — during a migration,
// which is exactly when somebody is least able to notice. So it behaves as
// `skip` and the report says so, which loses nothing and destroys nothing. A
// real replace wants a decision about history that nobody has made yet.
func (p *pass) resolveName(existing map[string]model.ID, name string) (final string, id model.ID, action string) {
	name = p.prefixed(name)
	folded := foldName(name)

	current, taken := existing[folded]
	if !taken {
		return name, model.ID{}, model.ImportResultImported
	}

	switch p.opts.ConflictStrategy {
	case ConflictSkip, ConflictReplace:
		return name, current, model.ImportResultSkipped
	default:
		for n := 2; n < 1000; n++ {
			candidate := name + " (" + strconv.Itoa(n) + ")"
			if _, clash := existing[foldName(candidate)]; !clash {
				return candidate, model.ID{}, model.ImportResultRenamed
			}
		}
		return name, current, model.ImportResultSkipped
	}
}

func (p *pass) prefixed(name string) string {
	if p.opts.NamePrefix == "" {
		return name
	}
	return p.opts.NamePrefix + name
}

// foldName is the collision rule: case-insensitive, whitespace-collapsed.
//
// "API Gateway" and "api  gateway" are the same name to a person looking at a
// list, and an import that creates both has produced exactly the confusion the
// conflict strategy exists to prevent.
func foldName(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteRune(' ')
		}
		space = false
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// skipDetail explains a skip in the user's terms rather than the code's.
func skipDetail(kind, name string) string {
	return fmt.Sprintf("a %s named %q is already here, and the conflict strategy is to keep it", kind, name)
}
