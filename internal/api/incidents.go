package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Incidents.
//
// The design decision worth defending: state changes do not go through PATCH.
// Advancing an incident from investigating to identified is done by posting a
// timeline entry that carries the new state, so every state change arrives with
// the sentence explaining it. An incident whose history reads
// "investigating → identified → resolved" with no words attached answers no
// question anybody will actually ask afterwards, and a PATCH that could set
// `state` would make that the path of least resistance.

const maxIncidentBody = 1 << 16

// IncidentStore is the incidents half of persistence.
type IncidentStore interface {
	CreateIncident(ctx context.Context, in model.Incident) error
	UpdateIncident(ctx context.Context, in model.Incident) error
	GetIncident(ctx context.Context, id model.ID) (model.Incident, error)
	ListIncidents(ctx context.Context, after *store.Cursor, limit int, filter store.IncidentFilter) ([]model.Incident, bool, error)
	DeleteIncident(ctx context.Context, id model.ID) error

	AddIncidentUpdate(ctx context.Context, u model.IncidentUpdate, at time.Time) error
	ListIncidentUpdates(ctx context.Context, incidentID model.ID) ([]model.IncidentUpdate, error)

	CountOpenIncidents(ctx context.Context) (int, error)
	IncidentsForStatusPage(ctx context.Context, pageID model.ID, since time.Time, limit int) ([]model.Incident, error)
}

func (s *Server) listIncidents(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}
	filter, ok := s.incidentFilter(w, r)
	if !ok {
		return
	}

	incidents, hasMore, err := s.store.ListIncidents(r.Context(), after, s.limit(r), filter)
	if err != nil {
		s.internal(w, r, "list incidents", err)
		return
	}

	// The timeline is deliberately omitted from the list. Fifty incidents each
	// carrying a dozen updates is a response nobody reads and a query fan-out
	// per row; the single read carries it.
	body := page[incidentJSON]{Data: []incidentJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, in := range incidents {
		body.Data = append(body.Data, toIncidentJSON(in, false))
	}
	if hasMore && len(incidents) > 0 {
		last := incidents[len(incidents)-1]
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createIncident(w http.ResponseWriter, r *http.Request) {
	var body incidentWrite
	if !s.readBody(w, r, maxIncidentBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	incident := model.Incident{
		ID:        model.NewID(),
		OrgID:     s.orgID,
		State:     model.IncidentInvestigating,
		Impact:    model.ImpactNone,
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}

	problems := s.buildIncident(r.Context(), &incident, incidentPatch{
		Title:         body.Title,
		Impact:        body.Impact,
		StartedAt:     body.StartedAt,
		MonitorIDs:    optionalSlice(body.MonitorIDs),
		StatusPageIDs: optionalSlice(body.StatusPageIDs),
	})
	if incident.Title == "" && body.Title == nil {
		problems = append(problems, ValidationItem{Pointer: "/title", Code: "required",
			Message: "title is required"})
	}
	if body.State != nil {
		if !model.ValidIncidentState(*body.State) {
			problems = append(problems, ValidationItem{Pointer: "/state", Code: "invalid",
				Message: fmt.Sprintf("state %q is not one of investigating, identified, monitoring, resolved", *body.State)})
		} else {
			incident.State = *body.State
		}
	}
	if incident.State == model.IncidentResolved {
		incident.ResolvedAt = &now
	}

	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The incident was not opened.", problems...)
		return
	}

	if err := s.store.CreateIncident(r.Context(), incident); err != nil {
		s.internal(w, r, "create incident", err)
		return
	}

	// The opening entry is written through the same call every later one takes,
	// so an incident opened with a note and one that gained its first note a
	// minute later are the same shape in the timeline.
	if body.Body != nil && *body.Body != "" {
		update := model.IncidentUpdate{
			ID:                  model.NewID(),
			IncidentID:          incident.ID,
			OrgID:               incident.OrgID,
			State:               incident.State,
			Body:                *body.Body,
			AuthorID:            s.actor(r),
			NotifiedSubscribers: body.NotifySubscribers == nil || *body.NotifySubscribers,
			CreatedAt:           now,
		}
		if err := s.store.AddIncidentUpdate(r.Context(), update, now); err != nil {
			s.internal(w, r, "add opening incident update", err)
			return
		}
	}

	s.raiseIncidentEvent(model.EventIncidentOpened, incident)
	s.log.Info("incident opened", "id", incident.ID.String(), "impact", incident.Impact)
	s.writeIncident(w, r, incident.ID, http.StatusCreated)
}

func (s *Server) getIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "incidentId", s.incidentNotFound)
	if !ok {
		return
	}
	s.writeIncident(w, r, id, http.StatusOK)
}

