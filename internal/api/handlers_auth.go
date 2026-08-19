package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// recoveryCodeCount is how many single-use codes confirming TOTP issues. Ten is
// the conventional number: enough that losing a phone is survivable, few enough
// that they fit on the piece of paper people actually print.
const recoveryCodeCount = 10

type setupRequest struct {
	Email        string `json:"email"`
	Name         string `json:"name"`
	Password     string `json:"password"`
	InstanceName string `json:"instance_name"`
	Timezone     string `json:"timezone"`
}

type loginRequest struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	TOTPCode     string `json:"totp_code"`
	RecoveryCode string `json:"recovery_code"`
}

type sessionJSON struct {
	PrincipalType string     `json:"principal_type"`
	User          *userJSON  `json:"user,omitempty"`
	APIKeyID      *string    `json:"api_key_id"`
	Scopes        []string   `json:"scopes"`
	CSRFToken     *string    `json:"csrf_token"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

type userJSON struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        *string    `json:"name"`
	Role        string     `json:"role"`
	Active      bool       `json:"active"`
	TOTPEnabled bool       `json:"totp_enabled"`
	Timezone    *string    `json:"timezone"`
	Locale      *string    `json:"locale"`
	LastLoginAt *time.Time `json:"last_login_at"`

	// TeamIDs is always empty in Phase 1 and always present, because the spec
	// marks it required. A client can render the field from the first release
	// rather than learning it exists in Phase 3.
	TeamIDs []string `json:"team_ids"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toUserJSON(u model.User) userJSON {
	out := userJSON{
		ID:          u.ID.String(),
		Email:       u.Email,
		Role:        u.Role,
		Active:      u.Active,
		TOTPEnabled: u.TOTPEnabled(),
		LastLoginAt: u.LastLoginAt,
		TeamIDs:     []string{},
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
	if u.Name != "" {
		out.Name = &u.Name
	}
	if u.Timezone != "" {
		out.Timezone = &u.Timezone
	}
	if u.Locale != "" {
		out.Locale = &u.Locale
	}
	return out
}

// setupStatus tells a client whether first-run setup is still open. Public,
// because a browser has to know which screen to show before it has a credential.
func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	required, err := s.setupRequired(r.Context())
	if err != nil {
		s.internal(w, r, "setup status", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, map[string]bool{"setup_required": required})
}

// completeSetup creates the first administrator and signs them in.
//
// Available only while setup_required is true. The window cannot be reopened:
// the check is a count of users, and the unique index on (org_id, email) is what
// makes two simultaneous callers resolve to one account rather than two.
func (s *Server) completeSetup(w http.ResponseWriter, r *http.Request) {
	var body setupRequest
	if !decodeJSON(w, r, s.log, &body) {
		return
	}

	required, err := s.setupRequired(r.Context())
	if err != nil {
		s.internal(w, r, "setup status", err)
		return
	}
	if !required {
		writeProblem(w, r, s.log, http.StatusConflict, "setup-complete",
			"Setup has already been completed",
			"An administrator account exists. Sign in, or reset the installation.")
		return
	}

	var problems []ValidationItem
	email, ok := normaliseEmail(body.Email)
	if !ok {
		problems = append(problems, ValidationItem{Pointer: "/email", Code: "invalid", Message: "a valid email address is required"})
	}
	if len([]rune(body.Password)) < auth.MinPasswordLength {
		problems = append(problems, ValidationItem{
			Pointer: "/password", Code: "too_short",
			Message: "password must be at least 12 characters — length, not punctuation, is what makes it hard to guess",
		})
	}
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The administrator account was not created.", problems...)
		return
	}

	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		s.internal(w, r, "hash password", err)
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	user := model.User{
		ID:           model.NewID(),
		OrgID:        s.orgID,
		Email:        email,
		Name:         body.Name,
		Role:         model.RoleOwner,
		Active:       true,
		PasswordHash: hash,
		Timezone:     body.Timezone,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if body.InstanceName != "" {
		if err := s.store.SetInstanceName(r.Context(), s.orgID, body.InstanceName); err != nil {
			s.internal(w, r, "save instance name", err)
			return
		}
	}

	if err := s.store.CreateUser(r.Context(), user); err != nil {
		// The unique index is the real guard against a race here; a second
		// caller lands on it and is told setup is done rather than being handed
		// a second owner account.
		if isUniqueViolation(err) {
			writeProblem(w, r, s.log, http.StatusConflict, "setup-complete",
				"Setup has already been completed", "An administrator account exists.")
			return
		}
		s.internal(w, r, "create user", err)
		return
	}

	s.log.Info("first administrator created", "email", email)
	s.issueSession(w, r, user, http.StatusCreated)
}

// login establishes a browser session.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body loginRequest
	if !decodeJSON(w, r, s.log, &body) {
		return
	}

	email, _ := normaliseEmail(body.Email)
	limitKey := clientIP(r) + "|" + email
	now := time.Now().UTC()

	if !s.limiter.allow(limitKey, now) {
		w.Header().Set("Retry-After", "900")
		writeProblem(w, r, s.log, http.StatusTooManyRequests, "rate-limited",
			"Too many attempts",
			"Too many failed sign-ins for this address. Try again in a few minutes.")
		return
	}

	user, err := s.store.UserByEmail(r.Context(), s.orgID, email)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internal(w, r, "look up user", err)
			return
		}
		// Hash a dummy password anyway. Returning immediately for an unknown
		// address makes account existence measurable with a stopwatch.
		_, _ = auth.HashPassword(body.Password)
		s.rejectLogin(w, r)
		return
	}

	valid, err := auth.VerifyPassword(body.Password, user.PasswordHash)
	if err != nil {
		s.internal(w, r, "verify password", err)
		return
	}
	if !valid || !user.Active {
		s.rejectLogin(w, r)
		return
	}

	if user.TOTPEnabled() {
		ok, err := s.secondFactorValid(r, user, body)
		if err != nil {
			s.internal(w, r, "verify second factor", err)
			return
		}
		if !ok {
			if body.TOTPCode == "" && body.RecoveryCode == "" {
				// The spec's contract: 401 with a type ending /totp-required,
				// and the client resubmits with the code.
				writeProblem(w, r, s.log, http.StatusUnauthorized, "totp-required",
					"Two-factor code required",
					"This account has TOTP enabled. Resubmit with `totp_code`, or a `recovery_code`.")
				return
			}
			s.rejectLogin(w, r)
			return
		}
	}

	s.limiter.succeed(limitKey)
	if err := s.store.TouchUserLogin(r.Context(), user.ID, now); err != nil {
		s.log.Warn("record login", "error", err)
	}
	s.issueSession(w, r, user, http.StatusOK)
}

