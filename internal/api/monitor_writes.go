package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	"github.com/webloomlabs/uptime-cairn/internal/telemetry"
)

// The monitor write surface beyond create and delete: partial update, pause,
// resume, run-a-check-now, and the bulk operation an agency running a thousand
// monitors needs and cannot do by hand.
//
// Two of these are more interesting than they look. PATCH has to merge a partial
// config against a stored one whose credentials the caller cannot read, which is
// what model.PreserveRedacted exists for. And check-now runs a checker inside a
// request, which is the only place in the system where that happens — everywhere
// else a probe does it on a schedule.

const maxMonitorBody = 1 << 20

// maxBulk is the spec's ceiling. A bulk call is one transaction per monitor
// rather than one for the batch, so the bound is about how long a request may
// hold the single writer, not about memory.
const maxBulk = 1000

// checkNowInterval is the per-monitor floor between manual checks.
//
// The endpoint exists so somebody can confirm a fix immediately; the limit
// exists because "check now" wired to a button is a request-per-click, and the
// target is somebody else's server. Ten seconds is short enough to feel
// immediate and long enough that a held-down key does not become an attack.
const checkNowInterval = 10 * time.Second

func (s *Server) updateMonitor(w http.ResponseWriter, r *http.Request) {
	id, ok := s.monitorID(w, r)
	if !ok {
		return
	}

	existing, err := s.store.GetMonitor(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get monitor", err)
		return
	}

	var body monitorUpdate
	if !s.readBody(w, r, maxMonitorBody, &body) {
		return
	}

	monitor := existing.Monitor
	monitor.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)

	problems := s.applyMonitorUpdate(r.Context(), &monitor, existing.Monitor, body)

	// The three association lists are resolved before anything is written, so a
	// bad tag id fails the whole edit rather than leaving the monitor renamed
	// and mis-tagged.
	var (
		tagIDs     []model.ID
		channelIDs []model.ID
	)
	if body.TagIDs != nil {
		ids, tagProblems := s.resolveTags(r.Context(), body.TagIDs)
		problems = append(problems, tagProblems...)
		tagIDs = ids
	}
	if body.NotificationChannelIDs != nil {
		ids, channelProblems := s.resolveChannels(r.Context(), body.NotificationChannelIDs)
		problems = append(problems, channelProblems...)
		channelIDs = ids
	}

	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The monitor was not updated.", problems...)
		return
	}

	if err := s.sealMonitor(&monitor); err != nil {
		s.internal(w, r, "seal monitor credentials", err)
		return
	}
	if err := s.store.UpdateMonitor(r.Context(), monitor); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		s.internal(w, r, "update monitor", err)
		return
	}

	if body.TagIDs != nil {
		if err := s.store.SetMonitorTags(r.Context(), monitor.ID, monitor.OrgID, tagIDs); err != nil {
			s.internal(w, r, "assign tags", err)
			return
		}
	}
	if body.NotificationChannelIDs != nil {
		if err := s.store.SetMonitorChannels(r.Context(), monitor.ID, monitor.OrgID, channelIDs); err != nil {
			s.internal(w, r, "assign notification channels", err)
			return
		}
	}

	// Enabling or disabling through PATCH has to move the state row too, or the
	// monitor would read as "up" while nothing is checking it. The dedicated
	// pause and resume endpoints exist for the same reason and do the same
	// thing; this is not a second code path, it is the same one.
	if body.Enabled != nil && *body.Enabled != existing.Monitor.Enabled {
		if err := s.store.SetMonitorEnabled(r.Context(), monitor.ID, monitor.Enabled, monitor.UpdatedAt); err != nil {
			s.internal(w, r, "set monitor enabled", err)
			return
		}
	}

	s.notify.Notify()
	s.writeMonitor(w, r, monitor.ID, http.StatusOK)
}

