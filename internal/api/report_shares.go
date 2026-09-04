package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Share links: a report published to anyone holding the URL.
//
// # The token in the path is the entire credential
//
// That sentence governs every decision in this file. There is no session, no key
// and no second factor on the public pair below — somebody who has the URL can
// read a client's uptime data, and somebody who can guess one can read a
// stranger's. So:
//
//   - The token is 256 bits from crypto/rand, well past the 128 ADR-008 item 14
//     asks for, and it is looked up **by hash against a unique index**. Guessing
//     costs one indexed probe rather than a walk, and the plaintext never reaches
//     the store.
//   - It is stored twice — hashed for the lookup, sealed for replay — following
//     `Subscriber`'s unsubscribe token. "Hash what you verify, encrypt what you
//     replay", and this token is both.
//   - The public read is **rate limited** and carries `X-Robots-Tag: noindex,
//     nofollow`. A share link publishes a client's figures to anyone with the
//     URL; having it also publish them to a search index is not a second-order
//     concern, it is the difference between a link and a press release.
//   - The response is a **separate public projection**, never a filter over
//     `reportRunJSON`. A field cannot leak through a projection that has no place
//     to put it — the status-page discipline, adopted here because this is the
//     other path in the product where a stranger reads something.
//
// # It serves the stored artifact, never a re-render
//
// ADR-008 item 15, and it follows from the cache-or-record finding rather than
// from performance: a client who bookmarked this link must not find the figures
// changed underneath them because retention has since dropped a tier. It is also
// what keeps the public path from being a denial-of-service primitive — a URL
// that triggered a full report computation is one somebody can point at the
// instance.
//
// # Three answers, not two
//
// 404 for no such token, 410 for a link that was revoked or has expired or whose
// bytes retention reclaimed, 429 for too many attempts. "It is gone" and "it was
// never here" are different facts, and only one of them is true for somebody
// holding a bookmark.

// ReportShareStore is the share half of persistence, declared by the consumer.
type ReportShareStore interface {
	CreateReportShareLink(ctx context.Context, link model.ReportShareLink) error
	ReportShareLinkForRun(ctx context.Context, runID model.ID) (model.ReportShareLink, error)
	ReportShareLinksForRuns(ctx context.Context, runIDs []model.ID) (map[model.ID]model.ReportShareLink, error)
	ReportShareLinkByTokenHash(ctx context.Context, hash []byte) (model.ReportShareLink, error)
	RevokeReportShareLink(ctx context.Context, runID model.ID, at time.Time) error
	TouchReportShareLink(ctx context.Context, id model.ID, at time.Time) error
}

// The public read's rate limit, and **its own limiter rather than the login
// one**.
//
// Reusing `loginLimiter` was the first cut and it was wrong in a way a live run
// caught immediately: five attempts in fifteen minutes is right for credential
// guessing and absurd for a document. A client who opens the report, downloads
// the CSV, downloads the PDF and refreshes once has spent five, and the sixth
// request tells them to go away — on a link somebody sent them.
//
// Keyed by token rather than by client address, which is the deliberate choice
// and the uncomfortable one. Keying by address would throttle a client's whole
// office behind one NAT while doing nothing about a distributed guesser; keying
// by token bounds what any single link can cost the instance and leaves guessing
// to be defended by the 256 bits, which is where that defence actually lives.
const (
	shareMaxRequests = 120
	shareWindow      = time.Minute
)

// shareLimiter is a fixed window per token. In memory and per process, the same
// scope and the same stated limitation as the login limiter — this is a
// single-binary install, not a distributed rate limiter.
type shareLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
}

func newShareLimiter() *shareLimiter {
	return &shareLimiter{requests: make(map[string][]time.Time)}
}

func (l *shareLimiter) allow(token string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-shareWindow)
	kept := l.requests[token][:0]
	for _, at := range l.requests[token] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}

	// Swept while the lock is held, so a burst of guesses against many tokens
	// does not leave a map entry per guess for the life of the process. The login
	// limiter can afford not to do this because its keys are email addresses and
	// client IPs; a share token is attacker-chosen, so the key space is the whole
	// of the input.
	if len(kept) == 0 {
		delete(l.requests, token)
		l.requests[token] = append(l.requests[token], now)
		return true
	}

	l.requests[token] = kept
	if len(kept) >= shareMaxRequests {
		return false
	}
	l.requests[token] = append(l.requests[token], now)
	return true
}

// --- create -----------------------------------------------------------------

