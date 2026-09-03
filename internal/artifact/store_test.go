package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

var march = time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir(), 0)
}

// The layout, the permissions, and the digest, checked on the disk rather than
// on the return value. An artifact is evidence: "is this the document we sent?"
// has to be answerable, and truncation from a full disk has to be detectable
// rather than silent.
func TestAnArtifactLandsWhereADRSaysWithItsDigest(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	id := model.NewID()
	data := []byte("uptime report")

	got, err := s.Write(id, model.FormatPDF, march, data)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	want := "2026/03/" + id.String() + ".pdf"
	if got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}
	if got.SizeBytes != int64(len(data)) {
		t.Errorf("size = %d, want %d", got.SizeBytes, len(data))
	}
	// Checked against a digest computed here rather than restated as a literal,
	// so the test proves the bytes and not a copy-paste.
	if got.SHA256 != sha256Of(data) {
		t.Errorf("sha256 = %q, want %q", got.SHA256, sha256Of(data))
	}

	full := filepath.Join(s.Root(), "2026", "03", id.String()+".pdf")
	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("file mode = %v, want %v — a report is a client's operational data", perm, filePerm)
	}
	dir, err := os.Stat(filepath.Dir(full))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != dirPerm {
		t.Errorf("directory mode = %v, want %v", perm, dirPerm)
	}

	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(data) {
		t.Errorf("stored bytes = %q, want %q", body, data)
	}
}

// **The path has no user input in it at all**, which is a stronger property than
// escaping one. A report definition titled `../../etc` is ordinary free text and
// must not be able to reach outside the reports directory — and here it cannot,
// because the title never touches the path.
func TestThePathIsBuiltFromIdsAndNeverFromATitle(t *testing.T) {
	t.Parallel()

	id := model.NewID()
	rel := RelPath(id, model.FormatCSV, march)

	if strings.Contains(rel, "..") {
		t.Fatalf("path %q contains a traversal", rel)
	}
	if !strings.HasSuffix(rel, id.String()+".csv") {
		t.Errorf("path %q is not named for the artifact id", rel)
	}
	// Sharded, so no single directory grows to the size of an install's history.
	if !strings.HasPrefix(rel, "2026/03/") {
		t.Errorf("path %q is not sharded by year and month", rel)
	}
}

// A stored path is one hand-edit away from being a traversal, and the cost of
// refusing one is a comparison. Nothing outside the root is reachable through
// Open or Remove even when the row says otherwise.
func TestATraversingPathCannotReachOutsideTheRoot(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	outside := filepath.Join(filepath.Dir(s.Root()), "cairn.db")
	if err := os.WriteFile(outside, []byte("the database"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, attempt := range []string{"../cairn.db", "2026/../../cairn.db", "/../cairn.db"} {
		if r, err := s.Open(attempt); err == nil {
			_ = r.Close()
			t.Errorf("opened %q from outside the artifact root", attempt)
		}
		if err := s.Remove(attempt); err != nil {
			t.Logf("remove %q refused: %v", attempt, err)
		}
	}

	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a traversing path reached the database file: %v", err)
	}
}

// The cap names the limit and the size reached. "Too large" without either is a
// support conversation; the case that hits it is a CSV over five thousand
// monitors for a year, not a PDF.
func TestTheSizeCapNamesWhatWasExceeded(t *testing.T) {
	t.Parallel()

	s := New(t.TempDir(), 1024)
	_, err := s.Write(model.NewID(), model.FormatCSV, march, make([]byte, 4096))

	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	for _, want := range []string{"csv", "4.0 KB", "1.0 KB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// And nothing was written, so a refused artifact does not leave residue for
	// the sweeper to find.
	entries, _ := os.ReadDir(s.Root())
	if len(entries) != 0 {
		t.Errorf("a refused write left %d entries behind", len(entries))
	}
}

// A file at its final path is always complete. The bytes are written to a
// temporary name and renamed in, so a crash mid-write cannot leave something
// that reads as a real artifact to anything short of the digest.
func TestNoPartialFileIsLeftAtTheFinalPath(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	if _, err := s.Write(model.NewID(), model.FormatJSON, march, []byte("{}")); err != nil {
		t.Fatal(err)
	}

	var partials []string
	_ = filepath.WalkDir(s.Root(), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".partial") {
			partials = append(partials, p)
		}
		return nil
	})
	if len(partials) != 0 {
		t.Errorf("temporary files left behind: %v", partials)
	}
}

