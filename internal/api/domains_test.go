package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestNormaliseHostStripsPortsAndCase(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"status.acme.example":     "status.acme.example",
		"STATUS.Acme.Example":     "status.acme.example",
		"status.acme.example:443": "status.acme.example",
		"  status.acme.example  ": "status.acme.example",
		"[2001:db8::1]:443":       "2001:db8::1",
		"":                        "",
	} {
		if got := normaliseHost(input); got != want {
			t.Errorf("normaliseHost(%q) = %q, want %q", input, got, want)
		}
	}
}

// The point of the whole mechanism: a customer's hostname shows the customer's
// page at its bare root, with no internal slug in the address bar.
func TestACustomDomainServesItsPageAtTheRoot(t *testing.T) {
	t.Parallel()

	cache := newDomainCache(func(context.Context) (map[string]string, error) {
		return map[string]string{"status.acme.example": "acme"}, nil
	})
	handler := newSPAHandler(fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><head><title>Uptime Cairn</title></head><body></body>")},
	}, cache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "status.acme.example"
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `window.__cairnStatusPage="acme"`) {
		t.Errorf("the shell does not carry the slug:\n%s", body)
	}
	// A cache in front of this must not serve one hostname's document for
	// another's, and the same path now produces different documents.
	if rec.Header().Get("Vary") != "Host" {
		t.Errorf("Vary = %q, want Host", rec.Header().Get("Vary"))
	}
}

// Every other hostname gets the dashboard, unmodified. A slug leaking into the
// shell on the install's own hostname would render a status page where the
// dashboard should be.
func TestAnOrdinaryHostGetsTheDashboardShell(t *testing.T) {
	t.Parallel()

	cache := newDomainCache(func(context.Context) (map[string]string, error) {
		return map[string]string{"status.acme.example": "acme"}, nil
	})
	handler := newSPAHandler(fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><head><title>Uptime Cairn</title></head><body></body>")},
	}, cache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "cairn.example.com"
	handler.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "__cairnStatusPage") {
		t.Error("the dashboard's own hostname was served a status page slug")
	}
}

// An unreadable domain map is not a reason to fail a request. The consequence
// of failing open is a custom domain showing the dashboard until the store
// answers; the consequence of failing closed would be the login page 500ing.
func TestAFailedLookupServesTheDashboardRatherThanFailing(t *testing.T) {
	t.Parallel()

	cache := newDomainCache(func(context.Context) (map[string]string, error) {
		return nil, context.DeadlineExceeded
	})
	handler := newSPAHandler(fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><head></head><body></body>")},
	}, cache)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "status.acme.example"
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want the dashboard rather than a failure", rec.Code)
	}
}

// A write to a status page drops the cache, because the one moment somebody is
// watching for a custom domain to start working is right after they saved it.
func TestInvalidateMakesTheNextLookupReadAgain(t *testing.T) {
	t.Parallel()

	loads := 0
	cache := newDomainCache(func(context.Context) (map[string]string, error) {
		loads++
		return map[string]string{"status.acme.example": "acme"}, nil
	})

	if _, ok := cache.slugFor(context.Background(), "status.acme.example"); !ok {
		t.Fatal("first lookup missed")
	}
	if _, ok := cache.slugFor(context.Background(), "status.acme.example"); !ok {
		t.Fatal("second lookup missed")
	}
	if loads != 1 {
		t.Errorf("%d loads for two lookups inside the TTL, want 1", loads)
	}

	cache.invalidate()
	if _, ok := cache.slugFor(context.Background(), "status.acme.example"); !ok {
		t.Fatal("lookup after invalidate missed")
	}
	if loads != 2 {
		t.Errorf("%d loads after invalidate, want 2", loads)
	}
}
