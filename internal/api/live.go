package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/live"
	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// The browser-facing half of ADR-004.
//
// The ADR's decision is that a client subscribes to exactly the monitor ids on
// its screen and receives scoped diffs for those, with filtered-view membership
// kept fresh separately by polling /monitors/membership. What it does not fix is
// the HTTP framing, and the framing is where the one real constraint lives: the
// ADR rejects any channel that has to be closed and reopened when the
// subscription scope changes, because paginating a list is the most ordinary
// thing a user does and a channel that reconnects on every page turn spends its
// life reconnecting.
//
// So: one Server-Sent Events stream per view, and scope changes are an ordinary
// PUT against that stream's id. The stream stays open across pagination,
// filtering, and search; only the set of ids it carries changes. That answers
// the objection directly rather than working around it, and it costs no
// dependency and no second transport — an SSE stream is readable with curl,
// which matters at 3am more than it looks like it should.
//
// What is underneath is the part ADR-004 actually decides, and it is unchanged:
// an in-process bus in solo mode, NATS with updates.{org_id}.{monitor_id}.status
// subjects in scaled mode. internal/live.Bus is that seam, and this handler
// cannot tell which implementation it holds — which is the ADR's own open
// follow-up, written down as a type rather than left as an intention.

// liveStreams holds the open streams so a scope change can find one.
//
// Keyed by an opaque id issued at stream open and sent to the client as the
// first event. The id is a credential in the same weak sense a push token is:
// holding it lets you change what a stream carries, and the stream is already
// authenticated, so what it protects is one authenticated session's stream from
// another's.
type liveStreams struct {
	mu      sync.Mutex
	streams map[string]*live.Subscription
}

func newLiveStreams() *liveStreams {
	return &liveStreams{streams: make(map[string]*live.Subscription)}
}

func (l *liveStreams) add(id string, s *live.Subscription) {
	l.mu.Lock()
	l.streams[id] = s
	l.mu.Unlock()
}

func (l *liveStreams) get(id string) (*live.Subscription, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.streams[id]
	return s, ok
}

func (l *liveStreams) remove(id string) {
	l.mu.Lock()
	delete(l.streams, id)
	l.mu.Unlock()
}

// liveKeepalive is how often a comment frame goes down an idle stream.
//
// Not for the browser, which is happy to wait — for everything between it and
// here. Proxies, load balancers and corporate middleboxes close a connection
// that has been quiet for a minute or so, and the failure that produces is a
// dashboard that stops updating with no error anywhere, which is the most
// expensive kind. Fifteen seconds is comfortably inside every default this
// project's own reverse-proxy recipes set.
const liveKeepalive = 15 * time.Second

// maxLiveScope bounds how many monitors one stream may hold.
//
// This is the ADR-004 invariant expressed as a number: client-side payload and
// render cost must be bounded by viewport size, never by total monitor count.
// A client that could subscribe to five thousand ids has reintroduced the
// full-state broadcast through the back door, one id at a time. The page limit
// is 100, so 500 is generous for a client holding several pages and still
// nowhere near the install size.
const maxLiveScope = 500

// streamUpdates opens the SSE stream.
//
// The initial scope comes from monitor_ids= on the query string so that the
// first page of rows starts updating without a second round trip. It may be
// empty: a view that has not loaded its rows yet still wants the summary
// channel, which every stream receives regardless of scope.
func (s *Server) streamUpdates(w http.ResponseWriter, r *http.Request) {
	if s.live == nil {
		writeProblem(w, r, s.log, http.StatusNotImplemented, "not-implemented",
			"Live updates are not available",
			"This build has no live-update bus wired in. Poll /monitors/membership instead.")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Every stdlib server supports this. A wrapper that does not would
		// buffer the whole stream until the handler returned, which is never.
		s.internal(w, r, "live stream", fmt.Errorf("response writer cannot flush"))
		return
	}

	scope, problem := parseLiveScope(r.URL.Query().Get("monitor_ids"))
	if problem != "" {
		writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-parameter",
			"Invalid parameter", problem)
		return
	}

	subscription := s.live.Subscribe(scope)
	defer subscription.Close()

	id := model.NewID().String()
	s.streams.add(id, subscription)
	defer s.streams.remove(id)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// Named explicitly because the one proxy behaviour that breaks SSE silently
	// is response buffering, and nginx's default does exactly that.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// The stream id first, because without it the client cannot change its own
	// scope and the stream is a fixed subscription to whatever it opened with.
	writeEvent(w, "stream", map[string]string{"stream_id": id})
	flusher.Flush()

	// The current summary immediately, so a view that has just opened shows the
	// real counts rather than waiting for the next monitor to change status —
	// which, on a healthy install, could be a long time.
	if summary, err := s.currentSummary(r.Context()); err == nil {
		writeEvent(w, "summary", toLiveSummaryJSON(summary))
		flusher.Flush()
	}

	keepalive := time.NewTicker(liveKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			if dropped := subscription.Dropped(); dropped > 0 {
				// Logged because it is invisible from the browser, which sees a
				// row that simply stopped moving rather than an error.
				s.log.Warn("live subscriber fell behind and lost updates",
					"stream", id, "dropped", dropped)
			}
			return

		case message := <-subscription.C:
			switch {
			case message.Update != nil:
				writeEvent(w, "monitor", toLiveUpdateJSON(*message.Update))
			case message.Summary != nil:
				writeEvent(w, "summary", toLiveSummaryJSON(*message.Summary))
			default:
				continue
			}
			flusher.Flush()

		case <-keepalive.C:
			// A comment frame. Ignored by every SSE client and enough to keep
			// the middleboxes from deciding the connection is idle.
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		}
	}
}