// The orphan sweeper walks the disk and checks the rows, which is the only
// correct direction: the other way round finds missing files, a different fault
// with a different answer.
func TestOrphansAreFoundByWalkingTheDiskAgainstTheRows(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	kept, _ := s.Write(model.NewID(), model.FormatPDF, march, []byte("kept"))
	orphan, _ := s.Write(model.NewID(), model.FormatPDF, march, []byte("orphan"))

	age(t, s, orphan.Path, -2*time.Hour)
	age(t, s, kept.Path, -2*time.Hour)

	live := map[string]bool{kept.Path: true}
	found, err := s.Orphans(live, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0] != orphan.Path {
		t.Fatalf("orphans = %v, want just %q", found, orphan.Path)
	}
}

// **The grace period is a correctness requirement, not a tuning knob.** ADR-008
// writes the file before it commits the row, so there is always an interval in
// which a perfectly good artifact has bytes and no row. A sweeper with no grace
// races a running report and deletes the file out from under it, and the failure
// looks like a disk fault rather than like a bug in the sweeper.
func TestAFileYoungerThanTheGraceIsNotAnOrphan(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	// Written a moment ago and not yet in any row: exactly the state of an
	// artifact whose transaction has not committed.
	justWritten, _ := s.Write(model.NewID(), model.FormatHTML, march, []byte("in flight"))

	found, err := s.Orphans(map[string]bool{}, time.Now().Add(-DefaultSweepGrace))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range found {
		if p == justWritten.Path {
			t.Fatal("the sweeper would have deleted an artifact that is still being committed")
		}
	}
}

// An install that has never generated a report has no reports directory, and
// sweeping it is not an error.
func TestSweepingBeforeTheFirstReportIsNotAnError(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	found, err := s.Orphans(map[string]bool{}, time.Now().Add(-DefaultSweepGrace))
	if err != nil {
		t.Fatalf("sweep with no directory: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("orphans = %v, want none", found)
	}
}

// Removing a file that is already gone is not an error. Retention runs after a
// restore from a backup taken before the artifact was written, and a sweep that
// failed on the first such row would stop reclaiming everything behind it.
func TestRemovingAnAbsentFileIsNotAnError(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	written, _ := s.Write(model.NewID(), model.FormatPDF, march, []byte("x"))

	if err := s.Remove(written.Path); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := s.Remove(written.Path); err != nil {
		t.Errorf("second remove: %v, want nil", err)
	}
}

// Round trip: what Write stored is what Open returns.
func TestOpenReturnsWhatWriteStored(t *testing.T) {
	t.Parallel()

	s := newStore(t)
	data := []byte("<html>report</html>")
	written, err := s.Write(model.NewID(), model.FormatHTML, march, data)
	if err != nil {
		t.Fatal(err)
	}

	r, err := s.Open(written.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("read %q, want %q", got, data)
	}
}

// The directory is dated from the run rather than from the clock, so
// regenerating last March's report files it under last March.
func TestTheDirectoryIsDatedFromTheRunNotTheClock(t *testing.T) {
	t.Parallel()

	rel := RelPath(model.NewID(), model.FormatPDF, time.Date(2024, 11, 2, 0, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(rel, "2024/11/") {
		t.Errorf("path %q is not filed under the run's own month", rel)
	}
}

func age(t *testing.T, s *Store, rel string, by time.Duration) {
	t.Helper()
	full := filepath.Join(s.Root(), filepath.FromSlash(rel))
	when := time.Now().Add(by)
	if err := os.Chtimes(full, when, when); err != nil {
		t.Fatal(err)
	}
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
