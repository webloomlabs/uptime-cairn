package controlplane

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// fakeStore is enough of a store to exercise the transition rules, which is the
// part worth testing: they decide who gets paged.
type fakeStore struct {
	monitor model.Monitor
	state   model.MonitorState
	written []model.Heartbeat
}

func (f *fakeStore) ListAssignable(context.Context) ([]model.Monitor, error) {
	return []model.Monitor{f.monitor}, nil
}

func (f *fakeStore) LoadMonitor(context.Context, model.ID) (model.Monitor, error) {
	return f.monitor, nil
}

func (f *fakeStore) GetState(context.Context, model.ID) (model.MonitorState, error) {
	return f.state, nil
}

func (f *fakeStore) SaveState(_ context.Context, s model.MonitorState) error {
	f.state = s
	return nil
}

func (f *fakeStore) WriteBatch(_ context.Context, beats []model.Heartbeat) error {
	f.written = append(f.written, beats...)
	return nil
}

func newTestServer(retries int) (*Server, *fakeStore) {
	id := model.NewID()
	store := &fakeStore{
		monitor: model.Monitor{ID: id, OrgID: model.SentinelOrgID, Retries: retries, Interval: time.Minute},
		state:   model.MonitorState{MonitorID: id, OrgID: model.SentinelOrgID, Status: model.MonitorStatusPending},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, NewPublisher(), log, model.EmbeddedProbeID, model.SentinelOrgID), store
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
