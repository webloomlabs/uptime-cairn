package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/importer/kuma"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The guided import flow.
//
// Same importer as the CLI, through the same seam, for the reason two write
// paths always give: the one that drifts is the one nobody exercises, and for
// an import that is the CLI a user runs exactly once.
//
// Three things about this endpoint are decided rather than incidental.
//
// It runs asynchronously and returns a job to poll. An import of a Kuma install
// with a thousand monitors and a year of history is minutes of work, and a
// request held open for minutes dies at the first proxy between the browser and
// here — which would leave the import running and the user with no way to find
// out how it went.
//
// The uploaded file is written to disk, processed, and deleted, and it is never
// logged. A kuma.db is a file full of somebody's URLs, basic-auth passwords, and
// bot tokens. It is spooled to disk rather than held in memory because a
// multi-instance merge is several of them and a hundred megabytes each is
// ordinary; it is deleted in a defer that runs on every path out, including the
// panic one.
//
// And it is one import at a time. Two concurrent imports would both read the
// existing-names catalogue before either wrote anything, and both would then
// decide the same name was free — producing exactly the duplicate the conflict
// strategy exists to prevent.

// maxImportUpload bounds one request. A Kuma database with a year of history for
// a few hundred monitors runs to a few hundred megabytes; the ceiling is
// generous against that and finite against a request that is not a database at
// all.
const maxImportUpload = 2 << 30 // 2 GiB

// importRunner serialises imports and holds the one in flight.
type importRunner struct {
	mu      sync.Mutex
	running bool
}

func (r *importRunner) start() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

func (r *importRunner) done() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

// importFromKuma accepts one or more kuma.db files and starts the import.
func (s *Server) importFromKuma(w http.ResponseWriter, r *http.Request) {
	if s.imports == nil {
		writeProblem(w, r, s.log, http.StatusNotImplemented, "not-implemented",
			"Import is not available",
			"This build has no importer wired in.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxImportUpload)
	reader, err := r.MultipartReader()
	if err != nil {
		writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-request",
			"Malformed request",
			"Send the databases as multipart/form-data, with each file in a `files` part and the options in an `options` part.")
		return
	}

	// Spooled into a directory of its own so the cleanup is one RemoveAll rather
	// than a list of paths that has to stay in step with what was written.
	spool, err := os.MkdirTemp("", "cairn-import-")
	if err != nil {
		s.internal(w, r, "create import spool", err)
		return
	}
	cleanup := func() { _ = os.RemoveAll(spool) }

	opts := kuma.DefaultOptions()
	var files []kuma.File
	var rawOptions json.RawMessage

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-request",
				"Malformed request", err.Error())
			return
		}

		switch part.FormName() {
		case "options":
			body, err := io.ReadAll(io.LimitReader(part, 1<<16))
			_ = part.Close()
			if err != nil {
				cleanup()
				s.internal(w, r, "read import options", err)
				return
			}
			rawOptions = json.RawMessage(body)
			if err := json.Unmarshal(body, &opts); err != nil {
				cleanup()
				writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-json",
					"Malformed options", err.Error())
				return
			}

		case "files":
			// Named by index rather than by the client's filename, which is
			// attacker-controlled and would otherwise decide a path on this
			// host. The original name survives in the report, where it is data
			// rather than a path.
			path := filepath.Join(spool, fmt.Sprintf("source-%d.db", len(files)))
			written, err := spoolTo(path, part)
			_ = part.Close()
			if err != nil {
				cleanup()
				s.internal(w, r, "spool uploaded database", err)
				return
			}
			if written == 0 {
				continue
			}
			name := part.FileName()
			if name == "" {
				name = fmt.Sprintf("source-%d.db", len(files)+1)
			}
			files = append(files, kuma.File{Path: path, Name: filepath.Base(name)})

		default:
			_ = part.Close()
		}
	}

	if len(files) == 0 {
		cleanup()
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Nothing to import", "Attach at least one kuma.db in a `files` part.",
			ValidationItem{Pointer: "/files", Code: "required", Message: "at least one database is required"})
		return
	}
	switch opts.ConflictStrategy {
	case "", kuma.ConflictSkip, kuma.ConflictRename, kuma.ConflictReplace:
	default:
		cleanup()
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Invalid options", "conflict_strategy must be skip, rename, or replace.",
			ValidationItem{Pointer: "/options/conflict_strategy", Code: "invalid",
				Message: "want skip, rename, or replace"})
		return
	}

	if !s.running.start() {
		cleanup()
		writeProblem(w, r, s.log, http.StatusConflict, "import-in-progress",
			"An import is already running",
			"Two imports at once would both decide the same name was free before either wrote it. "+
				"Wait for the running one to finish.")
		return
	}

	// The job row exists before the work starts, so the 202 hands back something
	// that can be polled immediately rather than a token for a row that may not
	// be there yet.
	at := time.Now().UTC().Truncate(time.Millisecond)
	job := model.ImportJob{
		ID: model.NewID(), OrgID: s.orgID, Source: "kuma",
		State: model.ImportQueued, DryRun: opts.DryRun, Options: rawOptions,
		CreatedAt: at, UpdatedAt: at,
	}
	if err := s.imports.CreateImportJob(r.Context(), job); err != nil {
		s.running.done()
		cleanup()
		s.internal(w, r, "create import job", err)
		return
	}

	go s.runImport(job, files, opts, cleanup)

	writeJSON(w, s.log, http.StatusAccepted, toImportJobJSON(job, nil))
}

