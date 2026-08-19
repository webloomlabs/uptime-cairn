package model

import "testing"

func TestOverallStatusPrefersTheTruthOverTheExcuse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"nothing listed is operational", nil, OverallOperational},
		{"all up", []string{MonitorStatusUp, MonitorStatusUp}, OverallOperational},
		{"one of four down is partial", []string{
			MonitorStatusDown, MonitorStatusUp, MonitorStatusUp, MonitorStatusUp}, OverallPartialOutage},
		{"half down is major", []string{MonitorStatusDown, MonitorStatusUp}, OverallMajorOutage},
		{"maintenance alone", []string{MonitorStatusMaintenance, MonitorStatusUp}, OverallMaintenance},
		{"pending alone is degraded", []string{MonitorStatusPending, MonitorStatusUp}, OverallDegraded},

		// The one that matters. A real outage during a declared maintenance
		// window is still an outage, and a page reporting it as scheduled work
		// is the sort of thing that ends up on a screenshot.
		{"an outage during maintenance is still an outage", []string{
			MonitorStatusMaintenance, MonitorStatusDown, MonitorStatusUp, MonitorStatusUp}, OverallPartialOutage},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := OverallStatus(tc.statuses); got != tc.want {
				t.Fatalf("OverallStatus(%v) = %q, want %q", tc.statuses, got, tc.want)
			}
		})
	}
}

func TestValidSlug(t *testing.T) {
	t.Parallel()

	valid := []string{"status", "acme-status", "a", "a1", "eu-west-1"}
	for _, slug := range valid {
		if !ValidSlug(slug) {
			t.Errorf("ValidSlug(%q) = false, want true", slug)
		}
	}

	invalid := []string{"", "-leading", "trailing-", "Upper", "with space", "under_score", "sl/ash"}
	for _, slug := range invalid {
		if ValidSlug(slug) {
			t.Errorf("ValidSlug(%q) = true, want false", slug)
		}
	}
}

func TestMaskTargetLeavesEnoughToRecogniseYourOwn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		channel, target, want string
	}{
		{SubscriberEmail, "alice@example.com", "al…@example.com"},
		{SubscriberEmail, "a@example.com", "a…@example.com"},
		{SubscriberWebhook, "https://hooks.example.com/services/T000/B000", "https://hooks.example.com/…"},
	}

	for _, tc := range cases {
		if got := MaskTarget(tc.channel, tc.target); got != tc.want {
			t.Errorf("MaskTarget(%q, %q) = %q, want %q", tc.channel, tc.target, got, tc.want)
		}
	}

	// The property the test is really about: the address does not survive.
	if got := MaskTarget(SubscriberEmail, "alice@example.com"); got == "alice@example.com" {
		t.Fatal("MaskTarget returned the address unchanged")
	}
}
