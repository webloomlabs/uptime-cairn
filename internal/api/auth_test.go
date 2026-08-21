package api

import (
	"net/http"
	"testing"
)

// Nothing under /api/v1 answers without a credential. This is the test that
// would have caught the build before this one, where the API had no
// authentication at all.
func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)

	for _, path := range []string{"/api/v1/monitors", "/api/v1/api-keys", "/api/v1/auth/session"} {
		resp, _ := c.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, resp.StatusCode)
		}
	}

	// Health checks stay open: one that needs a credential stops working at the
	// worst possible moment.
	if resp, _ := c.do(http.MethodGet, "/healthz", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
}

// The setup window closes for good once an administrator exists, or anyone who
// reaches the install afterwards can appoint themselves.
func TestSetupHappensOnce(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)

	_, status := c.do(http.MethodGet, "/api/v1/setup/status", nil)
	if required, _ := status["setup_required"].(bool); !required {
		t.Fatal("a fresh install did not report setup_required")
	}

	c.setup()

	_, status = c.do(http.MethodGet, "/api/v1/setup/status", nil)
	if required, _ := status["setup_required"].(bool); required {
		t.Error("setup_required is still true after setup")
	}

	resp, _ := c.do(http.MethodPost, "/api/v1/setup", map[string]string{
		"email": "attacker@example.com", "password": "another-long-password",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second setup = %d, want 409", resp.StatusCode)
	}
}

func TestSetupRejectsWeakInput(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)

	resp, body := c.do(http.MethodPost, "/api/v1/setup", map[string]string{
		"email": "not-an-email", "password": "short",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("setup = %d, want 422", resp.StatusCode)
	}
	errs, _ := body["errors"].([]any)
	if len(errs) != 2 {
		t.Errorf("got %d validation errors, want one each for email and password", len(errs))
	}
}

// Cookie authentication is ambient — the browser attaches it to requests a
// hostile page triggered — so a cookie write must prove the caller could read
// the login response.
func TestCookieWritesRequireCSRF(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	monitor := map[string]any{
		"name": "Example", "type": "http", "interval_seconds": 60, "timeout_seconds": 30,
		"config": map[string]string{"url": "https://example.com"},
	}

	// Reads are fine without it.
	if resp, _ := c.do(http.MethodGet, "/api/v1/monitors", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET with cookie = %d, want 200", resp.StatusCode)
	}

	held := c.csrf
	c.csrf = ""
	resp, _ := c.do(http.MethodPost, "/api/v1/monitors", monitor)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without CSRF = %d, want 403", resp.StatusCode)
	}

	c.csrf = "not-the-right-token"
	resp, _ = c.do(http.MethodPost, "/api/v1/monitors", monitor)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST with a wrong CSRF token = %d, want 403", resp.StatusCode)
	}

	c.csrf = held
	resp, _ = c.do(http.MethodPost, "/api/v1/monitors", monitor)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("POST with CSRF = %d, want 201", resp.StatusCode)
	}
}

func TestLoginFlow(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	fresh := newClient(t, server)
	resp, _ := fresh.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "wrong-password",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with a wrong password = %d, want 401", resp.StatusCode)
	}

	resp, body := fresh.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email":    "OWNER@example.com", // case-insensitive, because email is stored lowercased
		"password": "correct-horse-battery",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200 (%v)", resp.StatusCode, body)
	}
	csrf, _ := body["csrf_token"].(string)
	if csrf == "" {
		t.Error("login returned no csrf_token; a cookie session cannot write without one")
	}
	fresh.csrf = csrf

	resp, session := fresh.do(http.MethodGet, "/api/v1/auth/session", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session = %d, want 200", resp.StatusCode)
	}
	if session["principal_type"] != "user" {
		t.Errorf("principal_type = %v, want user", session["principal_type"])
	}

	if resp, _ := fresh.do(http.MethodPost, "/api/v1/auth/logout", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", resp.StatusCode)
	}
	// The session is destroyed server-side, not merely forgotten by the browser.
	if resp, _ := fresh.do(http.MethodGet, "/api/v1/auth/session", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("session after logout = %d, want 401", resp.StatusCode)
	}
}

// Five failures in fifteen minutes is where guessing stops being free.
func TestLoginIsRateLimited(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	attacker := newClient(t, server)
	body := map[string]string{"email": "owner@example.com", "password": "guess"}

	for i := range loginMaxAttempts {
		resp, _ := attacker.do(http.MethodPost, "/api/v1/auth/login", body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, resp.StatusCode)
		}
	}

	resp, _ := attacker.do(http.MethodPost, "/api/v1/auth/login", body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("attempt %d = %d, want 429", loginMaxAttempts+1, resp.StatusCode)
	}
	// Even the right password waits: otherwise the limiter is an oracle for
	// which guess was correct.
	resp, _ = attacker.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "owner@example.com", "password": "correct-horse-battery",
	})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("correct password while limited = %d, want 429", resp.StatusCode)
	}
}
