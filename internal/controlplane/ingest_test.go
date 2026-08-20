package controlplane

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// fakeStore is enough of a store to exercise the transition rules, which is the
// part worth testing: they decide who gets paged.
type fakeStore struct {
	monitor model.Monitor
	state   model.MonitorState
	written []model.Heartbeat

	// writeErr makes the heartbeat write fail, which is how the "nothing is
	// alerted about a transition that was never stored" rule is exercised.
	writeErr error

	// graph holds several monitors when a test needs more than one — dependency
	// suppression is about the relationship between two of them, which a single
	// monitor cannot express. Nil for every other test, which keeps the simple
	// ones simple.
	graph      map[model.ID]model.Monitor
	graphState map[model.ID]*model.MonitorState

	// certificate and domain are the observation rows, nil until something
	// writes one — which is the state every monitor that never completes a
	// handshake stays in.
	certificate *model.Certificate
	domain      *model.DomainExpiry
}

func (f *fakeStore) ListAssignable(context.Context) ([]model.Monitor, error) {
	return []model.Monitor{f.monitor}, nil
}

func (f *fakeStore) ListPushMonitors(context.Context) ([]store.MonitorWithState, error) {
	if f.monitor.Type != model.TypePush {
		return nil, nil
	}
	return []store.MonitorWithState{{Monitor: f.monitor, State: f.state}}, nil
}

func (f *fakeStore) LoadMonitor(_ context.Context, id model.ID) (model.Monitor, error) {
	if f.graph != nil {
		m, ok := f.graph[id]
		if !ok {
			return model.Monitor{}, store.ErrNotFound
		}
		return m, nil
	}
	return f.monitor, nil
}

func (f *fakeStore) GetState(_ context.Context, id model.ID) (model.MonitorState, error) {
	if f.graph != nil {
		st, ok := f.graphState[id]
		if !ok {
			return model.MonitorState{}, store.ErrNotFound
		}
		return *st, nil
	}
	return f.state, nil
}

func (f *fakeStore) SaveState(_ context.Context, s model.MonitorState) error {
	if f.graph != nil {
		f.graphState[s.MonitorID] = &s
		return nil
	}
	f.state = s
	return nil
}

func (f *fakeStore) WriteBatch(_ context.Context, beats []model.Heartbeat) (int64, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.written = append(f.written, beats...)
	return int64(len(beats)), nil
}

func (f *fakeStore) GetCertificate(_ context.Context, _ model.ID) (model.Certificate, error) {
	if f.certificate == nil {
		return model.Certificate{}, store.ErrNotFound
	}
	return *f.certificate, nil
}

func (f *fakeStore) SaveCertificate(_ context.Context, c model.Certificate) error {
	f.certificate = &c
	return nil
}

func (f *fakeStore) GetDomainExpiry(_ context.Context, _ model.ID) (model.DomainExpiry, error) {
	if f.domain == nil {
		return model.DomainExpiry{}, store.ErrNotFound
	}
	return *f.domain, nil
}

func (f *fakeStore) SaveDomainExpiry(_ context.Context, d model.DomainExpiry) error {
	f.domain = &d
	return nil
}

func newTestServer(retries int) (*Server, *fakeStore) {
	id := model.NewID()
	store := &fakeStore{
		monitor: model.Monitor{ID: id, OrgID: model.SentinelOrgID, Retries: retries, Interval: time.Minute},
		state:   model.MonitorState{MonitorID: id, OrgID: model.SentinelOrgID, Status: model.MonitorStatusPending},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, NewPublisher(), &recordingAlerter{}, nil, log, model.EmbeddedProbeID, model.SentinelOrgID), store
}

// recordingAlerter captures what ingest decided to tell the world, which is the
// half of a transition that no heartbeat row records.
type recordingAlerter struct {
	mu     sync.Mutex
	events []notify.Event
}

func (a *recordingAlerter) Publish(ev notify.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *recordingAlerter) Instance() notify.Instance {
	return notify.Instance{Name: "test", Version: "test"}
}

func (a *recordingAlerter) types() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.events))
	for _, e := range a.events {
		out = append(out, e.Type)
	}
	return out
}

func result(id model.ID, outcome probev1.Outcome, at time.Time) *probev1.Result {
	rid := model.NewID()
	return &probev1.Result{
		ResultId:       rid[:],
		MonitorId:      id[:],
		TimeUnixMicros: at.UnixMicro(),
		Outcome:        outcome,
		Attempt:        1,
	}
}

