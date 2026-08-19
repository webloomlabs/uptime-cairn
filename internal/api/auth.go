package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// sessionCookie is the browser's credential. HttpOnly so script cannot read it,
// SameSite=Lax so a cross-site form post does not carry it, and Secure whenever
// the request arrived over TLS.
const sessionCookie = "cairn_session"

// csrfHeader is echoed by the dashboard on writes. Cookie authentication is
// ambient — the browser attaches it to any request, including one a hostile page
// triggered — so a cookie-authenticated write additionally has to prove the
// caller could read the login response. Bearer tokens are not ambient and need
// no such proof.
const csrfHeader = "X-Cairn-CSRF-Token"

// sessionLifetime is absolute, not sliding. A session that renews itself
// forever is a credential with no expiry.
const sessionLifetime = 30 * 24 * time.Hour

// lastUsedThrottle bounds how often an API key's last_used_at is written. Per
// request it would make every authenticated call a write, and on SQLite that is
// the single writer taken for a timestamp nobody reads in real time.
const lastUsedThrottle = time.Minute

// Principal is who is making a request.
type Principal struct {
	// Type is "user" or "api_key", as the Session schema names them.
	Type string

	User      *model.User
	APIKeyID  *model.ID
	SessionID *model.ID
	Scopes    auth.Set
	ExpiresAt *time.Time

	// ViaCookie marks ambient authentication, which is what makes CSRF
	// protection necessary for this request and unnecessary for a bearer token.
	ViaCookie bool
}

type principalKey struct{}

// principalFrom returns the authenticated caller, if the request went through
// the authenticate middleware.
func principalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok
}

// authenticate resolves the caller and rejects anyone it cannot.
//
// Two schemes, either sufficient, exactly as the spec fixes them: a scoped
// bearer API key, or the browser session cookie — which additionally requires a
// matching CSRF token on writes.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.resolve(r)
		if err != nil {
			// One message for every failure mode. Distinguishing "no such key"
			// from "wrong password" from "revoked" tells an attacker which half
			// of a guess was right.
			s.log.Debug("authentication failed", "path", r.URL.Path, "reason", err)
			writeProblem(w, r, s.log, http.StatusUnauthorized, "unauthenticated",
				"Not authenticated",
				"Provide a scoped API key as `Authorization: Bearer cairn_…`, or sign in for a session cookie.")
			return
		}

		if principal.ViaCookie && !safeMethod(r.Method) {
			if !s.csrfValid(r, principal) {
				writeProblem(w, r, s.log, http.StatusForbidden, "csrf-token-invalid",
					"CSRF token missing or invalid",
					"Cookie-authenticated writes must echo the `csrf_token` from the session response in the "+csrfHeader+" header.")
				return
			}
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	})
}

func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func (s *Server) resolve(r *http.Request) (*Principal, error) {
	now := time.Now().UTC()

	if token, ok := auth.BearerToken(r.Header.Get("Authorization")); ok {
		return s.resolveAPIKey(r.Context(), token, now)
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		return s.resolveSession(r, cookie.Value, now)
	}
	return nil, errors.New("no credential presented")
}

func (s *Server) resolveAPIKey(ctx context.Context, token string, now time.Time) (*Principal, error) {
	if !strings.HasPrefix(token, auth.KeyPrefix) {
		// Probe credentials live in their own namespace and can do nothing here
		// (ADR-005 decision 8). Rejecting on the prefix means one never reaches
		// the database lookup at all.
		return nil, errors.New("token is not an API key")
	}

	key, err := s.store.APIKeyByHash(ctx, auth.HashToken(token))
	if err != nil {
		return nil, err
	}
	if !key.Usable(now) {
		return nil, errors.New("key is revoked or expired")
	}

	if key.LastUsedAt == nil || now.Sub(*key.LastUsedAt) > lastUsedThrottle {
		if err := s.store.TouchAPIKey(ctx, key.ID, now); err != nil {
			// Not fatal: failing a request because a usage timestamp could not be
			// written would trade the whole API for a statistic.
			s.log.Warn("record api key use", "error", err)
		}
	}

	scopes := make(auth.Set, 0, len(key.Scopes))
	for _, raw := range key.Scopes {
		scopes = append(scopes, auth.Scope(raw))
	}

	id := key.ID
	return &Principal{Type: "api_key", APIKeyID: &id, Scopes: scopes, ExpiresAt: key.ExpiresAt}, nil
}

