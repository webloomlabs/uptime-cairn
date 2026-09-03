package maintenance

import (
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Cron scheduling, exercised through a maintenance window rather than through
// the parser.
//
// The parser itself moved to internal/cron when report schedules became its
// second caller, and its own tests went with it. These stayed: what they check
// is that a window built on a cron expression produces the occurrences somebody
// expects, which is a question about this package and not about that one.

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
