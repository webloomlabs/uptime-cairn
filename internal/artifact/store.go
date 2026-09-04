// Package artifact stores rendered report files on local disk.
//
// ADR-008: the database holds the index, the filesystem holds the bytes. The
// three specifics behind that are worth restating because they are what makes it
// a decision rather than a preference — every `VACUUM INTO` backup would grow in
// proportion to the artifacts, fifty concurrent writes would contend with
// heartbeat ingest during the monthly burst, and there is no incremental blob
// access for a hundred-megabyte CSV.
//
// An artifact is a **record, not a cache**. Retention erases the data it was
// computed from, so it cannot be regenerated after the fact; that is why the row
// outlives the bytes as a tombstone, and why the digest is written beside them.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Permissions, matching what the codebase already chooses: 0750 on directories
// as `internal/app` does for the data directory, 0600 on files as the root key
// is written with. A report is a client's operational data and there is no
// second user on the box who needs to read it.
const (
	dirPerm  fs.FileMode = 0o750
	filePerm fs.FileMode = 0o600
)

// DefaultMaxBytes caps one artifact.
//
// The case that hits it is not the PDF: a CSV over 5,000 monitors for a year is
// roughly 1.8 million daily rows, on the order of a hundred megabytes. A cap
// that names what was exceeded and by how much is the difference between a
// refused report and a filled disk.
const DefaultMaxBytes int64 = 256 << 20

// DefaultSweepGrace is how old an unreferenced file must be before the orphan
// sweeper will reclaim it.
//
// **Not a tuning knob — a correctness requirement.** ADR-008 writes the file
// before it commits the row, so there is always an interval in which a perfectly
// good artifact has bytes and no row. A sweeper with no grace period would race
// a running report and delete the file out from under it, and the failure would
// look like a disk fault rather than like a bug in the sweeper.
const DefaultSweepGrace = time.Hour

// ErrTooLarge is returned when an artifact exceeds the cap. Its message names
// the limit and the size reached, because "too large" without either is a
// support conversation rather than an error.
var ErrTooLarge = errors.New("artifact exceeds the size limit")

// Store is the local artifact directory.
type Store struct {
	root     string
	maxBytes int64
}

// New opens <dataDir>/reports. The directory is created on first write rather
// than here, so an install that never generates a report never grows one.
func New(dataDir string, maxBytes int64) *Store {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Store{root: filepath.Join(dataDir, "reports"), maxBytes: maxBytes}
}

// Root is the directory an operator has to include in a backup. Exposed so the
// documentation and the code cannot disagree about where it is.
func (s *Store) Root() string { return s.root }

// Written is what the caller records on the artifact row.
type Written struct {
	// Path is relative to the store root, with forward slashes on every
	// platform. Relative because the absolute location changes when somebody
	// restores a backup to a different data directory, and a database full of
	// absolute paths would then point at nothing.
	Path string

	SizeBytes int64

	// SHA256 is hex of the bytes as written. The point is not corruption
	// detection for its own sake: it is what lets somebody assert that the file
	// restored from a backup is the file that was sent to the client.
	SHA256 string
}

