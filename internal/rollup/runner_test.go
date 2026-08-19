package rollup

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// The watermark is derived rather than stored, which is what makes the pipeline
// self-healing — and also what makes it easy to get subtly wrong. These assert
// the window each pass chooses, because a window that is off by one bucket
// produces history with a permanent hole in it and no error anywhere.

type fakeStore struct {
	latest    map[string]time.Time
	earliest  map[string]time.Time
	rawFrom   time.Time
	autoVac   int
	purgeLeft int

	// calls records the range each tier was asked for.
	calls []call
}

type call struct {
	tier     string
	from, to time.Time
}

func newFake() *fakeStore {
	return &fakeStore{
		latest:   map[string]time.Time{},
		earliest: map[string]time.Time{},
		autoVac:  2,
	}
}

func (f *fakeStore) RollUpRaw(_ context.Context, from, to time.Time) (int64, error) {
	f.calls = append(f.calls, call{"1m", from, to})
	return 1, nil
}

func (f *fakeStore) RollUpTier(_ context.Context, tier, _ Tier, from, to time.Time) (int64, error) {
	f.calls = append(f.calls, call{tier.Name, from, to})
	return 1, nil
}

func (f *fakeStore) LatestBucket(_ context.Context, tier Tier) (time.Time, error) {
	return f.latest[tier.Name], nil
}

func (f *fakeStore) EarliestBucket(_ context.Context, tier Tier) (time.Time, error) {
	return f.earliest[tier.Name], nil
}

func (f *fakeStore) EarliestRaw(context.Context) (time.Time, error) { return f.rawFrom, nil }

func (f *fakeStore) DeleteHeartbeatsBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (f *fakeStore) DeleteBucketsBefore(context.Context, Tier, time.Time, int) (int64, error) {
	return 0, nil
}

func (f *fakeStore) DeleteDeliveriesBefore(context.Context, string, time.Time, int) (int64, error) {
	return 0, nil
}

func (f *fakeStore) RefreshUptimeCache(context.Context, time.Time) (int64, error) { return 0, nil }

func (f *fakeStore) PurgeNext(context.Context, int) (int64, bool, error) {
	if f.purgeLeft <= 0 {
		return 0, true, nil
	}
	f.purgeLeft--
	return 10, false, nil
}

func (f *fakeStore) IncrementalVacuum(context.Context, int) error { return nil }

func (f *fakeStore) AutoVacuumMode(context.Context) (int, error) { return f.autoVac, nil }

