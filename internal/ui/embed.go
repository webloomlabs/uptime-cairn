// Package ui embeds the built frontend into the binary.
//
// One artefact: the dashboard ships inside the same binary as the API and the
// probe, which is what makes docker run into first-monitor-in-60-seconds
// possible with no web server to configure (PHASE-1-PLAN.md §2, §4.2).
//
// The build order is: web/ produces static output, that output is copied into
// dist/ here, and then the Go binary is built. dist/ is generated and
// gitignored apart from the placeholder, so a clean checkout still compiles —
// //go:embed on an empty or missing directory is a compile error, and a
// developer who has never run the frontend build should still be able to build
// the server.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// FS returns the embedded frontend rooted at dist/, ready to serve.
func FS() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
