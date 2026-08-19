package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The notification-channel surface.
//
// One rule shapes every handler here: a secret goes in and never comes back out.
// It is enforced in three places that have to agree — Split puts secrets in the
// encrypted envelope rather than in config, Redact replaces them with a marker
// on the way out, and StripRedacted drops the marker on the way back in so a
// form that round-trips its own GET cannot overwrite a bot token with asterisks.

// maxChannelBody bounds a channel write. A webhook body template is the largest
// legitimate field here and 256KB is far past any of them.
const maxChannelBody = 1 << 18

// eventTypes is the closed set a channel may subscribe to, from the spec.
var eventTypes = map[string]bool{
	model.EventMonitorUp: true, model.EventMonitorDown: true, model.EventMonitorPending: true,
	model.EventMonitorCreated: true, model.EventMonitorUpdated: true, model.EventMonitorDeleted: true,
	model.EventMonitorPaused: true, model.EventMonitorResumed: true,
	model.EventMonitorCertificateExpiring: true, model.EventMonitorDomainExpiring: true,
	model.EventIncidentOpened: true, model.EventIncidentUpdated: true, model.EventIncidentResolved: true,
	model.EventMaintenanceStarted: true, model.EventMaintenanceEnded: true,
	model.EventReportGenerated: true,
}

// emittedEvents is the subset this build actually raises. Subscribing to one of
// the others is accepted — it is in the contract and the stored value must round
// trip — but it will never fire, and saying so at save time is better than
// leaving somebody to work it out during an incident.
var emittedEvents = map[string]bool{
	model.EventMonitorUp: true, model.EventMonitorDown: true, model.EventMonitorPending: true,
}

func (s *Server) listNotificationChannels(w http.ResponseWriter, r *http.Request) {
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

	filter := store.ChannelFilter{Search: r.URL.Query().Get("search")}
	for _, t := range r.URL.Query()["type"] {
		if !notify.KnownType(t) {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid type filter",
				fmt.Sprintf("type %q is not one the spec defines: want %s", t, strings.Join(notify.Types(), ", ")))
			return
		}
		filter.Types = append(filter.Types, t)
	}
	switch r.URL.Query().Get("enabled") {
	case "true":
		enabled := true
		filter.Enabled = &enabled
	case "false":
		enabled := false
		filter.Enabled = &enabled
	}

	channels, hasMore, err := s.store.ListChannels(r.Context(), after, s.limit(r), filter)
	if err != nil {
		s.internal(w, r, "list notification channels", err)
		return
	}

	body := page[channelJSON]{Data: []channelJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, c := range channels {
		rendered, err := s.renderChannel(c)
		if err != nil {
			s.internal(w, r, "render notification channel", err)
			return
		}
		body.Data = append(body.Data, rendered)
	}
	if hasMore && len(channels) > 0 {
		last := channels[len(channels)-1].Channel
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}

	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createNotificationChannel(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readChannelBody(w, r)
	if !ok {
		return
	}

	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	channel := model.NotificationChannel{
		ID:        model.NewID(),
		OrgID:     s.orgID,
		Enabled:   true,
		Events:    []string{},
		CreatedAt: now,
		UpdatedAt: now,
	}

	switch {
	case body.Name == nil || strings.TrimSpace(*body.Name) == "":
		bad("/name", "required", "name is required")
	case len(*body.Name) > 200:
		bad("/name", "too_long", "name must be at most 200 characters")
	default:
		channel.Name = *body.Name
	}

	switch {
	case body.Type == nil || *body.Type == "":
		bad("/type", "required", "type is required")
	case !notify.KnownType(*body.Type):
		bad("/type", "invalid", fmt.Sprintf("type %q is not one the spec defines: want %s",
			*body.Type, strings.Join(notify.Types(), ", ")))
	case *body.Type == model.ChannelApprise && !s.alerts.AppriseAvailable():
		// The one channel whose availability is a property of the host. Refused
		// at creation rather than accepted and silently undeliverable.
		bad("/type", "unavailable",
			"apprise is not installed on this instance: install it (pipx install apprise) and restart, or use a native channel type")
	default:
		channel.Type = *body.Type
	}

	if body.Enabled != nil {
		channel.Enabled = *body.Enabled
	}
	if body.IsDefault != nil {
		channel.IsDefault = *body.IsDefault
	}
	if body.Events != nil {
		events, eventProblems := validateEvents(*body.Events)
		problems = append(problems, eventProblems...)
		channel.Events = events
	}

	config := body.Config
	if config == nil {
		config = map[string]any{}
	}
	notify.StripRedacted(config)
	if channel.Type != "" {
		problems = append(problems, toValidationItems(notify.Validate(channel.Type, config))...)
	}

	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The notification channel was not created.", problems...)
		return
	}

	if err := s.sealChannel(&channel, config); err != nil {
		s.internal(w, r, "seal channel secrets", err)
		return
	}
	if err := s.store.CreateChannel(r.Context(), channel); err != nil {
		s.internal(w, r, "create notification channel", err)
		return
	}
	s.log.Info("notification channel created", "id", channel.ID.String(), "type", channel.Type)

	created, err := s.store.GetChannel(r.Context(), channel.ID)
	if err != nil {
		s.internal(w, r, "read back notification channel", err)
		return
	}
	rendered, err := s.renderChannel(created)
	if err != nil {
		s.internal(w, r, "render notification channel", err)
		return
	}

	w.Header().Set("Location", "/api/v1/notification-channels/"+channel.ID.String())
	writeJSON(w, s.log, http.StatusCreated, rendered)
}

