// Package runner executes one report run: compute, render, store, record.
//
// It is the only place the four halves of the subsystem meet — `internal/report`
// computes the model, `internal/report/render` turns it into files,
// `internal/artifact` puts those files on disk, and the store records what
// happened. Keeping the meeting point in one package is what lets each of the
// four be tested without the other three.
//
// # The failure discipline
//
// ADR-007 item 7 is binding and it is the spine of this file: **a report run
// never fails silently, and never fails wholly because one format could not be
// produced.** A PDF that will not render degrades the run to `partial`, records
// the reason against that artifact, and delivers the HTML, the CSV and the JSON
// that did render. Every format is attempted independently for that reason, and
// a panic in one renderer is recovered rather than allowed to take the run with
// it — a renderer is the part of this system most likely to meet a shape nobody
// anticipated, because its input is a client's data.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/artifact"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/report/render"
)

// Store is what executing a run needs from persistence, declared here by the
// consumer rather than centrally — the convention `internal/store/store.go` asks
// for in as many words.
type Store interface {
	report.Store

	StartReportRun(ctx context.Context, id model.ID, at time.Time) error
	FinishReportRun(ctx context.Context, id model.ID, state, failure string, at time.Time) error
	CreateReportArtifact(ctx context.Context, a model.ReportArtifact) error

	// ReportTemplateForRun rather than GetReportTemplate: a run may name a
	// template that has since been soft-deleted, and a queued run that fails
	// because somebody tidied up between queueing and execution would be a
	// confusing way to discover the deletion.
	ReportTemplateForRun(ctx context.Context, id model.ID) (model.ReportTemplate, error)

	GetBrandProfile(ctx context.Context, id model.ID) (model.BrandProfile, error)
	DefaultBrandProfile(ctx context.Context) (model.BrandProfile, error)
	BrandLogo(ctx context.Context, id model.ID) ([]byte, string, error)

	// GetSettings is read per run rather than at start-up, following the rollup
	// runner, which reads retention on every pass for the same reason: an
	// operator who shortens artifact retention at 09:00 should not have to
	// restart the instance for the change to reach the next report. It also
	// carries the appearance section, which is what an install that has never
	// opened the brand-profile screen is branded from.
	GetSettings(ctx context.Context, orgID model.ID) (model.Settings, error)

	// RecordArtifactMirror writes the outcome of one offsite upload. Separate
	// from CreateReportArtifact because the ordering is the same one ADR-008
	// item 4 fixes locally: row first, upload second, outcome third — an upload
	// attempted before the row would have nowhere to record a failure.
	RecordArtifactMirror(ctx context.Context, id model.ID, state string, uploadedAt *time.Time, failure string) error
}

// Files is the artifact directory.
type Files interface {
	Write(id model.ID, format string, when time.Time, data []byte) (artifact.Written, error)
}

// Options are the compiled-in fallbacks a run uses when stored settings do not
// answer.
//
// Retention and ArtifactDays are **defaults, not configuration**: the values a
// run actually uses come from the settings row, read fresh at execution. They
// are here so that a run on an install whose settings row cannot be read still
// produces a report against sensible tiers rather than against zero.
type Options struct {
	Retention report.Retention

	// ArtifactDays is the fallback for settings.retention.report_artifact_days.
	// Zero keeps artifacts indefinitely, which is the convention that settings
	// section already uses — and is deliberately independent of the rollup
	// tiers, because an artifact is expected to outlive the data it was computed
	// from.
	ArtifactDays int

	// Fonts is the embedded family the PDF backend draws with. **A zero Family
	// is a supported configuration**: PDF then fails with a reason and the run
	// degrades to partial, which is the same path any other render failure
	// takes. That is what keeps an unvendored font file from blocking the three
	// formats that do not need one.
	Fonts render.Family
}

// Runner executes runs.
type Runner struct {
	store Store
	files Files
	opts  Options

	// mirrors resolves the offsite durability copy from the settings snapshot
	// this run read, rather than from a client built at start-up. That is the
	// same rule retention follows and for the same reason: an operator who
	// enables the mirror at 09:00 should not have to restart the instance for it
	// to reach the next report.
	//
	// Nil on a build that is not running one, and it resolves to nil on every
	// install that has not configured one — which is the common case and is why
	// this is a nil check rather than a no-op implementation. A durability copy
	// and never a read path: nothing in this package consults it to find bytes.
	mirrors MirrorSource

	// log carries a mirror failure to where an operator sees it. Nil in tests
	// that do not assert on logging, which every call site tolerates.
	log *slog.Logger
}