func (s *Server) updateIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "incidentId", s.incidentNotFound)
	if !ok {
		return
	}

	incident, err := s.store.GetIncident(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.incidentNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get incident", err)
		return
	}

	var body incidentPatch
	if !s.readBody(w, r, maxIncidentBody, &body) {
		return
	}

	incident.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if problems := s.buildIncident(r.Context(), &incident, body); len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The incident was not updated.", problems...)
		return
	}

	if err := s.store.UpdateIncident(r.Context(), incident); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.incidentNotFound(w, r)
			return
		}
		s.internal(w, r, "update incident", err)
		return
	}
	s.raiseIncidentEvent(model.EventIncidentUpdated, incident)
	s.writeIncident(w, r, id, http.StatusOK)
}

func (s *Server) deleteIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "incidentId", s.incidentNotFound)
	if !ok {
		return
	}
	if err := s.store.DeleteIncident(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.incidentNotFound(w, r)
			return
		}
		s.internal(w, r, "delete incident", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listIncidentUpdates(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "incidentId", s.incidentNotFound)
	if !ok {
		return
	}
	if _, err := s.store.GetIncident(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		s.incidentNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get incident", err)
		return
	}

	updates, err := s.store.ListIncidentUpdates(r.Context(), id)
	if err != nil {
		s.internal(w, r, "list incident updates", err)
		return
	}

	// No pagination: the timeline is the incident, and an incident with enough
	// updates to need a second page is one where being handed a partial history
	// would be worse than the size of the response.
	data := make([]incidentUpdateJSON, 0, len(updates))
	for _, u := range updates {
		data = append(data, toIncidentUpdateJSON(u))
	}
	writeJSON(w, s.log, http.StatusOK, map[string]any{"data": data})
}

// createIncidentUpdate appends a timeline entry and, when it carries a state,
// advances the incident to it.
func (s *Server) createIncidentUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "incidentId", s.incidentNotFound)
	if !ok {
		return
	}

	var body timelineWrite
	if !s.readBody(w, r, maxIncidentBody, &body) {
		return
	}

	var problems []ValidationItem
	switch {
	case body.Body == nil || *body.Body == "":
		problems = append(problems, ValidationItem{Pointer: "/body", Code: "required",
			Message: "body is required"})
	case len(*body.Body) > 20000:
		problems = append(problems, ValidationItem{Pointer: "/body", Code: "too_long",
			Message: "body must be at most 20000 characters"})
	}
	if body.State != nil && !model.ValidIncidentState(*body.State) {
		problems = append(problems, ValidationItem{Pointer: "/state", Code: "invalid",
			Message: fmt.Sprintf("state %q is not one of investigating, identified, monitoring, resolved", *body.State)})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The update was not posted.", problems...)
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	update := model.IncidentUpdate{
		ID:                  model.NewID(),
		IncidentID:          id,
		OrgID:               s.orgID,
		Body:                *body.Body,
		AuthorID:            s.actor(r),
		NotifiedSubscribers: body.NotifySubscribers == nil || *body.NotifySubscribers,
		CreatedAt:           now,
	}
	if body.State != nil {
		update.State = *body.State
	}

	if err := s.store.AddIncidentUpdate(r.Context(), update, now); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.incidentNotFound(w, r)
			return
		}
		s.internal(w, r, "add incident update", err)
		return
	}

	// Read back so the event describes the incident as it now is, including the
	// state this entry may just have moved it to.
	if incident, err := s.store.GetIncident(r.Context(), id); err == nil {
		event := model.EventIncidentUpdated
		if incident.State == model.IncidentResolved {
			event = model.EventIncidentResolved
		}
		s.raiseIncidentEvent(event, incident)
	}

	w.Header().Set("Location", "/api/v1/incidents/"+id.String()+"/updates")
	writeJSON(w, s.log, http.StatusCreated, toIncidentUpdateJSON(update))
}

