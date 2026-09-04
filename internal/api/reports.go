package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/report"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The reporting surface: templates, runs, and artifact download.
//
// Spec-first, unchanged and not softened for this phase — every operation below
// is already in the frozen `docs/api/openapi.yaml` and no handler reshapes one.
// Two of its choices are worth naming here because they shape the code:
//
//   - **`generate` returns 202 with a run to poll**, not the document. So the
//     handler queues and answers; the rendering happens on the bounded worker
//     pool, off the request and off the check path.
//   - **An expired artifact is 410, not 404.** Retention reclaims the bytes and
//     keeps the row, so somebody holding a bookmarked link is told "this existed
//     and is gone" rather than "no such thing". They are different facts and
//     only one of them is true.

const maxReportBody = 1 << 16

// ReportStore is the reporting half of persistence.
type ReportStore interface {
	CreateReportTemplate(ctx context.Context, t model.ReportTemplate) error
	UpdateReportTemplate(ctx context.Context, t model.ReportTemplate) error
	GetReportTemplate(ctx context.Context, id model.ID) (model.ReportTemplate, error)
	ListReportTemplates(ctx context.Context, after *store.Cursor, limit int) ([]model.ReportTemplate, bool, error)
	DeleteReportTemplate(ctx context.Context, id model.ID, at time.Time) error

	CreateReportRun(ctx context.Context, r model.ReportRun) error
	GetReportRun(ctx context.Context, id model.ID) (model.ReportRun, error)
	ListReportRuns(ctx context.Context, after *store.Cursor, limit int, filter store.ReportRunFilter) ([]model.ReportRun, bool, error)

	// ReportTemplateForRun rather than GetReportTemplate: a run may name a
	// template that has since been soft-deleted, and a shared report whose title
	// disappeared because somebody tidied up is a broken link from the client's
	// side.
	ReportTemplateForRun(ctx context.Context, id model.ID) (model.ReportTemplate, error)

	GetReportArtifact(ctx context.Context, id model.ID) (model.ReportArtifact, error)
	ArtifactByFormat(ctx context.Context, runID model.ID, format string) (model.ReportArtifact, error)
	ArtifactsForRuns(ctx context.Context, runIDs []model.ID) (map[model.ID][]model.ReportArtifact, error)
	DeliveriesForRun(ctx context.Context, runID model.ID) ([]model.ReportDelivery, error)
}

// Reporter queues a run for the worker pool.
type Reporter interface {
	Submit(run model.ReportRun) error
}

// ArtifactFiles is the read half of artifact storage. Local only, deliberately:
// ADR-008 item 9 makes the S3 copy a durability mirror and never a read path,
// because one read path is the property being protected.
type ArtifactFiles interface {
	Open(path string) (io.ReadCloser, error)

	// Exists answers whether the bytes are actually on disk, which the database
	// cannot: a row and its file are two stores. ADR-008's Consequences require
	// the artifact list to render a missing file **as a missing file** rather
	// than offering a download that fails, and this is what lets it.
	Exists(path string) bool
}

// artifactAvailable reports whether an artifact can actually be downloaded.
//
// A rendered row is not the same claim as a readable file. The state that pulls
// them apart is a `cairn.db` restored without `<data-dir>/reports/` — the silent
// half of the backup procedure — and until this existed, the UI offered a
// download link per row and the server answered each one with a problem
// document. Offering a link that cannot work is a worse way to learn a file is
// gone than not being offered one, which is the rule the expired and failed
// states already follow.
//
// A build with no artifact storage answers true rather than false: it has no
// basis for the negative, and reporting every artifact as missing would be worse
// than reporting them optimistically.
func (s *Server) artifactAvailable(a model.ReportArtifact) bool {
	if a.State != model.ArtifactRendered || a.Path == "" {
		return false
	}
	if s.artifacts == nil {
		return true
	}
	return s.artifacts.Exists(a.Path)
}

// --- templates --------------------------------------------------------------