func New(store Store, files Files, opts Options) *Runner {
	return &Runner{store: store, files: files, opts: opts}
}

// WithMirror attaches the offsite copy's resolver.
//
// A setter rather than a fourth argument to New, following WithReporting on the
// API server and for the same reason: the mirror is optional in a way the other
// three are not, and a test exercising a render has no reason to construct one.
func (r *Runner) WithMirror(m MirrorSource, log *slog.Logger) *Runner {
	r.mirrors, r.log = m, log
	return r
}

// mirrorFor resolves this run's mirror from the settings it read.
func (r *Runner) mirrorFor(settings model.Settings) Uploader {
	if r.mirrors == nil {
		return nil
	}
	return r.mirrors.For(settings.ReportStorage)
}

// Execute renders one queued run to completion.
//
// `now` is a parameter rather than a call to the clock, for the reason ADR-007
// item 6 gives: the same definition over the same window against the same data
// has to produce the same bytes, and a generated-at timestamp read from inside
// the renderer would make that false in the one field nobody looks at.
func (r *Runner) Execute(ctx context.Context, run model.ReportRun, now time.Time) error {
	if err := r.store.StartReportRun(ctx, run.ID, now); err != nil {
		// ErrConflict means another worker has it. That is the bounded pool
		// working, not a failure, and the caller distinguishes them.
		return err
	}

	// One settings read per run, at the top, so every decision below is made
	// against the same snapshot. Reading twice could straddle an operator's save
	// and render a document under one retention policy while filing it under
	// another.
	settings := r.settingsFor(ctx, run)

	doc, brand, sections, err := r.compose(ctx, run, settings, now)
	if err != nil {
		// Nothing rendered because nothing could be computed. This is the one
		// genuinely failed run: no artifact exists, so there is no partial state
		// to be in.
		return r.finish(ctx, run.ID, model.RunFailed, err.Error(), now)
	}

	formats, err := r.formatsFor(ctx, run)
	if err != nil {
		return r.finish(ctx, run.ID, model.RunFailed, err.Error(), now)
	}

	var rendered, failed int
	var firstFailure string

	for _, format := range formats {
		// **Cancellation is checked between formats, never inside one.** A
		// shutdown arriving mid-PDF still finishes that PDF, because the write
		// is a rename of a complete temporary file and abandoning it halfway
		// would be the one way to produce the half-written artifact this
		// subsystem promises never to leave. What cancellation buys is that the
		// *next* format is not started and the run is recorded as interrupted
		// rather than left claiming to be running.
		if ctx.Err() != nil {
			return r.interrupted(ctx, run, rendered, now)
		}

		data, renderErr := r.render(doc, brand, sections, format)
		if renderErr == nil {
			renderErr = r.storeArtifact(ctx, run, settings, format, data, now)
		}
		if renderErr != nil {
			failed++
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("%s: %s", format, renderErr)
			}
			// The failure is recorded as an artifact row rather than only on the
			// run, because "which format failed" is the question asked next and
			// a single sentence on the run cannot answer it for two failures.
			if err := r.recordFailure(ctx, run, format, renderErr, now); err != nil {
				return err
			}
			continue
		}
		rendered++
	}

	switch {
	case rendered == 0:
		return r.finish(ctx, run.ID, model.RunFailed, firstFailure, now)
	case failed > 0:
		return r.finish(ctx, run.ID, model.RunPartial, firstFailure, now)
	default:
		return r.finish(ctx, run.ID, model.RunSucceeded, "", now)
	}
}

func (r *Runner) finish(ctx context.Context, id model.ID, state, failure string, now time.Time) error {
	// **Detached from the caller's context on purpose.** The one thing that must
	// happen when a run is cancelled is the row being written; using the context
	// that was just cancelled would guarantee it is not, leaving the run stuck at
	// `running` for the recovery pass to clean up — the exact outcome this
	// function exists to avoid. The write is one statement against a local
	// database and cannot hang for long enough to be worth a deadline of its own.
	return r.store.FinishReportRun(context.WithoutCancel(ctx), id, state, failure, now)
}

