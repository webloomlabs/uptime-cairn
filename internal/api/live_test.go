package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// sseEvent is one parsed frame.
type sseEvent struct {
	name string
	data map[string]any
}

// openStream opens the live channel and returns a reader over its frames. The
// caller cancels ctx to close it.
func openStream(t *testing.T, c *client, ctx context.Context, query string) (<-chan sseEvent, func()) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/live"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.authorise(req)

	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("open stream = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	events := make(chan sseEvent, 64)
	go func() {
		defer close(events)
		defer func() { _ = resp.Body.Close() }()

		scanner := bufio.NewScanner(resp.Body)
		var name string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var payload map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err == nil {
					select {
					case events <- sseEvent{name: name, data: payload}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()

	return events, func() { _ = resp.Body.Close() }
}

func waitFor(t *testing.T, events <-chan sseEvent, name string) sseEvent {
	t.Helper()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case e, open := <-events:
			if !open {
				t.Fatalf("the stream closed before a %q event arrived", name)
			}
			if e.name == name {
				return e
			}
		case <-deadline:
			t.Fatalf("no %q event within the deadline", name)
		}
	}
}

// The whole of ADR-004's live half, end to end: a check lands, and the browser
// holding that row is told without waiting for the next membership poll.
func TestALiveStreamCarriesDiffsForTheRowsItHolds(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	watchedToken, watched := createPushMonitor(t, c, map[string]any{
		"name": "Nightly backup", "type": "push", "config": map[string]any{},
	})
	offToken, off := createPushMonitor(t, c, map[string]any{
		"name": "Reporting job", "type": "push", "config": map[string]any{},
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, closeStream := openStream(t, c, ctx, "?monitor_ids="+watched)
	defer closeStream()

	// The stream id first: without it a client cannot change its own scope.
	if id := waitFor(t, events, "stream").data["stream_id"]; id == nil || id == "" {
		t.Fatal("the stream carried no id")
	}
	// And the current counts, so a view that has just opened is not blank until
	// something happens to break.
	waitFor(t, events, "summary")

	pusher := newClient(t, server)

	// A monitor outside the scope produces nothing. This is the assertion the
	// whole design turns on: there is no monitor count at which an unwatched
	// row starts costing this browser anything.
	pusher.do(http.MethodGet, "/api/v1/push/"+offToken+"?status=down", nil)
	select {
	case e := <-events:
		if e.name == "monitor" && e.data["monitor_id"] == off {
			t.Fatal("a monitor outside the subscription reached the stream")
		}
	case <-time.After(300 * time.Millisecond):
	}

	pusher.do(http.MethodGet, "/api/v1/push/"+watchedToken+"?status=down&msg=disk+full", nil)
	diff := waitFor(t, events, "monitor")
	if diff.data["monitor_id"] != watched {
		t.Fatalf("diff is for %v, want the watched monitor", diff.data["monitor_id"])
	}
	if diff.data["status"] != "down" {
		t.Errorf("status = %v, want down", diff.data["status"])
	}
	if diff.data["message"] != "disk full" {
		t.Errorf("message = %v, want the pusher's", diff.data["message"])
	}
}

// Paginating is the most ordinary thing a user does, and a stream that had to
// be torn down and re-established for it would spend its life reconnecting.
// This is the endpoint that makes the framing viable.
func TestScopeChangesWithoutReopeningTheStream(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	firstToken, first := createPushMonitor(t, c, map[string]any{
		"name": "Page one", "type": "push", "config": map[string]any{},
	})
	secondToken, second := createPushMonitor(t, c, map[string]any{
		"name": "Page two", "type": "push", "config": map[string]any{},
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, closeStream := openStream(t, c, ctx, "?monitor_ids="+first)
	defer closeStream()

	streamID := waitFor(t, events, "stream").data["stream_id"].(string)

	resp, body := c.do(http.MethodPut, "/api/v1/live/"+streamID+"/scope",
		map[string]any{"monitor_ids": []string{second}})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("scope change = %d (%v), want 204", resp.StatusCode, body)
	}

	pusher := newClient(t, server)
	pusher.do(http.MethodGet, "/api/v1/push/"+firstToken+"?status=down", nil)
	pusher.do(http.MethodGet, "/api/v1/push/"+secondToken+"?status=down", nil)

	diff := waitFor(t, events, "monitor")
	if diff.data["monitor_id"] != second {
		t.Errorf("after the scope change the stream carried %v, want the new page's monitor",
			diff.data["monitor_id"])
	}

	// A stream id that never existed is a 404, not a 500, because the client's
	// answer to both "closed" and "never was" is to open a new stream.
	resp, _ = c.do(http.MethodPut, "/api/v1/live/00000000-0000-7000-8000-00000000ffff/scope",
		map[string]any{"monitor_ids": []string{second}})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("scope change on an unknown stream = %d, want 404", resp.StatusCode)
	}
}

// A client that could subscribe to five thousand ids has reintroduced the
// full-state broadcast one id at a time, which is exactly what ADR-004's second
// invariant forbids.
func TestALiveScopeIsBoundedByViewportRatherThanInstallSize(t *testing.T) {
	t.Parallel()

	c := newClient(t, testServer(t))
	c.setup()

	ids := make([]string, maxLiveScope+1)
	for i := range ids {
		ids[i] = "00000000-0000-7000-8000-0000000000" + string("0123456789abcdef"[i%16]) + "0"
	}

	resp, _ := c.do(http.MethodGet, "/api/v1/live?monitor_ids="+strings.Join(ids, ","), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an oversized scope = %d, want 400", resp.StatusCode)
	}

	// And an unparseable id is refused rather than skipped: silently
	// subscribing to fewer monitors than asked for produces rows that never
	// update, which reads as a broken dashboard rather than a client bug.
	resp, _ = c.do(http.MethodGet, "/api/v1/live?monitor_ids=not-an-id", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unparseable id = %d, want 400", resp.StatusCode)
	}
}
