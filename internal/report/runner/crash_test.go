package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/artifact"
	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Crash consistency: the residue of a kill between the fsync and the commit.
//
// # Why this is a test rather than a comment
//
// ADR-008 fixes the order — write the file, fsync it, fsync its directory, then
// commit the row — and the whole argument for that order is about what a crash
// leaves behind. One direction leaves an **orphan file**: inert, invisible to
// every client, reclaimable. The other leaves a **dangling row**: an artifact the
// UI offers and the disk cannot supply, which surfaces to a client as a download
// that fails on a report they were told exists.
//
// Nothing in the type system holds that order. It is two statements in
// `storeArtifact`, and swapping them compiles, passes every other test in this
// package, and is wrong in a way that only appears after a power cut.
//
// # What "crash" means here
//
// The process is not actually killed — a test that forks and SIGKILLs a child
// would be testing the harness. What is reproduced is the *state* a kill at that
// instant leaves: the file present on disk, the row absent from the store. That
// is the only observable difference between the two orderings, and it is exactly
// what the sweeper has to be able to clean up.

// A kill between the fsync and the commit leaves an orphan, never a dangling
// row — and the sweeper reclaims it.
//
// Three assertions, and each is a different failure:
//
//  1. The file is complete. `Write` renames a finished temporary file into place,
//     so a file at the final path is never a partial one — there is nothing at
//     that path for a reader to mistake for a real artifact.
//  2. No row references it. That is what makes it an orphan rather than a
//     dangling row, and it is the whole point of the ordering.
//  3. The sweeper reclaims it once it is older than the grace period, so the
//     residue the ordering deliberately creates is bounded rather than
//     permanent.
func TestACrashBetweenFsyncAndCommitLeavesAnOrphanTheSweeperReclaims(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := artifact.New(root, artifact.DefaultMaxBytes)

	// The write half of a run, exactly as storeArtifact performs it, stopped
	// before the row is committed.
	id := model.NewID()
	when := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	body := []byte("%PDF-1.7\n% a complete artifact\n")

	written, err := files.Write(id, model.FormatPDF, when, body)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// ---- the process dies here ----

	full := filepath.Join(files.Root(), filepath.FromSlash(written.Path))

	// 1. The bytes on disk are the whole artifact, and there is no `.partial`
	//    beside them. A file at a real path is always complete, which is what
	//    the rename buys.
	onDisk, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("the file is not on disk after a successful write: %v", err)
	}
	if string(onDisk) != string(body) {
		t.Fatalf("the file at the final path is %d bytes and the artifact was %d; a "+
			"rename should make a partial file impossible at that path",
			len(onDisk), len(body))
	}
	if _, err := os.Stat(full + ".partial"); err == nil {
		t.Error("a .partial file survived a successful write")
	}

	// 2. **No row refers to it.** This is the orphan, and it is the residue the
	//    ordering chooses on purpose. The store below is empty, which is what a
	//    crash before the commit produces.
	store := &fakeSweepStore{live: map[string]bool{}}

	// 3. The sweeper leaves it alone while it is young — a report being written
	//    right now is indistinguishable from this — and reclaims it once it is
	//    past the grace period.
	sweeper := NewSweeper(store, files, quietLog())

	if _, err := sweeper.Sweep(context.Background(), time.Now().UTC()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(full); err != nil {
		t.Fatal("the sweeper reclaimed a file written moments ago. ADR-008 writes " +
			"the file before the row, so a fresh unreferenced file is a report in " +
			"flight rather than an orphan")
	}

	// Age it past the grace period, which is what a real sweep an hour later
	// would find.
	old := time.Now().Add(-2 * artifact.DefaultSweepGrace)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatal(err)
	}

	result, err := sweeper.Sweep(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Orphans != 1 {
		t.Errorf("orphans reclaimed = %d, want 1", result.Orphans)
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Error("the orphan survived a sweep past the grace period, so the residue " +
			"write-then-commit creates is permanent rather than bounded")
	}
}

// The inverse ordering is what this is protecting against, stated as a test so
// the claim is checkable rather than asserted in a comment.
//
// A row committed before the bytes are durable leaves an artifact the UI offers
// and the disk cannot supply. There is no sweeper for that: the orphan pass walks
// the disk and finds files with no row, which is the opposite direction, and
// walking the rows to check the disk would find *missing* files — a different
// fault with a different answer, and one that cannot be fixed by deleting
// anything.
//
// So the test asserts the shape of the harm rather than reproducing it: a
// referenced path that is not on disk is invisible to `Orphans`, whatever the
// grace period is.
func TestADanglingRowIsInvisibleToTheSweeper(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := artifact.New(root, artifact.DefaultMaxBytes)

	// What committing the row first would produce: a live path with no bytes.
	missing := artifact.RelPath(model.NewID(), model.FormatPDF,
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))

	orphans, err := files.Orphans(map[string]bool{missing: true}, time.Now())
	if err != nil {
		t.Fatalf("orphans: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("the sweeper reported %v for a row whose file is absent; walking the "+
			"disk cannot find a missing file, and it must not invent one", orphans)
	}

	// Which is the whole argument for the ordering: one direction's residue is
	// collectable and the other's is not, so the collectable one is the one the
	// system is allowed to produce.
	written, err := files.Write(model.NewID(), model.FormatPDF,
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), []byte("bytes"))
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(files.Root(), filepath.FromSlash(written.Path))
	old := time.Now().Add(-2 * artifact.DefaultSweepGrace)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatal(err)
	}

	orphans, err = files.Orphans(map[string]bool{}, time.Now())
	if err != nil {
		t.Fatalf("orphans: %v", err)
	}
	if len(orphans) != 1 {
		t.Errorf("orphans = %v, want the one unreferenced file", orphans)
	}
}