// buildIncident folds a metadata patch onto an incident.
func (s *Server) buildIncident(ctx context.Context, in *model.Incident, body incidentPatch) []ValidationItem {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	if body.Title != nil {
		switch {
		case *body.Title == "":
			bad("/title", "required", "title must not be empty")
		case len(*body.Title) > 300:
			bad("/title", "too_long", "title must be at most 300 characters")
		default:
			in.Title = *body.Title
		}
	}
	if body.Impact != nil {
		if !model.ValidIncidentImpact(*body.Impact) {
			bad("/impact", "invalid", fmt.Sprintf("impact %q is not one of none, minor, major, critical", *body.Impact))
		} else {
			in.Impact = *body.Impact
		}
	}
	if body.StartedAt != nil {
		in.StartedAt = body.StartedAt.UTC()
	}

	if body.MonitorIDs != nil {
		ids, itemProblems := s.resolveEntityIDs(ctx, "monitors", "/monitor_ids", *body.MonitorIDs)
		problems = append(problems, itemProblems...)
		in.MonitorIDs = ids
	}
	if body.StatusPageIDs != nil {
		ids, itemProblems := s.resolveEntityIDs(ctx, "status_pages", "/status_page_ids", *body.StatusPageIDs)
		problems = append(problems, itemProblems...)
		in.StatusPageIDs = ids
	}
	if len(body.AssignedTo) > 0 {
		id, itemProblems := s.resolveNullableID(ctx, "users", "/assigned_to", body.AssignedTo)
		problems = append(problems, itemProblems...)
		in.AssignedTo = id
	}
	return problems
}

// raiseIncidentEvent publishes an incident lifecycle event.
//
// Fire-and-forget through the same alerter the control plane uses, so an
// incident opened by hand reaches the same channels and the same outbound
// webhooks a monitor going down does. A separate path would mean an operator
// wiring up alerting twice and discovering the gap during an outage.
func (s *Server) raiseIncidentEvent(eventType string, in model.Incident) {
	if s.alerts == nil {
		return
	}
	s.alerts.Publish(notify.NewIncidentEvent(eventType, s.alerts.Instance(), notify.Incident{
		ID:         in.ID,
		Title:      in.Title,
		State:      in.State,
		Impact:     in.Impact,
		StartedAt:  in.StartedAt,
		ResolvedAt: in.ResolvedAt,
		MonitorIDs: idStrings(in.MonitorIDs),
	}, time.Now().UTC()))
}

func (s *Server) writeIncident(w http.ResponseWriter, r *http.Request, id model.ID, status int) {
	incident, err := s.store.GetIncident(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.incidentNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get incident", err)
		return
	}
	if status == http.StatusCreated {
		w.Header().Set("Location", "/api/v1/incidents/"+id.String())
	}
	writeJSON(w, s.log, status, toIncidentJSON(incident, true))
}

