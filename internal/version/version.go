// Package version carries the build identity of this binary.
//
// The values are set at link time by the release build:
//
//	go build -ldflags "-X github.com/webloomlabs/uptime-cairn/internal/version.Version=v0.1.0 …"
//
// They are variables rather than constants for exactly that reason. A build with
// none of them set reports "dev", which is what a developer's own build should
// say — a local build claiming to be v0.1.0 is how an unreproducible binary ends
// up in a bug report.
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the semver tag, or "dev".
	Version = "dev"
	// Commit is the full git SHA the artefact was built from.
	Commit = "unknown"
	// BuildDate is an RFC 3339 timestamp, and must come from the commit rather
	// than the clock: reproducible builds are a Phase 0 §3.6 deliverable and a
	// wall-clock timestamp defeats them on its own.
	BuildDate = "unknown"
)

// String is what --version prints and what bug reports quote.
func String() string {
	return fmt.Sprintf("uptime-cairn %s (%s, built %s, %s %s/%s)",
		Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
