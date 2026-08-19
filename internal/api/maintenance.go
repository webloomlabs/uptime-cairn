package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/maintenance"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Maintenance windows.
//
// The interesting part is not the CRUD, it is that a schedule which cannot be
// evaluated is refused here rather than discovered by the sweep at two in the
// morning. Every write runs the same evaluator the sweep runs, and a window that
// produces no occurrence at all is reported as a mistake rather than saved as a
// schedule that will never fire.

const maxMaintenanceBody = 1 << 16

// maintenanceStates is the closed set the state filter accepts.
var maintenanceStates = map[string]bool{
	model.MaintenanceScheduled: true, model.MaintenanceActive: true,
	model.MaintenanceEnded: true, model.MaintenanceCancelled: true,
}

var maintenanceStrategies = map[string]bool{
	model.StrategySingle: true, model.StrategyRecurringDaily: true,
	model.StrategyRecurringWeekly: true, model.StrategyRecurringMonthly: true,
	model.StrategyCron: true,
}

func (s *Server) listMaintenanceWindows(w http.ResponseWriter, r *http.Request) {
	var after *store.Cursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := store.DecodeCursor(raw)
		if err != nil {
			writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-cursor",
				"Malformed cursor", "The cursor must be one returned by a previous page of this collection.")
			return
		}
		after = &c
	}

	wanted := map[string]bool{}
	for _, state := range r.URL.Query()["state"] {
		if !maintenanceStates[state] {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid state filter",
				fmt.Sprintf("state %q is not one the spec defines: want scheduled, active, ended, or cancelled", state))
			return
		}
		wanted[state] = true
	}

	var monitorID *model.ID
	if raw := r.URL.Query().Get("monitor_id"); raw != "" {
		id, ok := model.ParseID(raw)
		if !ok {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid monitor filter",
				fmt.Sprintf("monitor_id %q is not a valid identifier", raw))
			return
		}
		monitorID = &id
	}

	windows, hasMore, err := s.store.ListMaintenanceWindows(
		r.Context(), after, s.limit(r), r.URL.Query().Get("search"), monitorID)
	if err != nil {
		s.internal(w, r, "list maintenance windows", err)
		return
	}

	rendered, err := s.renderWindows(r.Context(), windows)
	if err != nil {
		s.internal(w, r, "render maintenance windows", err)
		return
	}

	// State is derived, so it is filtered here rather than in SQL — the
	// alternative is a cron evaluator in SQL. The page size is honoured before
	// the filter, which means a filtered page can come back short; that is the
	// same trade every derived filter makes, and the cursor still advances.
	body := page[maintenanceWindowJSON]{Data: []maintenanceWindowJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, window := range rendered {
		if len(wanted) > 0 && !wanted[window.State] {
			continue
		}
		body.Data = append(body.Data, window)
	}
	if hasMore && len(windows) > 0 {
		last := windows[len(windows)-1]
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}

	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readMaintenanceBody(w, r)
	if !ok {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	window := model.MaintenanceWindow{
		ID:                    model.NewID(),
		OrgID:                 s.orgID,
		Timezone:              "UTC",
		SuppressNotifications: true,
		ShowOnStatusPages:     true,
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	statusPageIDs, problems := s.buildMaintenanceWindow(r.Context(), &window, body, now)
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The maintenance window was not created.", problems...)
		return
	}

	if err := s.store.CreateMaintenanceWindow(r.Context(), window, statusPageIDs); err != nil {
		s.internal(w, r, "create maintenance window", err)
		return
	}
	s.log.Info("maintenance window created", "id", window.ID.String(),
		"strategy", window.Strategy, "timezone", window.Timezone)

	// Woken immediately rather than left to the next tick: a window that starts
	// now should suppress the check that is about to run, not the one after it.
	s.sweepNow()

	s.writeMaintenanceWindow(w, r, window.ID, http.StatusCreated)
}

func (s *Server) getMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	id, ok := s.maintenanceID(w, r)
	if !ok {
		return
	}
	s.writeMaintenanceWindow(w, r, id, http.StatusOK)
}

func (s *Server) updateMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	id, ok := s.maintenanceID(w, r)
	if !ok {
		return
	}
	existing, err := s.store.GetMaintenanceWindow(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.maintenanceNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get maintenance window", err)
		return
	}

	body, ok := s.readMaintenanceBody(w, r)
	if !ok {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	window := existing
	window.UpdatedAt = now

	statusPageIDs, problems := s.buildMaintenanceWindow(r.Context(), &window, body, now)
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The maintenance window was not updated.", problems...)
		return
	}

	// Cleared so the sweep re-evaluates on its next pass rather than trusting a
	// materialised occurrence computed from the old schedule.
	window.NextOccurrenceAt = nil

	if err := s.store.UpdateMaintenanceWindow(r.Context(), window, statusPageIDs); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.maintenanceNotFound(w, r)
			return
		}
		s.internal(w, r, "update maintenance window", err)
		return
	}
	s.sweepNow()

	s.writeMaintenanceWindow(w, r, window.ID, http.StatusOK)
}

