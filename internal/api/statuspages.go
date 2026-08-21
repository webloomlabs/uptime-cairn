package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Status pages: the authenticated management surface, and the unauthenticated
// read path a stranger judges the product by.
//
// The public projection is assembled from a dedicated store query rather than
// filtered down from the monitor read path. It costs a second query and it buys
// the property that matters: a field cannot leak through a shape that has no
// place to put it. This is the one endpoint in the system where a leak reaches
// people who are not customers of the operator, let alone of this project.

const maxStatusPageBody = 1 << 18

// recentIncidentWindow is how far back a public page looks for resolved
// incidents. Long enough that yesterday's outage is still visible to somebody
// arriving late, short enough that a page is not a year of history.
const recentIncidentWindow = 14 * 24 * time.Hour

// maxPublicIncidents bounds what one page render will read. A page with more
// open incidents than this has bigger problems than pagination.
const maxPublicIncidents = 50

// StatusPageStore is the status pages half of persistence.
type StatusPageStore interface {
	CreateStatusPage(ctx context.Context, p model.StatusPage) error
	UpdateStatusPage(ctx context.Context, p model.StatusPage) error
	GetStatusPage(ctx context.Context, id model.ID) (model.StatusPage, error)
	StatusPageBySlug(ctx context.Context, slug string) (model.StatusPage, error)
	ListStatusPages(ctx context.Context, after *store.Cursor, limit int, filter store.StatusPageFilter) ([]model.StatusPage, bool, error)
	DeleteStatusPage(ctx context.Context, id model.ID) error

	MonitorsOnStatusPage(ctx context.Context, pageID model.ID) (map[model.ID]store.PublicMonitor, error)

	// CustomDomains is the hostname-to-slug map for custom-domain pages, read
	// on document requests and cached. Published pages only: a draft answering
	// on a customer's hostname is the one thing an operator must not get by
	// accident.
	CustomDomains(ctx context.Context) (map[string]string, error)

	CreateSubscriber(ctx context.Context, sub model.Subscriber) error
	ListSubscribers(ctx context.Context, pageID model.ID, limit int) ([]model.Subscriber, error)
	SubscriberByToken(ctx context.Context, confirmHash, unsubscribeHash []byte) (model.Subscriber, error)
	ConfirmSubscriber(ctx context.Context, id model.ID, at time.Time) error
	DeleteSubscriber(ctx context.Context, id model.ID) error
}

func (s *Server) listStatusPages(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}

	filter := store.StatusPageFilter{Search: r.URL.Query().Get("search")}
	if raw := r.URL.Query().Get("published"); raw != "" {
		published, ok := parseBool(raw)
		if !ok {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-filter", "Invalid filter",
				fmt.Sprintf("published %q must be true or false", raw))
			return
		}
		filter.Published = &published
	}

	pages, hasMore, err := s.store.ListStatusPages(r.Context(), after, s.limit(r), filter)
	if err != nil {
		s.internal(w, r, "list status pages", err)
		return
	}

	body := page[statusPageJSON]{Data: []statusPageJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, p := range pages {
		body.Data = append(body.Data, toStatusPageJSON(p))
	}
	if hasMore && len(pages) > 0 {
		last := pages[len(pages)-1]
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createStatusPage(w http.ResponseWriter, r *http.Request) {
	var body statusPageWrite
	if !s.readBody(w, r, maxStatusPageBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	page := model.StatusPage{
		ID:                   model.NewID(),
		OrgID:                s.orgID,
		Visibility:           model.VisibilityPublic,
		Theme:                "auto",
		Timezone:             "UTC",
		ShowUptimePercentage: true,
		UptimeBarDays:        90,
		ShowPoweredBy:        true,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	problems := s.buildStatusPage(r.Context(), &page, body)
	if body.Slug == nil {
		problems = append(problems, ValidationItem{Pointer: "/slug", Code: "required",
			Message: "slug is required"})
	}
	if body.Title == nil {
		problems = append(problems, ValidationItem{Pointer: "/title", Code: "required",
			Message: "title is required"})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The status page was not created.", problems...)
		return
	}

	// The custom-domain cache is dropped on every status page write rather than
	// left to expire. The one moment somebody is watching for a custom domain to
	// start working is the moment after they saved it.
	defer s.domains.invalidate()

	if err := s.store.CreateStatusPage(r.Context(), page); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.pageSlugTaken(w, r, page)
			return
		}
		s.internal(w, r, "create status page", err)
		return
	}
	s.writeStatusPage(w, r, page.ID, http.StatusCreated)
}

func (s *Server) getStatusPage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "statusPageId", s.statusPageNotFound)
	if !ok {
		return
	}
	s.writeStatusPage(w, r, id, http.StatusOK)
}

