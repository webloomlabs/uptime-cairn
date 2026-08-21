package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// A pin is only worth anything if the assignment set honours it. Everything up
// to here — the column, the API field, the validation — is bookkeeping; this is
// the behaviour: a monitor pinned to one probe is not handed to another.
//
// Solo mode has one probe, so this cannot be exercised end to end in this
// build. It is exercised at the seam it will run at, which is the point of
// building the mechanism before Phase 4 rather than retrofitting it.
func TestAPinnedMonitorIsWithheldFromEveryOtherProbe(t *testing.T) {
	t.Parallel()

	elsewhere := model.NewID()
	pinned := model.Monitor{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Name: "web container", Type: model.TypeDocker,
		Config:   json.RawMessage(`{"container_name":"web"}`),
		Interval: time.Minute, Timeout: 30 * time.Second,
		ProbeID: &elsewhere,
	}

	store := &fakeStore{monitor: pinned}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(store, NewPublisher(), nil, nil, log, model.EmbeddedProbeID, model.SentinelOrgID)

	set, _, err := server.assignments(context.Background(), model.EmbeddedProbeID)
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	if _, present := set[pinned.ID.String()]; present {
		t.Error("a monitor pinned to another probe was handed to this one; " +
			"\"is this container running\" would then be answered by a host with a different container set")
	}

	// And the probe it names does get it, or the pin would be a way to switch a
	// monitor off by accident.
	set, _, err = server.assignments(context.Background(), elsewhere)
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	if _, present := set[pinned.ID.String()]; !present {
		t.Error("the probe the monitor is pinned to was not given it")
	}
}

// An unpinned monitor still goes everywhere, which is what makes multi-region
// probing possible at all: two probes checking one http target is two opinions
// about it, not a duplicate.
func TestAnUnpinnedMonitorGoesToEveryProbe(t *testing.T) {
	t.Parallel()

	unpinned := model.Monitor{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Name: "Checkout", Type: model.TypeHTTP,
		Config:   json.RawMessage(`{"url":"https://example.com/"}`),
		Interval: time.Minute, Timeout: 30 * time.Second,
	}

	store := &fakeStore{monitor: unpinned}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(store, NewPublisher(), nil, nil, log, model.EmbeddedProbeID, model.SentinelOrgID)

	for _, probe := range []model.ID{model.EmbeddedProbeID, model.NewID()} {
		set, _, err := server.assignments(context.Background(), probe)
		if err != nil {
			t.Fatalf("assignments: %v", err)
		}
		if _, present := set[unpinned.ID.String()]; !present {
			t.Errorf("probe %s was not given the unpinned monitor", probe)
		}
	}
}