func (s *Server) deleteMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	id, ok := s.maintenanceID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteMaintenanceWindow(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.maintenanceNotFound(w, r)
			return
		}
		s.internal(w, r, "delete maintenance window", err)
		return
	}

	// The schedule goes; the annotation on history stays. Past uptime figures do
	// not silently change because somebody tidied up a window.
	s.sweepNow()
	w.WriteHeader(http.StatusNoContent)
}

// buildMaintenanceWindow validates the request onto a window and returns the
// status pages to attach. Returns one problem per bad field, for the same reason
// the monitor endpoint does.
func (s *Server) buildMaintenanceWindow(ctx context.Context, window *model.MaintenanceWindow, body maintenanceWindowWrite, now time.Time) ([]model.ID, []ValidationItem) {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	if body.State != nil {
		bad("/state", "read_only",
			"state is derived from the schedule and the clock; cancel a window by deleting it")
	}

	switch {
	case body.Title != nil && strings.TrimSpace(*body.Title) != "":
		if len(*body.Title) > 200 {
			bad("/title", "too_long", "title must be at most 200 characters")
		} else {
			window.Title = *body.Title
		}
	case window.Title == "":
		bad("/title", "required", "title is required")
	}
	if body.Description != nil {
		if len(*body.Description) > 5000 {
			bad("/description", "too_long", "description must be at most 5000 characters")
		} else {
			window.Description = *body.Description
		}
	}

	if body.Strategy != nil {
		if !maintenanceStrategies[*body.Strategy] {
			bad("/strategy", "invalid", fmt.Sprintf(
				"strategy %q is not one the spec defines: want single, recurring_daily, recurring_weekly, recurring_monthly, or cron",
				*body.Strategy))
		} else {
			window.Strategy = *body.Strategy
		}
	}
	if window.Strategy == "" {
		bad("/strategy", "required", "strategy is required")
	}

	if body.Timezone != nil {
		if _, err := time.LoadLocation(*body.Timezone); err != nil {
			bad("/timezone", "invalid", fmt.Sprintf(
				"timezone %q is not an IANA zone name such as Europe/London", *body.Timezone))
		} else {
			window.Timezone = *body.Timezone
		}
	}

	if body.StartsAt != nil {
		window.StartsAt = body.StartsAt.UTC()
	}
	if window.StartsAt.IsZero() {
		bad("/starts_at", "required", "starts_at is required")
	}
	if body.EndsAt != nil {
		ends := body.EndsAt.UTC()
		window.EndsAt = &ends
	}
	if body.DurationMinutes != nil {
		switch v := *body.DurationMinutes; {
		case v < 1:
			bad("/duration_minutes", "below_minimum", "duration_minutes must be at least 1")
		case v > 60*24*30:
			bad("/duration_minutes", "above_maximum", "duration_minutes must be at most 43200 (thirty days)")
		default:
			window.Duration = time.Duration(v) * time.Minute
		}
	}
	if body.Recurrence != nil {
		window.Recurrence = model.Recurrence{
			Weekdays:    body.Recurrence.Weekdays,
			DaysOfMonth: body.Recurrence.DaysOfMonth,
			Until:       body.Recurrence.Until,
		}
		if body.Recurrence.Cron != nil {
			window.Recurrence.Cron = *body.Recurrence.Cron
		}
	}

	switch window.Strategy {
	case model.StrategySingle:
		if window.EndsAt == nil && window.Duration <= 0 {
			bad("/ends_at", "required", "a single window needs an ends_at or a duration_minutes")
		}
		if window.EndsAt != nil && !window.EndsAt.After(window.StartsAt) {
			bad("/ends_at", "invalid", "ends_at must be after starts_at")
		}
	case "":
		// Already reported above.
	default:
		if window.Duration <= 0 {
			bad("/duration_minutes", "required",
				fmt.Sprintf("a %s window needs a duration_minutes: it is how long each occurrence lasts", window.Strategy))
		}
	}

	if body.SuppressNotifications != nil {
		window.SuppressNotifications = *body.SuppressNotifications
	}
	if body.ShowOnStatusPages != nil {
		window.ShowOnStatusPages = *body.ShowOnStatusPages
	}

	if body.Targets != nil {
		window.Targets = model.MaintenanceTargets{}
		window.Targets.MonitorIDs = s.resolveTargets(ctx, "monitors", "/targets/monitor_ids", body.Targets.MonitorIDs, &problems)
		window.Targets.GroupIDs = s.resolveTargets(ctx, "groups", "/targets/group_ids", body.Targets.GroupIDs, &problems)
		window.Targets.TagIDs = s.resolveTargets(ctx, "tags", "/targets/tag_ids", body.Targets.TagIDs, &problems)
	}
	if window.Targets.Empty() {
		// A window with no targets suppresses nothing, which is never what
		// anybody wanted and is invisible until the outage it failed to silence.
		bad("/targets", "required",
			"a maintenance window needs at least one target; a window covering nothing suppresses nothing")
	}

	var statusPageIDs []model.ID
	if body.StatusPageIDs != nil {
		statusPageIDs = s.resolveTargets(ctx, "status_pages", "/status_page_ids", *body.StatusPageIDs, &problems)
	}

	// Last, and only if the parts are individually sound: run the real
	// evaluator. A schedule that parses but produces no occurrence is a window
	// that will never fire, and finding that out now beats finding it out by its
	// silence.
	if len(problems) == 0 {
		if _, ok, err := maintenance.Next(*window, now); err != nil {
			bad("/recurrence", "invalid", err.Error())
		} else if !ok && window.Strategy != model.StrategySingle {
			bad("/recurrence", "never_occurs",
				"this schedule produces no occurrence in the next four years; check starts_at, recurrence, and until")
		}
	}

	return statusPageIDs, problems
}

