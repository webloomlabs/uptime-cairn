package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Outbound webhooks: the management surface. The delivery engine lives in
// internal/outbound, because it runs on the event stream rather than on a
// request.
//
// The one thing this file is careful about is the signing secret. It is minted
// on creation, returned exactly once, and encrypted at rest — encrypted rather
// than hashed, because every delivery recomputes an HMAC with it, which is the
// distinction data model §12.1 draws and the one that only surfaces when the
// first delivery goes out.

const maxWebhookBody = 1 << 16

// WebhookStore is the outbound-webhook half of persistence.
type WebhookStore interface {
	CreateWebhook(ctx context.Context, h model.Webhook, headers []byte) error
	UpdateWebhook(ctx context.Context, h model.Webhook, headers []byte) error
	GetWebhook(ctx context.Context, id model.ID) (model.Webhook, []byte, error)
	ListWebhooks(ctx context.Context, after *store.Cursor, limit int) ([]model.Webhook, bool, error)
	DeleteWebhook(ctx context.Context, id model.ID) error

	ListWebhookDeliveries(ctx context.Context, webhookID model.ID, before *time.Time, limit int, outcome string) ([]model.WebhookDelivery, bool, error)
	GetWebhookDelivery(ctx context.Context, id model.ID) (model.WebhookDelivery, error)
}