func (s *Server) getNotificationChannel(w http.ResponseWriter, r *http.Request) {
	channel, ok := s.loadChannel(w, r)
	if !ok {
		return
	}
	rendered, err := s.renderChannel(channel)
	if err != nil {
		s.internal(w, r, "render notification channel", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, rendered)
}

func (s *Server) updateNotificationChannel(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.loadChannel(w, r)
	if !ok {
		return
	}
	body, ok := s.readChannelBody(w, r)
	if !ok {
		return
	}

	channel := existing.Channel
	var problems []ValidationItem

	if body.Type != nil && *body.Type != channel.Type {
		problems = append(problems, ValidationItem{Pointer: "/type", Code: "immutable",
			Message: "type cannot be changed: it selects how config is interpreted, and changing it would reinterpret the stored configuration against a different schema"})
	}
	if body.Name != nil {
		switch {
		case strings.TrimSpace(*body.Name) == "":
			problems = append(problems, ValidationItem{Pointer: "/name", Code: "required", Message: "name must not be empty"})
		case len(*body.Name) > 200:
			problems = append(problems, ValidationItem{Pointer: "/name", Code: "too_long", Message: "name must be at most 200 characters"})
		default:
			channel.Name = *body.Name
		}
	}
	if body.Enabled != nil {
		channel.Enabled = *body.Enabled
	}
	if body.IsDefault != nil {
		channel.IsDefault = *body.IsDefault
	}
	if body.Events != nil {
		events, eventProblems := validateEvents(*body.Events)
		problems = append(problems, eventProblems...)
		channel.Events = events
	}

	// The merge that makes "omit a secret to leave it unchanged" work: the
	// stored halves are recombined, the incoming partial is laid over them, and
	// the result is validated as a whole. Validating the partial alone would
	// reject every PATCH that does not resend the bot token.
	current, err := s.channelConfig(existing.Channel)
	if err != nil {
		s.internal(w, r, "open channel secrets", err)
		return
	}
	merged := current
	if body.Config != nil {
		incoming := body.Config
		notify.StripRedacted(incoming)
		for key, value := range incoming {
			if value == nil {
				delete(merged, key)
				continue
			}
			merged[key] = value
		}
	}
	problems = append(problems, toValidationItems(notify.Validate(channel.Type, merged))...)

	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The notification channel was not updated.", problems...)
		return
	}

	channel.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if err := s.sealChannel(&channel, merged); err != nil {
		s.internal(w, r, "seal channel secrets", err)
		return
	}
	if err := s.store.UpdateChannel(r.Context(), channel); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.channelNotFound(w, r)
			return
		}
		s.internal(w, r, "update notification channel", err)
		return
	}

	updated, err := s.store.GetChannel(r.Context(), channel.ID)
	if err != nil {
		s.internal(w, r, "read back notification channel", err)
		return
	}
	rendered, err := s.renderChannel(updated)
	if err != nil {
		s.internal(w, r, "render notification channel", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, rendered)
}

