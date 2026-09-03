package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Groups and tags.
//
// Two things here are worth more than the CRUD around them. A group reports the
// worst status among its monitors *including its children's*, because a parent
// group showing green during an outage in the child underneath it is the single
// worst thing a monitoring tool can do. And a tag's slug is derived rather than
// supplied, because two tags that render identically in a list are two tags
// nobody can tell apart.

const maxTaxonomyBody = 1 << 14

// hexColour is the spec's pattern, compiled once.
var hexColour = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}

	groups, hasMore, err := s.store.ListGroups(r.Context(), after, s.limit(r), r.URL.Query().Get("search"))
	if err != nil {
		s.internal(w, r, "list groups", err)
		return
	}

	body := page[groupJSON]{Data: []groupJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, g := range groups {
		body.Data = append(body.Data, toGroupJSON(g))
	}
	if hasMore && len(groups) > 0 {
		last := groups[len(groups)-1].Group
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	var body groupWrite
	if !s.readBody(w, r, maxTaxonomyBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	group := model.Group{ID: model.NewID(), OrgID: s.orgID, CreatedAt: now, UpdatedAt: now}

	if problems := s.buildGroup(r.Context(), &group, body); len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The group was not created.", problems...)
		return
	}
	if err := s.store.CreateGroup(r.Context(), group); err != nil {
		s.internal(w, r, "create group", err)
		return
	}
	s.writeGroup(w, r, group.ID, http.StatusCreated)
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "groupId", s.groupNotFound)
	if !ok {
		return
	}
	s.writeGroup(w, r, id, http.StatusOK)
}

func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "groupId", s.groupNotFound)
	if !ok {
		return
	}
	existing, err := s.store.GetGroup(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.groupNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get group", err)
		return
	}

	var body groupWrite
	if !s.readBody(w, r, maxTaxonomyBody, &body) {
		return
	}

	group := existing.Group
	group.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if problems := s.buildGroup(r.Context(), &group, body); len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The group was not updated.", problems...)
		return
	}
	if err := s.store.UpdateGroup(r.Context(), group); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.groupNotFound(w, r)
			return
		}
		s.internal(w, r, "update group", err)
		return
	}
	s.writeGroup(w, r, group.ID, http.StatusOK)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "groupId", s.groupNotFound)
	if !ok {
		return
	}
	if err := s.store.DeleteGroup(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.groupNotFound(w, r)
			return
		}
		s.internal(w, r, "delete group", err)
		return
	}
	// The monitors and the child groups survive, ungrouped and top-level:
	// deleting a container must never delete what it contained. The assignment
	// change is a monitor change, so the probe is told.
	s.notify.Notify()
	w.WriteHeader(http.StatusNoContent)
}

// buildGroup validates the request onto a group.
func (s *Server) buildGroup(ctx context.Context, group *model.Group, body groupWrite) []ValidationItem {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	switch {
	case body.Name != nil && strings.TrimSpace(*body.Name) != "":
		if len(*body.Name) > 200 {
			bad("/name", "too_long", "name must be at most 200 characters")
		} else {
			group.Name = *body.Name
		}
	case group.Name == "":
		bad("/name", "required", "name is required")
	}
	if body.Description != nil {
		if len(*body.Description) > 2000 {
			bad("/description", "too_long", "description must be at most 2000 characters")
		} else {
			group.Description = *body.Description
		}
	}

	problems = append(problems, applySLOTarget(&group.SLOTargetPercent, body.SLOTargetPercent)...)

	if len(body.ParentGroupID) > 0 {
		problems = append(problems, s.resolveParentGroup(ctx, group, body.ParentGroupID)...)
	}
	return problems
}