// incidentFilter reads the filters an incident listing accepts.
func (s *Server) incidentFilter(w http.ResponseWriter, r *http.Request) (store.IncidentFilter, bool) {
	var filter store.IncidentFilter
	query := r.URL.Query()

	for _, raw := range query["state"] {
		if !model.ValidIncidentState(raw) {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
				fmt.Sprintf("state %q is not one of investigating, identified, monitoring, resolved", raw))
			return store.IncidentFilter{}, false
		}
		filter.States = append(filter.States, raw)
	}
	for _, raw := range query["impact"] {
		if !model.ValidIncidentImpact(raw) {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
				fmt.Sprintf("impact %q is not one of none, minor, major, critical", raw))
			return store.IncidentFilter{}, false
		}
		filter.Impacts = append(filter.Impacts, raw)
	}

	for _, param := range []struct {
		name string
		into **model.ID
	}{
		{"monitor_id", &filter.MonitorID},
		{"status_page_id", &filter.StatusPageID},
	} {
		raw := query.Get(param.name)
		if raw == "" {
			continue
		}
		id, ok := model.ParseID(raw)
		if !ok {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
				fmt.Sprintf("%s %q is not a valid identifier", param.name, raw))
			return store.IncidentFilter{}, false
		}
		*param.into = &id
	}

	for _, param := range []struct {
		name string
		into **time.Time
	}{
		{"from", &filter.From},
		{"to", &filter.To},
	} {
		raw := query.Get(param.name)
		if raw == "" {
			continue
		}
		when, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
				fmt.Sprintf("%s %q must be an RFC 3339 timestamp", param.name, raw))
			return store.IncidentFilter{}, false
		}
		utc := when.UTC()
		*param.into = &utc
	}

	filter.Search = query.Get("search")
	return filter, true
}

// actor is who to attribute a write to. An API key has no user behind it in
// Phase 1, so the entry is authored by nobody rather than by a fabricated
// identity — a timeline that names the wrong person is worse than one that
// names none.
func (s *Server) actor(r *http.Request) *model.ID {
	principal, ok := principalFrom(r.Context())
	if !ok || principal.User == nil {
		return nil
	}
	id := principal.User.ID
	return &id
}

func (s *Server) incidentNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "incident-not-found",
		"Incident not found", "No incident with that identifier exists.")
}

// resolveEntityIDs parses and existence-checks a list of identifiers, so a bad
// one is a 422 naming the index rather than a foreign-key error nobody can map
// back to a field.
func (s *Server) resolveEntityIDs(ctx context.Context, table, pointer string, raws []string) ([]model.ID, []ValidationItem) {
	if len(raws) == 0 {
		return nil, nil
	}

	var (
		ids      []model.ID
		problems []ValidationItem
	)
	for i, raw := range raws {
		id, ok := model.ParseID(raw)
		if !ok {
			problems = append(problems, ValidationItem{
				Pointer: fmt.Sprintf("%s/%d", pointer, i), Code: "invalid",
				Message: fmt.Sprintf("%q is not a valid identifier", raw)})
			continue
		}
		ids = append(ids, id)
	}
	if len(problems) > 0 {
		return nil, problems
	}

	missing, err := s.store.MissingIDs(ctx, table, s.orgID, ids)
	if err != nil {
		s.log.Error("check ids", "error", err, "table", table)
		return nil, []ValidationItem{{Pointer: pointer, Code: "unavailable",
			Message: "the referenced records could not be checked"}}
	}
	for _, id := range missing {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: "not_found",
			Message: fmt.Sprintf("no record %s exists in %s", id, table)})
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return ids, nil
}

// resolveNullableID handles the null-clears-it case for a single reference.
func (s *Server) resolveNullableID(ctx context.Context, table, pointer string, raw []byte) (*model.ID, []ValidationItem) {
	var supplied *string
	if err := json.Unmarshal(raw, &supplied); err != nil {
		return nil, []ValidationItem{{Pointer: pointer, Code: "invalid",
			Message: "must be an identifier or null"}}
	}
	if supplied == nil || *supplied == "" {
		return nil, nil
	}
	ids, problems := s.resolveEntityIDs(ctx, table, pointer, []string{*supplied})
	if len(problems) > 0 || len(ids) == 0 {
		return nil, problems
	}
	return &ids[0], nil
}

// optionalSlice turns a plain slice into the pointer form the patch shape uses,
// so create and update can share one builder.
func optionalSlice(values []string) *[]string {
	if values == nil {
		return nil
	}
	return &values
}