func (s *Server) updateStatusPage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "statusPageId", s.statusPageNotFound)
	if !ok {
		return
	}

	page, err := s.store.GetStatusPage(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.statusPageNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get status page", err)
		return
	}

	var body statusPageWrite
	if !s.readBody(w, r, maxStatusPageBody, &body) {
		return
	}

	page.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if problems := s.buildStatusPage(r.Context(), &page, body); len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The status page was not updated.", problems...)
		return
	}

	defer s.domains.invalidate()

	if err := s.store.UpdateStatusPage(r.Context(), page); err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			s.pageSlugTaken(w, r, page)
		case errors.Is(err, store.ErrNotFound):
			s.statusPageNotFound(w, r)
		default:
			s.internal(w, r, "update status page", err)
		}
		return
	}
	s.writeStatusPage(w, r, id, http.StatusOK)
}

func (s *Server) deleteStatusPage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "statusPageId", s.statusPageNotFound)
	if !ok {
		return
	}
	defer s.domains.invalidate()

	if err := s.store.DeleteStatusPage(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.statusPageNotFound(w, r)
			return
		}
		s.internal(w, r, "delete status page", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// buildStatusPage folds the request onto a page.
func (s *Server) buildStatusPage(ctx context.Context, p *model.StatusPage, body statusPageWrite) []ValidationItem {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	if body.Slug != nil {
		if !model.ValidSlug(*body.Slug) {
			bad("/slug", "invalid",
				"slug must be 1-64 characters of lower-case letters, digits, and interior hyphens — it is a public URL path segment")
		} else {
			p.Slug = *body.Slug
		}
	}
	if body.Title != nil {
		switch {
		case *body.Title == "":
			bad("/title", "required", "title must not be empty")
		case len(*body.Title) > 200:
			bad("/title", "too_long", "title must be at most 200 characters")
		default:
			p.Title = *body.Title
		}
	}
	if body.Description != nil {
		p.Description = *body.Description
	}
	if body.Published != nil {
		p.Published = *body.Published
	}
	if len(body.CustomDomain) > 0 {
		var domain *string
		if err := json.Unmarshal(body.CustomDomain, &domain); err != nil {
			bad("/custom_domain", "invalid", "custom_domain must be a hostname or null")
		} else if domain == nil {
			p.CustomDomain = ""
		} else {
			p.CustomDomain = strings.ToLower(strings.TrimSpace(*domain))
		}
	}

	if body.Visibility != nil {
		switch *body.Visibility {
		case model.VisibilityPublic, model.VisibilityPassword:
			p.Visibility = *body.Visibility
		default:
			bad("/visibility", "invalid", "visibility must be public or password")
		}
	}
	if len(body.Password) > 0 {
		problems = append(problems, s.applyPagePassword(p, body.Password)...)
	}
	// Checked after both, so setting visibility and password in one call works
	// and setting only the visibility is refused with the reason.
	if p.Visibility == model.VisibilityPassword && p.PasswordHash == "" {
		bad("/password", "required", "a password is required when visibility is password")
	}

	if body.Theme != nil {
		switch *body.Theme {
		case "light", "dark", "auto":
			p.Theme = *body.Theme
		default:
			bad("/theme", "invalid", "theme must be light, dark, or auto")
		}
	}
	for _, field := range []struct {
		pointer string
		value   *string
		into    *string
	}{
		{"/logo_url", body.LogoURL, &p.LogoURL},
		{"/favicon_url", body.FaviconURL, &p.FaviconURL},
	} {
		if field.value == nil {
			continue
		}
		if *field.value != "" {
			if _, err := url.ParseRequestURI(*field.value); err != nil {
				bad(field.pointer, "invalid", "must be an absolute URL")
				continue
			}
		}
		*field.into = *field.value
	}
	if body.PrimaryColor != nil {
		if *body.PrimaryColor != "" && !hexColour.MatchString(*body.PrimaryColor) {
			bad("/primary_color", "invalid", "primary_color must be a hex triple such as #6b7280")
		} else {
			p.PrimaryColor = *body.PrimaryColor
		}
	}
	if body.FooterText != nil {
		if len(*body.FooterText) > 2000 {
			bad("/footer_text", "too_long", "footer_text must be at most 2000 characters")
		} else {
			p.FooterText = *body.FooterText
		}
	}
	if body.CustomCSS != nil {
		switch {
		case len(*body.CustomCSS) > 50000:
			bad("/custom_css", "too_long", "custom_css must be at most 50000 characters")
		case containsScript(*body.CustomCSS):
			// Refused rather than sanitised. Stripping tags from CSS is a game
			// of whack-a-mole against an attacker who only has to win once, and
			// the page this lands on is served to the operator's customers.
			bad("/custom_css", "invalid",
				"custom_css must not contain markup or javascript: URLs — it is injected into a page served to your visitors")
		default:
			p.CustomCSS = *body.CustomCSS
		}
	}
	if body.Timezone != nil {
		if *body.Timezone != "" {
			if _, err := time.LoadLocation(*body.Timezone); err != nil {
				bad("/timezone", "invalid", fmt.Sprintf("timezone %q is not an IANA zone name", *body.Timezone))
			} else {
				p.Timezone = *body.Timezone
			}
		} else {
			p.Timezone = "UTC"
		}
	}

	if body.ShowUptimePercentage != nil {
		p.ShowUptimePercentage = *body.ShowUptimePercentage
	}
	if body.ShowResponseTimeChart != nil {
		p.ShowResponseTimeChart = *body.ShowResponseTimeChart
	}
	if body.UptimeBarDays != nil {
		switch v := *body.UptimeBarDays; {
		case v < 7:
			bad("/uptime_bar_days", "below_minimum", "uptime_bar_days must be at least 7")
		case v > 365:
			bad("/uptime_bar_days", "above_maximum", "uptime_bar_days must be at most 365")
		default:
			p.UptimeBarDays = v
		}
	}
	if body.ShowPoweredBy != nil {
		p.ShowPoweredBy = *body.ShowPoweredBy
	}
	if body.SubscriptionsEnabled != nil {
		p.SubscriptionsEnabled = *body.SubscriptionsEnabled
	}
	if body.GoogleAnalyticsID != nil {
		p.GoogleAnalyticsID = *body.GoogleAnalyticsID
	}

	if body.Sections != nil {
		sections, sectionProblems := s.buildSections(ctx, *body.Sections)
		problems = append(problems, sectionProblems...)
		p.Sections = sections
	}
	return problems
}

// buildSections validates the ordered groupings, including the rule the schema
// enforces and the caller would rather hear about here: a monitor appears in at
// most one section per page.
func (s *Server) buildSections(ctx context.Context, requested []statusPageSectionJSON) ([]model.StatusPageSection, []ValidationItem) {
	var (
		sections []model.StatusPageSection
		problems []ValidationItem
		seen     = map[model.ID]int{}
		allIDs   []model.ID
	)

	for i, section := range requested {
		pointer := fmt.Sprintf("/sections/%d", i)
		if section.Name == "" {
			problems = append(problems, ValidationItem{Pointer: pointer + "/name", Code: "required",
				Message: "name is required"})
		}

		built := model.StatusPageSection{
			Name:        section.Name,
			Description: derefOr(section.Description),
			Position:    i,
		}
		for j, raw := range section.MonitorIDs {
			id, ok := model.ParseID(raw)
			if !ok {
				problems = append(problems, ValidationItem{
					Pointer: fmt.Sprintf("%s/monitor_ids/%d", pointer, j), Code: "invalid",
					Message: fmt.Sprintf("%q is not a valid identifier", raw)})
				continue
			}
			if previous, duplicate := seen[id]; duplicate {
				problems = append(problems, ValidationItem{
					Pointer: fmt.Sprintf("%s/monitor_ids/%d", pointer, j), Code: "duplicate",
					Message: fmt.Sprintf("monitor %s is already in section %d; a monitor appears in at most one section per page", id, previous)})
				continue
			}
			seen[id] = i
			built.MonitorIDs = append(built.MonitorIDs, id)
			allIDs = append(allIDs, id)
		}
		sections = append(sections, built)
	}

	if len(problems) == 0 && len(allIDs) > 0 {
		missing, err := s.store.MissingIDs(ctx, "monitors", s.orgID, allIDs)
		if err != nil {
			return nil, []ValidationItem{{Pointer: "/sections", Code: "unavailable",
				Message: "the referenced monitors could not be checked"}}
		}
		for _, id := range missing {
			problems = append(problems, ValidationItem{Pointer: "/sections", Code: "not_found",
				Message: fmt.Sprintf("no monitor %s exists", id)})
		}
	}
	return sections, problems
}

// applyPagePassword hashes a page password, or clears it on null.
func (s *Server) applyPagePassword(p *model.StatusPage, raw []byte) []ValidationItem {
	var supplied *string
	if err := json.Unmarshal(raw, &supplied); err != nil {
		return []ValidationItem{{Pointer: "/password", Code: "invalid",
			Message: "password must be a string or null"}}
	}
	if supplied == nil || *supplied == "" {
		p.PasswordHash = ""
		return nil
	}
	if len(*supplied) < 8 {
		return []ValidationItem{{Pointer: "/password", Code: "too_short",
			Message: "password must be at least 8 characters"}}
	}

	// Hashed with the same argon2id the login path uses. It is verified against
	// and never replayed, which is the rule that decides hashing from encryption
	// (data model §12.1).
	hash, err := auth.HashPassword(*supplied)
	if err != nil {
		s.log.Error("hash status page password", "error", err)
		return []ValidationItem{{Pointer: "/password", Code: "unavailable",
			Message: "the password could not be stored"}}
	}
	p.PasswordHash = hash
	return nil
}

// containsScript is a deliberately blunt refusal rather than a sanitiser.
func containsScript(css string) bool {
	lowered := strings.ToLower(css)
	for _, needle := range []string{"<script", "</style", "javascript:", "expression(", "@import"} {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

func derefOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Server) writeStatusPage(w http.ResponseWriter, r *http.Request, id model.ID, status int) {
	page, err := s.store.GetStatusPage(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.statusPageNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get status page", err)
		return
	}
	if status == http.StatusCreated {
		w.Header().Set("Location", "/api/v1/status-pages/"+id.String())
	}
	writeJSON(w, s.log, status, toStatusPageJSON(page))
}

func (s *Server) statusPageNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "status-page-not-found",
		"Status page not found", "No status page with that identifier exists.")
}

