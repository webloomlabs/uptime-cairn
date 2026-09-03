package runner

import (
	"context"
	"log/slog"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/artifact"
	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The artifact sweeper: the two reclaim passes ADR-008 requires.
//
// They are different jobs that happen to share a directory, and keeping them in
// one type is a decision rather than a convenience — each is the other's safety
// net, and running only one of them is a slow leak in a system nobody is
// watching.
//
//   - **Retention** reclaims the bytes of artifacts whose `expires_at` has
//     passed, leaving the row as an `expired` tombstone.
//   - **Orphans** reclaims files no row refers to at all, which
//     write-then-commit deliberately leaves behind.
//
// # The order within one artifact, and why it is the opposite of the write
//
// Writing is file-then-row, because the residue of a crash is an orphan file:
// inert, invisible to every client, and reclaimable. Deleting is **row-then-file**
// for the same reason and it is the mirror image, not a contradiction. Mark the
// row first and a crash before the unlink leaves a file no row references — an
// orphan again, which the second pass collects. Unlink first and a crash leaves a
// row pointing at bytes that are gone: the UI offers a download, the disk cannot
// supply it, and the failure surfaces to a client as a broken link rather than as
// the honest 410 the tombstone would have given them.
//
// So both directions leave the same reclaimable residue, and neither leaves a
// dangling row. That is the property worth protecting when somebody reorders
// these two statements.

// SweepBatch bounds one retention pass.
//
// The pass is not the only thing running: a bounded batch keeps a first sweep
// after a long outage — potentially every artifact an install ever wrote — from
// holding a write transaction for the length of it. What is left is picked up on
// the next tick — an hour later, which is the right cost for bytes that are
// already past a retention measured in days.
const SweepBatch = 500

// SweepInterval is how often the sweeper wakes.
//
// Hourly rather than per-minute. Retention is measured in days and an artifact
// reclaimed fifty-nine minutes late costs nothing; the orphan pass walks the
// whole reports directory, which is the one part of this that is not free.
const SweepInterval = time.Hour

// SweepStore is the persistence half, declared here by the consumer.
type SweepStore interface {
	ExpirableArtifacts(ctx context.Context, now time.Time, limit int) ([]model.ReportArtifact, error)
	MarkArtifactExpired(ctx context.Context, id model.ID) error
	LiveArtifactPaths(ctx context.Context) (map[string]bool, error)
}

// SweepFiles is the directory half.
type SweepFiles interface {
	Remove(rel string) error
	Orphans(live map[string]bool, before time.Time) ([]string, error)
}

// Sweeper reclaims artifact bytes.
type Sweeper struct {
	store SweepStore
	files SweepFiles
	log   *slog.Logger

	// grace is how old an unreferenced file must be before the orphan pass will
	// touch it. It is a correctness requirement rather than a tuning knob — see
	// artifact.DefaultSweepGrace — and it is a field only so that a test can
	// exercise both sides of it without sleeping for an hour.
	grace time.Duration
}

// NewSweeper builds the sweeper with the standard grace period.
func NewSweeper(store SweepStore, files SweepFiles, log *slog.Logger) *Sweeper {
	return &Sweeper{store: store, files: files, log: log, grace: artifact.DefaultSweepGrace}
}

// SweepResult is what one pass did, returned so the caller can log it and a test
// can assert on it rather than on the disk.
type SweepResult struct {
	Expired int
	Orphans int

	// Failures is how many artifacts could not be reclaimed. They are not an
	// error from Sweep: one unreadable file must not stop the pass, or a single
	// permission problem becomes an unbounded disk.
	Failures int
}

// Run sweeps on a ticker until the context is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			result, err := s.Sweep(ctx, now.UTC())
			if err != nil {
				s.log.Warn("sweep report artifacts", "error", err)
				continue
			}
			// Logged only when it did something. An hourly line saying nothing
			// happened is how an operator learns to stop reading these.
			if result.Expired > 0 || result.Orphans > 0 || result.Failures > 0 {
				s.log.Info("reclaimed report artifacts",
					"expired", result.Expired, "orphans", result.Orphans,
					"failures", result.Failures)
			}
		}
	}
}

// Sweep runs both passes once.
//
// Retention first, then orphans, and the order matters: retention clears the
// path off every row it tombstones, so any file it marked and failed to unlink
// is no longer live and the orphan pass in the same call collects it. Reversed,
// that residue would wait an hour.
func (s *Sweeper) Sweep(ctx context.Context, now time.Time) (SweepResult, error) {
	var result SweepResult

	expired, failures, err := s.expire(ctx, now)
	result.Expired, result.Failures = expired, failures
	if err != nil {
		return result, err
	}

	orphans, orphanFailures, err := s.orphans(ctx, now)
	result.Orphans = orphans
	result.Failures += orphanFailures
	return result, err
}

// expire reclaims the bytes of artifacts past their retention date.
func (s *Sweeper) expire(ctx context.Context, now time.Time) (reclaimed, failures int, err error) {
	rows, err := s.store.ExpirableArtifacts(ctx, now, SweepBatch)
	if err != nil {
		return 0, 0, err
	}

	for _, a := range rows {
		if err := ctx.Err(); err != nil {
			return reclaimed, failures, nil
		}

		// The row first. See the note at the top of this file: a crash between
		// the two statements has to leave an orphan file, never a row promising
		// bytes that are gone.
		if err := s.store.MarkArtifactExpired(ctx, a.ID); err != nil {
			failures++
			s.log.Warn("expire report artifact", "artifact_id", a.ID, "error", err)
			continue
		}

		if a.Path == "" {
			// A tombstone whose path was already cleared, or a row from a failed
			// render that never had bytes. Nothing to unlink and nothing wrong.
			reclaimed++
			continue
		}
		if err := s.files.Remove(a.Path); err != nil {
			// Counted as a failure and left alone: the row is a tombstone now,
			// so the file is unreferenced and the orphan pass will reclaim it.
			// Retrying here would be a second attempt at the same syscall in the
			// same second.
			failures++
			s.log.Warn("remove expired artifact file", "path", a.Path, "error", err)
			continue
		}
		reclaimed++
	}
	return reclaimed, failures, nil
}

// orphans reclaims files no row refers to.
func (s *Sweeper) orphans(ctx context.Context, now time.Time) (reclaimed, failures int, err error) {
	live, err := s.store.LiveArtifactPaths(ctx)
	if err != nil {
		return 0, 0, err
	}

	// **The grace period, and it is load-bearing.** ADR-008 writes the file
	// before the row, so there is always an interval in which a good artifact
	// has bytes and no row. Sweeping without it races a report that is still
	// being written, and the client sees a missing file rather than a bug in the
	// sweeper — which is why the cutoff is computed here rather than left to the
	// caller to remember.
	before := now.Add(-s.grace)

	paths, err := s.files.Orphans(live, before)
	if err != nil {
		return 0, 0, err
	}

	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return reclaimed, failures, nil
		}
		if err := s.files.Remove(rel); err != nil {
			failures++
			s.log.Warn("remove orphaned artifact file", "path", rel, "error", err)
			continue
		}
		reclaimed++
	}
	return reclaimed, failures, nil
}

// The directory half is satisfied by the real store, which is what keeps this
// interface a description of artifact.Store rather than a wish about it.
var _ SweepFiles = (*artifact.Store)(nil)
