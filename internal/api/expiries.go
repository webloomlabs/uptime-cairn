package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The expiry calendar.
//
// A report over data that has existed since migration 0003 rather than a new
// kind of observation, which is what the phase plan says it is: certificates and
// domain registrations are already recorded, and this is the one collection that
// reads them together and orders them by when they run out.
//
// It carries `monitors:read` rather than `reports:read`, which is the spec's
// choice and the right one: the rows are facts about monitors, and a key that can
// see a monitor can already see its certificate through
// `/monitors/{id}/certificate`. Requiring a reporting scope would make the
// calendar less reachable than the same facts one at a time.

// ExpiryStore is the calendar's read side, declared here by the consumer.
type ExpiryStore interface {
	ListUpcomingExpiries(ctx context.Context, after *store.Cursor, limit int,
		filter store.ExpiryFilter, now time.Time) ([]model.UpcomingExpiry, bool, error)
}

// maxWithinDays matches the spec's bound. Ten years, which is longer than any
// certificate is issued for and long enough for a registration renewed in bulk.
const maxWithinDays = 3650

func (s *Server) listUpcomingExpiries(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}
	filter, ok := s.expiryFilter(w, r)
	if !ok {
		return
	}

	// The instant is taken once and passed down, so every `days_remaining` on
	// one page is measured against the same moment. Reading the clock per row
	// would let a page straddle midnight and report two different answers for
	// the same date.
	now := time.Now().UTC()

	entries, hasMore, err := s.store.ListUpcomingExpiries(r.Context(), after, s.limit(r), filter, now)
	if err != nil {
		s.internal(w, r, "list upcoming expiries", err)
		return
	}

	body := page[upcomingExpiryJSON]{Data: []upcomingExpiryJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, e := range entries {
		body.Data = append(body.Data, toUpcomingExpiryJSON(e))
	}
	if hasMore && len(entries) > 0 {
		last := entries[len(entries)-1]
		// The keyset is (expires_at, monitor_id) rather than the
		// (updated_at, id) the name suggests. The cursor type carries two
		// components and is reused rather than duplicated; what those components
		// mean is the collection's business, and this one orders by when things
		// run out.
		next := store.Cursor{UpdatedAt: last.ExpiresAt, ID: last.MonitorID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) expiryFilter(w http.ResponseWriter, r *http.Request) (store.ExpiryFilter, bool) {
	var filter store.ExpiryFilter
	query := r.URL.Query()

	if raw := query.Get("within_days"); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days < 1 || days > maxWithinDays {
			s.refuse(w, r, ValidationItem{
				Pointer: "/within_days",
				Code:    "out_of_range",
				Message: "within_days is a whole number of days between 1 and " +
					strconv.Itoa(maxWithinDays),
			})
			return filter, false
		}
		filter.WithinDays = &days
	}

	for _, kind := range query["kind"] {
		switch kind {
		case model.ExpiryCertificate, model.ExpiryDomain:
			filter.Kinds = append(filter.Kinds, kind)
		default:
			s.refuse(w, r, ValidationItem{
				Pointer: "/kind",
				Code:    "invalid",
				Message: "kind is certificate or domain",
			})
			return filter, false
		}
	}

	for _, raw := range query["tag_id"] {
		id, ok := model.ParseID(raw)
		if !ok {
			s.refuse(w, r, ValidationItem{
				Pointer: "/tag_id",
				Code:    "invalid",
				Message: "tag_id is a UUID",
			})
			return filter, false
		}
		filter.TagIDs = append(filter.TagIDs, id)
	}

	return filter, true
}

// refuse answers 422 with one problem, in the shape every other write path here
// uses. A query parameter out of range is a validation failure and not a 400:
// the request is well-formed and the value is not allowed, which is exactly the
// distinction RFC 9457 exists to carry.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, item ValidationItem) {
	writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
		"Validation failed", "One or more query parameters were rejected.", item)
}