func (s *Server) pageSlugTaken(w http.ResponseWriter, r *http.Request, p model.StatusPage) {
	detail := fmt.Sprintf("another status page already uses the slug %q", p.Slug)
	if p.CustomDomain != "" {
		detail += fmt.Sprintf(", or the domain %q is already serving a page — a hostname serves exactly one", p.CustomDomain)
	}
	writeProblem(w, r, s.log, http.StatusConflict, "slug-taken", "Status page address already in use", detail)
}

// --------------------------------------------------------------------------
// Subscribers
// --------------------------------------------------------------------------

func (s *Server) listStatusPageSubscribers(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "statusPageId", s.statusPageNotFound)
	if !ok {
		return
	}
	if _, err := s.store.GetStatusPage(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		s.statusPageNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get status page", err)
		return
	}

	subscribers, err := s.store.ListSubscribers(r.Context(), id, s.limit(r))
	if err != nil {
		s.internal(w, r, "list subscribers", err)
		return
	}

	data := make([]subscriberJSON, 0, len(subscribers))
	for _, sub := range subscribers {
		// Masked even for an authenticated operator. This list is an export of
		// somebody else's customers, and the reason to open it is "did my
		// subscriber confirm", which a mask answers.
		target := "(unreadable)"
		if plain, err := s.subscriberTarget(sub); err == nil {
			target = model.MaskTarget(sub.Channel, plain)
		} else {
			s.log.Error("open subscriber target", "error", err, "subscriber", sub.ID.String())
		}
		data = append(data, subscriberJSON{
			ID:          sub.ID.String(),
			Channel:     sub.Channel,
			Target:      target,
			Confirmed:   sub.Confirmed(),
			ConfirmedAt: sub.ConfirmedAt,
			CreatedAt:   sub.CreatedAt,
		})
	}
	writeJSON(w, s.log, http.StatusOK, map[string]any{"data": data})
}