// createReportShareLink mints a link for a run and returns it once.
func (s *Server) createReportShareLink(w http.ResponseWriter, r *http.Request) {
	runID, ok := s.reportRunID(w, r)
	if !ok {
		return
	}

	// The run has to exist before a link to it does. Without this a link could be
	// created against a typo'd id and then answer 404 forever, which reads as a
	// broken link rather than as a mistake at creation.
	run, err := s.store.GetReportRun(r.Context(), runID)
	if err != nil {
		s.reportStoreError(w, r, "get report run", err)
		return
	}

	// **The body is optional**, which the spec fixes (`required: false`) and which
	// readBody does not express: it decodes and a missing body is an EOF. A link
	// with no expiry is the ordinary case — an operator sending a client last
	// month's report has no reason to type anything — so an empty body means "no
	// expiry" rather than a 400.
	var body reportShareLinkWrite
	if r.ContentLength != 0 {
		if !s.readBody(w, r, maxReportBody, &body) {
			return
		}
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	var expiresAt *time.Time
	if body.ExpiresAt != nil {
		if !body.ExpiresAt.After(now) {
			writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
				"Validation failed", "The share link was not created.",
				ValidationItem{Pointer: "/expires_at", Code: "invalid",
					Message: "expires_at must be in the future; a link that is already expired is a link nobody can use"})
			return
		}
		at := body.ExpiresAt.UTC()
		expiresAt = &at
	}

	token, err := auth.NewToken()
	if err != nil {
		s.internal(w, r, "generate share token", err)
		return
	}

	link := model.ReportShareLink{
		ID:          model.NewID(),
		OrgID:       s.orgID,
		ReportRunID: run.ID,
		TokenHash:   auth.HashToken(token),
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
	}
	// Sealed against this row, so a ciphertext lifted from another link's row
	// fails to open rather than opening as somebody else's credential.
	link.TokenSealed, err = s.reportShares.Seal(link.OrgID[:], link.ID[:], []byte(token))
	if err != nil {
		s.internal(w, r, "seal share token", err)
		return
	}

	if err := s.store.CreateReportShareLink(r.Context(), link); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// The partial unique index refused a second live link. **409 rather
			// than replacing the first**, which the spec fixes and which is the
			// safer of the two: silently revoking a link somebody has already
			// sent to a client, because a colleague pressed the button again, is
			// a support call that starts with "the report link you sent me
			// stopped working".
			writeProblem(w, r, s.log, http.StatusConflict, "share-link-exists",
				"Share link exists",
				"This run already has a share link. Revoke it first, or use the one that exists.")
			return
		}
		s.internal(w, r, "create share link", err)
		return
	}

	// **The only place the plaintext appears outside the sealed envelope.**
	writeJSON(w, s.log, http.StatusCreated, reportShareLinkCreatedJSON{
		URL:       s.shareURL(r, token),
		ExpiresAt: link.ExpiresAt,
		CreatedAt: link.CreatedAt,
	})
}

// revokeReportShareLink withdraws a run's link. The artifacts are untouched.
func (s *Server) revokeReportShareLink(w http.ResponseWriter, r *http.Request) {
	runID, ok := s.reportRunID(w, r)
	if !ok {
		return
	}
	if err := s.store.RevokeReportShareLink(r.Context(), runID, time.Now().UTC().Truncate(time.Millisecond)); err != nil {
		// ErrNotFound covers both "no such run" and "no live link", which are the
		// same answer to this request: there is nothing here to revoke.
		s.reportStoreError(w, r, "revoke share link", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// shareURL is what the operator copies.
//
// The configured base URL wins, for the reason pushURL gives: it is the operator
// saying how this install is reached from outside, and a link built from the
// request that created it is right only while the creator and the recipient reach
// the install the same way. A share link is sent to somebody outside the network
// by definition, so this is the field that matters most.
func (s *Server) shareURL(r *http.Request, token string) string {
	if s.baseURL != "" {
		return s.baseURL + "/api/v1/public/reports/" + token
	}
	scheme := "http"
	if requestIsTLS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/v1/public/reports/" + token
}

// --- the public pair --------------------------------------------------------

// getPublicReport serves the shared report's public projection.
func (s *Server) getPublicReport(w http.ResponseWriter, r *http.Request) {
	link, run, ok := s.resolveShare(w, r)
	if !ok {
		return
	}

	artifacts, err := s.store.ArtifactsForRuns(r.Context(), []model.ID{run.ID})
	if err != nil {
		s.internal(w, r, "load shared artifacts", err)
		return
	}
	rendered := artifacts[run.ID]

	template, err := s.store.ReportTemplateForRun(r.Context(), run.ReportTemplateID)
	if err != nil {
		s.internal(w, r, "load shared template", err)
		return
	}

	projection := s.publicReport(r.Context(), run, template, rendered)
	if len(projection.Formats) == 0 {
		// The link resolved and there is nothing behind it. Two different causes
		// reach here — retention reclaimed the bytes, or the files are not on
		// disk at all — and the answer is 410 either way, because from the
		// reader's side both are "this existed and is gone".
		//
		// The wording does not guess between them. An earlier version asserted
		// retention, which would have been a confident lie to a client whose
		// report was missing because somebody restored a backup without the
		// reports directory.
		s.shareGone(w, r, "This report's files are no longer available.")
		return
	}

	// Best effort, and its failure is swallowed on purpose: refusing a client's
	// report because a statistics column would not update is the wrong trade.
	if err := s.store.TouchReportShareLink(r.Context(), link.ID, time.Now().UTC()); err != nil {
		s.log.Warn("touch share link", "error", err)
	}

	shareHeaders(w)
	// An artifact is immutable once generated, so this caches hard — which is
	// what ADR-008 item 15 buys beyond correctness. The ETag is the run id and
	// the state, so a run whose artifacts expire stops matching a cached copy.
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("ETag", `"`+run.ID.String()+"-"+run.State+`"`)
	writeJSON(w, s.log, http.StatusOK, projection)
}

// downloadPublicReport serves one stored artifact through the link.
func (s *Server) downloadPublicReport(w http.ResponseWriter, r *http.Request) {
	_, run, ok := s.resolveShare(w, r)
	if !ok {
		return
	}

	format := r.URL.Query().Get("format")
	if format == "" {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "A format is required.",
			ValidationItem{Pointer: "/format", Code: "required", Message: "format is required"})
		return
	}

	artifact, err := s.store.ArtifactByFormat(r.Context(), run.ID, format)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// This run never produced that format. A 404 on the format rather
			// than a 410: nothing was reclaimed, the document simply does not
			// exist in the shape that was asked for.
			s.shareNotFound(w, r)
			return
		}
		s.internal(w, r, "find shared artifact", err)
		return
	}

	shareHeaders(w)
	// The artifact-addressed serve, unchanged from the authenticated path: the
	// stored bytes, the digest header, and 410 for a tombstone. Deliberately the
	// same function, because two code paths serving the same file is how one of
	// them ends up re-rendering.
	s.serveArtifact(w, r, artifact)
}

