package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

type apiKeyJSON struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  *string    `json:"created_by"`

	// Key is the plaintext, present in the creation response and nowhere else.
	// The server stores a hash and genuinely cannot show it again.
	Key string `json:"key,omitempty"`
}

type apiKeyWrite struct {
	Name      *string    `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func toAPIKeyJSON(k model.APIKey) apiKeyJSON {
	out := apiKeyJSON{
		ID:         k.ID.String(),
		Name:       k.Name,
		Prefix:     k.Prefix,
		Scopes:     k.Scopes,
		ExpiresAt:  k.ExpiresAt,
		LastUsedAt: k.LastUsedAt,
		RevokedAt:  k.RevokedAt,
		CreatedAt:  k.CreatedAt,
	}
	if k.CreatedBy != nil {
		id := k.CreatedBy.String()
		out.CreatedBy = &id
	}
	return out
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
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

	keys, hasMore, err := s.store.ListAPIKeys(r.Context(), after, s.limit(r))
	if err != nil {
		s.internal(w, r, "list api keys", err)
		return
	}

	body := page[apiKeyJSON]{Data: []apiKeyJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, k := range keys {
		body.Data = append(body.Data, toAPIKeyJSON(k))
	}
	if hasMore && len(keys) > 0 {
		last := keys[len(keys)-1]
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

// createAPIKey mints a key. The plaintext is returned exactly once.
func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var body apiKeyWrite
	if !decodeJSON(w, r, s.log, &body) {
		return
	}

	caller, _ := principalFrom(r.Context())

	var problems []ValidationItem
	if body.Name == nil || *body.Name == "" {
		problems = append(problems, ValidationItem{Pointer: "/name", Code: "required", Message: "name is required"})
	} else if len(*body.Name) > 200 {
		problems = append(problems, ValidationItem{Pointer: "/name", Code: "too_long", Message: "name must be at most 200 characters"})
	}

	scopes := make(auth.Set, 0, len(body.Scopes))
	if len(body.Scopes) == 0 {
		problems = append(problems, ValidationItem{Pointer: "/scopes", Code: "required", Message: "at least one scope is required"})
	}
	for i, raw := range body.Scopes {
		scope := auth.Scope(raw)
		if !scope.Valid() {
			problems = append(problems, ValidationItem{
				Pointer: "/scopes", Code: "invalid",
				Message: "unknown scope " + raw + " at position " + strconv.Itoa(i),
			})
			continue
		}
		scopes = append(scopes, scope)
	}

	// A key may not be granted a scope its creator does not hold. Without this,
	// the weakest key in an install can mint the strongest one, and every scope
	// boundary below it becomes decorative.
	if caller != nil && len(problems) == 0 && !caller.Scopes.Covers(scopes) {
		writeProblem(w, r, s.log, http.StatusForbidden, "insufficient-scope",
			"Insufficient scope",
			"A key cannot be granted a scope its creator does not hold.")
		return
	}
	if body.ExpiresAt != nil && !body.ExpiresAt.After(time.Now()) {
		problems = append(problems, ValidationItem{Pointer: "/expires_at", Code: "in_the_past", Message: "expires_at must be in the future"})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The key was not created.", problems...)
		return
	}

	plaintext, prefix, err := auth.NewAPIKey()
	if err != nil {
		s.internal(w, r, "generate api key", err)
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	key := model.APIKey{
		ID:        model.NewID(),
		OrgID:     s.orgID,
		Name:      *body.Name,
		Prefix:    prefix,
		KeyHash:   auth.HashToken(plaintext),
		Scopes:    scopes.Strings(),
		ExpiresAt: body.ExpiresAt,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if caller != nil && caller.User != nil {
		id := caller.User.ID
		key.CreatedBy = &id
	}

	if err := s.store.CreateAPIKey(r.Context(), key); err != nil {
		s.internal(w, r, "create api key", err)
		return
	}

	s.log.Info("api key created", "id", key.ID.String(), "prefix", prefix, "scopes", len(key.Scopes))

	out := toAPIKeyJSON(key)
	out.Key = plaintext
	w.Header().Set("Location", "/api/v1/api-keys/"+key.ID.String())
	writeJSON(w, s.log, http.StatusCreated, out)
}

func (s *Server) getAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiKeyID(w, r)
	if !ok {
		return
	}

	key, err := s.store.GetAPIKey(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.keyNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get api key", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toAPIKeyJSON(key))
}

// updateAPIKey changes name, scopes, and expiry. The key material may not
// change: rotating a secret in place would silently unauthenticate every holder
// of the old value.
func (s *Server) updateAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiKeyID(w, r)
	if !ok {
		return
	}

	var body apiKeyWrite
	if !decodeJSON(w, r, s.log, &body) {
		return
	}

	key, err := s.store.GetAPIKey(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.keyNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get api key", err)
		return
	}

	caller, _ := principalFrom(r.Context())
	if body.Name != nil {
		key.Name = *body.Name
	}
	if body.Scopes != nil {
		scopes := make(auth.Set, 0, len(body.Scopes))
		for _, raw := range body.Scopes {
			scope := auth.Scope(raw)
			if !scope.Valid() {
				writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
					"Validation failed", "The key was not updated.",
					ValidationItem{Pointer: "/scopes", Code: "invalid", Message: "unknown scope " + raw})
				return
			}
			scopes = append(scopes, scope)
		}
		if caller != nil && !caller.Scopes.Covers(scopes) {
			writeProblem(w, r, s.log, http.StatusForbidden, "insufficient-scope",
				"Insufficient scope", "A key cannot be granted a scope its editor does not hold.")
			return
		}
		key.Scopes = scopes.Strings()
	}
	if body.ExpiresAt != nil {
		key.ExpiresAt = body.ExpiresAt
	}
	key.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)

	if err := s.store.UpdateAPIKey(r.Context(), key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.keyNotFound(w, r)
			return
		}
		s.internal(w, r, "update api key", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toAPIKeyJSON(key))
}

// revokeAPIKey takes effect immediately: authentication checks revoked_at on
// every request, so an in-flight caller is refused on its next call rather than
// at the end of some cache window.
func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.apiKeyID(w, r)
	if !ok {
		return
	}

	if err := s.store.RevokeAPIKey(r.Context(), id, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.keyNotFound(w, r)
			return
		}
		s.internal(w, r, "revoke api key", err)
		return
	}

	s.log.Info("api key revoked", "id", id.String())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiKeyID(w http.ResponseWriter, r *http.Request) (model.ID, bool) {
	id, ok := model.ParseID(r.PathValue("apiKeyId"))
	if !ok {
		s.keyNotFound(w, r)
		return model.ID{}, false
	}
	return id, true
}

func (s *Server) keyNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "api-key-not-found",
		"API key not found", "No API key with that identifier exists.")
}