func (s *Server) deleteStatusPageSubscriber(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.taxonomyID(w, r, "statusPageId", s.statusPageNotFound); !ok {
		return
	}
	id, ok := s.taxonomyID(w, r, "subscriberId", s.subscriberNotFound)
	if !ok {
		return
	}
	if err := s.store.DeleteSubscriber(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.subscriberNotFound(w, r)
			return
		}
		s.internal(w, r, "delete subscriber", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) subscriberNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "subscriber-not-found",
		"Subscriber not found", "No subscriber with that identifier exists.")
}

// subscriberTarget opens the encrypted address. Encrypted rather than hashed
// because a notification replays it; bound by AAD to its row, so relocating the
// blob onto another subscriber fails to open.
func (s *Server) subscriberTarget(sub model.Subscriber) (string, error) {
	plain, err := s.subscribers.Open(sub.OrgID[:], sub.ID[:], sub.SealedTarget)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// --------------------------------------------------------------------------
// The public read path
// --------------------------------------------------------------------------

// getPublicStatusPage renders a page for a visitor.
//
// Unauthenticated by design and unpublished pages are 404, not 403: an operator
// building a page before launch should not have its existence confirmed by the
// error code.
func (s *Server) getPublicStatusPage(w http.ResponseWriter, r *http.Request) {
	page, ok := s.publicPage(w, r)
	if !ok {
		return
	}

	if page.Visibility == model.VisibilityPassword && !s.pageUnlocked(r, page) {
		writeProblem(w, r, s.log, http.StatusUnauthorized, "password-required",
			"Password required", "This status page is password protected.")
		return
	}

	rendered, err := s.renderPublicPage(r.Context(), page)
	if err != nil {
		s.internal(w, r, "render public status page", err)
		return
	}

	// A short cache. The page is the endpoint that gets hit hardest exactly when
	// the instance is least healthy, and thirty seconds of staleness is a
	// trade every status page makes.
	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, s.log, http.StatusOK, rendered)
}

