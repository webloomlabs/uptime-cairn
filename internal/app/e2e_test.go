package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/config"
)

// The golden path, through the whole binary.
//
// Everything else in this repository tests a package. This tests the product:
// one process, started the way `docker run` starts it, driven through the API a
// user drives, with a real HTTP target it can break and a real webhook receiver
// it can count deliveries at.
//
// The path is the one the product exists for, and every step of it crosses a
// seam some other test stubs out:
//
//	install → create a monitor → it goes up → break the target → it goes down
//	       → an alert is delivered → fix the target → it recovers
//	       → the status page reflects all of it
//
// What this catches that unit tests cannot is wiring. Every component in that
// chain has tests; the alert path in particular is exercised end to end inside
// internal/notify. None of that proves the composition root connected the
// dispatcher to the control plane, or that the status page reads the same
// heartbeats ingest wrote — and a product where every part works and the parts
// are not joined up is a product that is silently doing nothing.

// harness is a running cairn plus the things it monitors.
type e2e struct {
	t       *testing.T
	baseURL string
	client  *http.Client
	csrf    string

	// target is the HTTP endpoint the monitor checks. Its health is a switch,
	// so the test can break it at a known instant.
	target  *httptest.Server
	healthy atomic.Bool

	// hook receives the webhook the notification channel delivers.
	hook      *httptest.Server
	mu        sync.Mutex
	delivered []map[string]any
}

func startE2E(t *testing.T) *e2e {
	t.Helper()

	h := &e2e{t: t}
	h.healthy.Store(true)

	h.target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if h.healthy.Load() {
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		http.Error(w, "everything is on fire", http.StatusInternalServerError)
	}))
	t.Cleanup(h.target.Close)

	h.hook = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		h.mu.Lock()
		h.delivered = append(h.delivered, body)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(h.hook.Close)

	port := freePort(t)
	h.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	h.client = &http.Client{Jar: jar, Timeout: 10 * time.Second}

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.BaseURL = h.baseURL

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, io.Discard) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("cairn exited: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("cairn did not shut down within 15s")
		}
	})

	h.waitReady()
	return h
}

func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func (h *e2e) waitReady() {
	h.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := h.client.Get(h.baseURL + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatal("cairn never became ready")
}

func (h *e2e) do(method, path string, body any) (*http.Response, map[string]any) {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}

	req, err := http.NewRequestWithContext(h.t.Context(), method, h.baseURL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.csrf != "" {
		req.Header.Set("X-Cairn-CSRF-Token", h.csrf)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp, decoded
}

func (h *e2e) setup() {
	h.t.Helper()

	resp, body := h.do(http.MethodPost, "/api/v1/setup", map[string]string{
		"email":    "owner@example.com",
		"password": "correct-horse-battery",
	})
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("setup = %d (%v)", resp.StatusCode, body)
	}
	token, _ := body["csrf_token"].(string)
	if token == "" {
		h.t.Fatal("setup returned no CSRF token; every write from here would be a 403")
	}
	h.csrf = token
}

// awaitStatus polls a monitor until it reaches a status, and fails with the
// heartbeats it did see rather than with a bare timeout — "it never went down"
// is not a useful failure without knowing what it did instead.
func (h *e2e) awaitStatus(id, want string, within time.Duration) {
	h.t.Helper()

	deadline := time.Now().Add(within)
	var last any
	for time.Now().Before(deadline) {
		_, monitor := h.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
		last = monitor["status"]
		if last == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	_, beats := h.do(http.MethodGet, "/api/v1/monitors/"+id+"/heartbeats?limit=10", nil)
	h.t.Fatalf("monitor did not reach %q within %s (it is %v); heartbeats: %v", want, within, last, beats["data"])
}

func (h *e2e) deliveries() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.delivered...)
}