// The transition table. Each step feeds one result and asserts the verdict the
// control plane reaches, because this is the logic that decides whether an alert
// fires.
func TestIngestTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		retries    int
		outcomes   []probev1.Outcome
		wantStatus string
		wantFails  int
	}{
		{
			name:       "first success is up",
			outcomes:   []probev1.Outcome{probev1.Outcome_OUTCOME_UP},
			wantStatus: model.MonitorStatusUp,
		},
		{
			name:       "with no retries the first failure is down",
			outcomes:   []probev1.Outcome{probev1.Outcome_OUTCOME_DOWN},
			wantStatus: model.MonitorStatusDown,
			wantFails:  1,
		},
		{
			name:       "with retries the first failure is only pending",
			retries:    2,
			outcomes:   []probev1.Outcome{probev1.Outcome_OUTCOME_DOWN},
			wantStatus: model.MonitorStatusPending,
			wantFails:  1,
		},
		{
			name:       "down once the retry budget is spent",
			retries:    2,
			outcomes:   []probev1.Outcome{probev1.Outcome_OUTCOME_DOWN, probev1.Outcome_OUTCOME_DOWN, probev1.Outcome_OUTCOME_DOWN},
			wantStatus: model.MonitorStatusDown,
			wantFails:  3,
		},
		{
			name:       "a success clears the failure count",
			retries:    2,
			outcomes:   []probev1.Outcome{probev1.Outcome_OUTCOME_DOWN, probev1.Outcome_OUTCOME_UP},
			wantStatus: model.MonitorStatusUp,
			wantFails:  0,
		},
		{
			// The invariant the whole outcome taxonomy exists for: a probe that
			// cannot run the check must not turn into an outage.
			name:       "unknown does not change the verdict",
			outcomes:   []probev1.Outcome{probev1.Outcome_OUTCOME_UP, probev1.Outcome_OUTCOME_UNKNOWN},
			wantStatus: model.MonitorStatusUp,
		},
		{
			name:       "skipped does not change the verdict",
			outcomes:   []probev1.Outcome{probev1.Outcome_OUTCOME_UP, probev1.Outcome_OUTCOME_SKIPPED},
			wantStatus: model.MonitorStatusUp,
		},
		{
			name:       "unknown does not count as a failure",
			retries:    1,
			outcomes:   []probev1.Outcome{probev1.Outcome_OUTCOME_DOWN, probev1.Outcome_OUTCOME_UNKNOWN, probev1.Outcome_OUTCOME_UNKNOWN},
			wantStatus: model.MonitorStatusPending,
			wantFails:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server, store := newTestServer(tc.retries)
			at := time.Now().UTC()

			for i, outcome := range tc.outcomes {
				_, err := server.ingest(t.Context(), []*probev1.Result{
					result(store.monitor.ID, outcome, at.Add(time.Duration(i)*time.Second)),
				})
				if err != nil {
					t.Fatalf("ingest: %v", err)
				}
			}

			if store.state.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", store.state.Status, tc.wantStatus)
			}
			if store.state.ConsecutiveFailures != tc.wantFails {
				t.Errorf("consecutive_failures = %d, want %d", store.state.ConsecutiveFailures, tc.wantFails)
			}
			if len(store.written) != len(tc.outcomes) {
				t.Errorf("wrote %d heartbeats, want %d", len(store.written), len(tc.outcomes))
			}
		})
	}
}

// important marks the heartbeat that changed things, and only that one. It is
// what important_only filters to and what alerting will hang off, so a flag on
// every row would be as useless as a flag on none.
func TestIngestMarksOnlyTransitions(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(0)
	at := time.Now().UTC()

	outcomes := []probev1.Outcome{
		probev1.Outcome_OUTCOME_UP,   // pending -> up: important
		probev1.Outcome_OUTCOME_UP,   // no change
		probev1.Outcome_OUTCOME_DOWN, // up -> down: important
		probev1.Outcome_OUTCOME_DOWN, // no change
	}
	for i, outcome := range outcomes {
		if _, err := server.ingest(t.Context(), []*probev1.Result{
			result(store.monitor.ID, outcome, at.Add(time.Duration(i)*time.Second)),
		}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}

	want := []bool{true, false, true, false}
	for i, beat := range store.written {
		if beat.Important != want[i] {
			t.Errorf("heartbeat %d important = %v, want %v", i, beat.Important, want[i])
		}
	}
}

// The acknowledged high-water mark must be the last result of the batch, and it
// must only be returned after the write succeeded — the probe frees its buffer
// on this value and nothing else.
func TestIngestAcknowledgesThroughLastResult(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(0)
	at := time.Now().UTC()

	batch := []*probev1.Result{
		result(store.monitor.ID, probev1.Outcome_OUTCOME_UP, at),
		result(store.monitor.ID, probev1.Outcome_OUTCOME_UP, at.Add(time.Second)),
	}
	ack, err := server.ingest(t.Context(), batch)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if string(ack.GetAcknowledgedThroughResultId()) != string(batch[1].GetResultId()) {
		t.Errorf("acknowledged through %x, want %x", ack.GetAcknowledgedThroughResultId(), batch[1].GetResultId())
	}
	if ack.GetAccepted() != 2 {
		t.Errorf("accepted = %d, want 2", ack.GetAccepted())
	}
}
