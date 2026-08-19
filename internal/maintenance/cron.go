package maintenance

import (
	"fmt"
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

type cronExpression struct {
	minutes       map[int]bool
	hours         map[int]bool
	doms          map[int]bool
	months        map[int]bool
	dows          map[int]bool
	domRestricted bool
	dowRestricted bool
}

// matchesDay applies cron's day rule: when both day-of-month and day-of-week are
// restricted the match is a union, not an intersection. It is a genuine oddity
// of the format rather than a bug here — "0 0 1 * 1" is the first of the month
// *and* every Monday.
func (e *cronExpression) matchesDay(day time.Time) bool {
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

func parseCron(expression string) (*cronExpression, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron needs five fields (minute hour day-of-month month day-of-week), got %d in %q",
			len(fields), expression)
	}
	if strings.HasPrefix(strings.TrimSpace(expression), "@") {
		return nil, fmt.Errorf("cron aliases such as @daily are not supported; write the five fields out")
	}

	out := &cronExpression{}
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
