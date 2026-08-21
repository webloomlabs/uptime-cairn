package model

import (
	"encoding/json"
	"time"
)

// Import job states, matching ImportJob.state in docs/api/openapi.yaml.
//
// The vocabulary is the spec's rather than the first migration's, which used
// pending/analysing/importing/completed/cancelled. Migration 0007 aligns the
// schema; the spec is the contract, and an API that reports `completed` where
// the contract says `succeeded` is a client bug waiting to be written.
//
// `partial` is the state that earns its keep. An import of a thousand monitors
// where thirty were an unsupported Kuma type has not failed and has not
// succeeded, and collapsing it into either is how somebody concludes their
// migration finished when thirty monitors are missing.
const (
	ImportQueued    = "queued"
	ImportRunning   = "running"
	ImportSucceeded = "succeeded"
	ImportPartial   = "partial"
	ImportFailed    = "failed"
)

// Per-entity outcomes, matching ImportJob.entries[].result.
//
// Five rather than three, and the two extra are the point of the report.
// `renamed` says an entity arrived under a different name and which one, so a
// user looking for "API gateway" and finding "API gateway (2)" is not looking
// at a bug. `unsupported` says this build cannot represent the entity at all,
// which is a different statement from having skipped it — and it is the one an
// evaluating user needs before they commit to the migration.
const (
	ImportResultImported    = "imported"
	ImportResultRenamed     = "renamed"
	ImportResultSkipped     = "skipped"
	ImportResultFailed      = "failed"
	ImportResultUnsupported = "unsupported"
)

// Entity types an import entry can describe.
const (
	ImportEntityMonitor        = "monitor"
	ImportEntityTag            = "tag"
	ImportEntityNotification   = "notification"
	ImportEntityStatusPage     = "status_page"
	ImportEntityHeartbeatRange = "heartbeat_range"
)

// ImportJob is one import, from queue to report.
type ImportJob struct {
	ID    ID
	OrgID ID

	// Source names the tool imported from. "kuma" today; the field exists
	// because the next importer should not need a second table.
	Source string

	State  string
	DryRun bool

	// Options is the request's options object, stored verbatim so the report
	// can say what was asked for. A report that does not record whether
	// import_history was on cannot explain why there is no history.
	Options json.RawMessage

	// Sources is one entry per uploaded file: its name, the Kuma version it
	// declares, and the entity census taken before anything was written.
	Sources []ImportSource

	Error string

	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ImportSource is one uploaded database.
type ImportSource struct {
	Filename         string         `json:"filename"`
	KumaVersion      string         `json:"kuma_version,omitempty"`
	DetectedEntities map[string]int `json:"detected_entities"`
}

// ImportEntry is one source entity and what became of it.
//
// Every source entity appears exactly once. That is the guarantee the whole
// report rests on: an import that maps 900 of 1,000 monitors and says so is
// trustworthy, and one that reports success is not.
type ImportEntry struct {
	ID    ID
	JobID ID
	OrgID ID

	SourceFile string
	EntityType string
	SourceID   string
	SourceName string

	Result string

	// TargetID is the row this became. Nil for anything not written.
	TargetID *ID

	// Detail is why, in plain language — an unsupported type named, a
	// collision and what was done about it, a feature that did not come across.
	Detail string

	CreatedAt time.Time
}

// ImportSummary is the per-entity-type tally the API renders.
type ImportSummary struct {
	Imported    int `json:"imported"`
	Renamed     int `json:"renamed"`
	Skipped     int `json:"skipped"`
	Failed      int `json:"failed"`
	Unsupported int `json:"unsupported"`
}

// Tally builds the summary the spec's ImportJob.summary describes: counts by
// entity type and outcome, computed from the entries rather than counted
// alongside them, so the two can never disagree.
func Tally(entries []ImportEntry) map[string]ImportSummary {
	out := map[string]ImportSummary{}
	for _, e := range entries {
		s := out[e.EntityType]
		switch e.Result {
		case ImportResultImported:
			s.Imported++
		case ImportResultRenamed:
			s.Renamed++
		case ImportResultSkipped:
			s.Skipped++
		case ImportResultFailed:
			s.Failed++
		case ImportResultUnsupported:
			s.Unsupported++
		}
		out[e.EntityType] = s
	}
	return out
}

// StateFor decides a finished job's state from its entries.
//
// Anything that did not come across makes the job partial rather than
// succeeded, including an unsupported type. That is deliberate: "succeeded" has
// to mean the install now monitors what the old one monitored, and a user who
// sees it and stops reading must not be wrong.
func StateFor(entries []ImportEntry) string {
	imported, incomplete := 0, 0
	for _, e := range entries {
		switch e.Result {
		case ImportResultImported, ImportResultRenamed:
			imported++
		default:
			incomplete++
		}
	}
	switch {
	case incomplete == 0:
		return ImportSucceeded
	case imported == 0:
		return ImportFailed
	default:
		return ImportPartial
	}
}