// applyMonitorUpdate folds the request onto a loaded monitor.
//
// Every bound here is the one createMonitor enforces, restated rather than
// shared, because the two differ in exactly one way that matters: create has
// defaults and update has a current value. Sharing the switch would mean
// threading "is this a create" through it, which is how a validation rule ends
// up applying on one path and not the other.
func (s *Server) applyMonitorUpdate(ctx context.Context, m *model.Monitor, stored model.Monitor, body monitorUpdate) []ValidationItem {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	if body.Name != nil {
		switch {
		case *body.Name == "":
			bad("/name", "required", "name must not be empty")
		case len(*body.Name) > 200:
			bad("/name", "too_long", "name must be at most 200 characters")
		default:
			m.Name = *body.Name
		}
	}
	if body.Description != nil {
		m.Description = *body.Description
	}
	if body.Enabled != nil {
		m.Enabled = *body.Enabled
	}
	if body.UpsideDown != nil {
		m.UpsideDown = *body.UpsideDown
	}
	if body.NotifyOnRecovery != nil {
		m.NotifyOnRecovery = *body.NotifyOnRecovery
	}

	if body.IntervalSeconds != nil {
		switch v := *body.IntervalSeconds; {
		case v < 20:
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
	// Checked against the merged pair rather than against whichever one the
	// request happened to carry: raising the timeout past an interval the caller
	// did not mention is the same mistake as lowering the interval below it.
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
	if len(body.RetryIntervalSeconds) > 0 {
		var seconds *int
		if err := json.Unmarshal(body.RetryIntervalSeconds, &seconds); err != nil {
			bad("/retry_interval_seconds", "invalid", "retry_interval_seconds must be a number or null")
		} else if seconds == nil {
			m.RetryInterval = 0
		} else {
			switch v := *seconds; {
			case v < 20:
				bad("/retry_interval_seconds", "below_minimum", "retry_interval_seconds must be at least 20")
			case v > 86400:
				bad("/retry_interval_seconds", "above_maximum", "retry_interval_seconds must be at most 86400")
			default:
				m.RetryInterval = time.Duration(v) * time.Second
			}
		}
	}
	if body.ResendAfter != nil {
		if *body.ResendAfter < 0 {
			bad("/resend_after", "below_minimum", "resend_after must be at least 0")
		} else {
			m.ResendAfter = *body.ResendAfter
		}
	}

	if len(body.GroupID) > 0 {
		problems = append(problems, s.applyGroupChange(ctx, m, body.GroupID)...)
	}
	if len(body.ParentMonitorID) > 0 {
		problems = append(problems, s.applyParentChange(ctx, m, body.ParentMonitorID)...)
	}
	if len(body.Config) > 0 {
		problems = append(problems, s.applyConfigChange(m, stored, body.Config)...)
	}
	return problems
}

// applyConfigChange merges a partial config against the stored one.
//
// The order matters and is the whole of it: the stored halves are put back
// together first, so the merge happens against the real configuration rather
// than against the redacted view the caller last read; markers in the incoming
// document are then resolved back to the stored values they stand for; and only
// then is the result validated as a whole. Validating the patch alone would pass
// a config that is invalid once merged, which is a monitor that starts failing
// with a message about a field the caller never sent.
func (s *Server) applyConfigChange(m *model.Monitor, stored model.Monitor, patch json.RawMessage) []ValidationItem {
	fields := s.registry.SecretFields(stored.Type)

	full := stored.Config
	if len(stored.ConfigSecrets) > 0 {
		secret, err := s.configs.Open(stored.OrgID[:], stored.ID[:], stored.ConfigSecrets)
		if err != nil {
			s.log.Error("open monitor credentials", "error", err, "monitor", stored.ID.String())
			return []ValidationItem{{Pointer: "/config", Code: "unavailable",
				Message: "the monitor's stored credentials could not be read"}}
		}
		merged, err := model.MergeConfig(stored.Config, secret)
		if err != nil {
			return []ValidationItem{{Pointer: "/config", Code: "unavailable",
				Message: "the monitor's stored configuration could not be assembled"}}
		}
		full = merged
	}

	resolved, err := model.PreserveRedacted(full, patch, fields)
	if err != nil {
		return []ValidationItem{{Pointer: "/config", Code: "invalid", Message: err.Error()}}
	}

	config, err := shallowMerge(full, resolved)
	if err != nil {
		return []ValidationItem{{Pointer: "/config", Code: "invalid", Message: err.Error()}}
	}

	switch checker, ok := s.registry.Lookup(stored.Type); {
	case ok:
		if err := checker.Validate(config); err != nil {
			return []ValidationItem{{Pointer: "/config", Code: "invalid", Message: err.Error()}}
		}
		if targeter, ok := checker.(check.Targeter); ok {
			m.Target = targeter.Target(config)
		}
	case stored.Type == model.TypePush:
		if err := validatePushConfig(config); err != nil {
			return []ValidationItem{{Pointer: "/config", Code: "invalid", Message: err.Error()}}
		}
	}

	m.Config = config
	return nil
}

// shallowMerge replaces top-level keys, which is what the spec says a config
// patch does. Deep merging would be friendlier and would make it impossible to
// remove a nested key, so the spec chose the version that can express both
// operations and this implements that one.
func shallowMerge(base, patch json.RawMessage) (json.RawMessage, error) {
	var merged map[string]any
	if len(base) > 0 {
		if err := json.Unmarshal(base, &merged); err != nil {
			return nil, fmt.Errorf("stored config: %w", err)
		}
	}
	if merged == nil {
		merged = map[string]any{}
	}

	var incoming map[string]any
	if err := json.Unmarshal(patch, &incoming); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for key, value := range incoming {
		merged[key] = value
	}
	return json.Marshal(merged)
}

// applyGroupChange handles the null-clears-it case that a *string cannot express.
func (s *Server) applyGroupChange(ctx context.Context, m *model.Monitor, raw json.RawMessage) []ValidationItem {
	var supplied *string
	if err := json.Unmarshal(raw, &supplied); err != nil {
		return []ValidationItem{{Pointer: "/group_id", Code: "invalid",
			Message: "group_id must be an identifier or null"}}
	}
	if supplied == nil || *supplied == "" {
		m.GroupID = nil
		return nil
	}
	return s.resolveGroup(ctx, m, supplied)
}

// applyParentChange is where the dependency-cycle walk finally earns its keep.
//
// resolveParent has always walked the chain, and until now no endpoint could
// produce a cycle: a new monitor is nobody's ancestor. This one can — reparenting
// an existing monitor onto its own descendant closes the loop — which is exactly
// the case the walk was written for.
func (s *Server) applyParentChange(ctx context.Context, m *model.Monitor, raw json.RawMessage) []ValidationItem {
	var supplied *string
	if err := json.Unmarshal(raw, &supplied); err != nil {
		return []ValidationItem{{Pointer: "/parent_monitor_id", Code: "invalid",
			Message: "parent_monitor_id must be an identifier or null"}}
	}
	if supplied == nil || *supplied == "" {
		m.ParentMonitorID = nil
		return nil
	}
	return s.resolveParent(ctx, m, supplied)
}

func (s *Server) pauseMonitor(w http.ResponseWriter, r *http.Request) {
	s.setEnabled(w, r, false)
}

func (s *Server) resumeMonitor(w http.ResponseWriter, r *http.Request) {
	s.setEnabled(w, r, true)
}

func (s *Server) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id, ok := s.monitorID(w, r)
	if !ok {
		return
	}

	err := s.store.SetMonitorEnabled(r.Context(), id, enabled, time.Now().UTC().Truncate(time.Millisecond))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "set monitor enabled", err)
		return
	}

	// The probe learns immediately rather than on the next reconcile, which is
	// what makes "pause" mean the check stops now instead of within fifteen
	// minutes.
	s.notify.Notify()
	s.writeMonitor(w, r, id, http.StatusOK)
}

