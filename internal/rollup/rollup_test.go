package rollup

import (
	"testing"
	"time"
)

// The bucket contract is the one thing SQLite and Timescale have to agree on
// byte for byte (§5.4), and it is the kind of agreement that breaks silently:
// nothing fails, the two backends just quietly report different uptime.

func TestBucketAlignment(t *testing.T) {
	t.Parallel()

	// Deliberately awkward: mid-minute, mid-hour, mid-day, with a non-zero
	// nanosecond so truncation of the sub-second part is exercised too.
	at := time.Date(2026, 8, 19, 13, 47, 31, 987654321, time.UTC)

	want := map[time.Duration]time.Time{
		time.Minute:     time.Date(2026, 8, 19, 13, 47, 0, 0, time.UTC),
		5 * time.Minute: time.Date(2026, 8, 19, 13, 45, 0, 0, time.UTC),
		time.Hour:       time.Date(2026, 8, 19, 13, 0, 0, 0, time.UTC),
		24 * time.Hour:  time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC),
	}
	for interval, expected := range want {
		if got := Bucket(at, interval); !got.Equal(expected) {
			t.Errorf("Bucket(%s, %s) = %s, want %s", at, interval, got, expected)
		}
	}
}

// Buckets are aligned to the epoch, not to the caller's timezone. A day bucket
// computed in Asia/Kolkata would start at 18:30 UTC the day before, and two
// probes in two regions would then disagree about which day a heartbeat is in.
func TestBucketIgnoresLocation(t *testing.T) {
	t.Parallel()

	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	at := time.Date(2026, 8, 19, 13, 47, 31, 0, time.UTC)

	for _, interval := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour} {
		utc := Bucket(at, interval)
		local := Bucket(at.In(kolkata), interval)
		if !utc.Equal(local) {
			t.Errorf("interval %s: UTC gave %s, Asia/Kolkata gave %s", interval, utc, local)
		}
	}
}

// Inclusive start, exclusive end — so a heartbeat lands in exactly one bucket at
// each tier and no heartbeat is counted twice or dropped.
func TestBucketBoundariesAreHalfOpen(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 19, 13, 45, 0, 0, time.UTC)
	interval := 5 * time.Minute

	cases := map[string]struct {
		at   time.Time
		want time.Time
	}{
		"exact start":       {start, start},
		"one nanosecond in": {start.Add(time.Nanosecond), start},
		"last nanosecond":   {start.Add(interval - time.Nanosecond), start},
		"exact end":         {start.Add(interval), start.Add(interval)},
	}
	for name, tc := range cases {
		if got := Bucket(tc.at, interval); !got.Equal(tc.want) {
			t.Errorf("%s: Bucket(%s) = %s, want %s", name, tc.at, got, tc.want)
		}
	}
}

// Every tier's bucket must nest exactly inside the tier above it, or a coarse
// bucket would be computed from a fractional set of finer ones and the sums
// would not add up.
func TestTiersNest(t *testing.T) {
	t.Parallel()

	for _, tier := range Tiers {
		if tier.Source == nil {
			continue
		}
		if tier.Interval%tier.Source.Interval != 0 {
			t.Errorf("%s (%s) is not a whole multiple of %s (%s)",
				tier.Name, tier.Interval, tier.Source.Name, tier.Source.Interval)
		}
	}

	// And the chain has to actually be a chain, finest first, since each run
	// computes a tier from what the previous one just wrote.
	for i := 1; i < len(Tiers); i++ {
		if Tiers[i].Interval <= Tiers[i-1].Interval {
			t.Errorf("tier %d (%s) is not coarser than tier %d (%s)",
				i, Tiers[i].Name, i-1, Tiers[i-1].Name)
		}
		if Tiers[i].Source == nil || Tiers[i].Source.Name != Tiers[i-1].Name {
			t.Errorf("tier %s does not read from %s", Tiers[i].Name, Tiers[i-1].Name)
		}
	}
}

// A coarser tier retained for less time than a finer one leaves a hole in the
// middle of history: detail deleted before the summary that replaced it exists.
func TestRetentionValidate(t *testing.T) {
	t.Parallel()

	if err := DefaultRetention().Validate(); err != nil {
		t.Errorf("the documented defaults do not validate: %v", err)
	}

	bad := DefaultRetention()
	bad.Rollup5mDays = 10 // shorter than the 1m tier's 30
	if err := bad.Validate(); err == nil {
		t.Error("a coarser tier retained for less than a finer one was accepted")
	}

	// Zero means indefinite, which is longer than any finite value — the 1d
	// tier's default, and it must not read as "shorter than everything".
	indefinite := DefaultRetention()
	indefinite.Rollup1dDays = 0
	if err := indefinite.Validate(); err != nil {
		t.Errorf("indefinite 1d retention rejected: %v", err)
	}

	// The reverse: a finite coarse tier under an indefinite finer one is a hole.
	inverted := DefaultRetention()
	inverted.Rollup1hDays = 0
	inverted.Rollup1dDays = 365
	if err := inverted.Validate(); err == nil {
		t.Error("a finite coarse tier under an indefinite finer one was accepted")
	}

	zeroRaw := DefaultRetention()
	zeroRaw.RawDays = 0
	if err := zeroRaw.Validate(); err == nil {
		t.Error("raw_days = 0 was accepted; raw history is not optional")
	}
}