// interrupted records a run the process was told to stop in the middle of.
//
// The formats that finished are kept, because each is a complete file with a
// committed row and a digest — a client who received three of four artifacts has
// three real ones, and throwing them away to make the state tidier would be
// deleting evidence. The state follows the same rule every other partial outcome
// does: something arrived, so it is `partial`; nothing did, so it is `failed`.
//
// Either way it is **finished**, which is the point: an interrupted run must not
// be left at `running`, because a row in that state is indistinguishable from a
// report that is genuinely in flight and a person looking at the screen cannot
// tell whether to wait.
func (r *Runner) interrupted(ctx context.Context, run model.ReportRun, rendered int, now time.Time) error {
	const reason = "the run was interrupted before every format was produced, " +
		"most likely by a restart; generate it again"
	state := model.RunFailed
	if rendered > 0 {
		state = model.RunPartial
	}
	return r.finish(ctx, run.ID, state, reason, now)
}

// settingsFor reads the instance settings, falling back to the compiled-in
// defaults.
//
// A settings read that fails does not fail the run. The same judgement ADR-007
// item 7 makes about formats applies: a report computed against default tiers is
// a worse report, and no report is none at all. The fallback is logged by
// nothing here on purpose — the store logs its own errors, and a per-run line
// for a condition that would repeat on every run is noise.
func (r *Runner) settingsFor(ctx context.Context, run model.ReportRun) model.Settings {
	settings, err := r.store.GetSettings(ctx, run.OrgID)
	if err != nil {
		return model.Settings{}
	}
	return settings
}

// retentionFor is the tier policy this run reads its figures at.
//
// Each field falls back independently rather than the block falling back whole:
// an operator who set only raw_days should get their raw_days and the defaults
// for the rest, which is how every other consumer of this section behaves.
func (r *Runner) retentionFor(settings model.Settings) report.Retention {
	out := r.opts.Retention
	for _, field := range []struct {
		supplied *int
		into     *int
	}{
		{settings.Retention.RawDays, &out.RawDays},
		{settings.Retention.Rollup1mDays, &out.Rollup1mDays},
		{settings.Retention.Rollup5mDays, &out.Rollup5mDays},
		{settings.Retention.Rollup1hDays, &out.Rollup1hDays},
		{settings.Retention.Rollup1dDays, &out.Rollup1dDays},
	} {
		if field.supplied != nil {
			*field.into = *field.supplied
		}
	}
	return out
}

// compose builds the document and resolves the branding.
func (r *Runner) compose(ctx context.Context, run model.ReportRun, settings model.Settings, now time.Time) (report.Document, render.Brand, []string, error) {
	template, err := r.store.ReportTemplateForRun(ctx, run.ReportTemplateID)
	if err != nil {
		return report.Document{}, render.Brand{}, nil, fmt.Errorf("load template: %w", err)
	}

	doc, err := report.Build(ctx, r.store, specFor(template, run), r.retentionFor(settings), run.ID, now)
	if err != nil {
		return report.Document{}, render.Brand{}, nil, fmt.Errorf("compute report: %w", err)
	}

	// **Resolved here and copied onto the document**, so the artifact records
	// the branding it was produced under rather than pointing at a row that can
	// change. A profile edited in June must not alter what a January report
	// claims it said.
	brand := r.brandFor(ctx, template, settings)
	doc.Meta.Brand = brand.Denormalised()

	// **The sections are the template's, not the run's**, and they are read here
	// with everything else the template decides. A run records the window it
	// covered and the branding it was produced under; which blocks it contained
	// follows the definition, so re-running a report after narrowing a template
	// produces the narrowed document. That is the same rule the formats already
	// follow, and the alternative — freezing the selection onto the run — would
	// need a column the frozen schema does not have.
	return doc, brand, template.Sections, nil
}

