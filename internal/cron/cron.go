// Package cron parses the five-field cron expressions this system accepts, and
// answers the two questions asked of them: does a day match, and when does the
// next firing fall.
//
// **One implementation, two callers.** Maintenance windows have used it since
// Phase 1; report schedules use it from Phase 2. A second copy would agree with
// the first on the day it was written and diverge on the first bug fix — and the
// rule most likely to diverge is the day-of-month/day-of-week union below, which
// is a genuine oddity of the format that a fresh implementation gets wrong.
package cron

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A five-field cron parser: minute, hour, day-of-month, month, day-of-week.
//
// Hand-written rather than a dependency, and the reason is the same one
// AGENTS.md §5 gives generally: this is 120 lines of set-building against a
// syntax that has not changed in forty years, and the alternative is a package
// with its own opinions about seconds fields, @weekly aliases, and timezone
// handling that would have to be reconciled with the window's own zone anyway.
//
// Deliberately not supported: seconds, @-aliases, ranges with names (MON-FRI),
// and the L/W/# extensions. Each is rejected with a message naming what is
// missing, which is a better answer than accepting an expression and running it
// on a schedule the user did not intend.

// Expression is a parsed five-field cron.
type Expression struct {
	minutes       map[int]bool
	hours         map[int]bool
	doms          map[int]bool
	months        map[int]bool
	dows          map[int]bool
	domRestricted bool
	dowRestricted bool
}

// MatchesDay applies cron's day rule: when both day-of-month and day-of-week are
// restricted the match is a union, not an intersection. It is a genuine oddity
// of the format rather than a bug here — "0 0 1 * 1" is the first of the month
// *and* every Monday.
func (e *Expression) MatchesDay(day time.Time) bool {
	if !e.months[int(day.Month())] {
		return false
	}
	switch {
	case e.domRestricted && e.dowRestricted:
		return e.doms[day.Day()] || e.dows[int(day.Weekday())]
	case e.domRestricted:
		return e.doms[day.Day()]
	case e.dowRestricted:
		return e.dows[int(day.Weekday())]
	default:
		return true
	}
}

// Parse reads an expression, or says exactly what it did not understand.
func Parse(expression string) (*Expression, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron needs five fields (minute hour day-of-month month day-of-week), got %d in %q",
			len(fields), expression)
	}
	if strings.HasPrefix(strings.TrimSpace(expression), "@") {
		return nil, fmt.Errorf("cron aliases such as @daily are not supported; write the five fields out")
	}

	out := &Expression{}
	var err error
	if out.minutes, _, err = parseField(fields[0], 0, 59, "minute"); err != nil {
		return nil, err
	}
	if out.hours, _, err = parseField(fields[1], 0, 23, "hour"); err != nil {
		return nil, err
	}
	if out.doms, out.domRestricted, err = parseField(fields[2], 1, 31, "day of month"); err != nil {
		return nil, err
	}
	if out.months, _, err = parseField(fields[3], 1, 12, "month"); err != nil {
		return nil, err
	}
	if out.dows, out.dowRestricted, err = parseField(fields[4], 0, 6, "day of week"); err != nil {
		return nil, err
	}
	return out, nil
}

// parseField expands one field into the set of values it matches, and reports
// whether it was restricted at all — which only the two day fields care about,
// for the union rule above.
func parseField(field string, low, high int, name string) (map[int]bool, bool, error) {
	set := map[int]bool{}
	restricted := false

	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false, fmt.Errorf("empty %s field", name)
		}

		step := 1
		if base, stepText, found := strings.Cut(part, "/"); found {
			parsed, err := strconv.Atoi(stepText)
			if err != nil || parsed < 1 {
				return nil, false, fmt.Errorf("%s step %q is not a positive number", name, stepText)
			}
			step = parsed
			part = base
		}

		start, end := low, high
		switch {
		case part == "*":
			// Unrestricted, unless a step narrows it.
			if step > 1 {
				restricted = true
			}
		default:
			restricted = true
			first, last, isRange := strings.Cut(part, "-")
			value, err := strconv.Atoi(strings.TrimSpace(first))
			if err != nil {
				return nil, false, fmt.Errorf("%s %q is not a number; names such as MON are not supported", name, first)
			}
			start = value
			if isRange {
				value, err := strconv.Atoi(strings.TrimSpace(last))
				if err != nil {
					return nil, false, fmt.Errorf("%s %q is not a number", name, last)
				}
				end = value
			} else {
				end = start
				if step > 1 {
					// "5/15" means "from 5, every 15", which is how cron reads a
					// bare value with a step.
					end = high
				}
			}
		}

		if start < low || end > high || start > end {
			return nil, false, fmt.Errorf("%s %q is outside %d-%d", name, part, low, high)
		}
		for value := start; value <= end; value += step {
			set[value] = true
		}
	}

	if len(set) == 0 {
		return nil, false, fmt.Errorf("%s field matches nothing", name)
	}
	return set, restricted, nil
}

// Hours and Minutes are the times of day this expression fires at, sorted.
//
// Exposed because a caller that already knows which days match still needs to
// know when on those days — the maintenance scheduler builds its occurrences
// that way, and duplicating the sort at each call site is how two callers end up
// disagreeing about ordering.
func (e *Expression) Hours() []int   { return sortedKeys(e.hours) }
func (e *Expression) Minutes() []int { return sortedKeys(e.minutes) }

// LookaheadDays bounds the search for a next firing.
//
// Four years and change, because "0 2 29 2 *" — 02:00 on the 29th of February —
// is a legal expression whose next match can be nearly four years out. The walk
// compares days, so the bound costs microseconds even when nothing matches.
const LookaheadDays = 1500

// Next is the first instant strictly after `after` at which this expression
// fires, cut in the given zone.
//
// **In the zone, not in UTC**, and that is the whole reason this takes a
// location. "0 9 1 * *" for an agency in Sydney means 09:00 Sydney on the first
// — which is 22:00 or 23:00 UTC on the last day of the previous month depending
// on daylight saving, and a scheduler that computed it in UTC would send a
// monthly report on the wrong day twice a year.
//
// A firing that falls in a gap when the clocks go forward is normalised by
// Go's time.Date rather than skipped: an 02:30 daily job runs at 03:30 on that
// one morning. Skipping it instead would mean a report that silently does not
// arrive, which is the worse of the two surprises.
func (e *Expression) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	local := after.In(loc)
	year, month, day := local.Date()

	hours, minutes := e.Hours(), e.Minutes()
	for range LookaheadDays {
		// Midnight is reconstructed each iteration rather than advanced by 24
		// hours, so a day that is 23 or 25 hours long does not drift the walk.
		start := time.Date(year, month, day, 0, 0, 0, 0, loc)
		if e.MatchesDay(start) {
			for _, hour := range hours {
				for _, minute := range minutes {
					at := time.Date(year, month, day, hour, minute, 0, 0, loc)
					if at.After(after) {
						return at, true
					}
				}
			}
		}
		year, month, day = start.Year(), start.Month(), start.Day()+1
	}
	return time.Time{}, false
}

func sortedKeys(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}
