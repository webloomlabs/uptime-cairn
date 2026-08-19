package maintenance

import (
	"fmt"
	"sort"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Occurrence is one concrete interval a window resolves to.
type Occurrence struct {
	Start time.Time
	End   time.Time
}

// Covers reports whether an instant falls inside. Start-inclusive and
// end-exclusive, matching the bucket contract the rest of the system uses — one
// convention for half-open intervals, not two.
func (o Occurrence) Covers(at time.Time) bool {
	return !at.Before(o.Start) && at.Before(o.End)
}

// lookaheadDays bounds the search for the next occurrence.
//
// Four years and change, because a cron of "0 2 29 2 *" — 02:00 on the 29th of
// February — is a legal expression whose next match can be nearly four years
// out. The walk is a day-level comparison, so the bound costs microseconds even
// when nothing matches.
const lookaheadDays = 1500

// Next returns the first occurrence that has not finished by `after`, which is
// the one a scheduler needs: an occurrence already in progress is more urgent
// than the next one to start.
func Next(w model.MaintenanceWindow, after time.Time) (Occurrence, bool, error) {
	if w.CancelledAt != nil {
		return Occurrence{}, false, nil
	}

	location, err := zone(w.Timezone)
	if err != nil {
		return Occurrence{}, false, err
	}

	if w.Strategy == model.StrategySingle {
		occurrence := Occurrence{Start: w.StartsAt.UTC(), End: singleEnd(w)}
		if !occurrence.End.After(after) {
			return Occurrence{}, false, nil
		}
		return occurrence, true, nil
	}

	duration := w.Duration
	if duration <= 0 {
		return Occurrence{}, false, fmt.Errorf("a %s window needs a duration", w.Strategy)
	}

	matches, err := dayMatcher(w)
	if err != nil {
		return Occurrence{}, false, err
	}
	starts, err := startTimes(w)
	if err != nil {
		return Occurrence{}, false, err
	}

	// Begin the day before the later of the anchor and the cursor, because an
	// occurrence that started yesterday can still be running now.
	from := w.StartsAt
	if after.After(from) {
		from = after
	}
	local := from.In(location).AddDate(0, 0, -1)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)

	for i := 0; i < lookaheadDays; i++ {
		candidateDay := day.AddDate(0, 0, i)
		if !matches(candidateDay) {
			continue
		}
		for _, clock := range starts {
			// Constructed in local time on purpose: 02:00 means 02:00 to the
			// person who scheduled it, whatever the offset is that week. Go
			// normalises an instant that a spring-forward transition skipped,
			// which is the only sane thing to do with a time that did not exist.
			start := time.Date(candidateDay.Year(), candidateDay.Month(), candidateDay.Day(),
				clock.hour, clock.minute, 0, 0, location)
			if start.Before(w.StartsAt) {
				continue
			}
			if until := w.Recurrence.Until; until != nil && start.After(*until) {
				return Occurrence{}, false, nil
			}
			occurrence := Occurrence{Start: start.UTC(), End: start.Add(duration).UTC()}
			if occurrence.End.After(after) {
				return occurrence, true, nil
			}
		}
	}
	return Occurrence{}, false, nil
}

// ActiveAt reports whether the window covers this instant.
func ActiveAt(w model.MaintenanceWindow, at time.Time) (bool, error) {
	occurrence, ok, err := Next(w, at)
	if err != nil || !ok {
		return false, err
	}
	return occurrence.Covers(at), nil
}

// State derives what the API reports. Derived rather than stored: a stored state
// is wrong between the moment a window starts and the moment something notices,
// which is precisely the interval anybody asks about.
func State(w model.MaintenanceWindow, at time.Time) string {
	if w.CancelledAt != nil {
		return model.MaintenanceCancelled
	}
	occurrence, ok, err := Next(w, at)
	switch {
	case err != nil:
		// An unparseable schedule has no future, and saying "scheduled" about it
		// would be a claim this package cannot support. Validation refuses these
		// at the API, so this is the defensive branch.
		return model.MaintenanceEnded
	case !ok:
		return model.MaintenanceEnded
	case occurrence.Covers(at):
		return model.MaintenanceActive
	default:
		return model.MaintenanceScheduled
	}
}