// resolveShare turns the token in the path into a run, or answers on its own.
//
// Every refusal below is written so that a stranger learns nothing they did not
// already have: an unparseable token, an unknown one, and a revoked one are three
// different internal states, and the first two both answer 404 because telling
// somebody their guess was well-formed is telling them something.
func (s *Server) resolveShare(w http.ResponseWriter, r *http.Request) (model.ReportShareLink, model.ReportRun, bool) {
	token := r.PathValue("shareToken")
	// Bounded before it is hashed. The spec's parameter is 22–128 characters, and
	// hashing an unbounded path segment is work an unauthenticated caller can ask
	// for repeatedly.
	if len(token) < 22 || len(token) > 128 {
		s.shareNotFound(w, r)
		return model.ReportShareLink{}, model.ReportRun{}, false
	}

	// Rate limited before the lookup, so a guesser pays nothing but the limiter.
	if !s.shares.allow(token, time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeProblem(w, r, s.log, http.StatusTooManyRequests, "rate-limited",
			"Too many requests", "Too many requests for this report. Try again shortly.")
		return model.ReportShareLink{}, model.ReportRun{}, false
	}

	link, err := s.store.ReportShareLinkByTokenHash(r.Context(), auth.HashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.shareNotFound(w, r)
			return model.ReportShareLink{}, model.ReportRun{}, false
		}
		s.internal(w, r, "resolve share link", err)
		return model.ReportShareLink{}, model.ReportRun{}, false
	}

	now := time.Now().UTC()
	switch {
	case link.RevokedAt != nil:
		s.shareGone(w, r, "This link was revoked.")
		return model.ReportShareLink{}, model.ReportRun{}, false
	case link.ExpiresAt != nil && !link.ExpiresAt.After(now):
		s.shareGone(w, r, "This link has expired.")
		return model.ReportShareLink{}, model.ReportRun{}, false
	}

	run, err := s.store.GetReportRun(r.Context(), link.ReportRunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.shareGone(w, r, "The report behind this link no longer exists.")
			return model.ReportShareLink{}, model.ReportRun{}, false
		}
		s.internal(w, r, "load shared run", err)
		return model.ReportShareLink{}, model.ReportRun{}, false
	}
	return link, run, true
}

// shareHeaders are what every public response carries.
//
// `noindex, nofollow` because a share link publishes a client's uptime data to
// anyone holding the URL, and a search index turns "anyone holding the URL" into
// "anyone". Set on the refusals as well as on the successes, which is not
// redundant: a 410 body naming a client's report is still a page.
func shareHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	// The whole point of the projection is that the response carries no
	// identifiers; this stops a referrer header carrying the token itself to
	// whatever the reader clicks next.
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func (s *Server) shareNotFound(w http.ResponseWriter, r *http.Request) {
	shareHeaders(w)
	writeProblem(w, r, s.log, http.StatusNotFound, "not-found",
		"Not found", "No report is shared at this link.")
}