// rejectLogin is the single answer for every failure: wrong password, unknown
// address, disabled account, bad code. Distinguishing them turns the login form
// into an account enumerator.
func (s *Server) rejectLogin(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusUnauthorized, "invalid-credentials",
		"Invalid credentials", "The email address or password is incorrect.")
}

func (s *Server) secondFactorValid(r *http.Request, user model.User, body loginRequest) (bool, error) {
	if body.RecoveryCode != "" {
		return s.store.ConsumeRecoveryCode(r.Context(), user.ID,
			auth.HashToken(normaliseRecoveryCode(body.RecoveryCode)))
	}
	if body.TOTPCode == "" {
		return false, nil
	}

	secret, err := s.decryptTOTP(user)
	if err != nil {
		return false, err
	}
	return auth.VerifyTOTP(secret, body.TOTPCode, time.Now()), nil
}

// issueSession mints the cookie and the CSRF token that pairs with it.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, user model.User, status int) {
	token, err := auth.NewToken()
	if err != nil {
		s.internal(w, r, "session token", err)
		return
	}
	csrf, err := auth.NewToken()
	if err != nil {
		s.internal(w, r, "csrf token", err)
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	expires := now.Add(sessionLifetime)

	// Only hashes are stored. A database leak then exposes no live session,
	// which is the entire reason the cookie value is not the primary key.
	session := model.Session{
		ID:            model.NewID(),
		UserID:        user.ID,
		TokenHash:     auth.HashToken(token),
		CSRFTokenHash: auth.HashToken(csrf),
		ExpiresAt:     expires,
		CreatedAt:     now,
		IP:            clientIP(r),
		UserAgent:     r.UserAgent(),
	}
	if err := s.store.CreateSession(r.Context(), session); err != nil {
		s.internal(w, r, "create session", err)
		return
	}

	setSessionCookie(w, r, token, expires)

	scopes := auth.ScopesFor(user.Role)
	userBody := toUserJSON(user)
	writeJSON(w, s.log, status, sessionJSON{
		PrincipalType: "user",
		User:          &userBody,
		Scopes:        scopes.Strings(),
		CSRFToken:     &csrf,
		ExpiresAt:     &expires,
	})
}

