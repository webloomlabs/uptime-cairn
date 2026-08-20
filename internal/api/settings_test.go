package api

import (
	"net/http"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/notify"
)

func TestSettingsReadReturnsTheValuesInForce(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	resp, body := c.do(http.MethodGet, "/api/v1/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get settings = %d (%v)", resp.StatusCode, body)
	}

	// A fresh install has no settings row. Returning a document of empty strings
	// would tell an operator nothing about what is actually happening to their
	// data, so the defaults are filled in.
	retention := body["retention"].(map[string]any)
	if retention["raw_days"] != 7.0 {
		t.Errorf("raw_days = %v, want the documented default of 7", retention["raw_days"])
	}
	general := body["general"].(map[string]any)
	if general["instance_name"] != "Test Instance" {
		t.Errorf("instance_name = %v, want the running instance's name", general["instance_name"])
	}
}

func TestRetentionMustKeepCoarserTiersLongerThanFinerOnes(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	// Reversed, history develops a hole in the middle: detail retained past the
	// summary that replaced it.
	resp, body := c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"retention": map[string]any{"raw_days": 30, "rollup_1m_days": 7},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("patch = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if pointer := firstErrorPointer(body); pointer != "/retention" {
		t.Fatalf("pointer = %q, want /retention", pointer)
	}
}

func TestSettingsPatchIsSectionBySection(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	if resp, body := c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"general": map[string]any{"instance_name": "Acme Monitoring", "base_url": "https://cairn.acme.test"},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("patch general = %d (%v)", resp.StatusCode, body)
	}

	// A second patch that names only retention leaves general alone, which is
	// what makes changing one tier possible without restating the instance name.
	resp, body := c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"retention": map[string]any{"raw_days": 14},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch retention = %d (%v)", resp.StatusCode, body)
	}
	general := body["general"].(map[string]any)
	if general["instance_name"] != "Acme Monitoring" {
		t.Fatalf("instance_name = %v, want it preserved", general["instance_name"])
	}
	if body["retention"].(map[string]any)["raw_days"] != 14.0 {
		t.Fatalf("raw_days = %v, want 14", body["retention"].(map[string]any)["raw_days"])
	}
}

// The connection this endpoint was blocking: until instance SMTP had somewhere
// to live, an email channel asking for it was refused at save time.
func TestInstanceSMTPUnblocksTheEmailChannel(t *testing.T) {
	// Deliberately not parallel. The instance relay is package-level state in
	// internal/notify — one relay per process is the whole idea — so a test that
	// sets it has to run while nothing else is reading it. Sequential tests
	// complete before any parallel one resumes, which is what makes this exact.

	c := newClient(t, testServer(t))
	c.setup()

	// Package-level state, so it has to be put back or a later test inherits a
	// relay it did not configure.
	t.Cleanup(func() { notify.SetInstanceSMTP(notify.InstanceSMTP{}) })
	notify.SetInstanceSMTP(notify.InstanceSMTP{})

	resp, body := c.do(http.MethodPost, "/api/v1/notification-channels", map[string]any{
		"name": "Ops mail", "type": "email",
		"config": map[string]any{"to": []string{"ops@example.com"}},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create before settings = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if code := firstErrorCode(body); code != "unconfigured" {
		t.Fatalf("code = %q, want unconfigured", code)
	}

	if resp, body := c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"smtp": map[string]any{
			"host": "smtp.example.com", "port": 587,
			"username": "cairn", "password": "mail-secret",
			"from_address": "cairn@example.com",
		},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("patch smtp = %d (%v)", resp.StatusCode, body)
	}

	resp, body = c.do(http.MethodPost, "/api/v1/notification-channels", map[string]any{
		"name": "Ops mail", "type": "email",
		"config": map[string]any{"to": []string{"ops@example.com"}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create after settings = %d, want 201 (%v)", resp.StatusCode, body)
	}
}

func TestSMTPPasswordIsNeverReturned(t *testing.T) {
	// Deliberately not parallel. The instance relay is package-level state in
	// internal/notify — one relay per process is the whole idea — so a test that
	// sets it has to run while nothing else is reading it. Sequential tests
	// complete before any parallel one resumes, which is what makes this exact.

	c := newClient(t, testServer(t))
	c.setup()
	t.Cleanup(func() { notify.SetInstanceSMTP(notify.InstanceSMTP{}) })

	c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"smtp": map[string]any{
			"host": "smtp.example.com", "password": "mail-secret",
			"from_address": "cairn@example.com",
		},
	})

	_, body := c.do(http.MethodGet, "/api/v1/settings", nil)
	smtp := body["smtp"].(map[string]any)
	if _, present := smtp["password"]; present {
		t.Fatal("the read shape carried the SMTP password")
	}
	if smtp["host"] != "smtp.example.com" {
		t.Errorf("host = %v, want it stored", smtp["host"])
	}

	// Changing an unrelated field must not clear the stored password: the sealed
	// envelope carries forward.
	c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"smtp": map[string]any{"port": 465},
	})
	if !notify.InstanceSMTPConfigured() {
		t.Fatal("the relay stopped being configured after an unrelated edit")
	}
}