func (s *Server) deleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.channelID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteChannel(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.channelNotFound(w, r)
			return
		}
		s.internal(w, r, "delete notification channel", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testNotificationChannel fires a sample notification and reports what the
// provider said. Every channel has this button because a channel that fails
// silently at 3am is worse than no channel at all.
func (s *Server) testNotificationChannel(w http.ResponseWriter, r *http.Request) {
	channel, ok := s.loadChannel(w, r)
	if !ok {
		return
	}

	var body struct {
		SampleEvent string `json:"sample_event"`
	}
	if r.ContentLength > 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-json",
				"Malformed request body", err.Error())
			return
		}
	}
	if body.SampleEvent == "" {
		body.SampleEvent = model.EventMonitorDown
	}
	if !eventTypes[body.SampleEvent] {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The test was not sent.", ValidationItem{
				Pointer: "/sample_event", Code: "invalid",
				Message: fmt.Sprintf("%q is not an event type the spec defines", body.SampleEvent)})
		return
	}

	started := time.Now()
	receipt, err := s.alerts.Test(r.Context(), channel.Channel, body.SampleEvent)
	elapsed := float64(time.Since(started).Microseconds()) / 1000.0

	result := notificationTestResult{
		Delivered:  err == nil,
		StatusCode: receipt.StatusCode,
		DurationMs: elapsed,
	}
	if receipt.Payload != "" {
		payload := receipt.Payload
		result.RenderedPayload = &payload
	}
	if err != nil {
		// Passed through verbatim. A summarised provider error is a support
		// ticket; the real one is usually the answer.
		message := err.Error()
		result.Error = &message
	}

	// 200 either way: the request succeeded, and whether the provider accepted
	// the message is the body's business. A 502 here would be the API claiming
	// the failure was its own.
	writeJSON(w, s.log, http.StatusOK, result)
}

// previewNotificationTemplate renders without sending. Exists so the UI can show
// exactly what will go out before the channel is saved — a preview that renders
// through a different path than delivery is a preview that lies, so this calls
// the same renderer.
func (s *Server) previewNotificationTemplate(w http.ResponseWriter, r *http.Request) {
	var body templatePreviewRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChannelBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-json",
			"Malformed request body", err.Error())
		return
	}
	if body.Event != "" && !eventTypes[body.Event] {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The template was not rendered.", ValidationItem{
				Pointer: "/event", Code: "invalid",
				Message: fmt.Sprintf("%q is not an event type the spec defines", body.Event)})
		return
	}

	ctx, ok := s.previewContext(w, r, body)
	if !ok {
		return
	}

	result := templatePreviewResult{ContextUsed: ctx}
	rendered, renderErr := notify.Render(body.Template, ctx, notify.EscapeNone)
	if renderErr != nil {
		result.Error = &templateError{Message: renderErr.Message, Line: &renderErr.Line, Column: &renderErr.Column}
		writeJSON(w, s.log, http.StatusOK, result)
		return
	}
	result.OK = true
	result.RenderedBody = &rendered

	if len(body.Headers) > 0 {
		result.RenderedHeaders = map[string]string{}
		for key, value := range body.Headers {
			out, headerErr := notify.Render(value, ctx, notify.EscapeNone)
			if headerErr != nil {
				result.OK = false
				result.RenderedBody = nil
				result.RenderedHeaders = nil
				result.Error = &templateError{
					Message: fmt.Sprintf("header %q: %s", key, headerErr.Message),
					Line:    &headerErr.Line, Column: &headerErr.Column,
				}
				writeJSON(w, s.log, http.StatusOK, result)
				return
			}
			result.RenderedHeaders[key] = out
		}
	}

	writeJSON(w, s.log, http.StatusOK, result)
}

