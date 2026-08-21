package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
)

// dockerStub stands in for the docker checker so the type is creatable in a
// test. What is under test is placement, not the daemon.
type dockerStub struct{}

func (dockerStub) Type() string          { return "docker" }
func (dockerStub) Version() uint32       { return 1 }
func (dockerStub) Validate([]byte) error { return nil }
func (dockerStub) Check(context.Context, []byte) check.Observation {
	return check.Observation{Status: model.StatusUp}
}

// A client cannot fill in probe_id without being able to find out what the
// choices are. Solo mode's answer is one row, and it must not carry a
// credential.
func TestProbesAreListableAndCarryNoCredential(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	resp, body := c.do(http.MethodGet, "/api/v1/probes", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list probes = %d (%v)", resp.StatusCode, body)
	}
	data, ok := body["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data = %v, want the one embedded probe", body["data"])
	}

	probe := data[0].(map[string]any)
	if probe["mode"] != "embedded" {
		t.Errorf("mode = %v, want embedded", probe["mode"])
	}
	for _, leak := range []string{"token_hash", "token", "secret"} {
		if _, present := probe[leak]; present {
			t.Errorf("the probe listing carries %q", leak)
		}
	}
}

// Solo mode has one probe, so a docker monitor's pin is implied — and written,
// so the response shows where the monitor actually landed rather than leaving
// the field null and the placement to luck.
func TestADockerMonitorIsPinnedToTheOnlyProbe(t *testing.T) {
	t.Parallel()

	server := testServer2(t, dockerStub{})
	c := newClient(t, server)
	c.setup()

	_, probes := c.do(http.MethodGet, "/api/v1/probes", nil)
	embedded := probes["data"].([]any)[0].(map[string]any)["id"].(string)

	resp, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "web container", "type": "docker",
		"config": map[string]any{"container_name": "web"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create docker monitor = %d (%v)", resp.StatusCode, created)
	}
	if created["probe_id"] != embedded {
		t.Errorf("probe_id = %v, want the embedded probe %s", created["probe_id"], embedded)
	}

	// And a type that answers the same from anywhere stays unpinned, because
	// pinning one would quietly halve a multi-region install's coverage.
	id := createHTTPMonitor(t, c, "Checkout")
	_, http1 := c.do(http.MethodGet, "/api/v1/monitors/"+id, nil)
	if http1["probe_id"] != nil {
		t.Errorf("an http monitor was pinned to %v", http1["probe_id"])
	}
}

// A pin has to name something that exists and can run, or the monitor is
// configured to run nowhere — which is a validation error naming the field
// rather than a foreign-key failure surfacing three layers away.
func TestAPinMustNameARealProbe(t *testing.T) {
	t.Parallel()

	server := testServer2(t, dockerStub{})
	c := newClient(t, server)
	c.setup()

	resp, body := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "web container", "type": "docker",
		"config":   map[string]any{"container_name": "web"},
		"probe_id": model.NewID().String(),
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("pin to an unknown probe = %d (%v), want 422", resp.StatusCode, body)
	}
	if got := firstErrorPointer(body); got != "/probe_id" {
		t.Errorf("error pointer = %q, want /probe_id", got)
	}

	resp, body = c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "web container", "type": "docker",
		"config":   map[string]any{"container_name": "web"},
		"probe_id": "not-an-identifier",
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("pin to nonsense = %d (%v), want 422", resp.StatusCode, body)
	}
}

// The pin survives an edit that does not mention it. An edit that silently
// unpinned a docker monitor would move it to a host with a different container
// set and report a container missing that was never meant to be there.
func TestAnEditThatDoesNotMentionThePinLeavesIt(t *testing.T) {
	t.Parallel()

	server := testServer2(t, dockerStub{})
	c := newClient(t, server)
	c.setup()

	_, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "web container", "type": "docker",
		"config": map[string]any{"container_name": "web"},
	})
	id := created["id"].(string)
	pinned := created["probe_id"]

	resp, updated := c.do(http.MethodPatch, "/api/v1/monitors/"+id, map[string]any{"name": "web"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d (%v)", resp.StatusCode, updated)
	}
	if updated["probe_id"] != pinned {
		t.Errorf("probe_id = %v after a rename, want %v", updated["probe_id"], pinned)
	}
}
