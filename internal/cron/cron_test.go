package cron

import (
	"strings"
	"testing"
	"time"
)

func TestCronRejectsWhatItDoesNotSupport(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"too few fields":  "0 2 * *",
		"too many fields": "0 0 2 * * *",
		"named weekday":   "0 9 * * MON",
		"out of range":    "0 25 * * *",
		"bad step":        "*/0 * * * *",
		"empty part":      "0,, * * * *",
	}

	for name, expression := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(expression)
			if err == nil {
				t.Fatalf("%q was accepted", expression)
			}
			// The message has to say what is missing, or the user is left
			// guessing which of five fields the parser disliked.
			if strings.TrimSpace(err.Error()) == "" {
				t.Error("empty error message")
			}
		})
	}
}

func TestCronAliasesAreRefusedByName(t *testing.T) {
	t.Parallel()

	// "@daily" is one field, so it fails the count check first; the point of the
	// assertion is that a user who tries it is told aliases are the problem.
	if _, err := Parse("@daily"); err == nil {
		t.Fatal("@daily was accepted")
	}
	if _, err := Parse("@daily * * * *"); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Errorf("err = %v, want it to name aliases", err)
	}
}

func TestCronStepsFromABase(t *testing.T) {
	t.Parallel()

	// "5/15" is "from 5, every 15" — 5, 20, 35, 50.
	expression, err := Parse("5/15 * * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, want := range []int{5, 20, 35, 50} {
		if !expression.minutes[want] {
			t.Errorf("minute %d is missing", want)
		}
	}
	if expression.minutes[0] || expression.minutes[10] {
		t.Error("the step matched a minute before its base")
	}
}

// --- Next -------------------------------------------------------------------

func at(t *testing.T, value string, loc *time.Location) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestNextFindsTheFollowingFiring(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		expression string
		after      string
		want       string
	}{
		{"later the same day", "0 9 * * *", "2026-08-19 06:00", "2026-08-19 09:00"},
		{"tomorrow when today has passed", "0 9 * * *", "2026-08-19 09:00", "2026-08-20 09:00"},
		{"strictly after, never equal", "0 9 * * *", "2026-08-19 08:59", "2026-08-19 09:00"},
		{"the first of the month", "0 9 1 * *", "2026-08-19 00:00", "2026-09-01 09:00"},
		{"weekdays only", "30 7 * * 1-5", "2026-08-22 00:00", "2026-08-24 07:30"},
		{"several times a day", "0 6,18 * * *", "2026-08-19 07:00", "2026-08-19 18:00"},
		{"the 29th of February", "0 2 29 2 *", "2026-08-19 00:00", "2028-02-29 02:00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expression, err := Parse(tc.expression)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := expression.Next(at(t, tc.after, time.UTC), time.UTC)
			if !ok {
				t.Fatalf("%q found no firing after %s", tc.expression, tc.after)
			}
			if want := at(t, tc.want, time.UTC); !got.Equal(want) {
				t.Errorf("next(%q, %s) = %s, want %s", tc.expression, tc.after,
					got.Format("2006-01-02 15:04 MST"), tc.want)
			}
		})
	}
}

// **The firing is cut in the schedule's zone, not in UTC.** "09:00 on the first"
// for a Sydney agency is 22:00 or 23:00 UTC on the last day of the *previous*
// month depending on daylight saving, and a scheduler that computed it in UTC
// would send a monthly report on the wrong day twice a year.
func TestNextIsCutInTheGivenZone(t *testing.T) {
	t.Parallel()

	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skip("no tzdata")
	}
	expression, err := Parse("0 9 1 * *")
	if err != nil {
		t.Fatal(err)
	}

	// Late August in UTC is already the 1st of September in Sydney's evening
	// terms; the firing is the September one, at 09:00 local.
	got, ok := expression.Next(time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), sydney)
	if !ok {
		t.Fatal("no firing found")
	}
	local := got.In(sydney)
	if local.Hour() != 9 || local.Day() != 1 || local.Month() != time.September {
		t.Errorf("next = %s, want 09:00 on 1 September in Sydney", local.Format(time.RFC3339))
	}
	// And in UTC it is the previous day, which is the whole point.
	if got.UTC().Day() != 31 || got.UTC().Month() != time.August {
		t.Errorf("in UTC that is %s; a UTC-computed schedule would fire on the wrong day",
			got.UTC().Format(time.RFC3339))
	}
}

// Both sides of a daylight-saving change are handled, and the awkward one is
// stated rather than left to chance: a firing inside the gap when clocks go
// forward is normalised rather than skipped, because a report that silently does
// not arrive is the worse of the two surprises.
func TestNextSurvivesDaylightSaving(t *testing.T) {
	t.Parallel()

	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("no tzdata")
	}
	// 02:30 daily. On 2026-03-29 the clocks go forward at 01:00 and 02:30 does
	// not exist.
	expression, err := Parse("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}

	got, ok := expression.Next(time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC), london)
	if !ok {
		t.Fatal("no firing found")
	}
	if want := time.Date(2026, 3, 29, 2, 30, 0, 0, london); !got.Equal(want) {
		t.Errorf("next = %s, want %s — the firing is normalised into the gap, not skipped",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	// It keeps moving forward afterwards rather than sticking.
	after, ok := expression.Next(got, london)
	if !ok || !after.After(got) {
		t.Errorf("the firing after %s is %s; the walk stalled", got, after)
	}
}

// A nil zone is UTC rather than the host's local time. Guessing the machine's
// zone would make the same schedule mean different things on different servers.
func TestANilZoneIsUTC(t *testing.T) {
	t.Parallel()

	expression, err := Parse("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := expression.Next(time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC), nil)
	if !ok || got.UTC().Hour() != 9 {
		t.Errorf("next = %s, want 09:00 UTC", got)
	}
}