func (s *Server) shareGone(w http.ResponseWriter, r *http.Request, detail string) {
	shareHeaders(w)
	writeProblem(w, r, s.log, http.StatusGone, "share-link-gone", "Link no longer available", detail)
}

// publicReport builds the projection.
//
// **A separate shape, assembled field by field from named sources.** Not a filter
// over reportRunJSON and not a struct embedding one: the guarantee being bought
// is that a field added to the private shape next year cannot appear here by
// default, and only a construction that names every field it emits has that
// property. There is no run id, no template id, no schedule, no delivery log and
// no monitor identifier in what follows — and no way for one to arrive without
// somebody typing it into this function.
func (s *Server) publicReport(
	ctx context.Context,
	run model.ReportRun,
	template model.ReportTemplate,
	artifacts []model.ReportArtifact,
) publicReportJSON {
	out := publicReportJSON{
		Title:       template.Name,
		PeriodStart: run.PeriodStart,
		PeriodEnd:   run.PeriodEnd,
		Timezone:    run.Timezone,
		GeneratedAt: run.CreatedAt,
		Formats:     []string{},
	}
	if run.FinishedAt != nil {
		out.GeneratedAt = *run.FinishedAt
	}

	for _, a := range artifacts {
		// Rendered **and actually on disk**. An expired or failed artifact is
		// offered to an operator so the run's own state is explicable; to a
		// stranger it is a download link that answers with a problem document,
		// which is a worse way to learn a file is gone than not being offered it.
		//
		// The disk check matters most here. An operator restoring a backup
		// without `<data-dir>/reports/` still has every share link they ever
		// issued, and a client following one would otherwise be shown a list of
		// formats where every download fails.
		if s.artifactAvailable(a) {
			out.Formats = append(out.Formats, a.Format)
		}
	}

	out.Brand = s.publicBrand(ctx, template)
	out.Document = s.publicDocument(artifacts)
	return out
}

// publicDocument inlines the stored JSON artifact, when there is one.
//
// **Read off disk, never re-computed.** The whole of ADR-008 item 15 is in that
// sentence: a client who bookmarked this link must not find the figures changed
// underneath them because retention has since dropped a tier, and a public URL
// that triggered a report computation would be a denial-of-service primitive
// pointed at the instance.
//
// A failure to read is a null document rather than a failed request. The formats
// list still offers the PDF and the CSV, and a reader who cannot see the inline
// figures can still download the file that was actually sent.
func (s *Server) publicDocument(artifacts []model.ReportArtifact) json.RawMessage {
	if s.artifacts == nil {
		return nil
	}
	for _, a := range artifacts {
		if a.Format != model.FormatJSON || !s.artifactAvailable(a) {
			continue
		}
		body, err := s.artifacts.Open(a.Path)
		if err != nil {
			s.log.Warn("open shared report document", "error", err, "artifact_id", a.ID.String())
			return nil
		}
		defer func() { _ = body.Close() }()

		// Bounded. The JSON artifact for a 5,000-monitor annual report is the
		// large one, and inlining it whole into a public response would be an
		// allocation an unauthenticated caller can ask for.
		data, err := io.ReadAll(io.LimitReader(body, maxSharedDocumentBytes))
		if err != nil {
			s.log.Warn("read shared report document", "error", err, "artifact_id", a.ID.String())
			return nil
		}
		if int64(len(data)) >= maxSharedDocumentBytes {
			// Too large to inline. The download link for the same format still
			// serves it in full, streamed rather than buffered, so nothing is
			// withheld — it is just not put in this response.
			return nil
		}
		return json.RawMessage(data)
	}
	return nil
}

// maxSharedDocumentBytes bounds the inline document. Four megabytes is far past
// any report a person reads on a page and far short of the hundred-megabyte CSV
// ADR-008 item 7 anticipates.
const maxSharedDocumentBytes = 4 << 20

// publicBrand is the client-facing chrome, and the only place the projection
// reaches outside the run.
//
// A failure to load is not a failure to serve: an unbranded report is a report,
// and refusing to show a client their figures because a logo URL would not load
// is the wrong trade.
func (s *Server) publicBrand(ctx context.Context, template model.ReportTemplate) *publicReportBrandJSON {
	if template.BrandProfileID == nil {
		return nil
	}
	profile, err := s.store.GetBrandProfile(ctx, *template.BrandProfileID)
	if err != nil {
		s.log.Warn("load brand profile for shared report", "error", err)
		return nil
	}
	return &publicReportBrandJSON{
		CompanyName:   optional(profile.CompanyName),
		PrimaryColor:  optional(profile.PrimaryColor),
		FooterText:    optional(profile.FooterText),
		HidePoweredBy: profile.HidePoweredBy,
	}
}