// setStreamScope replaces what an open stream carries, without reopening it.
//
// This is the endpoint that makes SSE a viable framing for ADR-004's model
// rather than the one it rejected. A client paginating from rows 1–25 to 26–50
// sends the new ids here and the stream keeps running.
func (s *Server) setStreamScope(w http.ResponseWriter, r *http.Request) {
	if s.live == nil {
		writeProblem(w, r, s.log, http.StatusNotImplemented, "not-implemented",
			"Live updates are not available",
			"This build has no live-update bus wired in.")
		return
	}

	subscription, ok := s.streams.get(r.PathValue("streamId"))
	if !ok {
		// 404 rather than 410: a stream id that has been closed and one that
		// never existed are the same thing to a client, and the answer to both
		// is to open a new stream.
		writeProblem(w, r, s.log, http.StatusNotFound, "not-found",
			"No such stream",
			"That stream is closed or never existed. Open a new one at /api/v1/live.")
		return
	}

	var body struct {
		MonitorIDs []string `json:"monitor_ids"`
	}
	if !s.readBody(w, r, 1<<18, &body) {
		return
	}

	scope, problem := parseLiveScope(strings.Join(body.MonitorIDs, ","))
	if problem != "" {
		writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-parameter",
			"Invalid parameter", problem)
		return
	}

	subscription.Update(scope)
	w.WriteHeader(http.StatusNoContent)
}

// parseLiveScope reads a comma-separated id list and enforces the bound.
//
// An unparseable id is refused rather than skipped, on the same reasoning as
// every other filter in this API: silently subscribing to fewer monitors than
// asked for produces rows that never update, which reads as a broken dashboard
// rather than as a client bug.
func parseLiveScope(raw string) ([]model.ID, string) {
	if strings.TrimSpace(raw) == "" {
		return nil, ""
	}

	parts := strings.Split(raw, ",")
	if len(parts) > maxLiveScope {
		return nil, fmt.Sprintf(
			"a stream may hold at most %d monitors; a client subscribing to more than its viewport "+
				"has reintroduced the full-state broadcast one id at a time", maxLiveScope)
	}

	out := make([]model.ID, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, ok := model.ParseID(part)
		if !ok {
			return nil, fmt.Sprintf("%q is not a monitor identifier", part)
		}
		out = append(out, id)
	}
	return out, ""
}

// currentSummary reads the global counts. The same numbers /overview reports,
// from the same query, because two ways of counting the same thing is how a
// header disagrees with the page under it.
func (s *Server) currentSummary(ctx context.Context) (live.Summary, error) {
	counts, err := s.store.StatusCounts(ctx)
	if err != nil {
		return live.Summary{}, err
	}
	return live.Summary{Counts: counts, At: time.Now().UTC()}, nil
}

// liveUpdateJSON is MonitorUpdate in docs/api/openapi.yaml.
//
// A diff, not a monitor. The client already holds the row; re-sending its
// configuration on every heartbeat would be the full-state broadcast this whole
// design exists to avoid, arriving one row at a time instead of all at once.
type liveUpdateJSON struct {
	MonitorID      string    `json:"monitor_id"`
	Status         string    `json:"status"`
	At             time.Time `json:"at"`
	ResponseTimeMs *float64  `json:"response_time_ms"`
	Important      bool      `json:"important"`
	Message        string    `json:"message,omitempty"`
	StateVersion   int64     `json:"state_version"`
}

func toLiveUpdateJSON(u live.Update) liveUpdateJSON {
	return liveUpdateJSON{
		MonitorID:      u.MonitorID.String(),
		Status:         u.Status,
		At:             u.At,
		ResponseTimeMs: u.ResponseTimeMs,
		Important:      u.Important,
		Message:        u.Message,
		StateVersion:   u.StateVersion,
	}
}

// liveSummaryJSON is the global header count. Same numbers /overview reports,
// from the same query, because two ways of counting one thing is how a header
// disagrees with the page under it.
type liveSummaryJSON struct {
	Counts map[string]int `json:"counts"`
	At     time.Time      `json:"at"`
}

func toLiveSummaryJSON(s live.Summary) liveSummaryJSON {
	counts := s.Counts
	if counts == nil {
		counts = map[string]int{}
	}
	return liveSummaryJSON{Counts: counts, At: s.At}
}

// writeEvent renders one SSE frame.
//
// The data is JSON on a single line, which the format requires: a newline
// inside a data field starts a second data line, and a client concatenating
// them would silently reassemble something that is no longer the document that
// was sent. json.Marshal never emits a bare newline, so this holds by
// construction rather than by escaping afterwards.
func writeEvent(w http.ResponseWriter, name string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
}
