package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

func (s *Server) listMonitors(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}
	filter, ok := s.monitorFilter(w, r)
	if !ok {
		return
	}

	monitors, hasMore, err := s.store.ListMonitors(r.Context(), after, s.limit(r), filter)
	if err != nil {
		s.internal(w, r, "list monitors", err)
		return
	}

	ids := make([]model.ID, 0, len(monitors))
	for _, m := range monitors {
		ids = append(ids, m.Monitor.ID)
	}
	assignments, err := s.store.ChannelIDsForMonitors(r.Context(), ids)
	if err != nil {
		s.internal(w, r, "list channel assignments", err)
		return
	}
	tags, err := s.store.TagIDsForMonitors(r.Context(), ids)
	if err != nil {
		s.internal(w, r, "list monitor tags", err)
		return
	}

	body := page[monitorJSON]{Data: []monitorJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, m := range monitors {
		rendered, err := s.renderMonitor(m)
		if err != nil {
			s.internal(w, r, "render monitor", err)
			return
		}
		body.Data = append(body.Data,
			withTags(withChannels(rendered, assignments[m.Monitor.ID]), tags[m.Monitor.ID]))
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

	channelIDs, channelProblems := s.resolveChannels(r.Context(), body.NotificationChannelIDs)
	problems = append(problems, channelProblems...)
	problems = append(problems, s.resolveParent(r.Context(), &monitor, body.ParentMonitorID)...)
	problems = append(problems, s.resolveGroup(r.Context(), &monitor, body.GroupID)...)

	tagIDs, tagProblems := s.resolveTags(r.Context(), body.TagIDs)
	problems = append(problems, tagProblems...)

	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The monitor was not created.", problems...)
		return
	}

	// A push monitor's token is minted here and shown exactly once. Storing only
	// the hash means it cannot be recovered later, which is why the response
	// below is the single opportunity to copy it.
	var pushToken string
	if monitor.Type == model.TypePush {
		token, err := auth.NewToken()
		if err != nil {
			s.internal(w, r, "generate push token", err)
			return
		}
		pushToken = token
		monitor.PushTokenHash = auth.HashToken(token)
	}

	// The credentials come out of config here, on the way in, and go back
	// together only in memory: in the control plane when it hands the monitor to
	// a probe, and nowhere else (data model §12.1).
	if err := s.sealMonitor(&monitor); err != nil {
		s.internal(w, r, "seal monitor credentials", err)
		return
	}

	if err := s.store.CreateMonitor(r.Context(), monitor); err != nil {
		s.internal(w, r, "create monitor", err)
		return
	}

	// Assignment is a separate write rather than part of the monitor
	// transaction, and a failure here is reported rather than swallowed: a
	// monitor that exists but alerts nowhere is precisely the outcome this
	// feature exists to prevent, so the caller is told.
	if len(channelIDs) > 0 {
		if err := s.store.SetMonitorChannels(r.Context(), monitor.ID, monitor.OrgID, channelIDs); err != nil {
			s.internal(w, r, "assign notification channels", err)
			return
		}
	}
	if len(tagIDs) > 0 {
		if err := s.store.SetMonitorTags(r.Context(), monitor.ID, monitor.OrgID, tagIDs); err != nil {
			s.internal(w, r, "assign tags", err)
			return
		}
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
	rendered, err := s.renderMonitor(created)
	if err != nil {
		s.internal(w, r, "render monitor", err)
		return
	}

	w.Header().Set("Location", "/api/v1/monitors/"+monitor.ID.String())
	writeJSON(w, s.log, http.StatusCreated,
		withPushToken(withTags(withChannels(rendered, channelIDs), tagIDs), pushToken, pushURL(r, pushToken)))
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

	channelIDs, err := s.store.ChannelIDsForMonitor(r.Context(), id)
	if err != nil {
		s.internal(w, r, "get channel assignments", err)
		return
	}
	tagIDs, err := s.store.TagIDsForMonitor(r.Context(), id)
	if err != nil {
		s.internal(w, r, "get monitor tags", err)
		return
	}

	rendered, err := s.renderMonitor(m)
	if err != nil {
		s.internal(w, r, "render monitor", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, withTags(withChannels(rendered, channelIDs), tagIDs))
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
	case s.runnable(*body.Type):
		m.Type = *body.Type
	case knownType(*body.Type):
		// The type is in the contract; this build cannot run it. Refusing at
		// creation beats accepting a monitor that would sit pending forever
		// because no probe will ever take it.
		bad("/type", "not_implemented",
			fmt.Sprintf("type %q is specified but not implemented in this build; %s run today",
				*body.Type, strings.Join(s.registry.Types(), ", ")))
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
		// A config echoing a redacted read is refused rather than stored.
		// Accepting "__redacted__" as a password produces a monitor that looks
		// configured and authenticates as nobody, and the failure surfaces hours
		// later as a 401 attributed to the target.
		for _, path := range model.FindRedacted(body.Config, s.registry.SecretFields(m.Type)) {
			bad("/config/"+strings.ReplaceAll(path, ".", "/"), "redacted",
				fmt.Sprintf("%s came back from a read with its value hidden; supply the real credential, or omit it", path))
		}

		switch checker, ok := s.registry.Lookup(m.Type); {
		case ok:
			if err := checker.Validate(body.Config); err != nil {
				bad("/config", "invalid", err.Error())
			}
			// Promoted out of config so "what else points at this host?" is an
			// indexed query, and so an alert can lead with the line that
			// actually tells the reader what broke.
			if targeter, ok := checker.(check.Targeter); ok {
				m.Target = targeter.Target(body.Config)
			}
		case m.Type == model.TypePush:
			if err := validatePushConfig(body.Config); err != nil {
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

// renderMonitor assembles the read shape: the stored config plus a marker
// wherever the encrypted half holds a credential.
//
// The redaction is assembled rather than applied — the secret is not in the
// column being serialised, so there is nothing here that could forget to hide
// it. That is the property worth having; a redacting serialiser is one refactor
// away from not redacting.
func (s *Server) renderMonitor(m store.MonitorWithState) (monitorJSON, error) {
	out := toMonitorJSON(m)

	fields := s.registry.SecretFields(m.Monitor.Type)
	if len(fields) == 0 || len(m.Monitor.ConfigSecrets) == 0 {
		return out, nil
	}

	secret, err := s.configs.Open(m.Monitor.OrgID[:], m.Monitor.ID[:], m.Monitor.ConfigSecrets)
	if err != nil {
		return monitorJSON{}, err
	}
	config, err := model.RedactConfig(m.Monitor.Config, secret, fields)
	if err != nil {
		return monitorJSON{}, err
	}
	out.Config = config
	return out, nil
}

// sealMonitor splits a validated config and encrypts the credential half.
func (s *Server) sealMonitor(m *model.Monitor) error {
	fields := s.registry.SecretFields(m.Type)
	if len(fields) == 0 {
		return nil
	}

	public, secret, err := model.SplitConfig(m.Config, fields)
	if err != nil {
		return err
	}
	sealed, err := s.configs.Seal(m.OrgID[:], m.ID[:], secret)
	if err != nil {
		return err
	}
	m.Config, m.ConfigSecrets = public, sealed
	return nil
}

// maxDependencyDepth is how deep a dependency chain may go.
//
// Ten is far past any real topology — router, switch, host, service is four —
// and the bound exists so that a chain which somehow became a cycle is a
// validation error here rather than an infinite walk in ingest.
const maxDependencyDepth = 10

// resolveParent validates parent_monitor_id: it must exist, it must not be the
// monitor itself, and the chain it joins must be acyclic and shallow.
//
// A cycle cannot form through this endpoint today, because a monitor's parent
// must already exist and the new monitor is not yet anybody's ancestor. The walk
// is here because that is a property of the endpoint rather than of the data,
// and the first PATCH that lets a parent change would silently lose it.
func (s *Server) resolveParent(ctx context.Context, m *model.Monitor, raw *string) []ValidationItem {
	if raw == nil || *raw == "" {
		return nil
	}

	parentID, ok := model.ParseID(*raw)
	if !ok {
		return []ValidationItem{{Pointer: "/parent_monitor_id", Code: "invalid",
			Message: fmt.Sprintf("%q is not a valid identifier", *raw)}}
	}
	if parentID == m.ID {
		return []ValidationItem{{Pointer: "/parent_monitor_id", Code: "cycle",
			Message: "a monitor cannot depend on itself"}}
	}

	seen := map[model.ID]bool{m.ID: true}
	cursor := &parentID
	for depth := 0; cursor != nil; depth++ {
		if depth >= maxDependencyDepth {
			return []ValidationItem{{Pointer: "/parent_monitor_id", Code: "too_deep",
				Message: fmt.Sprintf("dependency chains are limited to %d levels", maxDependencyDepth)}}
		}
		if seen[*cursor] {
			return []ValidationItem{{Pointer: "/parent_monitor_id", Code: "cycle",
				Message: "that parent would make the dependency chain a loop"}}
		}
		seen[*cursor] = true

		parent, err := s.store.GetMonitor(ctx, *cursor)
		if errors.Is(err, store.ErrNotFound) {
			return []ValidationItem{{Pointer: "/parent_monitor_id", Code: "not_found",
				Message: fmt.Sprintf("no monitor %s exists", cursor)}}
		} else if err != nil {
			s.log.Error("resolve dependency parent", "error", err)
			return []ValidationItem{{Pointer: "/parent_monitor_id", Code: "unavailable",
				Message: "the parent monitor could not be checked"}}
		}
		cursor = parent.Monitor.ParentMonitorID
	}

	m.ParentMonitorID = &parentID
	return nil
}

// resolveChannels turns the request's channel ids into a validated set.
//
// The three cases are deliberately distinct. Absent attaches the default
// channels, which is what makes "set up alerting once" work. An empty array
// means no alerts, which a deliberately quiet monitor needs. Anything else is
// checked for existence here so a bad id is a 422 naming the index rather than a
// foreign-key error nobody can map back to a field.
func (s *Server) resolveChannels(ctx context.Context, requested *[]string) ([]model.ID, []ValidationItem) {
	if requested == nil {
		defaults, err := s.store.DefaultChannelIDs(ctx, s.orgID)
		if err != nil {
			s.log.Error("load default notification channels", "error", err)
			return nil, nil
		}
		return defaults, nil
	}

	var (
		problems []ValidationItem
		ids      []model.ID
	)
	for i, raw := range *requested {
		id, ok := model.ParseID(raw)
		if !ok {
			problems = append(problems, ValidationItem{
				Pointer: fmt.Sprintf("/notification_channel_ids/%d", i),
				Code:    "invalid", Message: fmt.Sprintf("%q is not a valid identifier", raw)})
			continue
		}
		ids = append(ids, id)
	}
	if len(problems) > 0 {
		return nil, problems
	}

	missing, err := s.store.MissingChannels(ctx, s.orgID, ids)
	if err != nil {
		s.log.Error("check notification channel ids", "error", err)
		return nil, []ValidationItem{{Pointer: "/notification_channel_ids", Code: "unavailable",
			Message: "the notification channels could not be checked"}}
	}
	for _, id := range missing {
		problems = append(problems, ValidationItem{
			Pointer: "/notification_channel_ids", Code: "not_found",
			Message: fmt.Sprintf("no notification channel %s exists", id)})
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return ids, nil
}

// runnable asks the checker registry rather than naming types here, so adding a
// monitor type is a registration and nothing else — the API stops being a second
// place that has to be told.
//
// push is the one exception, and it is an exception in the architecture rather
// than in this function: it has no checker because no probe ever runs it. The
// control plane evaluates it against the clock.
func (s *Server) runnable(t string) bool {
	if t == model.TypePush {
		return true
	}
	_, ok := s.registry.Lookup(t)
	return ok
}

// validatePushConfig stands in for the checker push does not have. Without it a
// push monitor is the one type whose config reaches storage unvalidated.
func validatePushConfig(config []byte) error {
	var cfg struct {
		ExpectedIntervalSeconds *int `json:"expected_interval_seconds"`
		GracePeriodSeconds      *int `json:"grace_period_seconds"`
	}
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if cfg.ExpectedIntervalSeconds != nil {
		if v := *cfg.ExpectedIntervalSeconds; v < 20 || v > 2592000 {
			return fmt.Errorf("expected_interval_seconds %d is outside 20-2592000", v)
		}
	}
	if cfg.GracePeriodSeconds != nil {
		if v := *cfg.GracePeriodSeconds; v < 0 || v > 86400 {
			return fmt.Errorf("grace_period_seconds %d is outside 0-86400", v)
		}
	}
	return nil
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