// runImport is the background half.
//
// It takes its own context rather than the request's, deliberately: the request
// returned a 202 the moment the job row existed, so the request context is
// already cancelled and running the import on it would cancel it immediately.
// The bound is a timeout, because an import that has been running for an hour
// is stuck rather than thorough.
func (s *Server) runImport(job model.ImportJob, files []kuma.File, opts kuma.Options, cleanup func()) {
	defer s.running.done()
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), importTimeout)
	defer cancel()

	started := time.Now().UTC().Truncate(time.Millisecond)
	job.State = model.ImportRunning
	job.StartedAt = &started
	job.UpdatedAt = started
	if err := s.imports.UpdateImportJob(ctx, job); err != nil {
		s.log.Error("could not mark the import as running", "job", job.ID, "error", err)
	}

	target := kuma.NewTarget(s.imports, s.registry, s.keeper, s.orgID)
	finished, entries, err := kuma.New(target, s.orgID, s.log.With("component", "import")).
		Run(ctx, files, opts)

	// The job keeps the id it was issued: the caller is already polling it.
	finished.ID, finished.OrgID, finished.CreatedAt, finished.Options = job.ID, job.OrgID, job.CreatedAt, job.Options
	for i := range entries {
		entries[i].JobID = job.ID
	}

	// Entries first. A job marked succeeded with no report is worse than a
	// report with no job state, because the first reads as "nothing to see".
	if !opts.DryRun {
		if err := s.imports.AddImportEntries(ctx, entries); err != nil {
			s.log.Error("could not record the import report", "job", job.ID, "error", err)
		}
	} else {
		// A dry run's whole output is its report, so it is recorded even though
		// nothing else was written. The entries carry no target ids, because
		// there are none.
		if err := s.imports.AddImportEntries(ctx, entries); err != nil {
			s.log.Error("could not record the dry-run report", "job", job.ID, "error", err)
		}
	}
	if err := s.imports.UpdateImportJob(ctx, finished); err != nil {
		s.log.Error("could not finish the import job", "job", job.ID, "error", err)
	}

	if err != nil {
		s.log.Error("import failed", "job", job.ID, "error", err)
		return
	}
	// The assignment set has changed by a thousand monitors. Told once, after
	// everything is written, rather than per monitor — which is the same
	// coalescing the settle window in the control plane exists for.
	if !opts.DryRun && s.notify != nil {
		s.notify.Notify()
	}
	s.log.Info("import finished", "job", job.ID, "state", finished.State, "entries", len(entries))
}

// importTimeout bounds one import. An import of a Kuma install with a year of
// history is minutes; an import still running after an hour is stuck, and the
// job reporting that is more use than one that never finishes.
const importTimeout = time.Hour

// getImportJob returns the job and its report.
func (s *Server) getImportJob(w http.ResponseWriter, r *http.Request) {
	if s.imports == nil {
		writeProblem(w, r, s.log, http.StatusNotImplemented, "not-implemented",
			"Import is not available", "This build has no importer wired in.")
		return
	}

	id, ok := model.ParseID(r.PathValue("importJobId"))
	if !ok {
		s.notFound(w, r)
		return
	}

	job, entries, err := s.imports.GetImportJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get import job", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toImportJobJSON(job, entries))
}

// spoolTo streams one uploaded part to disk.
//
// 0600 and a directory only this process can read, because what is being
// written is somebody's monitoring database complete with its credentials, and
// it exists on disk for as long as the import takes and no longer.
func spoolTo(path string, r io.Reader) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	written, err := io.Copy(f, r)
	if err != nil {
		return written, err
	}
	return written, f.Sync()
}
