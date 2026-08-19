package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/controlplane"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
	"github.com/webloomlabs/uptime-cairn/internal/store/sqlite"
)

type noopNotifier struct{}

func (noopNotifier) Notify() {}

// testServer is a real server over a real database: the authentication path
// runs through the same storage and the same middleware it does in production,
// because a fake store here would only prove the fake agrees with itself.
func testServer(t *testing.T) *httptest.Server {
	t.Helper()

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "cairn.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	dek, err := secrets.NewDataKey()
	if err != nil {
		t.Fatalf("data key: %v", err)
	}
	keeper, err := secrets.NewKeeper(1, map[uint32][]byte{1: dek})
	if err != nil {
		t.Fatalf("keeper: %v", err)
	}

	registry := check.NewRegistry()
	registry.Register(check.NewHTTP())

	// A real control plane, not a stand-in: push ingest is only interesting if
	// the heartbeat it produces goes through the same state machine every other
	// result does.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cp := controlplane.New(store, controlplane.NewPublisher(), log,
		model.EmbeddedProbeID, model.SentinelOrgID)

	api := New(store, noopNotifier{}, cp, registry, keeper, log, "Test Instance")

	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server
}

type client struct {
	t      *testing.T
	base   string
	http   *http.Client
	csrf   string
	bearer string
}

func newClient(t *testing.T, server *httptest.Server) *client {
	t.Helper()

	jar := &cookieJar{}
	return &client{t: t, base: server.URL, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (*http.Response, map[string]any) {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(c.t.Context(), method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set(csrfHeader, c.csrf)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp, decoded
}

// setup completes first-run setup and keeps the session.
func (c *client) setup() {
	c.t.Helper()

	resp, body := c.do(http.MethodPost, "/api/v1/setup", map[string]string{
		"email":    "owner@example.com",
		"password": "correct-horse-battery",
	})
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("setup = %d, want 201 (%v)", resp.StatusCode, body)
	}
	csrf, _ := body["csrf_token"].(string)
	c.csrf = csrf
}

// cookieJar is the smallest jar that keeps one host's cookies.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	for _, incoming := range cookies {
		replaced := false
		for i, existing := range j.cookies {
			if existing.Name == incoming.Name {
				j.cookies[i] = incoming
				replaced = true
			}
		}
		if !replaced {
			j.cookies = append(j.cookies, incoming)
		}
	}
}

func (j *cookieJar) Cookies(*url.URL) []*http.Cookie {
	var live []*http.Cookie
	for _, c := range j.cookies {
		if c.MaxAge >= 0 && c.Value != "" {
			live = append(live, c)
		}
	}
	return live
}
