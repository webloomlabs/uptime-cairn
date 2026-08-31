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
}

// Files is the artifact directory.
type Files interface {
	Write(id model.ID, format string, when time.Time, data []byte) (artifact.Written, error)
}

// Options are the instance-level settings a run reads.
type Options struct {
	Retention report.Retention

	// ArtifactDays is settings.retention.report_artifact_days. Zero keeps
	// artifacts indefinitely, which is the convention that settings section
	// already uses — and is deliberately independent of the rollup tiers,
	// because an artifact is expected to outlive the data it was computed from.
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
}

func New(store Store, files Files, opts Options) *Runner {
	return &Runner{store: store, files: files, opts: opts}
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

	doc, brand, err := r.compose(ctx, run, now)
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
		data, renderErr := r.render(doc, brand, format)
		if renderErr == nil {
			renderErr = r.storeArtifact(ctx, run, format, data, now)
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
	return r.store.FinishReportRun(ctx, id, state, failure, now)
}

// compose builds the document and resolves the branding.
func (r *Runner) compose(ctx context.Context, run model.ReportRun, now time.Time) (report.Document, render.Brand, error) {
	template, err := r.store.ReportTemplateForRun(ctx, run.ReportTemplateID)
	if err != nil {
		return report.Document{}, render.Brand{}, fmt.Errorf("load template: %w", err)
	}

	doc, err := report.Build(ctx, r.store, specFor(template, run), r.opts.Retention, run.ID, now)
	if err != nil {
		return report.Document{}, render.Brand{}, fmt.Errorf("compute report: %w", err)
	}

	return doc, r.brandFor(ctx, template), nil
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
	}
}

// brandFor resolves the profile, falling back to the default.
//
// A brand that cannot be loaded does not fail the run. An unbranded report is a
// worse report; a missing one is no report, and a client would rather have the
// first. The same judgement ADR-007 item 7 makes about formats applies inside
// one.
func (r *Runner) brandFor(ctx context.Context, t model.ReportTemplate) render.Brand {
	var profile model.BrandProfile
	var err error

	if t.BrandProfileID != nil {
		profile, err = r.store.GetBrandProfile(ctx, *t.BrandProfileID)
	} else {
		profile, err = r.store.DefaultBrandProfile(ctx)
	}
	if err != nil {
		return render.Brand{}
	}

	brand := render.Brand{
		CompanyName:   profile.CompanyName,
		FooterText:    profile.FooterText,
		CoverText:     profile.CoverText,
		HidePoweredBy: profile.HidePoweredBy,
	}
	if profile.LogoBytes > 0 {
		if logo, contentType, err := r.store.BrandLogo(ctx, profile.ID); err == nil {
			brand.Logo, brand.LogoMIME = logo, contentType
		}
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
func (r *Runner) render(doc report.Document, brand render.Brand, format string) (data []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			data, err = nil, fmt.Errorf("renderer panicked: %v", recovered)
		}
	}()

	switch format {
	case model.FormatJSON:
		return render.JSON(doc)
	case model.FormatCSV:
		return render.CSV(doc)
	case model.FormatHTML:
		return render.HTML(doc, brand)
	case model.FormatPDF:
		if r.opts.Fonts.Regular == nil {
			// Stated rather than generic, because the fix is an operator action:
			// no font family is embedded in this build.
			return nil, errors.New("no embedded font family, so PDF cannot be rendered")
		}
		return render.PDFDocument(doc, brand, r.opts.Fonts)
	}
	return nil, fmt.Errorf("unknown format %q", format)
}

// storeArtifact writes the bytes and then records the row, in that order.
//
// ADR-008 item 4, and the order is load-bearing: a crash between the two leaves
// an orphan file, which is inert and reclaimable by the sweeper, where the
// reverse leaves a dangling row — an artifact the UI offers and the disk cannot
// supply.
func (r *Runner) storeArtifact(ctx context.Context, run model.ReportRun, format string, data []byte, now time.Time) error {
	id := model.NewID()

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
		ExpiresAt:   r.expiryFor(now),
		CreatedAt:   now,
	}
	return r.store.CreateReportArtifact(ctx, row)
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
func (r *Runner) expiryFor(now time.Time) *time.Time {
	if r.opts.ArtifactDays <= 0 {
		return nil
	}
	at := now.AddDate(0, 0, r.opts.ArtifactDays)
	return &at
}
