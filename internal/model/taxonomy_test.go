package model

import "testing"

func TestSlugify(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Production":      "production",
		"PRODUCTION":      "production",
		"Prod / EU":       "prod-eu",
		"  spaced  out  ": "spaced-out",
		"under_scores":    "under-scores",
		"already-hyphen":  "already-hyphen",
		"double--hyphen":  "double-hyphen",
		"trailing-":       "trailing",
		"-leading":        "leading",
		"v2.1":            "v2-1",
		"100% uptime":     "100-uptime",
		"emoji 🚀 rocket":  "emoji-rocket",
	}

	for name, want := range cases {
		if got := Slugify(name); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", name, got, want)
		}
	}
}

// Two tags that render identically in a list are two tags nobody can tell
// apart, which is the reason the slug is derived rather than supplied.
func TestSlugifyCollapsesLookalikes(t *testing.T) {
	t.Parallel()

	for _, pair := range [][2]string{
		{"Prod / EU", "prod-eu"},
		{"Prod EU", "prod eu"},
		{"web-tier", "Web Tier"},
	} {
		if Slugify(pair[0]) != Slugify(pair[1]) {
			t.Errorf("%q and %q slugify differently: %q vs %q",
				pair[0], pair[1], Slugify(pair[0]), Slugify(pair[1]))
		}
	}
}

// A name in another script leaves nothing behind. The write path refuses that
// rather than inventing an identifier, so the empty result has to be reachable.
func TestSlugifyCanBeEmpty(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"日本語", "///", "   ", "🚀"} {
		if got := Slugify(name); got != "" {
			t.Errorf("Slugify(%q) = %q, want empty", name, got)
		}
	}
}

// The order a group reports. down first, because an outage anywhere in a group
// is the group's headline — a parent showing green during an outage underneath
// it is the worst thing a monitoring tool can do.
func TestWorstStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		statuses []string
		want     string
	}{
		{nil, ""},
		{[]string{MonitorStatusUp}, MonitorStatusUp},
		{[]string{MonitorStatusUp, MonitorStatusDown}, MonitorStatusDown},
		{[]string{MonitorStatusDown, MonitorStatusUp}, MonitorStatusDown},
		{[]string{MonitorStatusPending, MonitorStatusUp}, MonitorStatusPending},
		{[]string{MonitorStatusMaintenance, MonitorStatusPending}, MonitorStatusMaintenance},
		{[]string{MonitorStatusPaused, MonitorStatusUp}, MonitorStatusPaused},
		{[]string{MonitorStatusUp, MonitorStatusPaused, MonitorStatusPending, MonitorStatusDown}, MonitorStatusDown},
	}

	for _, tc := range cases {
		if got := WorstStatus(tc.statuses); got != tc.want {
			t.Errorf("WorstStatus(%v) = %q, want %q", tc.statuses, got, tc.want)
		}
	}
}

// A status this function has not been taught about must be visible rather than
// silently benign.
func TestUnknownStatusSortsWorst(t *testing.T) {
	t.Parallel()

	if got := WorstStatus([]string{MonitorStatusUp, "something-new"}); got != "something-new" {
		t.Errorf("WorstStatus = %q, want the unrecognised value to surface", got)
	}
}