// singleEnd is the end of a one-off window: its explicit end, or its duration
// after the start when only a duration was given.
func singleEnd(w model.MaintenanceWindow) time.Time {
	if w.EndsAt != nil {
		return w.EndsAt.UTC()
	}
	if w.Duration > 0 {
		return w.StartsAt.Add(w.Duration).UTC()
	}
	return w.StartsAt.UTC()
}

// clock is a time of day in the window's zone.
type clock struct{ hour, minute int }

// startTimes is when an occurrence begins on a day it happens.
//
// Every strategy but cron takes the time of day from the anchor, which is what
// makes "starts_at 2026-08-19T02:00 Australia/Sydney, recurring weekly" mean
// 02:00 Sydney rather than 16:00 UTC in winter and 15:00 in summer.
func startTimes(w model.MaintenanceWindow) ([]clock, error) {
	if w.Strategy != model.StrategyCron {
		location, err := zone(w.Timezone)
		if err != nil {
			return nil, err
		}
		anchor := w.StartsAt.In(location)
		return []clock{{anchor.Hour(), anchor.Minute()}}, nil
	}

	expression, err := parseCron(w.Recurrence.Cron)
	if err != nil {
		return nil, err
	}
	var out []clock
	for _, hour := range sortedKeys(expression.hours) {
		for _, minute := range sortedKeys(expression.minutes) {
			out = append(out, clock{hour, minute})
		}
	}
	return out, nil
}

// dayMatcher answers "does an occurrence begin on this day?" for a local
// midnight.
func dayMatcher(w model.MaintenanceWindow) (func(time.Time) bool, error) {
	switch w.Strategy {
	case model.StrategyRecurringDaily:
		return func(time.Time) bool { return true }, nil

	case model.StrategyRecurringWeekly:
		if len(w.Recurrence.Weekdays) == 0 {
			return nil, fmt.Errorf("a weekly window needs at least one weekday")
		}
		wanted := map[time.Weekday]bool{}
		for _, day := range w.Recurrence.Weekdays {
			if day < 0 || day > 6 {
				return nil, fmt.Errorf("weekday %d is outside 0-6", day)
			}
			wanted[time.Weekday(day)] = true
		}
		return func(day time.Time) bool { return wanted[day.Weekday()] }, nil

	case model.StrategyRecurringMonthly:
		if len(w.Recurrence.DaysOfMonth) == 0 {
			return nil, fmt.Errorf("a monthly window needs at least one day of the month")
		}
		wanted := map[int]bool{}
		for _, day := range w.Recurrence.DaysOfMonth {
			if day < 1 || day > 31 {
				return nil, fmt.Errorf("day of month %d is outside 1-31", day)
			}
			wanted[day] = true
		}
		// A day past the end of a short month is skipped, not clamped: "the
		// 31st" resolving to the 28th of February is a guess about what somebody
		// meant, and a maintenance window is a poor place to guess.
		return func(day time.Time) bool { return wanted[day.Day()] }, nil

	case model.StrategyCron:
		expression, err := parseCron(w.Recurrence.Cron)
		if err != nil {
			return nil, err
		}
		return expression.matchesDay, nil

	default:
		return nil, fmt.Errorf("unknown strategy %q", w.Strategy)
	}
}

// zone resolves an IANA name. Empty means UTC, which is the schema's default and
// the only defensible fallback — guessing the host's zone would make the same
// window mean different things on different machines.
func zone(name string) (*time.Location, error) {
	if name == "" || name == "UTC" {
		return time.UTC, nil
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: want an IANA name such as Europe/London", name)
	}
	return location, nil
}

func sortedKeys(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}