// resolveParentGroup enforces the two rules that keep nesting one level deep and
// make a cycle impossible: a parent must itself have no parent, and a group that
// already has children cannot be given one.
func (s *Server) resolveParentGroup(ctx context.Context, group *model.Group, raw json.RawMessage) []ValidationItem {
	var supplied *string
	if err := json.Unmarshal(raw, &supplied); err != nil {
		return []ValidationItem{{Pointer: "/parent_group_id", Code: "invalid",
			Message: "parent_group_id must be an identifier or null"}}
	}
	if supplied == nil || *supplied == "" {
		// An explicit null promotes the group to the top level.
		group.ParentGroupID = nil
		return nil
	}

	parentID, ok := model.ParseID(*supplied)
	if !ok {
		return []ValidationItem{{Pointer: "/parent_group_id", Code: "invalid",
			Message: fmt.Sprintf("%q is not a valid identifier", *supplied)}}
	}
	if parentID == group.ID {
		return []ValidationItem{{Pointer: "/parent_group_id", Code: "cycle",
			Message: "a group cannot be its own parent"}}
	}

	parent, err := s.store.GetGroup(ctx, parentID)
	if errors.Is(err, store.ErrNotFound) {
		return []ValidationItem{{Pointer: "/parent_group_id", Code: "not_found",
			Message: fmt.Sprintf("no group %s exists", parentID)}}
	} else if err != nil {
		s.log.Error("resolve parent group", "error", err)
		return []ValidationItem{{Pointer: "/parent_group_id", Code: "unavailable",
			Message: "the parent group could not be checked"}}
	}
	if parent.Group.ParentGroupID != nil {
		return []ValidationItem{{Pointer: "/parent_group_id", Code: "too_deep",
			Message: fmt.Sprintf("%q is already nested, and groups nest one level deep in this release", parent.Group.Name)}}
	}

	hasChildren, err := s.store.GroupHasChildren(ctx, group.ID)
	if err != nil {
		s.log.Error("check group children", "error", err)
		return []ValidationItem{{Pointer: "/parent_group_id", Code: "unavailable",
			Message: "the group's children could not be checked"}}
	}
	if hasChildren {
		return []ValidationItem{{Pointer: "/parent_group_id", Code: "too_deep",
			Message: "this group has groups nested under it, so it cannot itself be nested"}}
	}

	group.ParentGroupID = &parentID
	return nil
}

func (s *Server) writeGroup(w http.ResponseWriter, r *http.Request, id model.ID, status int) {
	group, err := s.store.GetGroup(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.groupNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get group", err)
		return
	}
	if status == http.StatusCreated {
		w.Header().Set("Location", "/api/v1/groups/"+id.String())
	}
	writeJSON(w, s.log, status, toGroupJSON(group))
}

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}

	tags, hasMore, err := s.store.ListTags(r.Context(), after, s.limit(r), r.URL.Query().Get("search"))
	if err != nil {
		s.internal(w, r, "list tags", err)
		return
	}

	body := page[tagJSON]{Data: []tagJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, t := range tags {
		body.Data = append(body.Data, toTagJSON(t))
	}
	if hasMore && len(tags) > 0 {
		last := tags[len(tags)-1].Tag
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createTag(w http.ResponseWriter, r *http.Request) {
	var body tagWrite
	if !s.readBody(w, r, maxTaxonomyBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	tag := model.Tag{ID: model.NewID(), OrgID: s.orgID, Color: model.DefaultTagColor, CreatedAt: now, UpdatedAt: now}

	if problems := buildTag(&tag, body); len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The tag was not created.", problems...)
		return
	}

	if err := s.store.CreateTag(r.Context(), tag); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.slugTaken(w, r, tag.Slug)
			return
		}
		s.internal(w, r, "create tag", err)
		return
	}
	s.writeTag(w, r, tag.ID, http.StatusCreated)
}

func (s *Server) getTag(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "tagId", s.tagNotFound)
	if !ok {
		return
	}
	s.writeTag(w, r, id, http.StatusOK)
}

func (s *Server) updateTag(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "tagId", s.tagNotFound)
	if !ok {
		return
	}
	existing, err := s.store.GetTag(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.tagNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get tag", err)
		return
	}

	var body tagWrite
	if !s.readBody(w, r, maxTaxonomyBody, &body) {
		return
	}

	tag := existing.Tag
	tag.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if problems := buildTag(&tag, body); len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The tag was not updated.", problems...)
		return
	}

	if err := s.store.UpdateTag(r.Context(), tag); err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			s.slugTaken(w, r, tag.Slug)
		case errors.Is(err, store.ErrNotFound):
			s.tagNotFound(w, r)
		default:
			s.internal(w, r, "update tag", err)
		}
		return
	}
	s.writeTag(w, r, tag.ID, http.StatusOK)
}