// Redeliverer is the delivery engine's one API-facing entry point.
type Redeliverer interface {
	// Redeliver resends a logged delivery's exact body. Exact, because the
	// reason to press the button is that the receiver was broken and has been
	// fixed — and a payload regenerated from current state would describe a
	// world the original event did not.
	Redeliver(ctx context.Context, hook model.Webhook, sealedHeaders []byte, delivery model.WebhookDelivery) (model.WebhookDelivery, error)
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}

	hooks, hasMore, err := s.store.ListWebhooks(r.Context(), after, s.limit(r))
	if err != nil {
		s.internal(w, r, "list webhooks", err)
		return
	}

	body := page[webhookJSON]{Data: []webhookJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, h := range hooks {
		body.Data = append(body.Data, toWebhookJSON(h))
	}
	if hasMore && len(hooks) > 0 {
		last := hooks[len(hooks)-1]
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	var body webhookWrite
	if !s.readBody(w, r, maxWebhookBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	hook := model.Webhook{
		ID:        model.NewID(),
		OrgID:     s.orgID,
		Enabled:   true,
		VerifyTLS: true,
		CreatedAt: now,
		UpdatedAt: now,
	}

	problems := buildWebhook(&hook, body)
	if body.URL == nil {
		problems = append(problems, ValidationItem{Pointer: "/url", Code: "required", Message: "url is required"})
	}
	if body.Events == nil || len(*body.Events) == 0 {
		problems = append(problems, ValidationItem{Pointer: "/events", Code: "required",
			Message: "events must name at least one event type"})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The webhook was not created.", problems...)
		return
	}

	secret, err := auth.NewToken()
	if err != nil {
		s.internal(w, r, "generate webhook secret", err)
		return
	}
	sealed, err := s.webhooks.Seal(hook.OrgID[:], hook.ID[:], []byte(secret))
	if err != nil {
		s.internal(w, r, "seal webhook secret", err)
		return
	}
	hook.SecretEncrypted = sealed
	hook.SecretPrefix = secret[:8]

	headers, err := s.sealHeaders(hook, body.Headers)
	if err != nil {
		s.internal(w, r, "seal webhook headers", err)
		return
	}

	if err := s.store.CreateWebhook(r.Context(), hook, headers); err != nil {
		s.internal(w, r, "create webhook", err)
		return
	}

	s.log.Info("webhook created", "id", hook.ID.String(), "events", len(hook.Events))
	rendered := toWebhookJSON(hook)
	rendered.Secret = secret
	w.Header().Set("Location", "/api/v1/webhooks/"+hook.ID.String())
	writeJSON(w, s.log, http.StatusCreated, rendered)
}

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "webhookId", s.webhookNotFound)
	if !ok {
		return
	}
	hook, _, err := s.store.GetWebhook(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.webhookNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get webhook", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toWebhookJSON(hook))
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "webhookId", s.webhookNotFound)
	if !ok {
		return
	}

	hook, storedHeaders, err := s.store.GetWebhook(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.webhookNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get webhook", err)
		return
	}

	var body webhookWrite
	if !s.readBody(w, r, maxWebhookBody, &body) {
		return
	}

	wasDisabled := hook.DisabledAt != nil
	hook.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if problems := buildWebhook(&hook, body); len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The webhook was not updated.", problems...)
		return
	}

	// Re-enabling clears the auto-disable and its counter. An operator who has
	// fixed the receiver and switched the subscription back on should not have
	// it turn itself off again on the strength of failures from before the fix.
	if wasDisabled && hook.Enabled && body.Enabled != nil && *body.Enabled {
		hook.DisabledAt = nil
		hook.ConsecutiveFailures = 0
	}

	headers := storedHeaders
	if body.Headers != nil {
		if headers, err = s.sealHeaders(hook, body.Headers); err != nil {
			s.internal(w, r, "seal webhook headers", err)
			return
		}
	}

	if err := s.store.UpdateWebhook(r.Context(), hook, headers); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.webhookNotFound(w, r)
			return
		}
		s.internal(w, r, "update webhook", err)
		return
	}
	s.getWebhook(w, r)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "webhookId", s.webhookNotFound)
	if !ok {
		return
	}
	if err := s.store.DeleteWebhook(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.webhookNotFound(w, r)
			return
		}
		s.internal(w, r, "delete webhook", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "webhookId", s.webhookNotFound)
	if !ok {
		return
	}
	if _, _, err := s.store.GetWebhook(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		s.webhookNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get webhook", err)
		return
	}

	var before *time.Time
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		when, err := store.DecodeTimeCursor(raw)
		if err != nil {
			writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-cursor",
				"Malformed cursor", "The cursor must be one returned by a previous page of this collection.")
			return
		}
		before = &when
	}

	outcome := r.URL.Query().Get("outcome")
	switch outcome {
	case "", model.DeliverySucceeded, model.DeliveryFailed, model.DeliveryPending:
	default:
		writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
			fmt.Sprintf("outcome %q is not one of succeeded, failed, pending", outcome))
		return
	}

	deliveries, hasMore, err := s.store.ListWebhookDeliveries(r.Context(), id, before, s.limit(r), outcome)
	if err != nil {
		s.internal(w, r, "list webhook deliveries", err)
		return
	}

	body := page[webhookDeliveryJSON]{Data: []webhookDeliveryJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, d := range deliveries {
		body.Data = append(body.Data, toWebhookDeliveryJSON(d))
	}
	if hasMore && len(deliveries) > 0 {
		next := store.EncodeTimeCursor(deliveries[len(deliveries)-1].CreatedAt)
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

// redeliverWebhook resends one logged delivery.
func (s *Server) redeliverWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "webhookId", s.webhookNotFound)
	if !ok {
		return
	}
	deliveryID, ok := s.taxonomyID(w, r, "deliveryId", s.deliveryNotFound)
	if !ok {
		return
	}

	hook, sealedHeaders, err := s.store.GetWebhook(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.webhookNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get webhook", err)
		return
	}

	delivery, err := s.store.GetWebhookDelivery(r.Context(), deliveryID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && delivery.WebhookID != id) {
		// A delivery belonging to another webhook is 404 rather than 403: the
		// caller has no business learning that the id exists elsewhere.
		s.deliveryNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get webhook delivery", err)
		return
	}

	if s.outbound == nil {
		writeProblem(w, r, s.log, http.StatusServiceUnavailable, "delivery-unavailable",
			"Delivery is not running", "This instance has no outbound webhook dispatcher.")
		return
	}

	replayed, err := s.outbound.Redeliver(r.Context(), hook, sealedHeaders, delivery)
	if err != nil {
		s.internal(w, r, "redeliver webhook", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toWebhookDeliveryJSON(replayed))
}

// buildWebhook folds the request onto a webhook.
func buildWebhook(h *model.Webhook, body webhookWrite) []ValidationItem {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	if body.Name != nil {
		if len(*body.Name) > 200 {
			bad("/name", "too_long", "name must be at most 200 characters")
		} else {
			h.Name = *body.Name
		}
	}
	if body.URL != nil {
		parsed, err := url.ParseRequestURI(*body.URL)
		switch {
		case err != nil, parsed.Scheme != "http" && parsed.Scheme != "https":
			bad("/url", "invalid", "url must be an absolute http or https URL")
		default:
			h.URL = *body.URL
		}
	}
	if body.Events != nil {
		if len(*body.Events) == 0 {
			bad("/events", "required", "events must name at least one event type")
		}
		for i, event := range *body.Events {
			if !model.ValidEventType(event) {
				bad(fmt.Sprintf("/events/%d", i), "invalid",
					fmt.Sprintf("%q is not an event type the spec defines", event))
			}
		}
		h.Events = *body.Events
	}
	if body.Enabled != nil {
		h.Enabled = *body.Enabled
	}
	if body.VerifyTLS != nil {
		h.VerifyTLS = *body.VerifyTLS
	}
	for name := range body.Headers {
		switch {
		case name == "":
			bad("/headers", "invalid", "a header name must not be empty")
		case len(name) > 200:
			bad("/headers", "too_long", fmt.Sprintf("header name %q is too long", name))
		}
	}
	return problems
}

// sealHeaders encrypts the header map. Encrypted rather than stored in the
// clear because the expected use is an Authorization header for the receiver —
// putting a credential here is the normal case, not a misuse.
func (s *Server) sealHeaders(h model.Webhook, headers map[string]string) ([]byte, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(headers)
	if err != nil {
		return nil, err
	}
	return s.webhooks.Seal(h.OrgID[:], h.ID[:], encoded)
}

func (s *Server) webhookNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "webhook-not-found",
		"Webhook not found", "No webhook with that identifier exists.")
}

func (s *Server) deliveryNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "delivery-not-found",
		"Delivery not found", "No delivery with that identifier exists for this webhook.")
}