// renderPublicPage assembles the visitor-facing view.
func (s *Server) renderPublicPage(ctx context.Context, page model.StatusPage) (publicStatusPage, error) {
	monitors, err := s.store.MonitorsOnStatusPage(ctx, page.ID)
	if err != nil {
		return publicStatusPage{}, err
	}

	ids := make([]model.ID, 0, len(monitors))
	for id := range monitors {
		ids = append(ids, id)
	}

	var (
		ratios map[model.ID]float64
		bars   map[model.ID][]store.DailyUptime
	)
	if page.ShowUptimePercentage && len(ids) > 0 {
		if ratios, err = s.store.UptimeRatios(ctx, ids, "90d"); err != nil {
			return publicStatusPage{}, err
		}
		to := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		from := to.AddDate(0, 0, -page.UptimeBarDays)
		if bars, err = s.store.DailyUptime(ctx, ids, from, to); err != nil {
			return publicStatusPage{}, err
		}
	}

	out := publicStatusPage{
		Slug:                 page.Slug,
		Title:                page.Title,
		Description:          optional(page.Description),
		Theme:                page.Theme,
		LogoURL:              optional(page.LogoURL),
		FaviconURL:           optional(page.FaviconURL),
		PrimaryColor:         optional(page.PrimaryColor),
		FooterText:           optional(page.FooterText),
		CustomCSS:            optional(page.CustomCSS),
		ShowPoweredBy:        page.ShowPoweredBy,
		SubscriptionsEnabled: page.SubscriptionsEnabled,
		Sections:             []publicSection{},
		ActiveIncidents:      []publicIncident{},
		RecentIncidents:      []publicIncident{},
		ScheduledMaintenance: []publicMaintenanceWindow{},
		GeneratedAt:          time.Now().UTC(),
	}
	if out.Theme == "" {
		out.Theme = "auto"
	}

	var statuses []string
	for _, section := range page.Sections {
		rendered := publicSection{
			Name:        section.Name,
			Description: optional(section.Description),
			Monitors:    []publicMonitorRecord{},
		}
		for _, id := range section.MonitorIDs {
			monitor, ok := monitors[id]
			if !ok {
				// Deleted since the page was configured. Skipped rather than
				// rendered as unknown: a visitor should not be shown a row for
				// something that no longer exists.
				continue
			}
			statuses = append(statuses, monitor.Status)
			rendered.Monitors = append(rendered.Monitors, publicMonitorRecord{
				ID:               monitor.ID.String(),
				Name:             monitor.Name,
				Description:      optional(monitor.Description),
				Status:           monitor.Status,
				UptimePercentage: percentage(ratios, monitor.ID),
				UptimeBar:        toBar(bars[monitor.ID]),
				ResponseTimeMs:   monitor.ResponseTimeMs,
			})
		}
		out.Sections = append(out.Sections, rendered)
	}
	out.OverallStatus = model.OverallStatus(statuses)

	incidents, err := s.store.IncidentsForStatusPage(ctx, page.ID,
		time.Now().UTC().Add(-recentIncidentWindow), maxPublicIncidents)
	if err != nil {
		return publicStatusPage{}, err
	}
	for _, incident := range incidents {
		rendered := toPublicIncident(incident)
		if incident.ResolvedAt == nil {
			out.ActiveIncidents = append(out.ActiveIncidents, rendered)
		} else {
			out.RecentIncidents = append(out.RecentIncidents, rendered)
		}
	}

	windows, err := s.store.WindowsForStatusPage(ctx, page.ID)
	if err != nil {
		return publicStatusPage{}, err
	}
	for _, window := range windows {
		// next_occurrence_at rather than starts_at: for a recurring window
		// starts_at is the anchor the rule is derived from, which may be months
		// in the past, and a visitor wants the next time the lights go out.
		starts := window.StartsAt
		if window.NextOccurrenceAt != nil {
			starts = *window.NextOccurrenceAt
		}
		ends := window.EndsAt
		if ends == nil && window.Duration > 0 {
			finish := starts.Add(window.Duration)
			ends = &finish
		}
		out.ScheduledMaintenance = append(out.ScheduledMaintenance, publicMaintenanceWindow{
			Title:              window.Title,
			Description:        optional(window.Description),
			StartsAt:           starts,
			EndsAt:             ends,
			AffectedMonitorIDs: idStrings(window.Targets.MonitorIDs),
		})
	}
	return out, nil
}