// previewContext resolves which context to render against: an explicit one, a
// real monitor's current data, or the synthetic sample.
func (s *Server) previewContext(w http.ResponseWriter, r *http.Request, body templatePreviewRequest) (map[string]any, bool) {
	eventType := body.Event
	if eventType == "" {
		eventType = model.EventMonitorDown
	}

	ctx := notify.Context(s.alerts.SampleEvent(eventType))

	if body.MonitorID != nil && *body.MonitorID != "" {
		id, ok := model.ParseID(*body.MonitorID)
		if !ok {
			s.notFound(w, r)
			return nil, false
		}
		m, err := s.store.GetMonitor(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return nil, false
		} else if err != nil {
			s.internal(w, r, "load monitor for preview", err)
			return nil, false
		}
		// Rendered against the user's own values, which is the difference
		// between a preview they believe and one they have to imagine.
		ctx = notify.Context(s.eventForMonitor(eventType, m))
	}

	// An explicit context overrides, key by key, and is intended for testing.
	// Unknown keys are ignored rather than rejected: the renderer will refuse
	// any variable outside the catalogue anyway, and rejecting here would just
	// move the same error one step earlier with a worse message.
	for key, value := range body.Context {
		ctx[key] = value
	}
	return ctx, true
}

func (s *Server) listTemplateVariables(w http.ResponseWriter, r *http.Request) {
	vars := notify.Variables()
	sort.Slice(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })

	out := make([]templateVariableJSON, 0, len(vars))
	for _, v := range vars {
		out = append(out, templateVariableJSON{
			Name: v.Name, Type: v.Type, Description: v.Description, Example: v.Example,
		})
	}
	writeJSON(w, s.log, http.StatusOK, struct {
		Data []templateVariableJSON `json:"data"`
	}{Data: out})
}

// eventForMonitor builds an event from a monitor's current state, for the
// preview path.
func (s *Server) eventForMonitor(eventType string, m store.MonitorWithState) notify.Event {
	beat := model.Heartbeat{
		Time:      time.Now().UTC(),
		MonitorID: m.Monitor.ID,
		Message:   m.State.LastMessage,
	}
	if m.State.LastCheckAt != nil {
		beat.Time = *m.State.LastCheckAt
	}
	if m.State.LastResponseTimeMs != nil {
		d := time.Duration(*m.State.LastResponseTimeMs * float64(time.Millisecond))
		beat.ResponseTime = &d
	}
	switch m.State.Status {
	case model.MonitorStatusUp:
		beat.Status = model.StatusUp
	case model.MonitorStatusDown:
		beat.Status = model.StatusDown
	default:
		beat.Status = model.StatusPending
	}

	status := strings.TrimPrefix(eventType, "monitor.")
	return notify.NewEvent(eventType, s.alerts.SampleEvent(eventType).Instance,
		m.Monitor, m.State.Status, &beat, status, time.Now().UTC())
}

// renderChannel assembles the read shape: stored config plus a marker for each
// secret that is set.
func (s *Server) renderChannel(c store.ChannelWithCount) (channelJSON, error) {
	public, err := notify.DecodeConfig(c.Channel.Config)
	if err != nil {
		return channelJSON{}, err
	}
	secret, err := s.vault.Open(c.Channel.OrgID, c.Channel.ID, c.Channel.Secrets)
	if err != nil {
		return channelJSON{}, err
	}
	return toChannelJSON(c, notify.Redact(c.Channel.Type, public, secret)), nil
}

// channelConfig recombines the two halves for validation and merging. The only
// other place they are whole is inside a send.
func (s *Server) channelConfig(c model.NotificationChannel) (map[string]any, error) {
	public, err := notify.DecodeConfig(c.Config)
	if err != nil {
		return nil, err
	}
	secret, err := s.vault.Open(c.OrgID, c.ID, c.Secrets)
	if err != nil {
		return nil, err
	}
	return notify.Merge(public, secret), nil
}

