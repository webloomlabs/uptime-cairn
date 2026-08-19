package api

import (
	"testing"
	"time"
)

// Resolution selection decides how many rows a request reads. Get it wrong in
// one direction and a chart has four points; wrong in the other and a casual
// request for a year of one-minute buckets asks SQLite for half a million rows
// while every monitor waits behind it for the single writer.

func TestAutoResolution(t *testing.T) {
	t.Parallel()

	cases := []struct {
		span time.Duration
		want string
	}{
		// An hour has no tier coarse enough to give a useful number of points,
		// so it steps down to the finest that fits.
		{time.Hour, "1m"},
		{6 * time.Hour, "1m"},
		// A day at 5m is 288 points — a chart. At 1h it would be 24.
		{24 * time.Hour, "5m"},
		{2 * 24 * time.Hour, "5m"},
		// A week at 1h is 168.
		{7 * 24 * time.Hour, "1h"},
		{30 * 24 * time.Hour, "1h"},
		// Ninety days at 1h would be 2,160 — past the point where the response
		// is a chart — so it settles for 90 daily points instead.
		{90 * 24 * time.Hour, "1d"},
		{365 * 24 * time.Hour, "1d"},
	}

	for _, tc := range cases {
		got := autoHistoryTier(tc.span)
		if got.name != tc.want {
			t.Errorf("auto over %s = %s, want %s (%d buckets)",
				tc.span, got.name, tc.want, tc.span/got.interval)
		}
		if n := tc.span / got.interval; n > maxHistoryBuckets {
			t.Errorf("auto over %s chose %s, which is %d buckets", tc.span, got.name, n)
		}
	}
}

// The spec allows the response to be coarser than requested, and this is where
// that happens: refusing would make the endpoint hostile, and honouring it would
// make it a denial-of-service tool against the single writer.
func TestExplicitResolutionCoarsensWhenTooManyBuckets(t *testing.T) {
	t.Parallel()

	to := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	interval := 20 * time.Second

	cases := []struct {
		requested string
		span      time.Duration
		want      string
	}{
		// Honoured as asked.
		{"1m", time.Hour, "1m"},
		{"5m", 24 * time.Hour, "5m"},
		{"1h", 30 * 24 * time.Hour, "1h"},
		{"1d", 365 * 24 * time.Hour, "1d"},
		{"raw", time.Hour, "raw"},

		// Coarsened. A year of minutes is 525,600 buckets.
		{"1m", 365 * 24 * time.Hour, "1d"},
		{"1m", 90 * 24 * time.Hour, "1d"},
		{"5m", 365 * 24 * time.Hour, "1d"},
		// Raw over a month at a 20-second interval is 129,600 checks.
		{"raw", 30 * 24 * time.Hour, "1h"},
	}

	for _, tc := range cases {
		got, problem := resolveHistoryTier(tc.requested, to.Add(-tc.span), to, interval)
		if problem != "" {
			t.Errorf("%s over %s: %s", tc.requested, tc.span, problem)
			continue
		}
		if got.name != tc.want {
			t.Errorf("%s over %s = %s, want %s", tc.requested, tc.span, got.name, tc.want)
		}
	}
}

// raw means one bucket per check, so its width is the monitor's own interval.
// Any fixed number here would be an invention.
func TestRawResolutionUsesTheMonitorInterval(t *testing.T) {
	t.Parallel()

	to := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	got, problem := resolveHistoryTier("raw", to.Add(-time.Hour), to, 20*time.Second)
	if problem != "" {
		t.Fatalf("raw rejected: %s", problem)
	}
	if got.interval != 20*time.Second {
		t.Errorf("raw interval = %s, want the monitor's 20s", got.interval)
	}
}

func TestUnknownResolutionIsRejected(t *testing.T) {
	t.Parallel()

	to := time.Now().UTC()
	if _, problem := resolveHistoryTier("30s", to.Add(-time.Hour), to, time.Minute); problem == "" {
		t.Error("resolution 30s was accepted")
	}
}