// runMonitorCheck executes one check outside the schedule and returns its
// result.
//
// The check runs here, in the API, because the control plane must not import
// probe/check — that is the ADR-001 seam, and it is what lets the same control
// plane serve a probe in another process. The result goes back through the
// control plane's ingest, so a manual check counts, transitions, and alerts
// exactly like a scheduled one. A "test" that took a different path would be
// testing the test.
func (s *Server) runMonitorCheck(w http.ResponseWriter, r *http.Request) {
	id, ok := s.monitorID(w, r)
	if !ok {
		return
	}

	loaded, err := s.store.GetMonitor(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get monitor", err)
		return
	}
	monitor := loaded.Monitor

	if wait, ok := s.checks.allow(id, time.Now()); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())+1))
		writeProblem(w, r, s.log, http.StatusTooManyRequests, "rate-limited",
			"Checked too recently",
			fmt.Sprintf("This monitor was checked manually within the last %s. The target is somebody's server; try again in %s.",
				checkNowInterval, wait.Round(time.Second)))
		return
	}

	if monitor.Type == model.TypePush {
		// A push monitor is a deadline, not a check. There is nothing to run,
		// and pretending otherwise would write a heartbeat the target did not
		// send.
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "not-checkable",
			"Nothing to check",
			"A push monitor is evaluated against the clock rather than by running a check. Send a heartbeat to its push URL instead.")
		return
	}

	checker, ok := s.registry.Lookup(monitor.Type)
	if !ok {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "not-checkable",
			"Nothing to check",
			fmt.Sprintf("Monitor type %q is not implemented in this build.", monitor.Type))
		return
	}

	config, err := s.openConfig(monitor)
	if err != nil {
		s.internal(w, r, "open monitor credentials", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), monitor.Timeout)
	defer cancel()
	observation := checker.Check(ctx, config)

	telemetry.Engine.ChecksRunInline.Add(1)
	beat, err := s.push.RecordCheck(r.Context(), monitor, observation.Status,
		observation.Code, observation.Message, observation.ResponseTime)
	if err != nil {
		s.internal(w, r, "record manual check", err)
		return
	}

	writeJSON(w, s.log, http.StatusOK, toHeartbeatJSON(beat))
}