func testRunner(store Store) *Runner {
	return NewRunner(store, DefaultRetention(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (f *fakeStore) callFor(t *testing.T, tier string) call {
	t.Helper()

	for _, c := range f.calls {
		if c.tier == tier {
			return c
		}
	}
	t.Fatalf("no rollup call for tier %s", tier)
	return call{}
}

// A bucket is only computed once its interval has closed and the grace period
// has passed. Computing the open bucket would publish a partial figure that
// looks final — the minute would read as half its real traffic until something
// happened to recompute it.
func TestRunnerOnlyBuildsClosedBuckets(t *testing.T) {
	t.Parallel()

	store := newFake()
	now := time.Date(2026, 8, 19, 10, 30, 45, 0, time.UTC)
	store.latest["1m"] = now.Add(-time.Hour)

	if err := testRunner(store).Rollup(context.Background(), now); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	got := store.callFor(t, "1m")
	// 10:30:45 minus 30s of grace is 10:30:15, whose minute bucket starts at
	// 10:30 — so the exclusive end is 10:30 and the 10:30 bucket itself is not
	// computed yet.
	if want := time.Date(2026, 8, 19, 10, 30, 0, 0, time.UTC); !got.to.Equal(want) {
		t.Errorf("to = %s, want %s", got.to, want)
	}
}

// The window winds back by Tier.Reprocess so heartbeats replayed after a probe
// outage are folded into buckets that were already computed. Without it the
// outage leaves a permanently undercounted range.
func TestRunnerReprocessesATrailingWindow(t *testing.T) {
	t.Parallel()

	store := newFake()
	now := time.Date(2026, 8, 19, 10, 30, 45, 0, time.UTC)
	latest := time.Date(2026, 8, 19, 10, 25, 0, 0, time.UTC)
	store.latest["1m"] = latest

	if err := testRunner(store).Rollup(context.Background(), now); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	got := store.callFor(t, "1m")
	if want := latest.Add(-tier1m.Reprocess); !got.from.Equal(want) {
		t.Errorf("from = %s, want %s (the newest bucket less the reprocess window)", got.from, want)
	}
}

// An empty tier starts from the oldest thing there is to aggregate — raw for
// 1m, the tier below for the rest. A tier that started from "now" would leave
// everything before the first run permanently unrolled.
func TestRunnerBootstrapsFromTheOldestSource(t *testing.T) {
	t.Parallel()

	store := newFake()
	now := time.Date(2026, 8, 19, 10, 30, 45, 0, time.UTC)
	store.rawFrom = time.Date(2026, 8, 19, 8, 17, 33, 0, time.UTC)
	store.earliest["1m"] = time.Date(2026, 8, 19, 8, 17, 0, 0, time.UTC)

	if err := testRunner(store).Rollup(context.Background(), now); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	// Floored to the tier, not the raw timestamp: a bucket_start of 08:17:33
	// would not align with anything.
	got := store.callFor(t, "1m")
	if want := time.Date(2026, 8, 19, 8, 17, 0, 0, time.UTC); !got.from.Equal(want) {
		t.Errorf("1m from = %s, want %s", got.from, want)
	}

	got = store.callFor(t, "5m")
	if want := time.Date(2026, 8, 19, 8, 15, 0, 0, time.UTC); !got.from.Equal(want) {
		t.Errorf("5m from = %s, want %s", got.from, want)
	}
}

// Nothing to aggregate means nothing to do. A fresh install must not ask the
// database to build every bucket since the epoch.
func TestRunnerDoesNothingWithoutData(t *testing.T) {
	t.Parallel()

	store := newFake()
	if err := testRunner(store).Rollup(context.Background(), time.Now().UTC()); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(store.calls) != 0 {
		t.Errorf("made %d rollup calls against an empty database, want 0: %+v", len(store.calls), store.calls)
	}
}

// Catch-up is bounded. A process down for a week has a week of buckets to
// build, and doing it in one statement holds the single SQLite writer for the
// duration — which every monitor trying to record a heartbeat experiences as an
// outage. What is left over is picked up next tick.
func TestRunnerBoundsCatchUp(t *testing.T) {
	t.Parallel()

	store := newFake()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	store.latest["1m"] = now.AddDate(0, 0, -7)

	if err := testRunner(store).Rollup(context.Background(), now); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	got := store.callFor(t, "1m")
	span := got.to.Sub(got.from)
	if want := time.Duration(maxBucketsPerRun) * time.Minute; span != want {
		t.Errorf("one pass spans %s, want it capped at %s", span, want)
	}
	if got.to.After(now) {
		t.Errorf("to = %s is in the future", got.to)
	}
}

// Tiers are built finest first, because each reads what the previous one just
// wrote. Building 1d before 1h would summarise yesterday from an hour table
// that had not been updated yet.
func TestRunnerBuildsTiersFinestFirst(t *testing.T) {
	t.Parallel()

	store := newFake()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	for _, tier := range Tiers {
		store.latest[tier.Name] = now.Add(-2 * time.Hour)
	}

	if err := testRunner(store).Rollup(context.Background(), now); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if len(store.calls) != len(Tiers) {
		t.Fatalf("made %d calls, want one per tier", len(store.calls))
	}
	for i, c := range store.calls {
		if c.tier != Tiers[i].Name {
			t.Errorf("call %d was for %s, want %s", i, c.tier, Tiers[i].Name)
		}
	}
}

// A database that cannot reclaim space says so once. It is a real problem and
// also an unchanging one, so repeating it hourly forever would train people to
// ignore the log it appears in.
func TestRunnerWarnsOnceAboutAutoVacuum(t *testing.T) {
	t.Parallel()

	store := newFake()
	store.autoVac = 0

	runner := testRunner(store)
	runner.reclaim(context.Background())
	if !runner.warnedNoVacuum {
		t.Fatal("no warning recorded for a database that cannot reclaim space")
	}
	// Second call must be a no-op rather than a second warning.
	runner.reclaim(context.Background())
}

// The purge drains to completion rather than one batch per pass: a monitor
// deleted from a 5,000-monitor install leaves millions of rows, and one batch a
// minute would take days to clear them.
func TestRunnerDrainsThePurgeQueue(t *testing.T) {
	t.Parallel()

	store := newFake()
	store.purgeLeft = 5

	if err := testRunner(store).Purge(context.Background()); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if store.purgeLeft != 0 {
		t.Errorf("%d batches left; the queue was not drained", store.purgeLeft)
	}
}