// logout destroys the current session.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if p != nil && p.SessionID != nil {
		if err := s.store.DeleteSession(r.Context(), *p.SessionID); err != nil {
			s.internal(w, r, "delete session", err)
			return
		}
	}
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// describeSession identifies the caller, whether that is a user or a key.
func (s *Server) describeSession(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		writeProblem(w, r, s.log, http.StatusUnauthorized, "unauthenticated", "Not authenticated", "")
		return
	}

	body := sessionJSON{
		PrincipalType: p.Type,
		Scopes:        p.Scopes.Strings(),
		ExpiresAt:     p.ExpiresAt,
	}
	if p.User != nil {
		u := toUserJSON(*p.User)
		body.User = &u
	}
	if p.APIKeyID != nil {
		id := p.APIKeyID.String()
		body.APIKeyID = &id
	}
	// The CSRF token is deliberately absent here: it is handed out once, with
	// the session, and an endpoint that reissues it on demand would let a page
	// that can make a GET obtain the token that authorises a write.
	writeJSON(w, s.log, http.StatusOK, body)
}

// enrolTOTP begins two-factor enrolment. The secret is stored immediately but
// left unconfirmed, so an enrolment that is abandoned halfway cannot lock anyone
// out of their account.
func (s *Server) enrolTOTP(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	if user.TOTPEnabled() {
		writeProblem(w, r, s.log, http.StatusConflict, "totp-already-enabled",
			"TOTP is already enabled", "Disable it first if you want to enrol a new authenticator.")
		return
	}

	secret, err := auth.NewTOTPSecret()
	if err != nil {
		s.internal(w, r, "totp secret", err)
		return
	}

	sealed, err := s.keeper.Encrypt(secret, s.totpAAD(user.ID))
	if err != nil {
		s.internal(w, r, "encrypt totp secret", err)
		return
	}
	if err := s.store.SetUserTOTP(r.Context(), user.ID, sealed, nil); err != nil {
		s.internal(w, r, "store totp secret", err)
		return
	}

	writeJSON(w, s.log, http.StatusOK, map[string]string{
		"secret":           auth.EncodeTOTPSecret(secret),
		"provisioning_uri": auth.ProvisioningURI(s.issuer(r.Context()), user.Email, secret),
	})
}

// confirmTOTP activates enrolment and issues recovery codes.
func (s *Server) confirmTOTP(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	var body struct {
		TOTPCode string `json:"totp_code"`
	}
	if !decodeJSON(w, r, s.log, &body) {
		return
	}
	if len(user.TOTPSecret) == 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "totp-not-enrolled",
			"No enrolment in progress", "Call POST /api/v1/auth/totp first.")
		return
	}

	secret, err := s.decryptTOTP(user)
	if err != nil {
		s.internal(w, r, "decrypt totp secret", err)
		return
	}
	if !auth.VerifyTOTP(secret, body.TOTPCode, time.Now()) {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "totp-code-invalid",
			"That code is not valid", "Check the time on the device generating it, and try the next code.")
		return
	}

	codes, err := auth.NewRecoveryCodes(recoveryCodeCount)
	if err != nil {
		s.internal(w, r, "recovery codes", err)
		return
	}
	hashes := make([][]byte, len(codes))
	for i, code := range codes {
		hashes[i] = auth.HashToken(code)
	}
	if err := s.store.ReplaceRecoveryCodes(r.Context(), user.ID, hashes); err != nil {
		s.internal(w, r, "store recovery codes", err)
		return
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.store.SetUserTOTP(r.Context(), user.ID, user.TOTPSecret, &now); err != nil {
		s.internal(w, r, "enable totp", err)
		return
	}

	s.log.Info("totp enabled", "user", user.ID.String())
	writeJSON(w, s.log, http.StatusOK, map[string][]string{"recovery_codes": codes})
}