// sealChannel splits a validated config and encrypts the secret half.
func (s *Server) sealChannel(channel *model.NotificationChannel, config map[string]any) error {
	public, secret := notify.Split(channel.Type, config)

	encoded, err := json.Marshal(public)
	if err != nil {
		return fmt.Errorf("encode channel config: %w", err)
	}
	channel.Config = encoded

	sealed, err := s.vault.Seal(channel.OrgID, channel.ID, secret)
	if err != nil {
		return err
	}
	channel.Secrets = sealed
	return nil
}

func (s *Server) readChannelBody(w http.ResponseWriter, r *http.Request) (channelWrite, bool) {
	var body channelWrite
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChannelBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-json",
			"Malformed request body", err.Error())
		return channelWrite{}, false
	}
	return body, true
}

func (s *Server) loadChannel(w http.ResponseWriter, r *http.Request) (store.ChannelWithCount, bool) {
	id, ok := s.channelID(w, r)
	if !ok {
		return store.ChannelWithCount{}, false
	}
	c, err := s.store.GetChannel(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.channelNotFound(w, r)
		return store.ChannelWithCount{}, false
	} else if err != nil {
		s.internal(w, r, "get notification channel", err)
		return store.ChannelWithCount{}, false
	}
	return c, true
}

func (s *Server) channelID(w http.ResponseWriter, r *http.Request) (model.ID, bool) {
	id, ok := model.ParseID(r.PathValue("channelId"))
	if !ok {
		s.channelNotFound(w, r)
		return model.ID{}, false
	}
	return id, true
}

func (s *Server) channelNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "channel-not-found",
		"Notification channel not found", "No notification channel with that identifier exists.")
}

func validateEvents(events []string) ([]string, []ValidationItem) {
	var problems []ValidationItem
	seen := map[string]bool{}
	out := make([]string, 0, len(events))

	for i, e := range events {
		switch {
		case !eventTypes[e]:
			problems = append(problems, ValidationItem{
				Pointer: fmt.Sprintf("/events/%d", i), Code: "invalid",
				Message: fmt.Sprintf("%q is not an event type the spec defines", e)})
		case !emittedEvents[e]:
			problems = append(problems, ValidationItem{
				Pointer: fmt.Sprintf("/events/%d", i), Code: "not_implemented",
				Message: fmt.Sprintf("%q is in the contract but nothing in this build raises it; subscribe to monitor.up, monitor.down or monitor.pending", e)})
		case seen[e]:
			// Duplicates are folded rather than refused: the same channel
			// receiving one event twice is the only outcome a duplicate could
			// have, and nobody meant that.
		default:
			seen[e] = true
			out = append(out, e)
		}
	}
	return out, problems
}

func toValidationItems(problems []notify.Problem) []ValidationItem {
	out := make([]ValidationItem, 0, len(problems))
	for _, p := range problems {
		out = append(out, ValidationItem{Pointer: p.Pointer, Code: p.Code, Message: p.Message})
	}
	return out
}

// ChannelStore is the notification half of persistence.
type ChannelStore interface {
	CreateChannel(ctx context.Context, c model.NotificationChannel) error
	UpdateChannel(ctx context.Context, c model.NotificationChannel) error
	GetChannel(ctx context.Context, id model.ID) (store.ChannelWithCount, error)
	ListChannels(ctx context.Context, after *store.Cursor, limit int, filter store.ChannelFilter) ([]store.ChannelWithCount, bool, error)
	DeleteChannel(ctx context.Context, id model.ID) error

	ChannelIDsForMonitor(ctx context.Context, monitorID model.ID) ([]model.ID, error)
	ChannelIDsForMonitors(ctx context.Context, monitorIDs []model.ID) (map[model.ID][]model.ID, error)
	SetMonitorChannels(ctx context.Context, monitorID, orgID model.ID, channelIDs []model.ID) error
	DefaultChannelIDs(ctx context.Context, orgID model.ID) ([]model.ID, error)
	MissingChannels(ctx context.Context, orgID model.ID, ids []model.ID) ([]model.ID, error)
}

// Alerts is the delivery side the API drives directly.
type Alerts interface {
	Test(ctx context.Context, channel model.NotificationChannel, eventType string) (notify.Receipt, error)
	SampleEvent(eventType string) notify.Event
	AppriseAvailable() bool
}