func (s *Server) deleteTag(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "tagId", s.tagNotFound)
	if !ok {
		return
	}
	if err := s.store.DeleteTag(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.tagNotFound(w, r)
			return
		}
		s.internal(w, r, "delete tag", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// buildTag validates the request onto a tag, deriving the slug from the name.
func buildTag(tag *model.Tag, body tagWrite) []ValidationItem {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	switch {
	case body.Name != nil && strings.TrimSpace(*body.Name) != "":
		switch {
		case len(*body.Name) > 100:
			bad("/name", "too_long", "name must be at most 100 characters")
		default:
			tag.Name = *body.Name
			tag.Slug = model.Slugify(*body.Name)
			if tag.Slug == "" {
				// The slug is ASCII so it never needs percent-encoding in a URL.
				// A name that leaves nothing behind is refused rather than given
				// an identifier the user did not choose.
				bad("/name", "invalid",
					"the name must contain at least one ASCII letter or digit, because the URL-safe slug is derived from it")
			}
		}
	case tag.Name == "":
		bad("/name", "required", "name is required")
	}

	if body.Color != nil {
		if !hexColour.MatchString(*body.Color) {
			bad("/color", "invalid", fmt.Sprintf("color %q must be a hex triple such as #6b7280", *body.Color))
		} else {
			tag.Color = *body.Color
		}
	}
	if body.Description != nil {
		if len(*body.Description) > 2000 {
			bad("/description", "too_long", "description must be at most 2000 characters")
		} else {
			tag.Description = *body.Description
		}
	}
	return problems
}

func (s *Server) writeTag(w http.ResponseWriter, r *http.Request, id model.ID, status int) {
	tag, err := s.store.GetTag(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.tagNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get tag", err)
		return
	}
	if status == http.StatusCreated {
		w.Header().Set("Location", "/api/v1/tags/"+id.String())
	}
	writeJSON(w, s.log, status, toTagJSON(tag))
}

// resolveGroup validates a monitor's group_id.
func (s *Server) resolveGroup(ctx context.Context, m *model.Monitor, raw *string) []ValidationItem {
	if raw == nil || *raw == "" {
		return nil
	}
	id, ok := model.ParseID(*raw)
	if !ok {
		return []ValidationItem{{Pointer: "/group_id", Code: "invalid",
			Message: fmt.Sprintf("%q is not a valid identifier", *raw)}}
	}
	if _, err := s.store.GetGroup(ctx, id); errors.Is(err, store.ErrNotFound) {
		return []ValidationItem{{Pointer: "/group_id", Code: "not_found",
			Message: fmt.Sprintf("no group %s exists", id)}}
	} else if err != nil {
		s.log.Error("resolve group", "error", err)
		return []ValidationItem{{Pointer: "/group_id", Code: "unavailable",
			Message: "the group could not be checked"}}
	}
	m.GroupID = &id
	return nil
}

// resolveTags validates a monitor's tag_ids.
func (s *Server) resolveTags(ctx context.Context, requested *[]string) ([]model.ID, []ValidationItem) {
	if requested == nil || len(*requested) == 0 {
		return nil, nil
	}

	var (
		problems []ValidationItem
		ids      []model.ID
	)
	for i, raw := range *requested {
		id, ok := model.ParseID(raw)
		if !ok {
			problems = append(problems, ValidationItem{
				Pointer: fmt.Sprintf("/tag_ids/%d", i), Code: "invalid",
				Message: fmt.Sprintf("%q is not a valid identifier", raw)})
			continue
		}
		ids = append(ids, id)
	}
	if len(problems) > 0 {
		return nil, problems
	}

	missing, err := s.store.MissingIDs(ctx, "tags", s.orgID, ids)
	if err != nil {
		s.log.Error("check tag ids", "error", err)
		return nil, []ValidationItem{{Pointer: "/tag_ids", Code: "unavailable",
			Message: "the tags could not be checked"}}
	}
	for _, id := range missing {
		problems = append(problems, ValidationItem{Pointer: "/tag_ids", Code: "not_found",
			Message: fmt.Sprintf("no tag %s exists", id)})
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return ids, nil
}

// monitorFilter reads every filter a monitor listing accepts.
//
// Shared with the membership signal, deliberately: the two have to agree about
// what a filter means or a client polling for changes would be polling a
// different set from the one it is showing.
func (s *Server) monitorFilter(w http.ResponseWriter, r *http.Request) (store.MonitorFilter, bool) {
	var filter store.MonitorFilter
	query := r.URL.Query()

	for _, name := range []string{"group_id", "tag_id"} {
		for _, raw := range query[name] {
			id, ok := model.ParseID(raw)
			if !ok {
				writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
					fmt.Sprintf("%s %q is not a valid identifier", name, raw))
				return store.MonitorFilter{}, false
			}
			if name == "group_id" {
				filter.GroupIDs = append(filter.GroupIDs, id)
			} else {
				filter.TagIDs = append(filter.TagIDs, id)
			}
		}
	}

	// An unrecognised status or type is a 400 rather than a filter that matches
	// nothing. Silently returning an empty page for a typo is how somebody
	// concludes their monitors have been deleted.
	for _, raw := range query["status"] {
		if !validMonitorStatus(raw) {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
				fmt.Sprintf("status %q is not one of up, down, pending, paused, maintenance", raw))
			return store.MonitorFilter{}, false
		}
		filter.Statuses = append(filter.Statuses, raw)
	}
	for _, raw := range query["type"] {
		if !knownType(raw) {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
				fmt.Sprintf("type %q is not one the spec defines", raw))
			return store.MonitorFilter{}, false
		}
		filter.Types = append(filter.Types, raw)
	}

	if raw := query.Get("enabled"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
				fmt.Sprintf("enabled %q must be true or false", raw))
			return store.MonitorFilter{}, false
		}
		filter.Enabled = &enabled
	}
	filter.Search = query.Get("search")

	return filter, true
}

