package report

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/cron"
)

// When a report schedule fires.
//
// # One implementation, not five
//
// Every named frequency is expressed as a cron and answered by the shared parser
// in internal/cron. `daily` at 09:00 is `0 9 * * *`; `weekly` is `0 9 * * 1`;
// `monthly` is `0 9 1 * *`; `quarterly` is `0 9 1 1,4,7,10 *`. The alternative
// was a switch with five arms of calendar arithmetic beside a sixth that called
// the parser, and the two halves would have disagreed about something eventually
// — most likely daylight saving, which is the thing this file exists to get
// right and the thing hand-rolled month arithmetic gets wrong.
//
// It also means the named frequencies inherit the parser's zone handling for
// free: "09:00 on the first" for a Sydney agency is computed in Sydney, not in
// UTC where it would land on the wrong day twice a year.
//
// # Why Monday, and why the first
//
// A weekly report fires on **Monday** because window.go already cuts weeks
// starting Monday, so the report that arrives on Monday morning covers the week
// that just ended rather than a week with a weekend split across it. Monthly and
// quarterly fire on the **first** for the same reason: the period a client is
// invoiced for is the one that just closed.
//
// Neither is configurable, because the spec gives a schedule no weekday or
// day-of-month field. An operator who wants Thursdays writes a cron, which is
// what cron is there for.

// ScheduleSpec is the part of a schedule that decides when it fires. A struct
// rather than the stored row, so the API can validate a request body before it
// has a row to validate.
type ScheduleSpec struct {
	Frequency string
	Cron      string
	Timezone  string

	// SendAt is "HH:MM" in Timezone. Ignored when Frequency is cron, because the
	// expression carries its own times — and a schedule that showed both would
	// invite somebody to set them to different values.
	SendAt string
}

// CronFor is the expression a schedule fires on.
func CronFor(spec ScheduleSpec) (string, error) {
	if spec.Frequency == "cron" {
		if strings.TrimSpace(spec.Cron) == "" {
			return "", fmt.Errorf("a cron schedule needs an expression")
		}
		return spec.Cron, nil
	}
	if strings.TrimSpace(spec.Cron) != "" {
		// Refused rather than ignored. A stored expression that never runs is a
		// schedule an operator believes they configured, and the silence looks
		// like a bug in the product rather than in the request.
		return "", fmt.Errorf("cron is only accepted when frequency is cron, not %q", spec.Frequency)
	}

	hour, minute, err := parseSendAt(spec.SendAt)
	if err != nil {
		return "", err
	}

	switch spec.Frequency {
	case "daily":
		return fmt.Sprintf("%d %d * * *", minute, hour), nil
	case "weekly":
		return fmt.Sprintf("%d %d * * 1", minute, hour), nil
	case "monthly":
		return fmt.Sprintf("%d %d 1 * *", minute, hour), nil
	case "quarterly":
		return fmt.Sprintf("%d %d 1 1,4,7,10 *", minute, hour), nil
	}
	return "", fmt.Errorf("unknown frequency %q", spec.Frequency)
}

// NextRun is the first instant after `after` at which the schedule fires.
//
// An expression that will never fire again is an error rather than a zero time.
// "0 0 30 2 *" — the 30th of February — parses cleanly and matches nothing, and
// a schedule stored with it would sit silently forever. The API refuses it at
// write time on the strength of this, which is the difference between a
// misconfiguration somebody is told about and one they discover when a client
// asks where the report went.
func NextRun(spec ScheduleSpec, after time.Time) (time.Time, error) {
	expression, err := CronFor(spec)
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := cron.Parse(expression)
	if err != nil {
		return time.Time{}, err
	}

	loc, err := scheduleZone(spec.Timezone)
	if err != nil {
		return time.Time{}, err
	}

	next, ok := parsed.Next(after, loc)
	if !ok {
		return time.Time{}, fmt.Errorf("this schedule never fires: no match for %q within %d days",
			expression, cron.LookaheadDays)
	}
	return next.UTC(), nil
}

// parseSendAt reads "HH:MM". Strict, because "9:00" and "0900" are both things
// somebody types and both would otherwise be read as something else or silently
// defaulted.
func parseSendAt(value string) (hour, minute int, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		// The schema's default, applied here so a caller that omits the field
		// gets the documented behaviour rather than midnight.
		value = "09:00"
	}

	before, after, found := strings.Cut(value, ":")
	if !found || len(before) != 2 || len(after) != 2 {
		return 0, 0, fmt.Errorf("send_at %q is not HH:MM", value)
	}
	hour, err = strconv.Atoi(before)
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("send_at hour in %q is outside 00-23", value)
	}
	minute, err = strconv.Atoi(after)
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("send_at minute in %q is outside 00-59", value)
	}
	return hour, minute, nil
}

// scheduleZone resolves an IANA name, refusing an unknown one by name.
//
// Refusing rather than falling back to UTC, which is the same choice
// ResolveWindow makes and for the same reason: a schedule quietly moved to UTC
// sends a monthly report a working day early for half the world, and nothing
// says so.
func scheduleZone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: want an IANA name such as Australia/Sydney", name)
	}
	return loc, nil
}