// disableTOTP turns two-factor off. Requires the password and a current code or
// a recovery code — a stolen session must not be enough to remove the factor
// that protects the account.
func (s *Server) disableTOTP(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	var body struct {
		Password     string `json:"password"`
		TOTPCode     string `json:"totp_code"`
		RecoveryCode string `json:"recovery_code"`
	}
	if !decodeJSON(w, r, s.log, &body) {
		return
	}
	if !user.TOTPEnabled() {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "totp-not-enabled",
			"TOTP is not enabled", "There is nothing to disable.")
		return
	}

	valid, err := auth.VerifyPassword(body.Password, user.PasswordHash)
	if err != nil {
		s.internal(w, r, "verify password", err)
		return
	}
	if !valid {
		s.rejectLogin(w, r)
		return
	}

	secondFactor, err := s.secondFactorValid(r, user, loginRequest{TOTPCode: body.TOTPCode, RecoveryCode: body.RecoveryCode})
	if err != nil {
		s.internal(w, r, "verify second factor", err)
		return
	}
	if !secondFactor {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "totp-code-invalid",
			"A valid code or recovery code is required", "Disabling two-factor needs proof you still hold the factor.")
		return
	}

	if err := s.store.SetUserTOTP(r.Context(), user.ID, nil, nil); err != nil {
		s.internal(w, r, "disable totp", err)
		return
	}
	if err := s.store.ReplaceRecoveryCodes(r.Context(), user.ID, nil); err != nil {
		s.internal(w, r, "clear recovery codes", err)
		return
	}

	s.log.Info("totp disabled", "user", user.ID.String())
	w.WriteHeader(http.StatusNoContent)
}

// currentUser resolves the account behind the request, and refuses an API key:
// account settings are a user's own, and a key that could enrol or remove a
// second factor would be a key that could take the account.
func (s *Server) currentUser(w http.ResponseWriter, r *http.Request) (model.User, bool) {
	p, ok := principalFrom(r.Context())
	if !ok || p.User == nil {
		writeProblem(w, r, s.log, http.StatusForbidden, "user-session-required",
			"This operation needs a user session",
			"API keys cannot change account credentials. Sign in and retry.")
		return model.User{}, false
	}

	// Re-read rather than trusting the copy carried on the principal: enrolment
	// wrote a secret that the copy taken at authentication time predates.
	user, err := s.store.UserByID(r.Context(), p.User.ID)
	if err != nil {
		s.internal(w, r, "load user", err)
		return model.User{}, false
	}
	return user, true
}

// totpAAD binds a TOTP ciphertext to the row it belongs to, so a blob moved onto
// another user's row fails to open (data model §12.2).
func (s *Server) totpAAD(userID model.ID) secrets.AAD {
	return secrets.AAD{OrgID: s.orgID[:], Table: "users", Column: "totp_secret", RowID: userID[:]}
}

func (s *Server) decryptTOTP(user model.User) ([]byte, error) {
	return s.keeper.Decrypt(user.TOTPSecret, s.totpAAD(user.ID))
}

// issuer is what an authenticator app shows beside the account. The name the
// operator typed at setup wins over the flag, because that is the one they will
// recognise on their phone.
func (s *Server) issuer(ctx context.Context) string {
	if name, err := s.store.InstanceName(ctx, s.orgID); err == nil && name != "" {
		return name
	}
	return s.instanceName
}

func normaliseEmail(raw string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" {
		return "", false
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return email, false
	}
	return email, true
}

// normaliseRecoveryCode forgives the ways a code gets retyped: different case,
// spaces, missing dashes.
func normaliseRecoveryCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	clean := b.String()
	if len(clean) != 12 {
		return clean
	}
	return clean[0:4] + "-" + clean[4:8] + "-" + clean[8:12]
}

func decodeJSON(w http.ResponseWriter, r *http.Request, log *slog.Logger, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeProblem(w, r, log, http.StatusBadRequest, "malformed-json", "Malformed request body", err.Error())
		return false
	}
	return true
}

// isUniqueViolation recognises a constraint failure without importing the
// driver's error types into every caller.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}