// specFor reduces a stored template and a run to the questions the computation
// can answer.
//
// **The run's window wins over the template's period**, and that is what makes
// re-running a first-class action: the boundaries were recorded when the run was
// queued, so regenerating it after a correction produces a report over the same
// window rather than over whatever period the definition would resolve to today.
// The period and its style still travel, because a custom window with no name is
// harder to read on the face of the document.
func specFor(t model.ReportTemplate, run model.ReportRun) report.Spec {
	return report.Spec{
		TemplateID:   t.ID,
		TemplateName: t.Name,
		Type:         t.Type,
		Scope: report.Scope{
			MonitorIDs: t.Scope.MonitorIDs,
			GroupIDs:   t.Scope.GroupIDs,
			TagIDs:     t.Scope.TagIDs,
		},
		Period:               t.Period,
		PeriodStyle:          t.PeriodStyle,
		Timezone:             run.Timezone,
		From:                 run.PeriodStart,
		To:                   run.PeriodEnd,
		MaintenanceHandling:  t.MaintenanceHandling,
		SLATarget:            t.SLATarget,
		ResponseTimeTargetMs: t.ResponseTimeTargetMS,
		Comparison:           comparisonFor(t),
	}
}

// comparisonFor maps the stored comparison, if the template has one.
func comparisonFor(t model.ReportTemplate) report.ComparisonSpec {
	if t.Comparison == nil {
		return report.ComparisonSpec{}
	}
	return report.ComparisonSpec{
		Mode:       t.Comparison.Mode,
		MonitorIDs: t.Comparison.MonitorIDs,
		GroupIDs:   t.Comparison.GroupIDs,
	}
}

// brandFor resolves the profile, falling back to the default.
//
// A brand that cannot be loaded does not fail the run. An unbranded report is a
// worse report; a missing one is no report, and a client would rather have the
// first. The same judgement ADR-007 item 7 makes about formats applies inside
// one.
func (r *Runner) brandFor(ctx context.Context, t model.ReportTemplate, settings model.Settings) render.Brand {
	var profile model.BrandProfile
	var err error

	if t.BrandProfileID != nil {
		profile, err = r.store.GetBrandProfile(ctx, *t.BrandProfileID)
	} else {
		profile, err = r.store.DefaultBrandProfile(ctx)
	}
	if err != nil {
		// **No profile at all falls back to the appearance settings**, so an
		// install that has never opened the brand-profile screen still produces
		// a report carrying its own name and colour rather than one that looks
		// like nobody configured the product. This is the solo user's path and
		// it is the common one, not the edge.
		return brandFromAppearance(settings)
	}

	brand := render.Brand{
		CompanyName:   profile.CompanyName,
		PrimaryColor:  profile.PrimaryColor,
		AccentColor:   profile.AccentColor,
		FooterText:    profile.FooterText,
		CoverText:     profile.CoverText,
		HidePoweredBy: profile.HidePoweredBy,
	}
	// A profile that exists but sets no colour still gets the instance's, rather
	// than the renderer's grey. Creating a profile to set a client name should
	// not silently undo the appearance the operator already chose.
	if brand.PrimaryColor == "" {
		brand.PrimaryColor = brandFromAppearance(settings).PrimaryColor
	}
	if brand.CompanyName == "" {
		brand.CompanyName = settings.General.InstanceName
	}

	if profile.LogoBytes > 0 {
		if logo, contentType, err := r.store.BrandLogo(ctx, profile.ID); err == nil {
			brand.Logo, brand.LogoMIME = logo, contentType
		}
	}
	return brand
}

// brandFromAppearance derives the default branding from settings.
//
// Two fields and no more: the instance name and the dashboard's primary colour.
// There is nothing else in that section a report could honestly use — no logo,
// no footer, no client name — and inventing a footer here would put words on a
// client's document that nobody wrote.
func brandFromAppearance(settings model.Settings) render.Brand {
	brand := render.Brand{CompanyName: settings.General.InstanceName}
	if c := settings.Appearance.PrimaryColor; c != nil {
		brand.PrimaryColor = *c
	}
	return brand
}

// formatsFor is what this run should produce.
func (r *Runner) formatsFor(ctx context.Context, run model.ReportRun) ([]string, error) {
	template, err := r.store.ReportTemplateForRun(ctx, run.ReportTemplateID)
	if err != nil {
		return nil, fmt.Errorf("load template: %w", err)
	}
	if len(template.Formats) == 0 {
		// The API refuses an empty format list, so this is a row that predates
		// that rule or was written by hand. Refusing here beats producing
		// nothing and calling it success.
		return nil, errors.New("template requests no formats")
	}
	return template.Formats, nil
}

