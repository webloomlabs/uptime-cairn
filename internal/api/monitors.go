package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

func (s *Server) listMonitors(w http.ResponseWriter, r *http.Request) {
	limit := s.limit(r)

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

	monitors, hasMore, err := s.store.ListMonitors(r.Context(), after, limit)
	if err != nil {
		s.internal(w, r, "list monitors", err)
		return
	}

	body := page[monitorJSON]{Data: []monitorJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, m := range monitors {
		body.Data = append(body.Data, toMonitorJSON(m))
	}
	if hasMore && len(monitors) > 0 {
		last := monitors[len(monitors)-1]
		next := store.Cursor{UpdatedAt: last.Monitor.UpdatedAt, ID: last.Monitor.ID}.Encode()
		body.Pagination.NextCursor = &next
	}

	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createMonitor(w http.ResponseWriter, r *http.Request) {
	var body monitorWrite
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-json",
			"Malformed request body", err.Error())
		return
	}

	monitor, problems := s.buildMonitor(body)
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The monitor was not created.", problems...)
		return
	}

	if err := s.store.CreateMonitor(r.Context(), monitor); err != nil {
		s.internal(w, r, "create monitor", err)
		return
	}

	// Tell the control plane immediately. The monitor begins checking on the
	// next scheduler tick, not synchronously with this call — which is what the
	// spec promises and why the response carries no heartbeat.
	s.notify.Notify()
	s.log.Info("monitor created", "id", monitor.ID.String(), "type", monitor.Type, "interval", monitor.Interval)

	created, err := s.store.GetMonitor(r.Context(), monitor.ID)
	if err != nil {
		s.internal(w, r, "read back monitor", err)
		return
	}

	w.Header().Set("Location", "/api/v1/monitors/"+monitor.ID.String())
	writeJSON(w, s.log, http.StatusCreated, toMonitorJSON(created))
}

func (s *Server) getMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := s.monitorID(w, r)
	if !ok {
		return
	}

	m, err := s.store.GetMonitor(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get monitor", err)
		return
	}

	writeJSON(w, s.log, http.StatusOK, toMonitorJSON(m))
}

func (s *Server) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := s.monitorID(w, r)
	if !ok {
		return
	}

	err := s.store.DeleteMonitor(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "delete monitor", err)
		return
	}

	s.notify.Notify()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listHeartbeats(w http.ResponseWriter, r *http.Request) {
	id, ok := s.monitorID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}

	var before *time.Time
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		t, err := store.DecodeTimeCursor(raw)
		if err != nil {
			writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-cursor",
				"Malformed cursor", "The cursor must be one returned by a previous page of this collection.")
			return
		}
		before = &t
	}

	importantOnly := r.URL.Query().Get("important_only") == "true"

	beats, hasMore, err := s.store.ListHeartbeats(r.Context(), id, before, s.limit(r), importantOnly)
	if err != nil {
		s.internal(w, r, "list heartbeats", err)
		return
	}

	body := page[heartbeatJSON]{Data: []heartbeatJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, b := range beats {
		body.Data = append(body.Data, toHeartbeatJSON(b))
	}
	if hasMore && len(beats) > 0 {
		next := store.EncodeTimeCursor(beats[len(beats)-1].Time)
		body.Pagination.NextCursor = &next
	}

	writeJSON(w, s.log, http.StatusOK, body)
}

