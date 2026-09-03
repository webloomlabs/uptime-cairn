package maintenance

import (
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

func cronWindow(t *testing.T, expression string) model.MaintenanceWindow {
	t.Helper()

	w := window(model.StrategyCron)
	w.StartsAt = utc(t, "2026-01-01 00:00")
	w.Duration = 30 * time.Minute
	w.Recurrence.Cron = expression
	return w
}

func TestCronSchedules(t *testing.T) {
	t.Parallel()

	// Every `after` here is chosen to fall outside any occurrence, so these
	// exercise "when does the next one start". The in-progress case has its own
	// test, because the two answers differ and both matter.
	cases := []struct {
		name       string
		expression string
		duration   time.Duration
		after      string
		wantStart  string
	}{
		{"every day at 02:30", "30 2 * * *", 30 * time.Minute, "2026-08-19 12:00", "2026-08-20 02:30"},
		{"top of every hour", "0 * * * *", 30 * time.Minute, "2026-08-19 12:31", "2026-08-19 13:00"},
		{"every fifteen minutes", "*/15 * * * *", 5 * time.Minute, "2026-08-19 12:06", "2026-08-19 12:15"},
		{"a list of hours", "0 2,14 * * *", 30 * time.Minute, "2026-08-19 03:00", "2026-08-19 14:00"},
		{"a range of weekdays", "0 9 * * 1-5", 30 * time.Minute, "2026-08-22 00:00", "2026-08-24 09:00"}, // Sat 22nd -> Mon 24th
		{"the first of the month", "0 0 1 * *", 30 * time.Minute, "2026-08-19 00:00", "2026-09-01 00:00"},
		{"a single month", "0 0 1 2 *", 30 * time.Minute, "2026-08-19 00:00", "2027-02-01 00:00"},
		// The famous one: four years of lookahead is not theoretical.
		{"the 29th of February", "0 2 29 2 *", 30 * time.Minute, "2026-08-19 00:00", "2028-02-29 02:00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w := cronWindow(t, tc.expression)
			w.Duration = tc.duration
			occurrence := next(t, w, utc(t, tc.after))
			if !occurrence.Start.Equal(utc(t, tc.wantStart)) {
				t.Errorf("%s after %s = %s, want %s", tc.expression, tc.after,
					occurrence.Start.Format("2006-01-02 15:04"), tc.wantStart)
			}
		})
	}
}

// Cron's day rule is a union when both day fields are restricted, which is a
// genuine oddity of the format rather than a bug here. Encoding it wrong makes a
// schedule fire on the wrong days and nothing complains.
func TestCronDayFieldsAreAUnionWhenBothAreRestricted(t *testing.T) {
	t.Parallel()

	// The 1st of the month, or any Monday.
	w := cronWindow(t, "0 0 1 * 1")

	// 2026-09-01 is a Tuesday: it matches on day-of-month alone.
	first := next(t, w, utc(t, "2026-08-26 00:00"))
	if !first.Start.Equal(utc(t, "2026-08-31 00:00")) {
		// 2026-08-31 is a Monday, and comes first.
		t.Errorf("first = %s, want the Monday 2026-08-31", first.Start)
	}

	second := next(t, w, first.End)
	if !second.Start.Equal(utc(t, "2026-09-01 00:00")) {
		t.Errorf("second = %s, want the 1st", second.Start)
	}
}

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

			_, err := parseCron(expression)
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
	if _, err := parseCron("@daily"); err == nil {
		t.Fatal("@daily was accepted")
	}
	if _, err := parseCron("@daily * * * *"); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Errorf("err = %v, want it to name aliases", err)
	}
}

func TestCronStepsFromABase(t *testing.T) {
	t.Parallel()

	// "5/15" is "from 5, every 15" — 5, 20, 35, 50.
	expression, err := parseCron("5/15 * * * *")
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