// openConfig reassembles a monitor's configuration for the moment it is needed
// and no longer. The plaintext exists on this stack and is never written back.
func (s *Server) openConfig(m model.Monitor) ([]byte, error) {
	if len(m.ConfigSecrets) == 0 {
		return m.Config, nil
	}
	secret, err := s.configs.Open(m.OrgID[:], m.ID[:], m.ConfigSecrets)
	if err != nil {
		return nil, err
	}
	return model.MergeConfig(m.Config, secret)
}

// checkLimiter is the per-monitor floor on manual checks.
//
// Keyed by monitor rather than by caller: the thing being protected is the
// target, and two operators clicking the same button is the same load on it as
// one operator clicking twice.
type checkLimiter struct {
	mu   sync.Mutex
	last map[model.ID]time.Time
}

func newCheckLimiter() *checkLimiter {
	return &checkLimiter{last: make(map[model.ID]time.Time)}
}

// allow reports whether a check may run now, and how long to wait if not.
func (l *checkLimiter) allow(id model.ID, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if previous, seen := l.last[id]; seen {
		if elapsed := now.Sub(previous); elapsed < checkNowInterval {
			return checkNowInterval - elapsed, false
		}
	}
	l.last[id] = now
	return 0, true
}

// getMonitorMembership is ADR-004's reconciliation half: a cheap version and
// count for a filter, polled on a short interval by any client holding a
// filtered view.
//
// It exists because live updates only reach the monitors a client currently has
// on screen. A monitor that changes status off screen and would now match an
// active filter has no subscription telling the server anyone cares, and the
// alternative — evaluating live predicates for every connected client — costs
// per client per change rather than per poll.
func (s *Server) getMonitorMembership(w http.ResponseWriter, r *http.Request) {
	filter, ok := s.monitorFilter(w, r)
	if !ok {
		return
	}

	signal, err := s.store.Membership(r.Context(), filter)
	if err != nil {
		s.internal(w, r, "monitor membership", err)
		return
	}

	// A cached membership signal is a stale view, which is the one thing this
	// endpoint exists to prevent.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, s.log, http.StatusOK, membershipSignal{
		Version:     signal.Version,
		Count:       signal.Count,
		GeneratedAt: time.Now().UTC(),
	})
}