func percentage(ratios map[model.ID]float64, id model.ID) *float64 {
	ratio, ok := ratios[id]
	if !ok {
		return nil
	}
	// Rendered as a percentage because the field is named one. Two decimals:
	// 99.99 and 99.999 are different promises and one of them is not in a
	// hundred-day bar.
	value := ratio * 100
	return &value
}

// toBar renders the uptime stones. A day with no data carries a null ratio
// rather than being reported as downtime — the single most common way a status
// page lies.
func toBar(days []store.DailyUptime) []publicBarEntry {
	if len(days) == 0 {
		return nil
	}
	out := make([]publicBarEntry, 0, len(days))
	for _, day := range days {
		entry := publicBarEntry{Date: day.Date.Format(time.DateOnly), UptimeRatio: day.Ratio}
		if day.Ratio != nil {
			status := "up"
			switch {
			case *day.Ratio < 0.99:
				status = "down"
			case *day.Ratio < 1:
				status = "degraded"
			}
			entry.Status = &status
		}
		out = append(out, entry)
	}
	return out
}

func toPublicIncident(in model.Incident) publicIncident {
	out := publicIncident{
		ID:                 in.ID.String(),
		Title:              in.Title,
		State:              in.State,
		Impact:             in.Impact,
		StartedAt:          in.StartedAt,
		ResolvedAt:         in.ResolvedAt,
		AffectedMonitorIDs: idStrings(in.MonitorIDs),
		Updates:            []publicIncidentUpdate{},
	}
	for _, u := range in.Updates {
		entry := publicIncidentUpdate{Body: u.Body, CreatedAt: u.CreatedAt}
		if u.State != "" {
			state := u.State
			entry.State = &state
		}
		out.Updates = append(out.Updates, entry)
	}
	return out
}

// publicPage resolves the slug, refusing anything a visitor should not see.
func (s *Server) publicPage(w http.ResponseWriter, r *http.Request) (model.StatusPage, bool) {
	page, err := s.store.StatusPageBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !page.Published) {
		// One answer for "no such page" and "not published yet", so the endpoint
		// says nothing about which pages exist.
		writeProblem(w, r, s.log, http.StatusNotFound, "not-found",
			"Not found", "No published status page is served at that address.")
		return model.StatusPage{}, false
	}
	if err != nil {
		s.internal(w, r, "get public status page", err)
		return model.StatusPage{}, false
	}
	return page, true
}

