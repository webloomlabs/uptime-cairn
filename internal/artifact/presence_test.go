package artifact

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// How much does the presence check actually cost a listing?
//
// It is here because the answer was first guessed at and the guess was wrong.
// The worry was that statting every artifact would be I/O on a hot path "at
// 5,000-monitor scale" — but a page is capped at 100 runs and a run has at most
// four formats, so the worst case is 400 stats. **It is bounded by page size, not
// by estate size**, which is a different axis entirely.
//
// Measured at roughly 3µs a stat, so a worst-case full page costs about 1.3ms on
// a request that already makes two database round trips. That is the number that
// justified doing the check on the listing as well as on the single-run read —
// two screens disagreeing about whether a file exists is worse than either answer
// alone, and this is cheap enough not to have to choose.
//
// A quarter of the paths are deliberately absent, because the miss path is the
// one that matters and an all-hits benchmark would flatter it.
func BenchmarkListingPresenceCheck(b *testing.B) {
	dir := b.TempDir()
	s := New(dir, DefaultMaxBytes)

	var paths []string
	when := time.Now()
	for i := 0; i < 400; i++ {
		id := model.NewID()
		w, err := s.Write(id, model.FormatJSON, when.AddDate(0, -(i%12), 0), []byte("{}"))
		if err != nil {
			b.Fatal(err)
		}
		paths = append(paths, w.Path)
	}
	// The misses.
	for i := 0; i < len(paths); i += 4 {
		_ = os.Remove(filepath.Join(s.Root(), paths[i]))
	}

	b.ResetTimer()
	for b.Loop() {
		for _, p := range paths {
			_ = s.Exists(p)
		}
	}
}
