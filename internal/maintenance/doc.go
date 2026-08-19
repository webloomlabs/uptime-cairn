// Package maintenance turns a window's schedule into concrete intervals.
//
// Planned downtime is the one part of a monitoring system where being
// approximately right is worse than being obviously wrong: a window that fires
// an hour late because of a daylight-saving transition pages the on-call
// engineer during the change they scheduled it around. So the schedule is
// evaluated in the window's own IANA zone, in local time, and the zone database
// is embedded in the binary rather than assumed to exist on the host — a
// scratch container has no /usr/share/zoneinfo, and "Australia/Sydney" silently
// falling back to UTC is exactly the failure this package exists to avoid.
//
// Nothing here reads the clock on its own. Every function takes the instant it
// is reasoning about, which is what makes a schedule testable without waiting
// for one.
package maintenance

// The embedded zone database. ~450KB of binary for the guarantee that
// time.LoadLocation works on every host this ships to, including FROM scratch.
import _ "time/tzdata"
