package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/artifact"
	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// fakeSweepStore is the row half. The file half is the **real**
// artifact.Store against a temporary directory, deliberately: every property
// worth testing here is about what is on the disk afterwards, and a fake
// filesystem would let the test agree with a sweeper that reclaims nothing.
type fakeSweepStore struct {
	expirable []model.ReportArtifact
	live      map[string]bool

	expired []model.ID
	markErr error
}

func (f *fakeSweepStore) ExpirableArtifacts(_ context.Context, _ time.Time, _ int) ([]model.ReportArtifact, error) {
	return f.expirable, nil
}

func (f *fakeSweepStore) MarkArtifactExpired(_ context.Context, id model.ID) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.expired = append(f.expired, id)
	return nil
}

func (f *fakeSweepStore) LiveArtifactPaths(context.Context) (map[string]bool, error) {
	return f.live, nil
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeFile puts bytes at a relative path under the store root and backdates it,
// which is how the grace period gets exercised without sleeping for an hour.
func writeFile(t *testing.T, root, rel string, age time.Duration) string {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(full, when, when); err != nil {
		t.Fatal(err)
	}
	return full
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Retention reclaims the bytes and keeps the row.
//
// This is the whole of ADR-008's tombstone rule in one assertion: the file is
// gone and the artifact was marked rather than deleted, so a client following a
// bookmarked link is told "this existed and is gone" instead of "no such thing".
func TestRetentionReclaimsTheBytesAndKeepsTheRow(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := artifact.New(root, artifact.DefaultMaxBytes)
	rel := "2026/03/report.pdf"
	full := writeFile(t, files.Root(), rel, 2*time.Hour)

	id := model.NewID()
	store := &fakeSweepStore{
		expirable: []model.ReportArtifact{{ID: id, Path: rel, State: model.ArtifactRendered}},
		live:      map[string]bool{},
	}

	result, err := NewSweeper(store, files, quietLog()).Sweep(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Expired != 1 {
		t.Errorf("expired = %d, want 1", result.Expired)
	}
	if exists(full) {
		t.Error("the bytes are still on disk after their retention date")
	}
	if len(store.expired) != 1 || store.expired[0] != id {
		t.Error("the row was not tombstoned; an expired artifact that leaves no row " +
			"answers 404 to a bookmarked link, which is a different and less useful fact than 410")
	}
}

// The orphan pass reclaims what write-then-commit leaves behind.
//
// The residue is real rather than hypothetical: ADR-008 writes the file before it
// commits the row precisely so that a crash leaves bytes with no row, and this is
// the pass that makes that choice affordable.
func TestOrphansAreReclaimed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := artifact.New(root, artifact.DefaultMaxBytes)

	orphan := writeFile(t, files.Root(), "2026/03/orphan.json", 2*time.Hour)
	referenced := writeFile(t, files.Root(), "2026/03/kept.json", 2*time.Hour)

	store := &fakeSweepStore{live: map[string]bool{"2026/03/kept.json": true}}

	result, err := NewSweeper(store, files, quietLog()).Sweep(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Orphans != 1 {
		t.Errorf("orphans = %d, want 1", result.Orphans)
	}
	if exists(orphan) {
		t.Error("an unreferenced file survived the sweep")
	}
	if !exists(referenced) {
		t.Fatal("the sweeper deleted a file the database refers to — every artifact " +
			"on this install is one bad predicate away from the same fate")
	}
}

// **The grace period, and it is the assertion this file exists for.**
//
// A report being written right now has bytes and no row, because that is the
// order ADR-008 mandates. A sweeper with no grace deletes it out from under the
// run, and the failure surfaces as a missing artifact on a run that reported
// success — which reads as a disk fault, not as a bug in the sweeper. It is a
// correctness requirement rather than a tuning knob, and it is checked by
// putting a file exactly where a racing write would put one.
func TestTheGracePeriodProtectsAReportBeingWritten(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := artifact.New(root, artifact.DefaultMaxBytes)

	// A minute old: the row has not been committed yet, so it is not live.
	racing := writeFile(t, files.Root(), "2026/03/in-flight.pdf", time.Minute)
	// Two hours old and equally unreferenced: this one really is residue.
	stale := writeFile(t, files.Root(), "2026/03/residue.pdf", 2*time.Hour)

	store := &fakeSweepStore{live: map[string]bool{}}

	result, err := NewSweeper(store, files, quietLog()).Sweep(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !exists(racing) {
		t.Error("the sweeper deleted a file written a minute ago. ADR-008 writes the " +
			"file before the row, so that file is a report in flight, not an orphan")
	}
	if exists(stale) {
		t.Error("residue older than the grace period was not reclaimed")
	}
	if result.Orphans != 1 {
		t.Errorf("orphans = %d, want 1", result.Orphans)
	}
}

// A temporary file from a crashed write is reclaimed too.
//
// `Write` renames a `.partial` into place, so a crash mid-write leaves one
// behind. It is in no row and never will be, so the orphan pass is the only
// thing that will ever remove it — worth an assertion, because a sweeper that
// only considered files with known extensions would leak one per crash forever.
func TestAHalfWrittenTemporaryFileIsReclaimed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := artifact.New(root, artifact.DefaultMaxBytes)
	partial := writeFile(t, files.Root(), "2026/03/abandoned.pdf.partial", 3*time.Hour)

	store := &fakeSweepStore{live: map[string]bool{}}
	if _, err := NewSweeper(store, files, quietLog()).Sweep(context.Background(), time.Now().UTC()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if exists(partial) {
		t.Error("a .partial from a crashed write survives every sweep, so it leaks one per crash")
	}
}

// One artifact that cannot be reclaimed does not stop the pass.
//
// The alternative is that a single permission problem on one file turns retention
// off for the whole install — silently, because nothing downstream would notice
// the disk had stopped shrinking.
func TestOneFailureDoesNotStopTheSweep(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := artifact.New(root, artifact.DefaultMaxBytes)
	good := writeFile(t, files.Root(), "2026/03/good.pdf", 2*time.Hour)

	store := &fakeSweepStore{
		expirable: []model.ReportArtifact{
			{ID: model.NewID(), Path: "2026/03/good.pdf"},
			{ID: model.NewID(), Path: "2026/03/also-good.pdf"},
		},
		live: map[string]bool{},
	}

	result, err := NewSweeper(store, files, quietLog()).Sweep(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The second row's file does not exist, which artifact.Remove treats as
	// success — a sweep that runs after a restore from an older backup meets
	// exactly that, and failing on the first such row would stop everything
	// behind it.
	if result.Expired != 2 {
		t.Errorf("expired = %d, want 2 — an absent file is not a failure", result.Expired)
	}
	if exists(good) {
		t.Error("the first artifact's bytes survived")
	}
}

// A row that cannot be marked keeps its bytes.
//
// The order is row-then-file, so a failed mark must not be followed by an unlink:
// that would leave a live row promising bytes that are gone, which is the one
// residue this design refuses to produce.
func TestAFailedTombstoneLeavesTheBytesAlone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := artifact.New(root, artifact.DefaultMaxBytes)
	rel := "2026/03/still-referenced.pdf"
	full := writeFile(t, files.Root(), rel, 2*time.Hour)

	store := &fakeSweepStore{
		expirable: []model.ReportArtifact{{ID: model.NewID(), Path: rel}},
		// Still live, because the mark failed. The orphan pass must therefore
		// leave it alone as well.
		live:    map[string]bool{rel: true},
		markErr: errors.New("database is locked"),
	}

	result, err := NewSweeper(store, files, quietLog()).Sweep(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Failures != 1 {
		t.Errorf("failures = %d, want 1", result.Failures)
	}
	if !exists(full) {
		t.Fatal("the bytes were unlinked after the tombstone failed, so the row now " +
			"promises a file that is gone — the one residue row-then-file exists to prevent")
	}
}

// An install that has never generated a report has no reports directory, and a
// sweep over it is a no-op rather than an error. Otherwise every fresh install
// logs a warning every hour about a directory it is correct not to have.
func TestSweepingAnInstallWithNoReportsIsQuiet(t *testing.T) {
	t.Parallel()

	files := artifact.New(t.TempDir(), artifact.DefaultMaxBytes)
	store := &fakeSweepStore{live: map[string]bool{}}

	result, err := NewSweeper(store, files, quietLog()).Sweep(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Expired != 0 || result.Orphans != 0 || result.Failures != 0 {
		t.Errorf("result = %+v, want an empty sweep", result)
	}
}
