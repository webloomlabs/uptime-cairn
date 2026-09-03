package report

import (
	"strings"
	"testing"
	"time"
)

func atUTC(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02 15:04", value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// Each named frequency is an expression, so there is one implementation of "when
// does this next fire" rather than five arms of calendar arithmetic beside a
// sixth that calls the parser.
func TestNamedFrequenciesAreExpressions(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ frequency, sendAt, want string }{
		"daily":     {"daily", "09:00", "0 9 * * *"},
		"weekly":    {"weekly", "07:30", "30 7 * * 1"},
		"monthly":   {"monthly", "09:00", "0 9 1 * *"},
		"quarterly": {"quarterly", "06:15", "15 6 1 1,4,7,10 *"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := CronFor(ScheduleSpec{Frequency: tc.frequency, SendAt: tc.sendAt})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("cron = %q, want %q", got, tc.want)
			}
		})
	}
}

// The firings a client actually experiences, checked against dates somebody can
// verify on a calendar.
func TestNextRunForEachFrequency(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		spec  ScheduleSpec
		after string
		want  string
	}{
		{"daily, later today", ScheduleSpec{Frequency: "daily", SendAt: "09:00"}, "2026-08-19 06:00", "2026-08-19 09:00"},
		{"daily, tomorrow", ScheduleSpec{Frequency: "daily", SendAt: "09:00"}, "2026-08-19 09:00", "2026-08-20 09:00"},
		// 2026-08-19 is a Wednesday; the next Monday is the 24th.
		{"weekly lands on Monday", ScheduleSpec{Frequency: "weekly", SendAt: "09:00"}, "2026-08-19 00:00", "2026-08-24 09:00"},
		{"monthly lands on the first", ScheduleSpec{Frequency: "monthly", SendAt: "09:00"}, "2026-08-19 00:00", "2026-09-01 09:00"},
		// August is in Q3, so the next quarter starts on 1 October.
		{"quarterly lands on a quarter boundary", ScheduleSpec{Frequency: "quarterly", SendAt: "09:00"}, "2026-08-19 00:00", "2026-10-01 09:00"},
		{"cron carries its own time", ScheduleSpec{Frequency: "cron", Cron: "0 3 * * 6"}, "2026-08-19 00:00", "2026-08-22 03:00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := NextRun(tc.spec, atUTC(t, tc.after))
			if err != nil {
				t.Fatal(err)
			}
			if want := atUTC(t, tc.want); !got.Equal(want) {
				t.Errorf("next = %s, want %s", got.Format("2006-01-02 15:04"), tc.want)
			}
		})
	}
}

// **A weekly report fires on Monday and therefore covers the week that just
// ended**, matching the Monday-start weeks window.go already cuts. A Sunday
// firing would deliver a week with a weekend split across it.
func TestWeeklyFiresOnMonday(t *testing.T) {
	t.Parallel()

	next, err := NextRun(ScheduleSpec{Frequency: "weekly", SendAt: "09:00"}, atUTC(t, "2026-08-19 00:00"))
	if err != nil {
		t.Fatal(err)
	}
	if next.Weekday() != time.Monday {
		t.Errorf("weekly fires on %s, want Monday", next.Weekday())
	}
}

// **The zone is the schedule's, not the server's.** "09:00 on the first" for a
// Sydney agency is 23:00 UTC on the 31st of the previous month in winter, and a
// schedule computed in UTC would send a monthly report a working day early.
func TestTheFiringIsCutInTheSchedulesZone(t *testing.T) {
	t.Parallel()

	if _, err := time.LoadLocation("Australia/Sydney"); err != nil {
		t.Skip("no tzdata")
	}
	spec := ScheduleSpec{Frequency: "monthly", SendAt: "09:00", Timezone: "Australia/Sydney"}

	next, err := NextRun(spec, atUTC(t, "2026-08-19 00:00"))
	if err != nil {
		t.Fatal(err)
	}

	sydney, _ := time.LoadLocation("Australia/Sydney")
	local := next.In(sydney)
	if local.Day() != 1 || local.Hour() != 9 || local.Month() != time.September {
		t.Errorf("local = %s, want 09:00 on 1 September", local.Format(time.RFC3339))
	}
	if next.Day() != 31 || next.Month() != time.August {
		t.Errorf("in UTC that is %s; a UTC schedule would fire on the wrong day", next.Format(time.RFC3339))
	}
}

// **A schedule that will never fire is refused rather than stored.** "0 0 30 2 *"
// parses cleanly and matches nothing; stored, it would sit silently forever and
// the operator would discover it when a client asked where the report went.
func TestAScheduleThatNeverFiresIsRefused(t *testing.T) {
	t.Parallel()

	_, err := NextRun(ScheduleSpec{Frequency: "cron", Cron: "0 0 30 2 *"}, atUTC(t, "2026-08-19 00:00"))
	if err == nil {
		t.Fatal("the 30th of February was accepted as a schedule")
	}
	if !strings.Contains(err.Error(), "never fires") {
		t.Errorf("err = %q, want it to say the schedule never fires", err)
	}
}

// A cron on a named frequency is refused rather than ignored. Silently dropping
// it leaves an operator with a schedule they believe they configured, and the
// silence reads as a bug in the product.
func TestACronOnANamedFrequencyIsRefused(t *testing.T) {
	t.Parallel()

	_, err := CronFor(ScheduleSpec{Frequency: "daily", SendAt: "09:00", Cron: "0 3 * * *"})
	if err == nil {
		t.Fatal("a cron expression on a daily schedule was accepted and would never have run")
	}
	if _, err := CronFor(ScheduleSpec{Frequency: "cron"}); err == nil {
		t.Error("a cron frequency with no expression was accepted")
	}
}

// send_at is read strictly. "9:00" and "0900" are both things somebody types,
// and reading either as 09:00 would be a guess; defaulting silently to midnight
// would be worse.
func TestSendAtIsReadStrictly(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"9:00", "0900", "24:00", "09:60", "nine", "09:00:00"} {
		if _, err := CronFor(ScheduleSpec{Frequency: "daily", SendAt: bad}); err == nil {
			t.Errorf("send_at %q was accepted", bad)
		}
	}
	// Empty takes the schema's documented default rather than midnight.
	got, err := CronFor(ScheduleSpec{Frequency: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "0 9 * * *" {
		t.Errorf("an omitted send_at produced %q, want the 09:00 default", got)
	}
}

// An unknown zone is refused by name rather than falling back to UTC, which is
// the same choice ResolveWindow makes: a schedule quietly moved to UTC sends a
// monthly report a working day early for half the world and nothing says so.
func TestAnUnknownZoneIsRefusedByName(t *testing.T) {
	t.Parallel()

	_, err := NextRun(ScheduleSpec{Frequency: "daily", SendAt: "09:00", Timezone: "Mars/Olympus"}, time.Now())
	if err == nil {
		t.Fatal("an unknown zone was accepted")
	}
	if !strings.Contains(err.Error(), "Mars/Olympus") {
		t.Errorf("err = %q, want it to name the zone", err)
	}
}
