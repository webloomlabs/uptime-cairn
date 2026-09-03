package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/artifact"
	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// A cancelled run leaves nothing half-written, and it leaves nothing at
// `running` either.
//
// Two separate promises, and the second is the one that is easy to miss. The
// first is structural: `artifact.Write` renames a complete temporary file into
// place, so there is no moment at which a partial file exists at a real path.
// The second is a decision — an interrupted run has to *finish*, because a row
// stuck at `running` looks exactly like a report that is still being produced and
// a person looking at the screen has no way to tell whether to wait.
//
// The formats that completed are kept. Each is a whole file with a committed row
// and a digest; a client who got three of four artifacts got three real ones, and
// discarding them to tidy the state would be deleting the only record of what was
// actually sent.
func TestACancelledRunIsFinishedRatherThanLeftRunning(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON, model.FormatCSV, model.FormatHTML)
	run.ReportTemplateID = s.template.ID

	ctx, cancel := context.WithCancel(context.Background())
	files := &cancellingFiles{fakeFiles: newFiles(), cancelAfter: 1, cancel: cancel}

	if err := New(s, files, Options{Retention: retention()}).Execute(ctx, run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	state, failure, artifacts := s.outcome()

	// Finished, not running.
	if state == model.RunRunning || state == "" {
		t.Fatalf("state = %q; an interrupted run that stays at running is "+
			"indistinguishable from one that is still going", state)
	}
	// Something landed, so the state says so rather than calling the whole run a
	// failure.
	if state != model.RunPartial {
		t.Errorf("state = %q, want %q — one format was produced", state, model.RunPartial)
	}
	if !strings.Contains(failure, "interrupted") {
		t.Errorf("error = %q, want it to say the run was interrupted; a blank or a "+
			"generic message sends somebody looking for a defect that is not there", failure)
	}

	// The one that finished is kept, and nothing was started after the
	// cancellation.
	if len(artifacts) != 1 {
		t.Fatalf("%d artifact rows, want 1 — the completed format is kept and no "+
			"further format is begun", len(artifacts))
	}
	if artifacts[0].State != model.ArtifactRendered {
		t.Errorf("artifact state = %q, want rendered", artifacts[0].State)
	}
	if artifacts[0].SHA256 == "" {
		t.Error("the surviving artifact has no digest, so it cannot be shown to be complete")
	}
}

// A run cancelled before anything rendered is a failure, not a partial. Partial
// means "some of it arrived"; a run that produced nothing has nothing to be
// partial about, and collapsing the two is how somebody concludes a delivery went
// out when it did not.
func TestACancelledRunThatProducedNothingIsAFailure(t *testing.T) {
	t.Parallel()

	s, run := fixture(model.FormatJSON, model.FormatCSV)
	run.ReportTemplateID = s.template.ID

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := New(s, newFiles(), Options{Retention: retention()}).Execute(ctx, run, now); err != nil {
		t.Fatalf("execute: %v", err)
	}

	state, failure, artifacts := s.outcome()
	if state != model.RunFailed {
		t.Errorf("state = %q, want %q", state, model.RunFailed)
	}
	if len(artifacts) != 0 {
		t.Errorf("%d artifacts on a run cancelled before it rendered anything", len(artifacts))
	}
	if failure == "" {
		t.Error("no reason recorded")
	}
}

// cancellingFiles cancels the run's context after a given number of successful
// writes, which is how a shutdown arriving mid-run is reproduced without timing.
type cancellingFiles struct {
	*fakeFiles
	cancelAfter int
	cancel      context.CancelFunc
	written     int
}

func (c *cancellingFiles) Write(id model.ID, format string, when time.Time, data []byte) (artifact.Written, error) {
	out, err := c.fakeFiles.Write(id, format, when, data)
	if err != nil {
		return out, err
	}
	c.written++
	if c.written == c.cancelAfter {
		c.cancel()
	}
	return out, nil
}