// Write puts one artifact on disk and makes it durable.
//
// The caller commits the database row **after** this returns, never before.
// A crash in between leaves an orphan file, which is inert and reclaimable; the
// reverse order leaves a dangling row, which is an artifact the UI offers and
// the disk cannot supply — a download that fails on a report the client was told
// exists.
//
// `when` dates the directory and comes from the run rather than the clock, so
// regenerating a report for last March files it under last March.
func (s *Store) Write(id model.ID, format string, when time.Time, data []byte) (Written, error) {
	if int64(len(data)) > s.maxBytes {
		return Written{}, fmt.Errorf("%w: %s is %s, limit is %s",
			ErrTooLarge, format, humanBytes(int64(len(data))), humanBytes(s.maxBytes))
	}

	rel := RelPath(id, format, when)
	full := filepath.Join(s.root, filepath.FromSlash(rel))

	if err := os.MkdirAll(filepath.Dir(full), dirPerm); err != nil {
		return Written{}, fmt.Errorf("create artifact directory: %w", err)
	}

	// Written to a temporary name and renamed into place, so that a file at the
	// final path is always complete. The sweeper's grace period covers the race
	// where a finished file has no row yet; this covers the one where a crash
	// leaves a half-written one, which would otherwise be indistinguishable from
	// a real artifact by anything short of the digest.
	tmp := full + ".partial"
	if err := writeSync(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return Written{}, err
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return Written{}, fmt.Errorf("place artifact: %w", err)
	}
	// The rename itself is not durable until the directory entry is. Without
	// this, a power loss can lose a file whose bytes were fsynced.
	if err := syncDir(filepath.Dir(full)); err != nil {
		return Written{}, err
	}

	sum := sha256.Sum256(data)
	return Written{Path: rel, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func writeSync(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return fmt.Errorf("create artifact: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		// The likely cause is a full disk, and it degrades the run rather than
		// aborting the schedule (ADR-008 item 8). The error travels so that the
		// reason recorded against the run is the real one.
		return fmt.Errorf("write artifact: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync artifact: %w", err)
	}
	return f.Close()
}

func syncDir(name string) error {
	d, err := os.Open(name)
	if err != nil {
		return fmt.Errorf("open artifact directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync artifact directory: %w", err)
	}
	return nil
}

// RelPath is the stored path for an artifact: <yyyy>/<mm>/<artifact-id>.<ext>.
//
// **Derived from the artifact id and the format, never from the report title.**
// Titles are free text, so a definition called `../../etc` would otherwise be a
// path. There is no sanitisation step here to get wrong, because there is no
// user input on the path to sanitise — which is a stronger property than any
// amount of escaping.
//
// Sharded by year and month so no single directory grows to the size of an
// install's whole history.
func RelPath(id model.ID, format string, when time.Time) string {
	return path.Join(when.UTC().Format("2006"), when.UTC().Format("01"), id.String()+extension(format))
}

func extension(format string) string {
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

// Open returns the bytes of a stored artifact.
//
// The path comes from a database row this process wrote, so it is trusted — and
// checked anyway. A stored path is one hand-edit or one future bug away from
// being a traversal, and the cost of refusing one here is a comparison.
func (s *Store) Open(rel string) (io.ReadCloser, error) {
	full, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	return f, nil
}

// Exists reports whether an artifact's bytes are actually on disk.
//
// The question is not rhetorical and it is not answerable from the database. A
// row and its file are two stores, and ADR-008's own Consequences name the way
// they come apart: "a restore of the database against a stale reports directory
// yields rows whose files are missing; the artifact list must render that as a
// missing file rather than an error page." This is what lets it.
//
// A stat rather than an open, because the caller wants an answer and not a
// handle — and any error at all counts as absent. A file this process cannot
// stat is one it cannot serve either, so reporting it as available would offer a
// download that fails.
func (s *Store) Exists(rel string) bool {
	if rel == "" {
		return false
	}
	full, err := s.resolve(rel)
	if err != nil {
		return false
	}
	info, err := os.Stat(full)
	return err == nil && !info.IsDir()
}

// Remove reclaims the bytes. The row stays: the caller turns it into a tombstone
// so that a bookmarked link answers "this existed and is gone" rather than "no
// such thing".
//
// A file that is already absent is not an error. Retention runs after a restore
// from a backup taken before the artifact was written, and a sweep that fails on
// the first such row would stop reclaiming anything behind it.
func (s *Store) Remove(rel string) error {
	full, err := s.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove artifact: %w", err)
	}
	return nil
}

func (s *Store) resolve(rel string) (string, error) {
	clean := path.Clean("/" + strings.ReplaceAll(rel, `\`, "/"))
	if clean == "/" {
		return "", fmt.Errorf("artifact path %q is empty", rel)
	}
	return filepath.Join(s.root, filepath.FromSlash(strings.TrimPrefix(clean, "/"))), nil
}

// Orphans lists files under the root that no database row refers to.
//
// This direction, and only this direction. Walking the rows and checking the
// disk finds *missing* files, which is a different fault with a different
// answer; walking the disk and checking the rows finds the residue that
// write-then-commit deliberately leaves behind.
//
// `live` is the set of paths the database believes in. `before` excludes
// anything recent, which is the grace period described on DefaultSweepGrace and
// is what stops the sweeper racing a report that is still being written.
func (s *Store) Orphans(live map[string]bool, before time.Time) ([]string, error) {
	var out []string

	err := filepath.WalkDir(s.root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// No reports directory yet, which is the state of every install
				// that has not generated one.
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(s.root, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if live[rel] {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.ModTime().After(before) {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("sweep artifacts: %w", err)
	}
	return out, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
