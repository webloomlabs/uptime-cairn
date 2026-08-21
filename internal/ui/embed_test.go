package ui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The embed pattern is the thing under test, and it fails in a way nothing else
// catches: `//go:embed dist` silently skips every path beginning with `_` or
// `.`, and SvelteKit puts the whole application bundle under `_app/`. The binary
// still compiles, still starts, and still serves an index.html that references
// files it does not contain — a blank dashboard with nothing in the log.
//
// The assertion is a relation rather than a fixed list: whatever index.html
// references must be present. A clean checkout carries only the placeholder,
// which references nothing, so this is quietly satisfied until somebody actually
// builds the frontend — which is exactly when the bug would otherwise appear.

var assetRef = regexp.MustCompile(`["'(]([./]*_app/[^"')]+)`)

func TestEmbeddedAssetsAreReachable(t *testing.T) {
	t.Parallel()

	assets, err := FS()
	if err != nil {
		t.Fatalf("open embedded assets: %v", err)
	}

	// A clean checkout embeds only .gitkeep, and building the server without ever
	// running the frontend toolchain is a supported thing to do — so no build is a
	// skip, not a failure. This test has something to say only once there is a
	// build to check.
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		t.Skip("no frontend build in this tree; run npm run build in web/")
	}

	matches := assetRef.FindAllStringSubmatch(string(index), -1)
	if len(matches) == 0 {
		t.Skip("the embedded index.html references no bundled assets")
	}

	for _, match := range matches {
		name := strings.TrimLeft(match[1], "./")
		if _, err := fs.Stat(assets, name); err != nil {
			t.Errorf("index.html references %s, which is not embedded: %v\n"+
				"the embed pattern in embed.go must be `all:dist` — a bare pattern skips _app/", name, err)
		}
	}
}