func validMonitorStatus(status string) bool {
	switch status {
	case model.MonitorStatusUp, model.MonitorStatusDown, model.MonitorStatusPending,
		model.MonitorStatusPaused, model.MonitorStatusMaintenance:
		return true
	}
	return false
}

func (s *Server) cursor(w http.ResponseWriter, r *http.Request) (*store.Cursor, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return nil, true
	}
	c, err := store.DecodeCursor(raw)
	if err != nil {
		writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-cursor",
			"Malformed cursor", "The cursor must be one returned by a previous page of this collection.")
		return nil, false
	}
	return &c, true
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request, limit int64, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-json",
			"Malformed request body", err.Error())
		return false
	}
	return true
}

func (s *Server) taxonomyID(w http.ResponseWriter, r *http.Request, param string, missing func(http.ResponseWriter, *http.Request)) (model.ID, bool) {
	id, ok := model.ParseID(r.PathValue(param))
	if !ok {
		missing(w, r)
		return model.ID{}, false
	}
	return id, true
}

func (s *Server) groupNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "group-not-found",
		"Group not found", "No group with that identifier exists.")
}

func (s *Server) tagNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "tag-not-found",
		"Tag not found", "No tag with that identifier exists.")
}

// slugTaken is a 409 rather than a 422: the request is well-formed and the
// problem is the current state, which the caller resolves by choosing another
// name rather than by correcting a field.
func (s *Server) slugTaken(w http.ResponseWriter, r *http.Request, slug string) {
	writeProblem(w, r, s.log, http.StatusConflict, "slug-taken", "Tag name already in use",
		fmt.Sprintf("another tag already uses the slug %q; tag names must be distinguishable once reduced to a URL-safe form", slug))
}

// TaxonomyStore is the groups-and-tags half of persistence.
type TaxonomyStore interface {
	CreateGroup(ctx context.Context, g model.Group) error
	UpdateGroup(ctx context.Context, g model.Group) error
	GetGroup(ctx context.Context, id model.ID) (model.GroupSummary, error)
	ListGroups(ctx context.Context, after *store.Cursor, limit int, search string) ([]model.GroupSummary, bool, error)
	DeleteGroup(ctx context.Context, id model.ID) error
	GroupHasChildren(ctx context.Context, id model.ID) (bool, error)

	CreateTag(ctx context.Context, t model.Tag) error
	UpdateTag(ctx context.Context, t model.Tag) error
	GetTag(ctx context.Context, id model.ID) (model.TagSummary, error)
	ListTags(ctx context.Context, after *store.Cursor, limit int, search string) ([]model.TagSummary, bool, error)
	DeleteTag(ctx context.Context, id model.ID) error

	SetMonitorTags(ctx context.Context, monitorID, orgID model.ID, tagIDs []model.ID) error
	TagIDsForMonitor(ctx context.Context, monitorID model.ID) ([]model.ID, error)
	TagIDsForMonitors(ctx context.Context, monitorIDs []model.ID) (map[model.ID][]model.ID, error)
}