// bulkUpdateMonitors applies one operation to many monitors.
//
// Partial success is the contract, not a fallback: each identifier gets its own
// outcome, and one monitor deleted five minutes ago does not fail the other
// nine hundred and ninety-nine. Failing the batch would make the endpoint
// useless at exactly the size it exists for.
func (s *Server) bulkUpdateMonitors(w http.ResponseWriter, r *http.Request) {
	var body monitorBulkRequest
	if !s.readBody(w, r, maxMonitorBody, &body) {
		return
	}

	ids, problems := s.bulkTargets(body)
	operation, opProblems := s.bulkOperation(r.Context(), body)
	problems = append(problems, opProblems...)

	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "No monitors were changed.", problems...)
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	result := bulkResult{Succeeded: []string{}, Failed: []bulkFailure{}}

	for _, id := range ids {
		if err := operation(r.Context(), id, now); err != nil {
			code, message := "failed", "the operation could not be applied"
			if errors.Is(err, store.ErrNotFound) {
				code, message = "not_found", "no monitor with that identifier exists"
			} else {
				s.log.Error("bulk operation", "error", err, "monitor", id.String(), "operation", body.Operation)
			}
			result.Failed = append(result.Failed, bulkFailure{ID: id.String(), Code: code, Message: message})
			continue
		}
		result.Succeeded = append(result.Succeeded, id.String())
	}

	if len(result.Succeeded) > 0 {
		s.notify.Notify()
	}
	s.log.Info("bulk monitor operation", "operation", body.Operation,
		"succeeded", len(result.Succeeded), "failed", len(result.Failed))
	writeJSON(w, s.log, http.StatusOK, result)
}

func (s *Server) bulkTargets(body monitorBulkRequest) ([]model.ID, []ValidationItem) {
	switch {
	case len(body.MonitorIDs) == 0:
		return nil, []ValidationItem{{Pointer: "/monitor_ids", Code: "required",
			Message: "monitor_ids must contain at least one identifier"}}
	case len(body.MonitorIDs) > maxBulk:
		return nil, []ValidationItem{{Pointer: "/monitor_ids", Code: "above_maximum",
			Message: fmt.Sprintf("monitor_ids must contain at most %d identifiers", maxBulk)}}
	}

	var (
		ids      []model.ID
		problems []ValidationItem
	)
	for i, raw := range body.MonitorIDs {
		id, ok := model.ParseID(raw)
		if !ok {
			problems = append(problems, ValidationItem{
				Pointer: fmt.Sprintf("/monitor_ids/%d", i), Code: "invalid",
				Message: fmt.Sprintf("%q is not a valid identifier", raw)})
			continue
		}
		ids = append(ids, id)
	}
	return ids, problems
}

// bulkOperation turns the request into the function applied per monitor, with
// everything it depends on resolved once rather than a thousand times.
func (s *Server) bulkOperation(ctx context.Context, body monitorBulkRequest) (func(context.Context, model.ID, time.Time) error, []ValidationItem) {
	switch body.Operation {
	case "enable", "disable":
		enabled := body.Operation == "enable"
		return func(ctx context.Context, id model.ID, at time.Time) error {
			return s.store.SetMonitorEnabled(ctx, id, enabled, at)
		}, nil

	case "delete":
		return func(ctx context.Context, id model.ID, _ time.Time) error {
			return s.store.DeleteMonitor(ctx, id)
		}, nil

	case "add_tags", "remove_tags":
		tagIDs, problems := s.resolveTags(ctx, &body.TagIDs)
		if len(problems) > 0 {
			return nil, problems
		}
		if len(tagIDs) == 0 {
			return nil, []ValidationItem{{Pointer: "/tag_ids", Code: "required",
				Message: fmt.Sprintf("tag_ids is required for %s", body.Operation)}}
		}
		add := body.Operation == "add_tags"
		return func(ctx context.Context, id model.ID, _ time.Time) error {
			// Read-modify-write rather than an insert-or-delete, because the
			// store's assignment call replaces the whole set — and because
			// "add" has to be idempotent for a caller retrying a partial batch.
			existing, err := s.store.TagIDsForMonitor(ctx, id)
			if err != nil {
				return err
			}
			return s.store.SetMonitorTags(ctx, id, s.orgID, combine(existing, tagIDs, add))
		}, nil

	case "set_group":
		groupID, problems := s.bulkGroup(ctx, body.GroupID)
		if len(problems) > 0 {
			return nil, problems
		}
		return func(ctx context.Context, id model.ID, at time.Time) error {
			loaded, err := s.store.GetMonitor(ctx, id)
			if err != nil {
				return err
			}
			monitor := loaded.Monitor
			monitor.GroupID = groupID
			monitor.UpdatedAt = at
			return s.store.UpdateMonitor(ctx, monitor)
		}, nil

	case "set_notification_channels":
		channelIDs, problems := s.resolveChannels(ctx, &body.NotificationChannelIDs)
		if len(problems) > 0 {
			return nil, problems
		}
		return func(ctx context.Context, id model.ID, _ time.Time) error {
			if _, err := s.store.GetMonitor(ctx, id); err != nil {
				return err
			}
			return s.store.SetMonitorChannels(ctx, id, s.orgID, channelIDs)
		}, nil

	default:
		return nil, []ValidationItem{{Pointer: "/operation", Code: "invalid",
			Message: fmt.Sprintf("operation %q is not one of enable, disable, delete, add_tags, remove_tags, set_group, set_notification_channels", body.Operation)}}
	}
}