// buildMonitor validates the request and fills in the spec's defaults. Returns
// one entry per invalid field rather than the first failure: a form that has to
// be submitted five times to learn five problems is a form nobody enjoys.
func (s *Server) buildMonitor(body monitorWrite) (model.Monitor, []ValidationItem) {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	m := model.Monitor{
		ID:               model.NewID(),
		OrgID:            model.SentinelOrgID,
		Enabled:          true,
		Interval:         60 * time.Second,
		Timeout:          30 * time.Second,
		NotifyOnRecovery: true,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	switch {
	case body.Name == nil || *body.Name == "":
		bad("/name", "required", "name is required")
	case len(*body.Name) > 200:
		bad("/name", "too_long", "name must be at most 200 characters")
	default:
		m.Name = *body.Name
	}
	if body.Description != nil {
		m.Description = *body.Description
	}

	switch {
	case body.Type == nil || *body.Type == "":
		bad("/type", "required", "type is required")
	case *body.Type == model.TypeHTTP:
		m.Type = model.TypeHTTP
	case knownType(*body.Type):
		// The type is in the contract; this build cannot run it. Refusing at
		// creation beats accepting a monitor that would sit pending forever
		// because no probe will ever take it.
		bad("/type", "not_implemented",
			fmt.Sprintf("type %q is specified but not implemented in this build; only %q runs today", *body.Type, model.TypeHTTP))
	default:
		bad("/type", "invalid", fmt.Sprintf("type %q is not one the spec defines", *body.Type))
	}

	if len(body.Config) == 0 {
		bad("/config", "required", "config is required")
	} else {
		m.Config = body.Config
		// The same validation the probe will run at assignment time, run here so
		// a bad URL is a 422 the caller can read rather than a rejection they
		// would have to go looking for in the logs.
		if checker, ok := s.registry.Lookup(m.Type); ok {
			if err := checker.Validate(body.Config); err != nil {
				bad("/config", "invalid", err.Error())
			}
		}
	}

	if body.Enabled != nil {
		m.Enabled = *body.Enabled
	}
	if body.NotifyOnRecovery != nil {
		m.NotifyOnRecovery = *body.NotifyOnRecovery
	}
	if body.UpsideDown != nil {
		m.UpsideDown = *body.UpsideDown
	}

	if body.IntervalSeconds != nil {
		switch v := *body.IntervalSeconds; {
		case v < 20:
			// The 20-second floor is a product commitment, and the schema
			// enforces it too — this is the message, not the enforcement.
			bad("/interval_seconds", "below_minimum", "interval_seconds must be at least 20")
		case v > 86400:
			bad("/interval_seconds", "above_maximum", "interval_seconds must be at most 86400")
		default:
			m.Interval = time.Duration(v) * time.Second
		}
	}
	if body.TimeoutSeconds != nil {
		switch v := *body.TimeoutSeconds; {
		case v < 1:
			bad("/timeout_seconds", "below_minimum", "timeout_seconds must be at least 1")
		case v > 300:
			bad("/timeout_seconds", "above_maximum", "timeout_seconds must be at most 300")
		default:
			m.Timeout = time.Duration(v) * time.Second
		}
	}
	if m.Timeout >= m.Interval {
		bad("/timeout_seconds", "not_less_than_interval",
			"timeout_seconds must be less than interval_seconds, or a check can still be running when the next one starts")
	}

	if body.Retries != nil {
		switch v := *body.Retries; {
		case v < 0:
			bad("/retries", "below_minimum", "retries must be at least 0")
		case v > 20:
			bad("/retries", "above_maximum", "retries must be at most 20")
		default:
			m.Retries = v
		}
	}
	if body.RetryIntervalSeconds != nil {
		switch v := *body.RetryIntervalSeconds; {
		case v < 20:
			bad("/retry_interval_seconds", "below_minimum", "retry_interval_seconds must be at least 20")
		case v > 86400:
			bad("/retry_interval_seconds", "above_maximum", "retry_interval_seconds must be at most 86400")
		default:
			m.RetryInterval = time.Duration(v) * time.Second
		}
	}
	if body.ResendAfter != nil {
		if *body.ResendAfter < 0 {
			bad("/resend_after", "below_minimum", "resend_after must be at least 0")
		} else {
			m.ResendAfter = *body.ResendAfter
		}
	}

	return m, problems
}

func knownType(t string) bool {
	switch t {
	case model.TypeHTTP, model.TypeTCP, model.TypeICMP, model.TypeDNS,
		model.TypeTLSExpiry, model.TypeDomainExpiry, model.TypePush,
		model.TypeDocker, model.TypeGRPC:
		return true
	}
	return false
}

func (s *Server) monitorID(w http.ResponseWriter, r *http.Request) (model.ID, bool) {
	id, ok := model.ParseID(r.PathValue("monitorId"))
	if !ok {
		s.notFound(w, r)
		return model.ID{}, false
	}
	return id, true
}

func (s *Server) limit(r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultLimit
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 {
		return defaultLimit
	}
	// Clamped, not rejected: the spec says the server caps this.
	return min(v, maxLimit)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "monitor-not-found",
		"Monitor not found", "No monitor with that identifier exists.")
}

// internal logs the cause and tells the caller nothing about it. An error
// message from the database is for the operator, not for whoever is holding the
// API key.
func (s *Server) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.Error(what, "error", err, "path", r.URL.Path)
	writeProblem(w, r, s.log, http.StatusInternalServerError, "internal-error",
		"Internal error", "The request could not be completed. The cause has been logged.")
}