// authenticatePublicStatusPage exchanges a page password for the cookie the read
// path checks.
func (s *Server) authenticatePublicStatusPage(w http.ResponseWriter, r *http.Request) {
	page, ok := s.publicPage(w, r)
	if !ok {
		return
	}
	if page.Visibility != model.VisibilityPassword {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "not-protected",
			"Page is not protected", "This status page does not require a password.")
		return
	}

	var body pageAuthRequest
	if !s.readBody(w, r, 1<<12, &body) {
		return
	}

	// Rate-limited on the page slug, reusing the login limiter: this endpoint is
	// unauthenticated and a page password is short, which is the exact shape of
	// thing that gets guessed.
	if !s.limiter.allow("status-page:"+page.Slug, time.Now()) {
		writeProblem(w, r, s.log, http.StatusTooManyRequests, "rate-limited",
			"Too many attempts", "Too many password attempts for this page. Try again shortly.")
		return
	}

	correct := false
	if body.Password != nil {
		ok, err := auth.VerifyPassword(*body.Password, page.PasswordHash)
		if err != nil {
			s.log.Error("verify status page password", "error", err, "page", page.Slug)
		}
		correct = ok
	}
	if !correct {
		writeProblem(w, r, s.log, http.StatusUnauthorized, "invalid-password",
			"Incorrect password", "That password does not unlock this page.")
		return
	}
	s.limiter.succeed("status-page:" + page.Slug)

	http.SetCookie(w, &http.Cookie{
		Name:     pageCookieName(page.Slug),
		Value:    pageUnlockToken(page),
		Path:     "/api/v1/public/status-pages/" + page.Slug,
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((12 * time.Hour).Seconds()),
	})
	writeJSON(w, s.log, http.StatusOK, map[string]bool{"ok": true})
}

// pageUnlocked reports whether the request carries a valid unlock cookie.
func (s *Server) pageUnlocked(r *http.Request, page model.StatusPage) bool {
	cookie, err := r.Cookie(pageCookieName(page.Slug))
	if err != nil {
		return false
	}
	// Compared in constant time against a value derived from the stored hash, so
	// the cookie is worthless the moment the password changes and carries
	// nothing that could be replayed elsewhere.
	return subtleEqual(cookie.Value, pageUnlockToken(page))
}

func pageCookieName(slug string) string { return "cairn_page_" + slug }

// pageUnlockToken derives the cookie value from the password hash. The hash
// already contains a per-page salt, so two pages sharing a password produce
// different tokens, and rotating the password invalidates every issued cookie
// without storing a session per visitor.
func pageUnlockToken(page model.StatusPage) string {
	sum := sha256.Sum256([]byte("cairn-status-page-unlock:" + page.ID.String() + ":" + page.PasswordHash))
	return fmt.Sprintf("%x", sum[:16])
}

