package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// Serving the dashboard, and the one rule that is not cosmetic: the three URLs
// subscriber mail carries are client-side routes, and a plain file server
// answers every one of them with 404. Those links are in people's inboxes and
// cannot be reissued, so they get a test rather than a comment.

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                       {Data: []byte("<!doctype html><title>Cairn</title>")},
		"favicon.svg":                      {Data: []byte("<svg/>")},
		"_app/immutable/entry/app.j7Fk.js": {Data: []byte("export default 1")},
	}
}

func TestSPAServesShellForClientRoutes(t *testing.T) {
	t.Parallel()

	handler := newSPAHandler(testAssets())

	// Every path here is one the frontend routes and the server has never heard
	// of. The two subscription links are the ones that matter most: they are
	// followed once, months after they were sent, by somebody who cannot ask for
	// a new one.
	paths := []string{
		"/",
		"/monitors",
		"/monitors/01JQ0000000000000000000000",
		"/status/acme",
		"/subscriptions/confirm/abcdef0123456789",
		"/subscriptions/unsubscribe/abcdef0123456789",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200", path, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("%s content type = %q", path, got)
			}
			if body := rec.Body.String(); body == "" {
				t.Errorf("%s served an empty shell", path)
			}
			// The shell names which asset bundle this build uses. A cached one
			// outliving a deploy is a dashboard that stays broken until somebody
			// clears their browser.
			if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
				t.Errorf("%s cache-control = %q, want no-cache", path, got)
			}
		})
	}
}

// A missing asset must not become the shell. Serving HTML where a script was
// asked for turns a broken build into a MIME error in a console three layers
// from the cause.
func TestSPADoesNotFallBackForAssets(t *testing.T) {
	t.Parallel()

	handler := newSPAHandler(testAssets())

	for _, path := range []string{
		"/_app/immutable/entry/missing.js",
		"/styles.css",
		"/logo.png",
		"/manifest.webmanifest",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

func TestSPAServesRealFiles(t *testing.T) {
	t.Parallel()

	handler := newSPAHandler(testAssets())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_app/immutable/entry/app.j7Fk.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("hashed asset = %d, want 200", rec.Code)
	}
	// The filename contains a hash of the contents, so the bytes at that path can
	// never change and the browser never needs to ask again.
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("hashed asset cache-control = %q", got)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.svg", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("favicon = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("unhashed asset cache-control = %q, want no-cache", got)
	}
}

// A monitor named for a hostname is a route with a dot in it. Treating every
// dotted path as a file would 404 the page for `example.com`.
func TestSPAFallsBackForDottedRoutes(t *testing.T) {
	t.Parallel()

	handler := newSPAHandler(testAssets())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status/status.example.com", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("dotted client route = %d, want 200", rec.Code)
	}
}

func TestSPARejectsWrites(t *testing.T) {
	t.Parallel()

	handler := newSPAHandler(testAssets())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/status/acme", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to a client route = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q", got)
	}
}

// Path traversal, which a file server handles and a hand-written fallback can
// undo by accident.
func TestSPARejectsTraversal(t *testing.T) {
	t.Parallel()

	handler := newSPAHandler(fstest.MapFS{
		"index.html": {Data: []byte("shell")},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = "/../../etc/passwd"
	handler.ServeHTTP(rec, req)

	// path.Clean collapses the traversal before the lookup, so the worst case is
	// a client route that does not exist — the shell — never a file outside the
	// embedded tree.
	if rec.Code != http.StatusOK || rec.Body.String() != "shell" {
		t.Errorf("traversal = %d %q", rec.Code, rec.Body.String())
	}
}