func TestSMTPHostWithoutASenderIsRefused(t *testing.T) {
	// Deliberately not parallel. The instance relay is package-level state in
	// internal/notify — one relay per process is the whole idea — so a test that
	// sets it has to run while nothing else is reading it. Sequential tests
	// complete before any parallel one resumes, which is what makes this exact.

	c := newClient(t, testServer(t))
	c.setup()
	t.Cleanup(func() { notify.SetInstanceSMTP(notify.InstanceSMTP{}) })

	// A relay with a host and no sender fails on its first message, and the
	// operator finds out when an alert does not arrive.
	resp, body := c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"smtp": map[string]any{"host": "smtp.example.com"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("patch = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if pointer := firstErrorPointer(body); pointer != "/smtp/from_address" {
		t.Fatalf("pointer = %q, want /smtp/from_address", pointer)
	}
}

func TestMonitoringDefaultsApplyToNewMonitors(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	if resp, body := c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"monitoring": map[string]any{
			"default_interval_seconds": 300, "default_timeout_seconds": 45, "default_retries": 2,
		},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d (%v)", resp.StatusCode, body)
	}

	// An operator who set a default and then watched a new monitor ignore it
	// would reasonably conclude the setting does nothing.
	_, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "Checkout", "type": "http",
		"config": map[string]any{"url": "https://example.com/health"},
	})
	if created["interval_seconds"] != 300.0 {
		t.Errorf("interval_seconds = %v, want the configured default", created["interval_seconds"])
	}
	if created["timeout_seconds"] != 45.0 {
		t.Errorf("timeout_seconds = %v, want the configured default", created["timeout_seconds"])
	}
	if created["retries"] != 2.0 {
		t.Errorf("retries = %v, want the configured default", created["retries"])
	}
}

func TestDefaultsThatNoMonitorCouldUseAreRefused(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	// The same rule createMonitor enforces, applied to the defaults: otherwise
	// the form refuses its own prefilled values.
	resp, body := c.do(http.MethodPatch, "/api/v1/settings", map[string]any{
		"monitoring": map[string]any{"default_interval_seconds": 20, "default_timeout_seconds": 60},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("patch = %d, want 422 (%v)", resp.StatusCode, body)
	}
}

func TestCurrentUserNeedsThePasswordToChangeTheEmail(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	// A live session is ambient authentication. The two fields that would let
	// somebody take the account over permanently are worth a second proof.
	resp, body := c.do(http.MethodPatch, "/api/v1/users/me", map[string]any{
		"email": "someone-else@example.com",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("patch = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if pointer := firstErrorPointer(body); pointer != "/current_password" {
		t.Fatalf("pointer = %q, want /current_password", pointer)
	}

	resp, body = c.do(http.MethodPatch, "/api/v1/users/me", map[string]any{
		"email": "someone-else@example.com", "current_password": "wrong",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("patch = %d, want 422 (%v)", resp.StatusCode, body)
	}
	if code := firstErrorCode(body); code != "incorrect" {
		t.Fatalf("code = %q, want incorrect", code)
	}

	resp, body = c.do(http.MethodPatch, "/api/v1/users/me", map[string]any{
		"email": "owner2@example.com", "current_password": "correct-horse-battery",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["email"] != "owner2@example.com" {
		t.Fatalf("email = %v", body["email"])
	}
}

func TestChangingTheNameNeedsNoPassword(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	resp, body := c.do(http.MethodPatch, "/api/v1/users/me", map[string]any{"name": "The Owner"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%v)", resp.StatusCode, body)
	}
	if body["name"] != "The Owner" {
		t.Fatalf("name = %v", body["name"])
	}
}

func TestSystemInfoReportsWhatThisBuildCanActuallyRun(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	resp, body := c.do(http.MethodGet, "/api/v1/system/info", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("system info = %d (%v)", resp.StatusCode, body)
	}

	// The test server registers http only, plus push, which is evaluated by the
	// control plane and has no checker. A dashboard reading this hides the
	// surfaces the instance does not have rather than showing dead controls.
	types := body["monitor_types"].([]any)
	if len(types) != 2 {
		t.Fatalf("monitor_types = %v, want exactly what this build registers", types)
	}
	if body["mode"] != "solo" || body["storage_engine"] != "sqlite" {
		t.Errorf("mode/%v engine/%v", body["mode"], body["storage_engine"])
	}

	caps := body["capabilities"].(map[string]any)
	if caps["kuma_import"] != false {
		t.Error("capabilities claim a Kuma importer this build does not have")
	}
	if caps["certificate_detail"] != true {
		t.Error("capabilities hide certificate detail this build populates")
	}
	if caps["monitors"] != true {
		t.Error("capabilities do not claim the thing this build is for")
	}
}

func TestOverviewCountsWhatIsThere(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	createHTTPMonitor(t, c, "Checkout")
	paused := createHTTPMonitor(t, c, "Search")
	c.do(http.MethodPost, "/api/v1/monitors/"+paused+"/pause", nil)
	openIncident(t, c, "Something", nil)

	_, body := c.do(http.MethodGet, "/api/v1/overview", nil)
	monitors := body["monitors"].(map[string]any)
	if monitors["total"] != 2.0 {
		t.Errorf("total = %v, want 2", monitors["total"])
	}
	if monitors["pending"] != 1.0 {
		t.Errorf("pending = %v, want 1", monitors["pending"])
	}
	if monitors["paused"] != 1.0 {
		t.Errorf("paused = %v, want 1", monitors["paused"])
	}
	if body["active_incidents"] != 1.0 {
		t.Errorf("active_incidents = %v, want 1", body["active_incidents"])
	}
}
