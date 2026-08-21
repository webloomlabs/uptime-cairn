package api

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// Serving the embedded dashboard.
//
// The dashboard is a single-page application: every route is resolved in the
// browser, so the server has to answer a deep path it has never heard of with
// index.html and let the client route it. Without that, three URLs that are
// written down in the frozen contract break —
// `/status/{slug}`, `/subscriptions/confirm/{token}` and
// `/subscriptions/unsubscribe/{token}` (docs/api/README.md) — and they are
// exactly the ones that arrive in somebody's inbox and cannot be changed
// afterwards.
//
// The fallback is deliberately not "anything that 404s becomes index.html". A
// missing script served as HTML turns a broken build into a MIME-type error in
// the browser console, three layers away from the cause. So a request that looks
// like an asset — it has an extension, and it is not a document — is answered
// honestly with 404, and only document-shaped paths fall back.

const indexFile = "index.html"

// spaHandler serves the built frontend with SPA fallback.
type spaHandler struct {
	assets fs.FS
	files  http.Handler
}

func newSPAHandler(assets fs.FS) http.Handler {
	return &spaHandler{assets: assets, files: http.FileServerFS(assets)}
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// A write to a static asset is a client bug, and 405 says which kind.
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		h.serveIndex(w, r)
		return
	}

	if info, err := fs.Stat(h.assets, name); err == nil && !info.IsDir() {
		// Content-addressed assets: SvelteKit puts a hash of the contents in the
		// filename, so the file at a given path can never change. Everything else
		// is revalidated, because index.html naming the wrong asset bundle is a
		// dashboard that stays broken until somebody clears their cache.
		if strings.HasPrefix(name, "_app/immutable/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.files.ServeHTTP(w, r)
		return
	}

	if looksLikeAsset(name) {
		http.NotFound(w, r)
		return
	}

	h.serveIndex(w, r)
}

// serveIndex writes the application shell for a route only the client knows.
//
// 200 rather than 404: the URL is valid, the server simply is not the component
// that resolves it. Answering 404 with the shell attached would make every
// client-side route look broken to anything reading status codes, monitoring
// included — which would be a particularly poor joke in this product.
func (h *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	file, err := h.assets.Open(indexFile)
	if err != nil {
		// A binary built without running the frontend toolchain. That is a
		// supported thing to do — a clean checkout compiles the server without
		// Node — so it is answered with an explanation rather than a bare 404
		// that reads like a routing bug. The API is unaffected and says so.
		notBuilt(w)
		return
	}
	defer file.Close()

	content, ok := file.(io.ReadSeeker)
	if !ok {
		notBuilt(w)
		return
	}

	// no-cache rather than no-store: the shell may be revalidated and reused, but
	// it must never be served stale, because it is the file that names which
	// asset bundle this build uses.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A zero modtime keeps ServeContent from emitting Last-Modified, which for an
	// embedded file would be the build machine's clock rather than a fact about
	// the content.
	http.ServeContent(w, r, indexFile, time.Time{}, content)
}

// looksLikeAsset reports whether a path should 404 rather than fall back.
//
// The test is the last segment's extension. `/monitors/abc.example.com` has no
// extension in the sense that matters — `.com` is not a file type this build
// serves — so the rule is an allow-list of what the frontend build actually
// emits, not a guess at what a dotted path means.
func looksLikeAsset(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return false
	}
	switch ext {
	case ".js", ".mjs", ".css", ".map", ".json", ".wasm",
		".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".avif", ".ico",
		".woff", ".woff2", ".ttf", ".otf", ".eot",
		".txt", ".xml", ".webmanifest":
		return true
	default:
		// Including .html: a request for a page that does not exist is still a
		// page, and the client renders its own not-found for it.
		return false
	}
}

// notBuilt explains an empty dist/ to whoever opened the page.
//
// 503 rather than 404: the dashboard is a thing this server has, and it is
// temporarily not present in this build. A 404 would say the address is wrong,
// which would send somebody looking in the wrong place entirely.
func notBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `<!doctype html>
<meta charset="utf-8">
<title>Uptime Cairn</title>
<h1>The dashboard is not in this binary</h1>
<p>This build was compiled without the frontend. The API at <code>/api/v1</code> is
unaffected and fully functional.</p>
<p>To include it: run <code>npm install &amp;&amp; npm run build</code> in <code>web/</code>,
then rebuild the binary. See <code>web/README.md</code>.</p>
`)
}