// subscribeToStatusPage records a subscription request.
//
// Double opt-in: the row is written unconfirmed and carries a confirmation
// token. Nothing is delivered until it is confirmed, because an endpoint that
// starts sending mail to any address a stranger types is a spam cannon with the
// operator's domain on it.
func (s *Server) subscribeToStatusPage(w http.ResponseWriter, r *http.Request) {
	page, ok := s.publicPage(w, r)
	if !ok {
		return
	}
	if !page.SubscriptionsEnabled {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "subscriptions-disabled",
			"Subscriptions are not enabled", "This status page does not accept subscriptions.")
		return
	}

	var body subscribeRequest
	if !s.readBody(w, r, 1<<12, &body) {
		return
	}

	var problems []ValidationItem
	channel := model.SubscriberEmail
	if body.Channel != nil {
		switch *body.Channel {
		case model.SubscriberEmail, model.SubscriberWebhook:
			channel = *body.Channel
		default:
			problems = append(problems, ValidationItem{Pointer: "/channel", Code: "invalid",
				Message: "channel must be email or webhook"})
		}
	}
	if body.Target == nil || *body.Target == "" {
		problems = append(problems, ValidationItem{Pointer: "/target", Code: "required",
			Message: "target is required"})
	} else if problem := validateSubscriberTarget(channel, *body.Target); problem != "" {
		problems = append(problems, ValidationItem{Pointer: "/target", Code: "invalid", Message: problem})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The subscription was not created.", problems...)
		return
	}

	// Rate-limited per page, because this is an unauthenticated write that
	// creates rows.
	if !s.limiter.allow("subscribe:"+page.Slug, time.Now()) {
		writeProblem(w, r, s.log, http.StatusTooManyRequests, "rate-limited",
			"Too many requests", "Too many subscription requests for this page. Try again shortly.")
		return
	}

	confirmToken, err := auth.NewToken()
	if err != nil {
		s.internal(w, r, "generate confirmation token", err)
		return
	}
	unsubscribeToken, err := auth.NewToken()
	if err != nil {
		s.internal(w, r, "generate unsubscribe token", err)
		return
	}

	subscriber := model.Subscriber{
		ID:                   model.NewID(),
		StatusPageID:         page.ID,
		OrgID:                s.orgID,
		Channel:              channel,
		TargetHash:           auth.HashToken(strings.ToLower(*body.Target)),
		ConfirmTokenHash:     auth.HashToken(confirmToken),
		UnsubscribeTokenHash: auth.HashToken(unsubscribeToken),
		CreatedAt:            time.Now().UTC().Truncate(time.Millisecond),
	}

	// Two envelopes, both bound to this row. The address, because every
	// notification replays it; and the unsubscribe token, because every
	// notification has to render its link again and a hash cannot be un-hashed.
	subscriber.SealedTarget, err = s.subscribers.Seal(subscriber.OrgID[:], subscriber.ID[:], []byte(*body.Target))
	if err != nil {
		s.internal(w, r, "seal subscriber target", err)
		return
	}
	subscriber.SealedUnsubscribeToken, err = s.subscribers.Seal(
		subscriber.OrgID[:], subscriber.ID[:], []byte(unsubscribeToken))
	if err != nil {
		s.internal(w, r, "seal unsubscribe token", err)
		return
	}

	if err := s.store.CreateSubscriber(r.Context(), subscriber); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// The same answer a new subscription gets, and no second
			// confirmation sent. Telling a stranger that an address is already
			// subscribed to a page turns this endpoint into an
			// address-membership oracle — and re-sending on a repeat request
			// would turn it into a way to mail somebody repeatedly.
			writeJSON(w, s.log, http.StatusAccepted, map[string]string{
				"status": "pending_confirmation",
			})
			return
		}
		s.internal(w, r, "create subscriber", err)
		return
	}

	// Queued after the row is durable, and never waited on: the answer below is
	// identical whether the message goes out, bounces, or finds no relay
	// configured, because each of those is a fact about this install that a
	// stranger is not entitled to learn from a status page.
	if s.relay != nil {
		s.relay.Confirm(notify.Confirmation{
			Page:             page,
			Subscriber:       subscriber,
			Target:           *body.Target,
			Token:            confirmToken,
			UnsubscribeToken: unsubscribeToken,
			BaseURL:          s.baseURL,
		})
	}

	s.log.Info("status page subscription requested", "page", page.Slug, "channel", channel)
	writeJSON(w, s.log, http.StatusAccepted, map[string]string{"status": "pending_confirmation"})
}

// confirmSubscription completes double opt-in.
func (s *Server) confirmSubscription(w http.ResponseWriter, r *http.Request) {
	subscriber, ok := s.subscriberByToken(w, r)
	if !ok {
		return
	}
	if subscriber.Confirmed() {
		writeJSON(w, s.log, http.StatusOK, map[string]string{"status": "confirmed"})
		return
	}
	if err := s.store.ConfirmSubscriber(r.Context(), subscriber.ID, time.Now().UTC()); err != nil {
		s.internal(w, r, "confirm subscriber", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, map[string]string{"status": "confirmed"})
}

// unsubscribe removes a subscription. One click, no confirmation step: making
// somebody prove who they are to stop receiving mail they did not confirm asking
// for is how a status page ends up reported as spam.
func (s *Server) unsubscribe(w http.ResponseWriter, r *http.Request) {
	subscriber, ok := s.subscriberByToken(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSubscriber(r.Context(), subscriber.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w, r, "delete subscriber", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) subscriberByToken(w http.ResponseWriter, r *http.Request) (model.Subscriber, bool) {
	hash := auth.HashToken(r.PathValue("token"))

	// Both token columns are looked up by hash through their own unique index,
	// so guessing costs one index probe rather than a scan — this endpoint is
	// unauthenticated and the token in the path is the whole credential.
	subscriber, err := s.store.SubscriberByToken(r.Context(), hash, hash)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, r, s.log, http.StatusNotFound, "not-found",
			"Not found", "That link is no longer valid.")
		return model.Subscriber{}, false
	}
	if err != nil {
		s.internal(w, r, "resolve subscription token", err)
		return model.Subscriber{}, false
	}
	return subscriber, true
}

func validateSubscriberTarget(channel, target string) string {
	switch channel {
	case model.SubscriberEmail:
		if _, err := mail.ParseAddress(target); err != nil {
			return "target must be an email address"
		}
	case model.SubscriberWebhook:
		parsed, err := url.ParseRequestURI(target)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return "target must be an http or https URL"
		}
	}
	return ""
}

// subtleEqual compares two strings in constant time, so the cookie check cannot
// be turned into a byte-at-a-time oracle.
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
