package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Brand profiles: the white-label identity reports are rendered under.
//
// Reports only. Status pages keep their inline theme, logo URL, primary colour
// and footer text from Phase 1 (spec decision Q2), so branding lives in three
// places — here, on StatusPage, and in settings.appearance — and can drift. That
// is a documented cost rather than an oversight: unifying them would turn four
// columns of a shipped schema into a foreign key, which is a breaking change
// this phase declined to make.

// maxLogoBytes bounds a logo.
//
// A megabyte is generous for a mark that renders at 240×56 points on a cover
// page, and the bound is what keeps the decision to store logos in the database
// defensible: they travel in every VACUUM INTO backup, which is the property
// that makes branding survive the documented restore procedure.
const maxLogoBytes = 1 << 20

// hexColor is the spec's pattern. Stored as written rather than normalised: a
// brand colour is a string somebody pasted from a brand guide, and handing it
// back in a different case is the kind of small wrongness that makes a
// white-label feature feel like somebody else's.
var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// BrandStore is the branding half of persistence.
type BrandStore interface {
	CreateBrandProfile(ctx context.Context, p model.BrandProfile) error
	UpdateBrandProfile(ctx context.Context, p model.BrandProfile) error
	GetBrandProfile(ctx context.Context, id model.ID) (model.BrandProfile, error)
	ListBrandProfiles(ctx context.Context, after *store.Cursor, limit int) ([]model.BrandProfile, bool, error)
	DeleteBrandProfile(ctx context.Context, id model.ID) error
	TemplatesUsingBrandProfile(ctx context.Context, id model.ID) (int, error)
	SetBrandLogo(ctx context.Context, id model.ID, logo []byte, contentType string, p model.BrandProfile) error
}