// resolveTargets parses and existence-checks one target list.
func (s *Server) resolveTargets(ctx context.Context, table, pointer string, raw []string, problems *[]ValidationItem) []model.ID {
	var ids []model.ID
	for i, value := range raw {
		id, ok := model.ParseID(value)
		if !ok {
			*problems = append(*problems, ValidationItem{
				Pointer: fmt.Sprintf("%s/%d", pointer, i), Code: "invalid",
				Message: fmt.Sprintf("%q is not a valid identifier", value)})
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}

	missing, err := s.store.MissingIDs(ctx, table, s.orgID, ids)
	if err != nil {
		s.log.Error("check maintenance target ids", "error", err, "table", table)
		*problems = append(*problems, ValidationItem{Pointer: pointer, Code: "unavailable",
			Message: "the targets could not be checked"})
		return nil
	}
	for _, id := range missing {
		*problems = append(*problems, ValidationItem{
			Pointer: pointer, Code: "not_found",
			Message: fmt.Sprintf("no %s with id %s exists", strings.TrimSuffix(table, "s"), id)})
	}
	return ids
}

func (s *Server) writeMaintenanceWindow(w http.ResponseWriter, r *http.Request, id model.ID, status int) {
	window, err := s.store.GetMaintenanceWindow(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.maintenanceNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get maintenance window", err)
		return
	}

	rendered, err := s.renderWindows(r.Context(), []model.MaintenanceWindow{window})
	if err != nil {
		s.internal(w, r, "render maintenance window", err)
		return
	}
	if status == http.StatusCreated {
		w.Header().Set("Location", "/api/v1/maintenance-windows/"+id.String())
	}
	writeJSON(w, s.log, status, rendered[0])
}

func (s *Server) renderWindows(ctx context.Context, windows []model.MaintenanceWindow) ([]maintenanceWindowJSON, error) {
	ids := make([]model.ID, 0, len(windows))
	for _, window := range windows {
		ids = append(ids, window.ID)
	}
	pages, err := s.store.StatusPageIDsForWindows(ctx, ids)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	out := make([]maintenanceWindowJSON, 0, len(windows))
	for _, window := range windows {
		out = append(out, toMaintenanceWindowJSON(window, maintenance.State(window, now), pages[window.ID]))
	}
	return out, nil
}

func (s *Server) readMaintenanceBody(w http.ResponseWriter, r *http.Request) (maintenanceWindowWrite, bool) {
	var body maintenanceWindowWrite
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMaintenanceBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-json",
			"Malformed request body", err.Error())
		return maintenanceWindowWrite{}, false
	}
	return body, true
}

func (s *Server) maintenanceID(w http.ResponseWriter, r *http.Request) (model.ID, bool) {
	id, ok := model.ParseID(r.PathValue("maintenanceWindowId"))
	if !ok {
		s.maintenanceNotFound(w, r)
		return model.ID{}, false
	}
	return id, true
}

func (s *Server) maintenanceNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "maintenance-window-not-found",
		"Maintenance window not found", "No maintenance window with that identifier exists.")
}

// sweepNow asks the sweeper to re-evaluate immediately. Nil in tests that are
// not exercising the schedule, and a no-op when the queue already holds a
// pending request — one wake-up recomputes everything.
func (s *Server) sweepNow() {
	if s.sweeps != nil {
		s.sweeps.Notify()
	}
}

// MaintenanceStore is the maintenance half of persistence.
type MaintenanceStore interface {
	CreateMaintenanceWindow(ctx context.Context, w model.MaintenanceWindow, statusPageIDs []model.ID) error
	UpdateMaintenanceWindow(ctx context.Context, w model.MaintenanceWindow, statusPageIDs []model.ID) error
	GetMaintenanceWindow(ctx context.Context, id model.ID) (model.MaintenanceWindow, error)
	ListMaintenanceWindows(ctx context.Context, after *store.Cursor, limit int, search string, monitorID *model.ID) ([]model.MaintenanceWindow, bool, error)
	DeleteMaintenanceWindow(ctx context.Context, id model.ID) error
	StatusPageIDsForWindows(ctx context.Context, windowIDs []model.ID) (map[model.ID][]model.ID, error)
	MissingIDs(ctx context.Context, table string, orgID model.ID, ids []model.ID) ([]model.ID, error)
}
