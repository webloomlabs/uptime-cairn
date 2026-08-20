package model

import (
	"testing"
	"time"
)

// Days remaining is floored rather than truncated, and the difference only shows
// on the side that matters: a certificate that expired eleven hours ago has -1
// days left, not 0. Truncation rounds towards zero, which would render "expires
// today" for something that has already gone.
func TestDaysRemainingIsFloored(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	cases := map[time.Duration]int{
		90 * 24 * time.Hour:  90,
		25 * time.Hour:       1,
		time.Hour:            0,
		-11 * time.Hour:      -1,
		-25 * time.Hour:      -2,
		-40 * 24 * time.Hour: -40,
	}
	for remaining, want := range cases {
		certificate := Certificate{ValidTo: now.Add(remaining)}
		if got := certificate.DaysRemaining(now); got != want {
			t.Errorf("certificate %s out: days = %d, want %d", remaining, got, want)
		}
		registration := DomainExpiry{ExpiresAt: now.Add(remaining)}
		if got := registration.DaysRemaining(now); got != want {
			t.Errorf("registration %s out: days = %d, want %d", remaining, got, want)
		}
	}
}