func (s *Server) listBrandProfiles(w http.ResponseWriter, r *http.Request) {
	after, ok := s.cursor(w, r)
	if !ok {
		return
	}

	profiles, hasMore, err := s.store.ListBrandProfiles(r.Context(), after, s.limit(r))
	if err != nil {
		s.internal(w, r, "list brand profiles", err)
		return
	}

	body := page[brandProfileJSON]{Data: []brandProfileJSON{}, Pagination: pagination{HasMore: hasMore}}
	for _, p := range profiles {
		body.Data = append(body.Data, toBrandProfileJSON(p))
	}
	if hasMore && len(profiles) > 0 {
		last := profiles[len(profiles)-1]
		next := store.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}.Encode()
		body.Pagination.NextCursor = &next
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

func (s *Server) createBrandProfile(w http.ResponseWriter, r *http.Request) {
	var body brandProfileWrite
	if !s.readBody(w, r, maxReportBody, &body) {
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	profile := model.BrandProfile{
		ID:        model.NewID(),
		OrgID:     s.orgID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	problems := applyBrandProfile(&profile, body)
	if body.Name == nil {
		problems = append(problems, ValidationItem{Pointer: "/name", Code: "required", Message: "name is required"})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The brand profile was not created.", problems...)
		return
	}

	if err := s.store.CreateBrandProfile(r.Context(), profile); err != nil {
		s.internal(w, r, "create brand profile", err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, toBrandProfileJSON(profile))
}

func (s *Server) getBrandProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.brandProfileID(w, r)
	if !ok {
		return
	}
	profile, err := s.store.GetBrandProfile(r.Context(), id)
	if err != nil {
		s.reportStoreError(w, r, "get brand profile", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toBrandProfileJSON(profile))
}

func (s *Server) updateBrandProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.brandProfileID(w, r)
	if !ok {
		return
	}
	var body brandProfileWrite
	if !s.readBody(w, r, maxReportBody, &body) {
		return
	}

	profile, err := s.store.GetBrandProfile(r.Context(), id)
	if err != nil {
		s.reportStoreError(w, r, "get brand profile", err)
		return
	}

	problems := applyBrandProfile(&profile, body)
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The brand profile was not updated.", problems...)
		return
	}

	profile.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if err := s.store.UpdateBrandProfile(r.Context(), profile); err != nil {
		s.reportStoreError(w, r, "update brand profile", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toBrandProfileJSON(profile))
}

// deleteBrandProfile refuses while a live template still names the profile.
//
// The foreign key would allow it and let the template fall back to the default.
// The spec refuses instead, and its reasoning is the better one: a report that
// silently loses its client's branding on the first of the month is worse than a
// refused delete. The fallback is invisible until an agency's client receives an
// unbranded document; the refusal happens while somebody is looking at the
// screen, and names what to unpick.
func (s *Server) deleteBrandProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.brandProfileID(w, r)
	if !ok {
		return
	}

	err := s.store.DeleteBrandProfile(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrConflict):
		using, countErr := s.store.TemplatesUsingBrandProfile(r.Context(), id)
		detail := "A report template still uses this brand profile."
		if countErr == nil {
			detail = fmt.Sprintf("%s still %s this brand profile. Point them at another profile first.",
				plural(using, "report template"), usesOrUse(using))
		}
		writeProblem(w, r, s.log, http.StatusConflict, "brand-profile-in-use",
			"Brand profile is in use", detail)
	case err != nil:
		s.reportStoreError(w, r, "delete brand profile", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// uploadBrandProfileLogo replaces the logo bytes.
//
// **PNG or JPEG only, refused here with the reason rather than dropped at render
// time.** ADR-007 renders PDF with an in-tree writer that embeds rasters and has
// no SVG path translator, and SVG is the expected case rather than the exotic
// one — status pages accept an arbitrary logo URL and the project's own mark is
// an SVG. So the refusal has to be legible to somebody holding one, at the
// moment they are holding it, and not at 09:00 on the first of the month.
func (s *Server) uploadBrandProfileLogo(w http.ResponseWriter, r *http.Request) {
	id, ok := s.brandProfileID(w, r)
	if !ok {
		return
	}
	profile, err := s.store.GetBrandProfile(r.Context(), id)
	if err != nil {
		s.reportStoreError(w, r, "get brand profile", err)
		return
	}

	// One byte over the limit, so a body exactly at it still succeeds and one
	// past it is detected rather than silently truncated into a corrupt image.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLogoBytes+1))
	if err != nil {
		s.internal(w, r, "read brand logo", err)
		return
	}
	if len(body) > maxLogoBytes {
		writeProblem(w, r, s.log, http.StatusRequestEntityTooLarge, "logo-too-large",
			"Logo is too large", "A logo must be 1 MB or smaller.")
		return
	}
	if len(body) == 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The logo was empty.")
		return
	}

	contentType, problem := logoContentType(body)
	if problem != "" {
		writeProblem(w, r, s.log, http.StatusUnsupportedMediaType, "unsupported-logo-format",
			"Unsupported logo format", problem)
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	profile.LogoContentType = contentType
	profile.LogoBytes = int64(len(body))
	profile.LogoUpdatedAt = &now
	profile.UpdatedAt = now

	if err := s.store.SetBrandLogo(r.Context(), id, body, contentType, profile); err != nil {
		s.reportStoreError(w, r, "set brand logo", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toBrandProfileJSON(profile))
}

// logoContentType decides from the bytes, not from the header.
//
// From the bytes deliberately: a declared Content-Type is a claim by the client,
// and the failure this guards against is a PDF that renders nothing because an
// SVG arrived labelled image/png. Browsers and curl both get this wrong often
// enough that trusting the label would make the refusal unreliable in exactly
// the case it exists for.
func logoContentType(body []byte) (contentType, problem string) {
	switch {
	case bytes.HasPrefix(body, []byte("\x89PNG\r\n\x1a\n")):
		return model.LogoPNG, ""
	case bytes.HasPrefix(body, []byte{0xFF, 0xD8, 0xFF}):
		return model.LogoJPEG, ""
	case looksLikeSVG(body):
		// Named, because it is the case that actually happens and a generic
		// "unsupported format" would leave the operator guessing which of their
		// files was wrong and why the obvious one is not allowed.
		return "", "This looks like an SVG. Reports are rendered to PDF by an in-tree writer " +
			"that embeds raster images and cannot draw SVG paths, so a vector logo would be " +
			"missing from the PDF rather than resized. Export the logo as PNG — at about " +
			"480 pixels wide it will be sharp on both the page and the print."
	}
	return "", "A logo must be a PNG or a JPEG."
}

func looksLikeSVG(body []byte) bool {
	head := body
	if len(head) > 1024 {
		head = head[:1024]
	}
	lower := bytes.ToLower(head)
	return bytes.Contains(lower, []byte("<svg")) || bytes.Contains(lower, []byte("<?xml"))
}

func (s *Server) brandProfileID(w http.ResponseWriter, r *http.Request) (model.ID, bool) {
	id, ok := model.ParseID(r.PathValue("brandProfileId"))
	if !ok {
		s.reportNotFound(w, r)
		return model.ID{}, false
	}
	return id, true
}

func applyBrandProfile(p *model.BrandProfile, body brandProfileWrite) []ValidationItem {
	var problems []ValidationItem

	if body.Name != nil {
		if *body.Name == "" {
			problems = append(problems, ValidationItem{Pointer: "/name", Code: "invalid", Message: "name must not be empty"})
		}
		p.Name = *body.Name
	}
	if body.CompanyName != nil {
		p.CompanyName = *body.CompanyName
	}
	// Plain text, and that is a rendering constraint rather than a storage one:
	// the PDF writer has no rich-text pipeline, and a field that renders in HTML
	// and not in PDF is worse than one that renders nowhere. Nothing is stripped
	// here — every backend escapes — but the constraint is why no markup is
	// interpreted anywhere downstream.
	if body.FooterText != nil {
		p.FooterText = *body.FooterText
	}
	if body.CoverText != nil {
		p.CoverText = *body.CoverText
	}
	if body.HidePoweredBy != nil {
		p.HidePoweredBy = *body.HidePoweredBy
	}
	if body.IsDefault != nil {
		p.IsDefault = *body.IsDefault
	}

	for _, field := range []struct {
		pointer  string
		supplied *string
		into     *string
	}{
		{"/primary_color", body.PrimaryColor, &p.PrimaryColor},
		{"/accent_color", body.AccentColor, &p.AccentColor},
	} {
		if field.supplied == nil {
			continue
		}
		if *field.supplied != "" && !hexColor.MatchString(*field.supplied) {
			problems = append(problems, ValidationItem{Pointer: field.pointer, Code: "invalid",
				Message: "must be a six-digit hex colour including the leading #, for example #1a8f5a"})
			continue
		}
		*field.into = *field.supplied
	}
	return problems
}

func usesOrUse(n int) string {
	if n == 1 {
		return "uses"
	}
	return "use"
}