func (s *Server) bulkGroup(ctx context.Context, raw json.RawMessage) (*model.ID, []ValidationItem) {
	if len(raw) == 0 {
		return nil, []ValidationItem{{Pointer: "/group_id", Code: "required",
			Message: "group_id is required for set_group; send null to remove the monitors from their group"}}
	}
	var supplied *string
	if err := json.Unmarshal(raw, &supplied); err != nil {
		return nil, []ValidationItem{{Pointer: "/group_id", Code: "invalid",
			Message: "group_id must be an identifier or null"}}
	}
	if supplied == nil || *supplied == "" {
		return nil, nil
	}

	var monitor model.Monitor
	if problems := s.resolveGroup(ctx, &monitor, supplied); len(problems) > 0 {
		return nil, problems
	}
	return monitor.GroupID, nil
}

// combine adds or removes a set of tags from an existing assignment, preserving
// order so a repeated call is a no-op rather than a reshuffle.
func combine(existing, change []model.ID, add bool) []model.ID {
	present := make(map[model.ID]bool, len(existing))
	for _, id := range existing {
		present[id] = true
	}

	if !add {
		for _, id := range change {
			delete(present, id)
		}
		out := make([]model.ID, 0, len(present))
		for _, id := range existing {
			if present[id] {
				out = append(out, id)
			}
		}
		return out
	}

	out := append([]model.ID(nil), existing...)
	for _, id := range change {
		if !present[id] {
			present[id] = true
			out = append(out, id)
		}
	}
	return out
}

// writeMonitor renders one monitor with its associations, the shape every write
// path answers with.
func (s *Server) writeMonitor(w http.ResponseWriter, r *http.Request, id model.ID, status int) {
	loaded, err := s.store.GetMonitor(r.Context(), id)
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

	rendered, err := s.renderMonitor(loaded)
	if err != nil {
		s.internal(w, r, "render monitor", err)
		return
	}
	writeJSON(w, s.log, status, withTags(withChannels(rendered, channelIDs), tagIDs))
}

// getMonitorCertificate returns the certificate observed on the most recent
// check.
//
// **Nothing writes monitor_certificates in this build.** The TLS and HTTP
// checkers see the certificate and report only an expiry verdict, because
// carrying the observation to the control plane means a field on the probe
// protocol's result frame — a protocol change, which is deliberately not part of
// finishing the REST API. So this endpoint is correct and currently answers 404
// for every monitor, which is the honest answer to "what certificate was
// observed" when none has been.
func (s *Server) getMonitorCertificate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.monitorID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetMonitor(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get monitor", err)
		return
	}

	certificate, err := s.store.GetCertificate(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, r, s.log, http.StatusNotFound, "certificate-not-observed",
			"No certificate observed",
			"No TLS certificate has been recorded for this monitor. Monitor types that never perform a handshake never will.")
		return
	} else if err != nil {
		s.internal(w, r, "get certificate", err)
		return
	}

	writeJSON(w, s.log, http.StatusOK, toCertificateJSON(certificate, time.Now().UTC()))
}
