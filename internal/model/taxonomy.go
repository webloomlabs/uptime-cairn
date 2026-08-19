package model

import (
	"strings"
	"time"
	"unicode"
)

// Groups and tags: the two organisational primitives everything else filters
// through (PHASE-1-PLAN.md §3.2).
//
// They are deliberately not the same idea. A monitor belongs to at most one
// group, which is where it lives; it carries any number of tags, which is what
// it is. Collapsing them into one mechanism is a common shortcut and it costs
// you the ability to ask both questions — "show me the EU stack" and "show me
// everything customer-facing" are different queries over the same monitors.

// Group is a place a monitor lives.
type Group struct {
	ID          ID
	OrgID       ID
	Name        string
	Description string

	// ParentGroupID nests one level deep in Phase 1. Deeper nesting is a
	// product decision rather than a technical one: every level makes the
	// filter query and the UI's tree harder, and nobody has asked for three.
	ParentGroupID *ID

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Tag is something a monitor is.
type Tag struct {
	ID    ID
	OrgID ID
	Name  string

	// Slug is the URL-safe form derived from the name and unique per
	// organisation. Derived rather than supplied, so two tags that look the same
	// in a list cannot both exist.
	Slug string

	// Color is a hex triple used in the UI and on status pages.
	Color string

	Description string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// DefaultTagColor matches the spec's default: a neutral grey, so a tag created
// without one does not claim a meaning it was not given.
const DefaultTagColor = "#6b7280"

// Slugify derives a tag's URL-safe form from its name.
//
// Deliberately narrow: lower-case ASCII letters, digits, and single hyphens.
// Anything else becomes a separator, so "Prod / EU" and "prod-eu" are the same
// tag — which is the point, because two tags that render identically in a list
// are two tags nobody can tell apart.
//
// ASCII rather than every Unicode letter, so a slug never needs percent-encoding
// to appear in a URL. The cost is that a name written entirely in another script
// slugifies to nothing; the write path refuses that with a message saying so,
// rather than inventing an identifier the user did not choose.
func Slugify(name string) string {
	var b strings.Builder
	previousHyphen := true // leading hyphens are dropped

	for _, r := range strings.ToLower(name) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			previousHyphen = false
		case !previousHyphen:
			b.WriteByte('-')
			previousHyphen = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// GroupSummary is a group with the two figures every list of them wants: how
// many monitors it holds, and the worst thing any of them is doing.
type GroupSummary struct {
	Group        Group
	MonitorCount int

	// Status is the worst status among the group's monitors — down beats
	// pending beats up. Empty when the group holds none, which is a different
	// statement from "up" and is rendered as null rather than green.
	Status string
}

// TagSummary is a tag with its monitor count.
type TagSummary struct {
	Tag          Tag
	MonitorCount int
}

// statusRank orders monitor statuses by how much they should worry somebody, so
// a group of fifty monitors reports the one that matters.
//
// down first, because an outage anywhere in a group is the group's headline.
// maintenance outranks pending because it is a fact rather than an absence of
// one; paused outranks up for the same reason, and an unrecognised status sorts
// worst of all so that a value this function has not been taught about is
// visible rather than silently benign.
func statusRank(status string) int {
	switch status {
	case MonitorStatusDown:
		return 0
	case MonitorStatusMaintenance:
		return 1
	case MonitorStatusPending:
		return 2
	case MonitorStatusPaused:
		return 3
	case MonitorStatusUp:
		return 4
	default:
		return -1
	}
}

// WorstStatus returns the status a group should report.
func WorstStatus(statuses []string) string {
	worst := ""
	for _, status := range statuses {
		if worst == "" || statusRank(status) < statusRank(worst) {
			worst = status
		}
	}
	return worst
}