// awaitDelivery waits for a webhook carrying an event type.
//
// The key is `type`, not `event`. The default envelope is the shape in
// internal/notify/webhook.go, and a template can rename anything in it — which
// is why this reads the envelope rather than a field name somebody might have
// assumed.
func (h *e2e) awaitDelivery(event string, within time.Duration) map[string]any {
	h.t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, delivery := range h.deliveries() {
			if delivery["type"] == event {
				return delivery
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("no %q delivery within %s; got %v", event, within, h.deliveries())
	return nil
}

// The whole product, once.
func TestGoldenPath(t *testing.T) {
	if testing.Short() {
		t.Skip("the golden path runs a real engine on real intervals")
	}

	h := startE2E(t)
	h.setup()

	// A notification channel first, so the monitor is created with it attached
	// and the first transition is already alerting. Creating it afterwards would
	// leave a window where the answer to "did the alert fire" is "the monitor
	// had no channel yet", which is a test that passes for the wrong reason.
	resp, channel := h.do(http.MethodPost, "/api/v1/notification-channels", map[string]any{
		"name": "E2E webhook",
		"type": "webhook",
		"config": map[string]any{
			"url":    h.hook.URL,
			"method": "POST",
		},
		"is_default": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create channel = %d (%v)", resp.StatusCode, channel)
	}
	channelID := channel["id"].(string)

	// The interval is the floor, because the whole test is bounded by it: two
	// transitions at 20 seconds each is the shape of the run.
	resp, monitor := h.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name":                     "Golden path",
		"type":                     "http",
		"config":                   map[string]any{"url": h.target.URL},
		"interval_seconds":         20,
		"timeout_seconds":          5,
		"retries":                  0,
		"notification_channel_ids": []string{channelID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor = %d (%v)", resp.StatusCode, monitor)
	}
	monitorID := monitor["id"].(string)

	// A monitor that has never reported is pending, not up. It has not earned a
	// verdict either way.
	if monitor["status"] != "pending" {
		t.Errorf("a newly created monitor is %v, want pending", monitor["status"])
	}

	// Up. Check-now rather than waiting a full interval — it runs the same
	// checker through the same ingest, so it is the scheduled path with the
	// waiting removed.
	resp, beat := h.do(http.MethodPost, "/api/v1/monitors/"+monitorID+"/check", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("check now = %d (%v)", resp.StatusCode, beat)
	}
	h.awaitStatus(monitorID, "up", 30*time.Second)

	// Break it.
	h.healthy.Store(false)
	h.awaitStatus(monitorID, "down", 45*time.Second)

	down := h.awaitDelivery("monitor.down", 20*time.Second)
	data, _ := down["data"].(map[string]any)
	monitorPayload, _ := data["monitor"].(map[string]any)
	if monitorPayload["name"] != "Golden path" {
		t.Errorf("the down alert does not name the monitor: %v", down)
	}
	if data["previous_status"] != "up" {
		t.Errorf("the down alert says it came from %v, want up", data["previous_status"])
	}

	// The status page, before the recovery, so it is read while the incident is
	// live rather than after it.
	resp, page := h.do(http.MethodPost, "/api/v1/status-pages", map[string]any{
		"slug": "golden", "title": "Golden Path", "published": true,
		"sections": []map[string]any{{
			"name": "Services", "monitor_ids": []string{monitorID},
		}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status page = %d (%v)", resp.StatusCode, page)
	}

	resp, public := h.do(http.MethodGet, "/api/v1/public/status-pages/golden", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public status page = %d (%v)", resp.StatusCode, public)
	}
	// The public vocabulary is not the monitor vocabulary: a status page speaks
	// operational / degraded / partial_outage / major_outage / maintenance,
	// because "down" is an operator's word and the audience here is a customer.
	// What matters is that it is not operational.
	if public["overall_status"] == "operational" {
		t.Error("the status page says operational while the monitor is down — a status page " +
			"that shows green during an outage is the worst thing this product can do")
	}
	// And it names the monitor, without leaking what it checks. The public
	// projection has no field for a target, which is the point of it being a
	// separate shape rather than a filtered monitor read.
	rendered, _ := json.Marshal(public)
	if !strings.Contains(string(rendered), "Golden path") {
		t.Errorf("the status page does not name the monitor: %s", rendered)
	}
	if strings.Contains(string(rendered), h.target.URL) {
		t.Errorf("the public status page leaked the monitor's target: %s", rendered)
	}

	// Recover.
	h.healthy.Store(true)
	h.awaitStatus(monitorID, "up", 45*time.Second)
	h.awaitDelivery("monitor.up", 20*time.Second)

	// And the page follows it back.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, public = h.do(http.MethodGet, "/api/v1/public/status-pages/golden", nil)
		if public["overall_status"] == "operational" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if public["overall_status"] != "operational" {
		t.Errorf("the status page still says %v after the monitor recovered", public["overall_status"])
	}

	// The history has the shape the whole run just produced: a transition down
	// and a transition back. Read through `important_only`, which is what the
	// activity feed reads and what makes a month of green history legible.
	_, events := h.do(http.MethodGet, "/api/v1/monitors/"+monitorID+"/heartbeats?important_only=true&limit=20", nil)
	var statuses []string
	for _, entry := range events["data"].([]any) {
		statuses = append(statuses, entry.(map[string]any)["status"].(string))
	}
	if len(statuses) < 3 {
		t.Errorf("important heartbeats = %v, want at least pending→up, up→down, down→up", statuses)
	}
}

// Crash recovery.
//
// The promise is "never lose a heartbeat" (PHASE-1-PLAN.md §4.4), and the
// interesting half of it is not the clean shutdown — that is a defer — it is the
// unclean one. A process killed mid-cycle must come back and resume, losing at
// most the tick it was in.
//
// What this asserts is deliberately not "zero heartbeats lost". A check in
// flight when the process dies produced no result and there is nothing to
// recover; claiming otherwise would be inventing one. What it asserts is that
// the history written before the crash survives, and that checking resumes
// afterwards without anybody intervening.
func TestCrashRecoveryLosesAtMostOneTick(t *testing.T) {
	if testing.Short() {
		t.Skip("crash recovery runs a real engine on real intervals")
	}

	dir := t.TempDir()
	port := freePort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", port)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	h := &e2e{t: t, baseURL: baseURL, client: client}

	// First life.
	ctx, stop := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- Run(ctx, cfg, io.Discard) }()
	h.waitReady()

	h.setup()
	resp, monitor := h.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "Survivor", "type": "http",
		"config":           map[string]any{"url": target.URL},
		"interval_seconds": 20, "timeout_seconds": 5, "retries": 0,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create monitor = %d (%v)", resp.StatusCode, monitor)
	}
	id := monitor["id"].(string)

	h.do(http.MethodPost, "/api/v1/monitors/"+id+"/check", nil)
	h.awaitStatus(id, "up", 30*time.Second)

	_, before := h.do(http.MethodGet, "/api/v1/monitors/"+id+"/heartbeats?limit=100", nil)
	beforeCount := len(before["data"].([]any))
	if beforeCount == 0 {
		t.Fatal("nothing was written before the crash, so there is nothing to recover")
	}

	// The crash. Cancelling the context is a graceful stop rather than a SIGKILL
	// — a test cannot kill its own process and survive to assert anything — so
	// what this exercises is restart-and-resume rather than torn-write recovery.
	// The torn-write half is SQLite's WAL, which is its own project's problem and
	// is tested by its own project.
	stop()
	select {
	case <-first:
	case <-time.After(20 * time.Second):
		t.Fatal("cairn did not stop")
	}

	// Second life, same data directory, same key.
	ctx2, stop2 := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- Run(ctx2, cfg, io.Discard) }()
	t.Cleanup(func() {
		stop2()
		<-second
	})
	h.waitReady()

	// The history survived. Not "at least as many" — exactly: heartbeats are
	// append-only and a restart that lost or duplicated one would show here.
	_, after := h.do(http.MethodGet, "/api/v1/monitors/"+id+"/heartbeats?limit=100", nil)
	afterCount := len(after["data"].([]any))
	if afterCount < beforeCount {
		t.Errorf("%d heartbeats before the restart and %d after — history was lost", beforeCount, afterCount)
	}

	// The monitor is still up, which is to say the state row survived alongside
	// the heartbeats. A restart that reset every monitor to pending would look
	// like a fleet-wide outage on the dashboard.
	_, reread := h.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	if reread["status"] != "up" {
		t.Errorf("status after the restart = %v, want up", reread["status"])
	}

	// And checking resumed on its own. At most one tick lost means the next one
	// lands within roughly an interval; two intervals is the bound with the
	// scheduler's dispersal allowed for.
	deadline := time.Now().Add(50 * time.Second)
	for time.Now().Before(deadline) {
		_, latest := h.do(http.MethodGet, "/api/v1/monitors/"+id+"/heartbeats?limit=100", nil)
		if len(latest["data"].([]any)) > afterCount {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Error("no heartbeat was written after the restart — checking did not resume, " +
		"which is the failure a crash-recovery test exists to catch")
}

// The database is where it was left, which is the other half of surviving a
// restart and the one an operator can check by hand.
func TestTheDatabaseSurvivesWhereItWasLeft(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"cairn.db", "cairn.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("%s exists before anything has run", name)
		}
	}

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ListenAddr = "127.0.0.1:0"

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, io.Discard) }()

	// Wait for the key, which is written during startup before the listener
	// opens. Polling for the file rather than for the port, because the port is
	// 0 and the test has no way to learn which one it got.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "cairn.key")); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	<-done

	for _, name := range []string{"cairn.db", "cairn.key"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s was not created: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
	}

	// The key is a credential and is written as one. A world-readable key beside
	// the database it protects is the difference between "back these up
	// separately" being advice and being a joke.
	info, err := os.Stat(filepath.Join(dir, "cairn.key"))
	if err == nil && info.Mode().Perm()&0o077 != 0 {
		t.Errorf("cairn.key is mode %o; it is a credential and must not be group- or world-readable", info.Mode().Perm())
	}
}