func (s *Server) resolveSession(r *http.Request, token string, now time.Time) (*Principal, error) {
	sess, err := s.store.SessionByTokenHash(r.Context(), auth.HashToken(token), now)
	if err != nil {
		return nil, err
	}

	user, err := s.store.UserByID(r.Context(), sess.UserID)
	if err != nil {
		return nil, err
	}
	if !user.Active {
		return nil, errors.New("account is disabled")
	}

	if sess.LastSeenAt == nil || now.Sub(*sess.LastSeenAt) > lastUsedThrottle {
		if err := s.store.TouchSession(r.Context(), sess.ID, now); err != nil {
			s.log.Warn("record session activity", "error", err)
		}
	}

	id := sess.ID
	expires := sess.ExpiresAt
	return &Principal{
		Type:      "user",
		User:      &user,
		SessionID: &id,
		Scopes:    auth.ScopesFor(user.Role),
		ExpiresAt: &expires,
		ViaCookie: true,
	}, nil
}

func (s *Server) csrfValid(r *http.Request, p *Principal) bool {
	presented := r.Header.Get(csrfHeader)
	if presented == "" || p.SessionID == nil {
		return false
	}

	sess, err := s.store.SessionByTokenHash(r.Context(), sessionTokenHashOf(r), time.Now().UTC())
	if err != nil {
		return false
	}
	return auth.EqualTokens(auth.HashToken(presented), sess.CSRFTokenHash)
}

func sessionTokenHashOf(r *http.Request) []byte {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	return auth.HashToken(cookie.Value)
}

// require wraps a handler with the scope its operation declares in
// x-cairn-scopes. The declaration in the spec and the check here are the same
// fact stated twice; contract tests are what will keep them that way.
func (s *Server) require(scope auth.Scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(r.Context())
		if !ok {
			writeProblem(w, r, s.log, http.StatusUnauthorized, "unauthenticated", "Not authenticated", "")
			return
		}
		if !p.Scopes.Grants(scope) {
			writeProblem(w, r, s.log, http.StatusForbidden, "insufficient-scope",
				"Insufficient scope",
				"This operation requires the "+string(scope)+" scope.")
			return
		}
		next(w, r)
	}
}

// setSessionCookie issues the browser credential.
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// requestIsTLS honours the proxy header as well as the connection, because the
// documented deployment shape puts Caddy, nginx, or Traefik in front. Marking a
// cookie Secure when the browser spoke HTTPS to the proxy is correct even though
// the last hop was plaintext.
func requestIsTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// loginLimiter throttles credential guessing. In memory and per process, which
// is the right scope for a single-binary install and is stated as a limitation
// rather than mistaken for a distributed rate limiter.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

const (
	loginMaxAttempts = 5
	loginWindow      = 15 * time.Minute
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string][]time.Time)}
}

// allow records an attempt and reports whether it may proceed.
func (l *loginLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-loginWindow)
	kept := l.attempts[key][:0]
	for _, at := range l.attempts[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	l.attempts[key] = kept

	if len(kept) >= loginMaxAttempts {
		return false
	}
	l.attempts[key] = append(l.attempts[key], now)
	return true
}

// succeed clears the counter after a successful login, so a user who mistypes
// twice and then gets it right is not one mistake from being locked out.
func (l *loginLimiter) succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// clientIP is the rate-limit key alongside the email. RemoteAddr only, on
// purpose: X-Forwarded-For is caller-controlled unless a trusted proxy is
// configured, and trusting it here would hand an attacker an unlimited supply
// of rate-limit buckets.
func clientIP(r *http.Request) string {
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

var errSetupComplete = errors.New("setup has already been completed")

// setupRequired reports whether the install still has no administrator.
func (s *Server) setupRequired(ctx context.Context) (bool, error) {
	n, err := s.store.CountUsers(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	return n == 0, nil
}