// render turns the document into bytes, with a recover around it.
//
// The recover is not defensive habit. A renderer's input is a client's data —
// monitor names, incident titles, arbitrary counts — and it is the part of this
// system most likely to meet a shape nobody anticipated. Taking down the run,
// and with it the three formats that would have rendered, is a worse answer than
// recording one format's failure.
func (r *Runner) render(doc report.Document, brand render.Brand, sections []string, format string) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			data, err = nil, fmt.Errorf("renderer panicked: %v", recovered)
		}
	}()

	// **Sections reach the HTML and the PDF and deliberately not the JSON or the
	// CSV.** A section is a decision about the report's *face* — which blocks a
	// reader sees and in what order — and those two are not a face. The JSON
	// artifact is the `ReportDocument` verbatim, which is what the frozen spec
	// says it is and what a BI tool binds to; the CSV is a row per bucket per
	// monitor. Filtering either would make a data export whose columns depended
	// on a presentation choice, and would make the JSON artifact stop being the
	// document it claims to be.
	switch format {
	case model.FormatJSON:
		return render.JSON(doc)
	case model.FormatCSV:
		return render.CSV(doc)
	case model.FormatHTML:
		return render.HTMLSections(doc, brand, sections)
	case model.FormatPDF:
		if r.opts.Fonts.Regular == nil {
			// Stated rather than generic, because the fix is an operator action:
			// no font family is embedded in this build.
			return nil, errors.New("no embedded font family, so PDF cannot be rendered")
		}
		return render.PDFSections(doc, brand, r.opts.Fonts, sections)
	}
	return nil, fmt.Errorf("unknown format %q", format)
}

// storeArtifact writes the bytes and then records the row, in that order.
//
// ADR-008 item 4, and the order is load-bearing: a crash between the two leaves
// an orphan file, which is inert and reclaimable by the sweeper, where the
// reverse leaves a dangling row — an artifact the UI offers and the disk cannot
// supply.
func (r *Runner) storeArtifact(ctx context.Context, run model.ReportRun, settings model.Settings, format string, data []byte, now time.Time) error {
	id := model.NewID()

	// Resolved from this run's settings snapshot, so the row is created
	// `pending` exactly when there is a mirror to be pending for.
	mirror := r.mirrorFor(settings)

	// Dated by the run's period rather than by now, so a report regenerated for
	// last March is filed under last March and a backup restored by month
	// contains what its name says.
	written, err := r.files.Write(id, format, run.PeriodEnd, data)
	if err != nil {
		return err
	}

	row := model.ReportArtifact{
		ID:          id,
		OrgID:       run.OrgID,
		ReportRunID: run.ID,
		Format:      format,
		State:       model.ArtifactRendered,
		Path:        written.Path,
		SizeBytes:   written.SizeBytes,
		SHA256:      written.SHA256,
		ExpiresAt:   r.expiryFor(settings, now),
		MirrorState: mirrorInitialState(mirror != nil),
		CreatedAt:   now,
	}
	if err := r.store.CreateReportArtifact(ctx, row); err != nil {
		return err
	}

	// The offsite copy, attempted after the row exists and unable to fail the
	// run. ADR-008 item 9: local is the source of truth and the only read path,
	// so a report that rendered, filed and delivered has not failed because a
	// bucket was unreachable. The outcome lands on the artifact row, where an
	// operator can see the queue rather than believe in the mirror.
	r.mirrorArtifact(ctx, mirror, id, written.Path, format, data)
	return nil
}

func (r *Runner) recordFailure(ctx context.Context, run model.ReportRun, format string, cause error, now time.Time) error {
	return r.store.CreateReportArtifact(ctx, model.ReportArtifact{
		ID:          model.NewID(),
		OrgID:       run.OrgID,
		ReportRunID: run.ID,
		Format:      format,
		State:       model.ArtifactFailed,
		Error:       cause.Error(),
		CreatedAt:   now,
	})
}

// expiryFor is when the sweeper may reclaim the bytes. Nil keeps them
// indefinitely, which is what report_artifact_days of zero selects.
//
// Read from the settings row rather than from start-up options, so shortening
// retention takes effect on the next report instead of on the next restart —
// the same rule the rollup runner follows for the tiers.
func (r *Runner) expiryFor(settings model.Settings, now time.Time) *time.Time {
	days := r.opts.ArtifactDays
	if d := settings.Retention.ReportArtifactDays; d != nil {
		days = *d
	}
	if days <= 0 {
		return nil
	}
	at := now.AddDate(0, 0, days)
	return &at
}