func (s *Server) listReportTemplates(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}

	templates, hasMore, err := s.store.ListReportTemplates(r.Context(), after, s.limit(r))
	if err != nil {
		s.internal(w, r, "list report templates", err)
		return
	}

	body := page[reportTemplateJSON]{Data: []reportTemplateJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, t := range templates {
		body.Data = append(body.Data, toReportTemplateJSON(t))
	}
	if hasMore && len(templates) > 0 {
		last := templates[len(templates)-1]
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createReportTemplate(w http.ResponseWriter, r *http.Request) {
	var body reportTemplateWrite
	if !s.readBody(w, r, maxReportBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	template := model.ReportTemplate{
		ID:                  model.NewID(),
		OrgID:               s.orgID,
		Type:                model.ReportTypeUptime,
		Period:              model.ReportPeriodMonth,
		PeriodStyle:         model.ReportStyleCalendar,
		MaintenanceHandling: report.MaintenanceExclude,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	problems := applyReportTemplate(&template, body)
	if body.Name == nil {
		problems = append(problems, ValidationItem{Pointer: "/name", Code: "required", Message: "name is required"})
	}
	if body.Formats == nil || len(*body.Formats) == 0 {
		problems = append(problems, ValidationItem{Pointer: "/formats", Code: "required",
			Message: "formats must name at least one of pdf, html, csv or json"})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The report template was not created.", problems...)
		return
	}

	if err := s.store.CreateReportTemplate(r.Context(), template); err != nil {
		s.internal(w, r, "create report template", err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, toReportTemplateJSON(template))
}

func (s *Server) getReportTemplate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportTemplateID(w, r)
	if !ok {
		return
	}
	template, err := s.store.GetReportTemplate(r.Context(), id)
	if err != nil {
		s.reportStoreError(w, r, "get report template", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toReportTemplateJSON(template))
}

func (s *Server) updateReportTemplate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportTemplateID(w, r)
	if !ok {
		return
	}
	var body reportTemplateWrite
	if !s.readBody(w, r, maxReportBody, &body) {
		return
	}

	template, err := s.store.GetReportTemplate(r.Context(), id)
	if err != nil {
		s.reportStoreError(w, r, "get report template", err)
		return
	}

	// A PATCH over the stored row, so an absent field means "leave it" rather
	// than "clear it". The store below rewrites wholesale, which is why the
	// merge has to happen here: those are two different contracts and only one
	// of them is the spec's.
	problems := applyReportTemplate(&template, body)
	if body.Formats != nil && len(*body.Formats) == 0 {
		problems = append(problems, ValidationItem{Pointer: "/formats", Code: "invalid",
			Message: "formats must name at least one of pdf, html, csv or json"})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The report template was not updated.", problems...)
		return
	}

	template.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if err := s.store.UpdateReportTemplate(r.Context(), template); err != nil {
		s.reportStoreError(w, r, "update report template", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toReportTemplateJSON(template))
}

// deleteReportTemplate hides the definition and keeps everything it produced.
//
// The spec's own description is "Already-generated runs and their artefacts are
// retained", and the store honours it with a soft delete rather than a cascade —
// a run is the record of what a client was sent, and it outlives the arrangement
// that sent it.
func (s *Server) deleteReportTemplate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportTemplateID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteReportTemplate(r.Context(), id, time.Now().UTC().Truncate(time.Millisecond)); err != nil {
		s.reportStoreError(w, r, "delete report template", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- generation -------------------------------------------------------------

// generateReport queues a one-off run and returns it to poll.
//
// 202 and a run, per the spec. The alternative — rendering inside the request —
// was never on the table: fifty reports on the first of the month would hold
// fifty connections open and put PDF layout on the same goroutines that answer
// health checks.
func (s *Server) generateReport(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportTemplateID(w, r)
	if !ok {
		return
	}
	template, err := s.store.GetReportTemplate(r.Context(), id)
	if err != nil {
		s.reportStoreError(w, r, "get report template", err)
		return
	}

	var body reportGenerateRequest
	if !s.readBody(w, r, maxReportBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	window, problems := s.resolveGenerateWindow(template, body, now)
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The report was not generated.", problems...)
		return
	}

	run := model.ReportRun{
		ID:               model.NewID(),
		OrgID:            s.orgID,
		ReportTemplateID: template.ID,
		State:            model.RunQueued,
		PeriodStart:      window.From,
		PeriodEnd:        window.To,
		Timezone:         window.Timezone,
		CreatedAt:        now,
	}

	if s.reports == nil {
		// Stated rather than queued into a void. A run row with nothing to
		// execute it would sit at `queued` forever and read as a hung report.
		writeProblem(w, r, s.log, http.StatusNotImplemented, "reporting-unavailable",
			"Reporting is not running", "This instance has no report worker configured.")
		return
	}
	if err := s.store.CreateReportRun(r.Context(), run); err != nil {
		s.internal(w, r, "create report run", err)
		return
	}
	if err := s.reports.Submit(run); err != nil {
		// The queue is full. A refusal with a reason beats a backlog that turns
		// into memory pressure, and the run is already recorded as queued so
		// nothing is lost — it is picked up by the recovery pass rather than by
		// this request.
		writeProblem(w, r, s.log, http.StatusServiceUnavailable, "report-queue-full",
			"Report queue is full", "Too many reports are already queued. The run has been recorded and will be picked up.")
		return
	}

	writeJSON(w, s.log, http.StatusAccepted, s.toReportRunJSON(r.Context(), run))
}

// resolveGenerateWindow decides what period this run covers.
//
// Explicit boundaries win, because "regenerate exactly what you sent them in
// March" is the request. Otherwise the template's own period is resolved
// **backwards from now**, giving the most recently completed period — a monthly
// report generated on the 3rd means last month, not the three days of this one.
func (s *Server) resolveGenerateWindow(t model.ReportTemplate, body reportGenerateRequest, now time.Time) (report.Window, []ValidationItem) {
	zone := s.reportTimezone()

	if body.PeriodStart != nil && body.PeriodEnd != nil {
		if !body.PeriodStart.Before(*body.PeriodEnd) {
			return report.Window{}, []ValidationItem{{Pointer: "/period_end", Code: "invalid",
				Message: "period_end must be after period_start"}}
		}
		return report.Window{From: *body.PeriodStart, To: *body.PeriodEnd, Timezone: zone.String()}, nil
	}
	if (body.PeriodStart == nil) != (body.PeriodEnd == nil) {
		return report.Window{}, []ValidationItem{{Pointer: "/period_start", Code: "invalid",
			Message: "period_start and period_end must be given together"}}
	}

	window, err := report.ResolveWindow(t.Period, t.PeriodStyle, zone, now)
	if err != nil {
		return report.Window{}, []ValidationItem{{Pointer: "/period_start", Code: "invalid", Message: err.Error()}}
	}
	return window, nil
}

// reportTimezone is the zone report boundaries are cut in.
//
// The instance zone today. A schedule carries its own, defaulted from this at
// write time so that changing the instance zone does not silently move the
// boundaries of a report somebody has been receiving for a year — and an ad-hoc
// run has no schedule to take one from.
func (s *Server) reportTimezone() *time.Location {
	if s.reportZone != nil {
		return s.reportZone
	}
	return time.UTC
}

// --- runs -------------------------------------------------------------------

func (s *Server) listReportRuns(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}

	var filter store.ReportRunFilter
	if raw := r.URL.Query().Get("report_template_id"); raw != "" {
		id, valid := model.ParseID(raw)
		if !valid {
			writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
				"Validation failed", "The run list could not be filtered.",
				ValidationItem{Pointer: "/report_template_id", Code: "invalid", Message: "not a uuid"})
			return
		}
		filter.ReportTemplateID = &id
	}
	filter.States = r.URL.Query()["state"]

	runs, hasMore, err := s.store.ListReportRuns(r.Context(), after, s.limit(r), filter)
	if err != nil {
		s.internal(w, r, "list report runs", err)
		return
	}

	ids := make([]model.ID, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	artifacts, err := s.store.ArtifactsForRuns(r.Context(), ids)
	if err != nil {
		s.internal(w, r, "list report artifacts", err)
		return
	}
	// One query for the page rather than one per row: a listing of twenty-five
	// runs would otherwise issue twenty-five extra lookups, which is the N+1 that
	// makes a page slow at exactly the scale this product promises to stay fast
	// at. A failure is logged and leaves the column empty rather than failing the
	// listing — a run list that will not load because a share link would not is
	// the wrong trade.
	shares, err := s.store.ReportShareLinksForRuns(r.Context(), ids)
	if err != nil {
		s.log.Error("list report share links", "error", err)
	}

	body := page[reportRunJSON]{Data: []reportRunJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, run := range runs {
		body.Data = append(body.Data, reportRunToJSON(run, artifacts[run.ID], nil,
			shareFor(shares, run.ID), s.artifactAvailable))
	}
	if hasMore && len(runs) > 0 {
		last := runs[len(runs)-1]
		// The run list keys on created_at rather than updated_at, because a run
		// has no updated_at and because a state change an hour later has not
		// made it newer history. The cursor type is shared, so the field carries
		// whichever instant the collection orders on.
		next := store.Cursor{UpdatedAt: last.CreatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) getReportRun(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportRunID(w, r)
	if !ok {
		return
	}
	run, err := s.store.GetReportRun(r.Context(), id)
	if err != nil {
		s.reportStoreError(w, r, "get report run", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, s.toReportRunJSON(r.Context(), run))
}

func (s *Server) toReportRunJSON(ctx context.Context, run model.ReportRun) reportRunJSON {
	byRun, err := s.store.ArtifactsForRuns(ctx, []model.ID{run.ID})
	if err != nil {
		s.log.Error("load report artifacts", "error", err, "run_id", run.ID.String())
	}
	deliveries, err := s.store.DeliveriesForRun(ctx, run.ID)
	if err != nil {
		s.log.Error("load report deliveries", "error", err, "run_id", run.ID.String())
	}

	var share *model.ReportShareLink
	switch link, err := s.store.ReportShareLinkForRun(ctx, run.ID); {
	case err == nil:
		share = &link
	case !errors.Is(err, store.ErrNotFound):
		// Most runs have no link, and ErrNotFound is the ordinary answer rather
		// than a fault worth logging.
		s.log.Error("load report share link", "error", err, "run_id", run.ID.String())
	}
	return reportRunToJSON(run, byRun[run.ID], deliveries, share, s.artifactAvailable)
}

// shareFor picks one run's link out of the batch read. A helper rather than an
// inline index because a map lookup returning a zero-valued struct is not the
// same as no link, and the distinction is the difference between a run that
// reports `"share": null` and one that reports a link created at the zero time.
func shareFor(links map[model.ID]model.ReportShareLink, runID model.ID) *model.ReportShareLink {
	link, ok := links[runID]
	if !ok {
		return nil
	}
	return &link
}

// --- download ---------------------------------------------------------------

// downloadReportArtifact serves one format of a run.
func (s *Server) downloadReportArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportRunID(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "A format is required.",
			ValidationItem{Pointer: "/format", Code: "required", Message: "format is required"})
		return
	}

	artifact, err := s.store.ArtifactByFormat(r.Context(), id, format)
	if err != nil {
		s.reportStoreError(w, r, "find report artifact", err)
		return
	}
	s.serveArtifact(w, r, artifact)
}

// downloadReportArtifactByID is the artifact-addressed path, which is what a
// share link and a stored bookmark resolve through — an id survives a
// re-generation that changes which file answers `?format=pdf`.
func (s *Server) downloadReportArtifactByID(w http.ResponseWriter, r *http.Request) {
	runID, ok := s.reportRunID(w, r)
	if !ok {
		return
	}
	artifactID, valid := model.ParseID(r.PathValue("artifactId"))
	if !valid {
		s.reportNotFound(w, r)
		return
	}

	artifact, err := s.store.GetReportArtifact(r.Context(), artifactID)
	if err != nil {
		s.reportStoreError(w, r, "get report artifact", err)
		return
	}
	if artifact.ReportRunID != runID {
		// Addressed under the wrong run. A 404 rather than serving it anyway:
		// the pair is the address, and honouring a mismatched one would make the
		// run id decorative on a path that a share link resolves.
		s.reportNotFound(w, r)
		return
	}
	s.serveArtifact(w, r, artifact)
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, a model.ReportArtifact) {
	switch a.State {
	case model.ArtifactExpired:
		// **410, not 404.** Retention reclaimed the bytes and kept the row, so
		// the honest answer to somebody holding a link is "this existed and is
		// gone". The digest and size survive on the row, so what the document
		// was remains answerable after the file is not.
		writeProblem(w, r, s.log, http.StatusGone, "artifact-expired",
			"Report expired", "This report's files were removed by the retention policy. Its digest and size are still recorded.")
		return
	case model.ArtifactFailed:
		// The run knows why, and the client asked for a file that was never
		// written. 409 rather than 404, which is what the spec's download
		// operation already declares.
		writeProblem(w, r, s.log, http.StatusConflict, "artifact-not-rendered",
			"Report not rendered", "This format could not be produced for this run: "+a.Error)
		return
	}

	if s.artifacts == nil || a.Path == "" {
		s.internal(w, r, "serve report artifact", errors.New("no artifact storage configured"))
		return
	}

	body, err := s.artifacts.Open(a.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// **A row whose file is not on disk, and this is 410 rather than 500.**
		//
		// Write-then-commit means normal operation cannot produce one, so an
		// earlier cut of this treated it as an internal fault. A backup drill
		// found that wrong on two counts. The frozen spec declares 200, 401, 404
		// and 410 for this operation and no 500; and ADR-008's Consequences
		// anticipate exactly this state — "a restore of the database against a
		// stale reports directory yields rows whose files are missing" — and
		// require it to render "as a missing file rather than an error page".
		//
		// The operator reaching this has almost always restored `cairn.db`
		// without `<data-dir>/reports/`, which is the silent half of the backup
		// procedure and the one the documentation warns about. "Internal error,
		// the cause has been logged" sends them to a log; naming the missing file
		// sends them to their backup.
		//
		// Still logged, because a missing file on an install that was *not* just
		// restored is a genuine fault and the 410 alone would bury it.
		s.log.Error("report artifact file is missing from disk",
			"artifact_id", a.ID.String(), "path", a.Path, "error", err)
		writeProblem(w, r, s.log, http.StatusGone, "artifact-file-missing",
			"Report file missing",
			"This report's row exists but its file is not in the reports directory. "+
				"The usual cause is a database restored without <data-dir>/reports/; "+
				"its digest and size are still recorded, so the file can be identified in a backup.")
		return
	case err != nil:
		// A permission problem or an I/O error — the file may well be there and
		// this process cannot read it. That is genuinely internal.
		s.internal(w, r, "open report artifact", err)
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", artifactContentType(a.Format))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "report-"+a.ID.String()+artifactExtension(a.Format)))
	if a.SizeBytes > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", a.SizeBytes))
	}
	// The digest is offered so a recipient can check the bytes against the row
	// without asking anybody. It is what makes an artifact evidence.
	if a.SHA256 != "" {
		w.Header().Set("X-Cairn-SHA256", a.SHA256)
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		s.log.Warn("report artifact download interrupted", "error", err, "artifact_id", a.ID.String())
	}
}

func artifactContentType(format string) string {
	switch format {
	case model.FormatPDF:
		return "application/pdf"
	case model.FormatHTML:
		return "text/html; charset=utf-8"
	case model.FormatCSV:
		return "text/csv; charset=utf-8"
	case model.FormatJSON:
		return "application/json"
	}
	return "application/octet-stream"
}

func artifactExtension(format string) string {
	switch format {
	case model.FormatPDF:
		return ".pdf"
	case model.FormatHTML:
		return ".html"
	case model.FormatCSV:
		return ".csv"
	case model.FormatJSON:
		return ".json"
	}
	return ".bin"
}

// --- shared -----------------------------------------------------------------

func (s *Server) reportTemplateID(w http.ResponseWriter, r *http.Request) (model.ID, bool) {
	id, ok := model.ParseID(r.PathValue("reportTemplateId"))
	if !ok {
		s.reportNotFound(w, r)
		return model.ID{}, false
	}
	return id, true
}

func (s *Server) reportRunID(w http.ResponseWriter, r *http.Request) (model.ID, bool) {
	id, ok := model.ParseID(r.PathValue("reportRunId"))
	if !ok {
		s.reportNotFound(w, r)
		return model.ID{}, false
	}
	return id, true
}

func (s *Server) reportNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "report-not-found",
		"Not found", "No report resource with that identifier exists.")
}

func (s *Server) reportStoreError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.reportNotFound(w, r)
		return
	}
	s.internal(w, r, what, err)
}

// applyReportTemplate merges a write body onto a template, returning what it
// could not accept. Absent fields are left alone; present ones replace.
func applyReportTemplate(t *model.ReportTemplate, body reportTemplateWrite) []ValidationItem {
	var problems []ValidationItem

	if body.Name != nil {
		if *body.Name == "" {
			problems = append(problems, ValidationItem{Pointer: "/name", Code: "invalid", Message: "name must not be empty"})
		}
		t.Name = *body.Name
	}
	if body.Description != nil {
		t.Description = *body.Description
	}
	if body.Type != nil {
		if !oneOf(*body.Type, model.ReportTypeUptime, model.ReportTypeSLA,
			model.ReportTypePostMortem, model.ReportTypeComparative, model.ReportTypeCustom) {
			problems = append(problems, ValidationItem{Pointer: "/type", Code: "invalid",
				Message: "type must be uptime, sla, post_mortem, comparative or custom"})
		} else {
			t.Type = *body.Type
		}
	}
	if body.Period != nil {
		if !oneOf(*body.Period, model.ReportPeriodDay, model.ReportPeriodWeek, model.ReportPeriodMonth,
			model.ReportPeriodQuarter, model.ReportPeriodYear, model.ReportPeriodCustom) {
			problems = append(problems, ValidationItem{Pointer: "/period", Code: "invalid",
				Message: "period must be day, week, month, quarter, year or custom"})
		} else {
			t.Period = *body.Period
		}
	}
	if body.PeriodStyle != nil {
		if !oneOf(*body.PeriodStyle, model.ReportStyleCalendar, model.ReportStyleRolling) {
			problems = append(problems, ValidationItem{Pointer: "/period_style", Code: "invalid",
				Message: "period_style must be calendar or rolling"})
		} else {
			t.PeriodStyle = *body.PeriodStyle
		}
	}
	if body.MaintenanceHandling != nil {
		if !oneOf(*body.MaintenanceHandling, report.MaintenanceExclude,
			report.MaintenanceCountAsUp, report.MaintenanceCountAsDown) {
			problems = append(problems, ValidationItem{Pointer: "/maintenance_handling", Code: "invalid",
				Message: "maintenance_handling must be exclude, count_as_up or count_as_down"})
		} else {
			t.MaintenanceHandling = *body.MaintenanceHandling
		}
	}

	if len(body.SLATarget) > 0 {
		if clearing(body.SLATarget) {
			t.SLATarget = nil
		} else {
			var target float64
			switch {
			case json.Unmarshal(body.SLATarget, &target) != nil:
				problems = append(problems, ValidationItem{Pointer: "/sla_target", Code: "invalid",
					Message: "sla_target must be a number or null"})
			case target < 0 || target >= 100:
				// Refused here as well as by a CHECK, because 100 has an error
				// budget of zero seconds: burn rate becomes undefined and every
				// report becomes a breach report. The API is the layer where the
				// reason can be said rather than only enforced.
				problems = append(problems, ValidationItem{Pointer: "/sla_target", Code: "invalid",
					Message: "sla_target must be at least 0 and below 100; a target of exactly 100 has an error budget of zero seconds"})
			default:
				t.SLATarget = &target
			}
		}
	}
	if len(body.ResponseTimeTargetMS) > 0 {
		if clearing(body.ResponseTimeTargetMS) {
			t.ResponseTimeTargetMS = nil
		} else {
			var ms int
			if err := json.Unmarshal(body.ResponseTimeTargetMS, &ms); err != nil || ms < 1 {
				problems = append(problems, ValidationItem{Pointer: "/response_time_target_ms", Code: "invalid",
					Message: "response_time_target_ms must be an integer of at least 1, or null"})
			} else {
				t.ResponseTimeTargetMS = &ms
			}
		}
	}

	if body.Scope != nil {
		scope, scopeProblems := parseReportScope(*body.Scope)
		problems = append(problems, scopeProblems...)
		t.Scope = scope
	}
	if len(body.BrandProfileID) > 0 {
		var raw string
		switch {
		case clearing(body.BrandProfileID):
			t.BrandProfileID = nil
		case json.Unmarshal(body.BrandProfileID, &raw) != nil:
			problems = append(problems, ValidationItem{Pointer: "/brand_profile_id", Code: "invalid", Message: "not a uuid"})
		default:
			if id, ok := model.ParseID(raw); ok {
				t.BrandProfileID = &id
			} else {
				problems = append(problems, ValidationItem{Pointer: "/brand_profile_id", Code: "invalid", Message: "not a uuid"})
			}
		}
	}
	if len(body.Comparison) > 0 {
		if clearing(body.Comparison) {
			t.Comparison = nil
		} else {
			comparison, comparisonProblems := parseReportComparison(body.Comparison)
			problems = append(problems, comparisonProblems...)
			if len(comparisonProblems) == 0 {
				t.Comparison = comparison
			}
		}
	}

	// **A comparative template with no comparison is refused at write time**
	// rather than discovered by a run that produces a report with nothing on it.
	// It is the same rule the schedule handler applies to a cron that matches
	// nothing: a definition that cannot do what its type says is a mistake
	// somebody is standing in front of a screen to fix, not a silence to
	// discover next month.
	if t.Type == model.ReportTypeComparative && t.Comparison == nil {
		problems = append(problems, ValidationItem{Pointer: "/comparison", Code: "required",
			Message: "a comparative report needs a comparison; choose previous_period, or name the monitors or groups to compare"})
	}
	// And the converse: a comparison on a template of any other type is
	// configuration nothing will read, which is worth refusing while the reason
	// is on the screen rather than storing and ignoring.
	if t.Comparison != nil && t.Type != model.ReportTypeComparative {
		problems = append(problems, ValidationItem{Pointer: "/comparison", Code: "conflict",
			Message: "comparison applies to a comparative report; this template's type is " + t.Type})
	}

	if body.Sections != nil {
		t.Sections = *body.Sections
	}
	if body.Formats != nil {
		for i, format := range *body.Formats {
			if !oneOf(format, model.FormatPDF, model.FormatHTML, model.FormatCSV, model.FormatJSON) {
				problems = append(problems, ValidationItem{
					Pointer: fmt.Sprintf("/formats/%d", i), Code: "invalid",
					Message: "format must be pdf, html, csv or json"})
			}
		}
		t.Formats = *body.Formats
	}
	return problems
}

func parseReportScope(in reportScopeJSON) (model.ReportScope, []ValidationItem) {
	var (
		scope    model.ReportScope
		problems []ValidationItem
	)

	collect := func(field string, raw []string) []model.ID {
		out := make([]model.ID, 0, len(raw))
		for i, s := range raw {
			id, ok := model.ParseID(s)
			if !ok {
				problems = append(problems, ValidationItem{
					Pointer: fmt.Sprintf("/scope/%s/%d", field, i), Code: "invalid", Message: "not a uuid"})
				continue
			}
			out = append(out, id)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}

	scope.MonitorIDs = collect("monitor_ids", in.MonitorIDs)
	scope.GroupIDs = collect("group_ids", in.GroupIDs)
	scope.TagIDs = collect("tag_ids", in.TagIDs)

	if in.IncidentID != nil && *in.IncidentID != "" {
		if id, ok := model.ParseID(*in.IncidentID); ok {
			scope.IncidentID = &id
		} else {
			problems = append(problems, ValidationItem{Pointer: "/scope/incident_id", Code: "invalid", Message: "not a uuid"})
		}
	}
	return scope, problems
}

func oneOf(value string, allowed ...string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

// parseReportComparison reads the comparison block.
//
// The two entity modes need something to compare, and an empty list is refused
// rather than accepted: "compare these monitors" naming no monitors produces a
// report with one column, which reads as a rendering bug rather than as a
// configuration mistake.
func parseReportComparison(raw json.RawMessage) (*model.ReportComparison, []ValidationItem) {
	var body reportComparisonJSON
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, []ValidationItem{{Pointer: "/comparison", Code: "invalid",
			Message: "comparison must be an object or null"}}
	}

	if !oneOf(body.Mode, report.CompareToPreviousPeriod, report.CompareMonitors, report.CompareGroups) {
		return nil, []ValidationItem{{Pointer: "/comparison/mode", Code: "invalid",
			Message: "mode must be previous_period, monitors or groups"}}
	}

	out := &model.ReportComparison{Mode: body.Mode}
	var problems []ValidationItem

	for _, raw := range body.MonitorIDs {
		id, ok := model.ParseID(raw)
		if !ok {
			problems = append(problems, ValidationItem{Pointer: "/comparison/monitor_ids",
				Code: "invalid", Message: "not a uuid: " + raw})
			continue
		}
		out.MonitorIDs = append(out.MonitorIDs, id)
	}
	for _, raw := range body.GroupIDs {
		id, ok := model.ParseID(raw)
		if !ok {
			problems = append(problems, ValidationItem{Pointer: "/comparison/group_ids",
				Code: "invalid", Message: "not a uuid: " + raw})
			continue
		}
		out.GroupIDs = append(out.GroupIDs, id)
	}

	switch {
	case body.Mode == report.CompareMonitors && len(out.MonitorIDs) < 2:
		problems = append(problems, ValidationItem{Pointer: "/comparison/monitor_ids",
			Code: "invalid", Message: "comparing monitors needs at least two of them"})
	case body.Mode == report.CompareGroups && len(out.GroupIDs) < 2:
		problems = append(problems, ValidationItem{Pointer: "/comparison/group_ids",
			Code: "invalid", Message: "comparing groups needs at least two of them"})
	}

	return out, problems
}
