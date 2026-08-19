package api

import (
	"context"
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
	"github.com/webloomlabs/uptime-cairn/internal/rollup"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Instance settings and the account endpoints.
//
// Settings that nothing reads would be a checklist that flatters itself, so it
// is worth being explicit about which sections this build actually consults:
//
//   - general.instance_name is the issuer an authenticator app shows and the
//     name on every alert, applied on save.
//   - general.base_url is what a push URL and a status page link are built from.
//   - retention is handed to the rollup runner on save, so a change takes effect
//     on the next sweep rather than on the next restart.
//   - smtp is what makes an email channel's use_instance_smtp mean something.
//     Until this endpoint existed that flag had nowhere to read from, and the
//     channel was refused at save time with a message saying so.
//   - monitoring supplies the defaults a newly created monitor inherits.
//
// appearance, security, and telemetry are stored and not yet consulted:
// appearance belongs to a dashboard this build does not ship, security's limits
// are compiled in, and telemetry has no exporter. They are persisted rather than
// refused so that a client can round-trip the whole document, and this comment
// is the honest list rather than a claim that all seven are live.

const maxSettingsBody = 1 << 16

// Retuner is the rollup runner's retention setter. Declared here by the
// consumer, so the API does not import the runner's concrete type.
type Retuner interface {
	SetRetention(r rollup.Retention)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetSettings(r.Context(), s.orgID)
	if err != nil {
		s.internal(w, r, "get settings", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toSettingsJSON(s.withDefaults(settings)))
}

// withDefaults fills in what the schema documents, so a caller reading settings
// on a fresh install sees the values that are actually in force rather than a
// document of empty strings.
func (s *Server) withDefaults(settings model.Settings) model.Settings {
	if settings.General.InstanceName == "" {
		settings.General.InstanceName = s.instanceName
	}
	if settings.Retention.RawDays == nil {
		defaults := rollup.DefaultRetention()
		settings.Retention = model.RetentionSettings{
			RawDays:             &defaults.RawDays,
			Rollup1mDays:        &defaults.Rollup1mDays,
			Rollup5mDays:        &defaults.Rollup5mDays,
			Rollup1hDays:        &defaults.Rollup1hDays,
			Rollup1dDays:        &defaults.Rollup1dDays,
			WebhookDeliveryDays: &defaults.WebhookDeliveryDays,
		}
	}
	if settings.SMTP.Encryption == "" {
		settings.SMTP.Encryption = model.SMTPStartTLS
	}
	return settings
}

// updateSettings merges the request onto the stored settings.
//
// Merged section by section rather than field by field: the spec says every
// section is optional on update, and a section that is present is a statement
// about that section as a whole. Within a section, an absent field leaves the
// stored value alone — which is what makes changing one retention tier possible
// without restating the other five.
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	stored, err := s.store.GetSettings(r.Context(), s.orgID)
	if err != nil {
		s.internal(w, r, "get settings", err)
		return
	}

	var body settingsWrite
	if !s.readBody(w, r, maxSettingsBody, &body) {
		return
	}

	settings := s.withDefaults(stored)
	settings.OrgID = s.orgID
	settings.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)

	problems := s.applySettings(&settings, stored, body)
	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "The settings were not changed.", problems...)
		return
	}

	if err := s.store.SaveSettings(r.Context(), settings); err != nil {
		s.internal(w, r, "save settings", err)
		return
	}

	// Applied rather than merely stored. Each of these has a consumer that read
	// its value at startup, and a setting whose effect waits for a restart is a
	// setting an operator changes twice.
	if settings.General.InstanceName != "" && settings.General.InstanceName != stored.General.InstanceName {
		if err := s.store.SetInstanceName(r.Context(), s.orgID, settings.General.InstanceName); err != nil {
			s.internal(w, r, "set instance name", err)
			return
		}
		s.instanceName = settings.General.InstanceName
	}
	s.baseURL = settings.General.BaseURL
	if s.retuner != nil && body.Retention != nil {
		s.retuner.SetRetention(retentionFrom(settings.Retention))
	}
	if body.SMTP != nil {
		s.applyInstanceSMTP(settings)
	}

	s.log.Info("settings updated")
	writeJSON(w, s.log, http.StatusOK, toSettingsJSON(settings))
}

// applyInstanceSMTP installs the relay for the delivery path.
//
// The password is opened here and held in memory only. It has to be recoverable
// — SMTP replays it on every connection — which is why it is encrypted rather
// than hashed, and why this is the one place the plaintext exists outside the
// moment of the write that created it.
func (s *Server) applyInstanceSMTP(settings model.Settings) {
	relay := notify.InstanceSMTP{
		Host:        settings.SMTP.Host,
		Port:        settings.SMTP.Port,
		Username:    settings.SMTP.Username,
		Encryption:  settings.SMTP.Encryption,
		FromAddress: settings.SMTP.FromAddress,
		FromName:    settings.SMTP.FromName,
	}
	if len(settings.SMTP.PasswordSealed) > 0 {
		password, err := s.settingsVault.Open(s.orgID[:], s.orgID[:], settings.SMTP.PasswordSealed)
		if err != nil {
			// Reported and the relay installed without it. A relay that needs no
			// authentication is common, and refusing to install one because a
			// password will not open would take mail down over a key problem the
			// operator can see in the log.
			s.log.Error("open instance smtp password", "error", err)
		} else {
			relay.Password = string(password)
		}
	}
	notify.SetInstanceSMTP(relay)
}

// LoadSettings applies the stored settings to the running process.
//
// Called once at startup, before the listener opens, so an install that was
// configured yesterday behaves today the way it was configured rather than the
// way the defaults say. It is a method on the server because the vault that
// opens the SMTP password is the server's.
func (s *Server) LoadSettings(ctx context.Context) error {
	settings, err := s.store.GetSettings(ctx, s.orgID)
	if err != nil {
		return err
	}
	settings = s.withDefaults(settings)

	if settings.General.InstanceName != "" {
		s.instanceName = settings.General.InstanceName
	}
	if s.retuner != nil {
		s.retuner.SetRetention(retentionFrom(settings.Retention))
	}
	s.applyInstanceSMTP(settings)
	s.defaults = settings.Monitoring
	s.baseURL = settings.General.BaseURL
	return nil
}

func (s *Server) applySettings(settings *model.Settings, stored model.Settings, body settingsWrite) []ValidationItem {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	if body.General != nil {
		if body.General.InstanceName != "" {
			if len(body.General.InstanceName) > 200 {
				bad("/general/instance_name", "too_long", "instance_name must be at most 200 characters")
			} else {
				settings.General.InstanceName = body.General.InstanceName
			}
		}
		if body.General.BaseURL != "" {
			parsed, err := url.ParseRequestURI(body.General.BaseURL)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				bad("/general/base_url", "invalid", "base_url must be an absolute http or https URL")
			} else {
				settings.General.BaseURL = strings.TrimSuffix(body.General.BaseURL, "/")
			}
		}
		if body.General.Timezone != "" {
			if _, err := time.LoadLocation(body.General.Timezone); err != nil {
				bad("/general/timezone", "invalid",
					fmt.Sprintf("timezone %q is not an IANA zone name", body.General.Timezone))
			} else {
				settings.General.Timezone = body.General.Timezone
			}
		}
		if body.General.Locale != "" {
			settings.General.Locale = body.General.Locale
		}
	}

	if body.Appearance != nil {
		if body.Appearance.Theme != "" {
			switch body.Appearance.Theme {
			case "light", "dark", "auto":
				settings.Appearance.Theme = body.Appearance.Theme
			default:
				bad("/appearance/theme", "invalid", "theme must be light, dark, or auto")
			}
		}
		if body.Appearance.PrimaryColor != nil {
			if *body.Appearance.PrimaryColor != "" && !hexColour.MatchString(*body.Appearance.PrimaryColor) {
				bad("/appearance/primary_color", "invalid", "primary_color must be a hex triple such as #6b7280")
			} else {
				settings.Appearance.PrimaryColor = body.Appearance.PrimaryColor
			}
		}
	}

	if body.Retention != nil {
		problems = append(problems, applyRetention(&settings.Retention, *body.Retention)...)
	}
	if body.SMTP != nil {
		problems = append(problems, s.applySMTP(settings, stored, *body.SMTP)...)
	}
	if body.Monitoring != nil {
		problems = append(problems, applyMonitoringDefaults(&settings.Monitoring, *body.Monitoring)...)
		if len(problems) == 0 {
			// Held on the server so createMonitor reads it without a round trip
			// on every write.
			s.defaults = settings.Monitoring
		}
	}
	if body.Security != nil {
		problems = append(problems, applySecurity(&settings.Security, *body.Security)...)
	}
	if body.Telemetry != nil && body.Telemetry.Enabled != nil {
		settings.Telemetry.Enabled = *body.Telemetry.Enabled
	}
	return problems
}

// applyRetention folds the tiers and then checks the one rule that makes the
// chain coherent: a coarser tier is kept at least as long as a finer one.
// Reversed, history develops a hole in the middle — detail retained past the
// summary that replaced it.
func applyRetention(into *model.RetentionSettings, body model.RetentionSettings) []ValidationItem {
	var problems []ValidationItem

	for _, field := range []struct {
		pointer  string
		supplied *int
		into     **int
		minimum  int
	}{
		{"/retention/raw_days", body.RawDays, &into.RawDays, 1},
		{"/retention/rollup_1m_days", body.Rollup1mDays, &into.Rollup1mDays, 0},
		{"/retention/rollup_5m_days", body.Rollup5mDays, &into.Rollup5mDays, 0},
		{"/retention/rollup_1h_days", body.Rollup1hDays, &into.Rollup1hDays, 0},
		{"/retention/rollup_1d_days", body.Rollup1dDays, &into.Rollup1dDays, 0},
		{"/retention/webhook_delivery_days", body.WebhookDeliveryDays, &into.WebhookDeliveryDays, 1},
	} {
		if field.supplied == nil {
			continue
		}
		if *field.supplied < field.minimum {
			problems = append(problems, ValidationItem{Pointer: field.pointer, Code: "below_minimum",
				Message: fmt.Sprintf("must be at least %d", field.minimum)})
			continue
		}
		value := *field.supplied
		*field.into = &value
	}
	if len(problems) > 0 {
		return problems
	}

	// The runner owns the rule, so it is checked by the runner's own Validate
	// rather than restated here. Two copies of "coarser must outlive finer" is
	// one copy too many.
	if err := retentionFrom(*into).Validate(); err != nil {
		problems = append(problems, ValidationItem{Pointer: "/retention", Code: "incoherent",
			Message: err.Error()})
	}
	return problems
}

func retentionFrom(settings model.RetentionSettings) rollup.Retention {
	out := rollup.DefaultRetention()
	for _, field := range []struct {
		supplied *int
		into     *int
	}{
		{settings.RawDays, &out.RawDays},
		{settings.Rollup1mDays, &out.Rollup1mDays},
		{settings.Rollup5mDays, &out.Rollup5mDays},
		{settings.Rollup1hDays, &out.Rollup1hDays},
		{settings.Rollup1dDays, &out.Rollup1dDays},
		{settings.WebhookDeliveryDays, &out.WebhookDeliveryDays},
	} {
		if field.supplied != nil {
			*field.into = *field.supplied
		}
	}
	return out
}

// applySMTP validates the relay and seals its password.
//
// The password is encrypted rather than hashed for the reason the data model
// gives: SMTP replays it on every connection, so it has to be recoverable
// (§12.1). It lives inside the section's JSON as a sealed envelope, which is why
// adding it needed no migration and why the read shape has nowhere to put it.
func (s *Server) applySMTP(settings *model.Settings, stored model.Settings, body smtpWrite) []ValidationItem {
	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	if body.Host != nil {
		settings.SMTP.Host = strings.TrimSpace(*body.Host)
	}
	if body.Port != nil {
		if *body.Port < 1 || *body.Port > 65535 {
			bad("/smtp/port", "invalid", "port must be between 1 and 65535")
		} else {
			settings.SMTP.Port = *body.Port
		}
	}
	if body.Username != nil {
		settings.SMTP.Username = *body.Username
	}
	if body.Encryption != nil {
		switch *body.Encryption {
		case model.SMTPNone, model.SMTPStartTLS, model.SMTPTLS:
			settings.SMTP.Encryption = *body.Encryption
		default:
			bad("/smtp/encryption", "invalid", "encryption must be none, starttls, or tls")
		}
	}
	if body.FromAddress != nil {
		if *body.FromAddress != "" {
			if _, err := mail.ParseAddress(*body.FromAddress); err != nil {
				bad("/smtp/from_address", "invalid", "from_address must be an email address")
			} else {
				settings.SMTP.FromAddress = *body.FromAddress
			}
		} else {
			settings.SMTP.FromAddress = ""
		}
	}
	if body.FromName != nil {
		settings.SMTP.FromName = *body.FromName
	}

	// The stored envelope carries forward unless this request replaced it, so
	// changing the port does not silently clear the password.
	settings.SMTP.PasswordSealed = stored.SMTP.PasswordSealed
	if len(body.Password) > 0 {
		var supplied *string
		if err := json.Unmarshal(body.Password, &supplied); err != nil {
			bad("/smtp/password", "invalid", "password must be a string or null")
		} else if supplied == nil || *supplied == "" {
			settings.SMTP.PasswordSealed = nil
		} else if *supplied == model.Redacted {
			// A client round-tripping its own read never sees the password, so a
			// marker here means somebody typed it. Refused rather than stored,
			// because storing it produces a relay that authenticates as nobody
			// and fails hours later against the mail server.
			bad("/smtp/password", "redacted",
				"supply the real password, or omit the field to leave the stored one alone")
		} else {
			sealed, err := s.settingsVault.Seal(s.orgID[:], s.orgID[:], []byte(*supplied))
			if err != nil {
				s.log.Error("seal smtp password", "error", err)
				bad("/smtp/password", "unavailable", "the password could not be stored")
			} else {
				settings.SMTP.PasswordSealed = sealed
			}
		}
	}

	// Refused as a pair rather than field by field: a relay with a host and no
	// sender address is one that fails on its first message, and the operator
	// finds out when an alert does not arrive.
	if settings.SMTP.Host != "" && settings.SMTP.FromAddress == "" {
		bad("/smtp/from_address", "required",
			"from_address is required once a host is set, or nothing sent through this relay will have a sender")
	}
	return problems
}

func applyMonitoringDefaults(into *model.MonitoringSettings, body model.MonitoringSettings) []ValidationItem {
	var problems []ValidationItem

	for _, field := range []struct {
		pointer  string
		supplied *int
		into     **int
		minimum  int
		maximum  int
	}{
		{"/monitoring/default_interval_seconds", body.DefaultIntervalSeconds, &into.DefaultIntervalSeconds, 20, 86400},
		{"/monitoring/default_timeout_seconds", body.DefaultTimeoutSeconds, &into.DefaultTimeoutSeconds, 1, 300},
		{"/monitoring/default_retries", body.DefaultRetries, &into.DefaultRetries, 0, 20},
		{"/monitoring/max_concurrent_checks", body.MaxConcurrentChecks, &into.MaxConcurrentChecks, 1, 1 << 20},
	} {
		if field.supplied == nil {
			continue
		}
		if *field.supplied < field.minimum || *field.supplied > field.maximum {
			problems = append(problems, ValidationItem{Pointer: field.pointer, Code: "out_of_range",
				Message: fmt.Sprintf("must be between %d and %d", field.minimum, field.maximum)})
			continue
		}
		value := *field.supplied
		*field.into = &value
	}

	// The same rule createMonitor enforces, applied to the defaults: a default
	// pair that no monitor could be created with would be a form that refuses
	// its own prefilled values.
	interval, timeout := 60, 30
	if into.DefaultIntervalSeconds != nil {
		interval = *into.DefaultIntervalSeconds
	}
	if into.DefaultTimeoutSeconds != nil {
		timeout = *into.DefaultTimeoutSeconds
	}
	if timeout >= interval {
		problems = append(problems, ValidationItem{
			Pointer: "/monitoring/default_timeout_seconds", Code: "not_less_than_interval",
			Message: "default_timeout_seconds must be less than default_interval_seconds"})
	}

	if body.DefaultNotificationChannelIDs != nil {
		into.DefaultNotificationChannelIDs = body.DefaultNotificationChannelIDs
	}
	return problems
}

func applySecurity(into *model.SecuritySettings, body model.SecuritySettings) []ValidationItem {
	var problems []ValidationItem

	for _, field := range []struct {
		pointer  string
		supplied *int
		into     **int
		minimum  int
	}{
		{"/security/session_timeout_minutes", body.SessionTimeoutMinutes, &into.SessionTimeoutMinutes, 5},
		{"/security/login_rate_limit_per_minute", body.LoginRateLimitPerMinute, &into.LoginRateLimitPerMinute, 1},
		{"/security/api_rate_limit_per_minute", body.APIRateLimitPerMinute, &into.APIRateLimitPerMinute, 1},
	} {
		if field.supplied == nil {
			continue
		}
		if *field.supplied < field.minimum {
			problems = append(problems, ValidationItem{Pointer: field.pointer, Code: "below_minimum",
				Message: fmt.Sprintf("must be at least %d", field.minimum)})
			continue
		}
		value := *field.supplied
		*field.into = &value
	}
	if body.RequireTOTP != nil {
		into.RequireTOTP = body.RequireTOTP
	}
	if body.TrustedProxies != nil {
		into.TrustedProxies = body.TrustedProxies
	}
	return problems
}

// --------------------------------------------------------------------------
// Accounts
// --------------------------------------------------------------------------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context(), s.orgID)
	if err != nil {
		s.internal(w, r, "list users", err)
		return
	}

	// Not cursor-paginated. A Phase 1 install has one account, and the endpoint
	// exists so a client written against it does not have to be rewritten when
	// Phase 3 adds the second — at which point the page shape is the change,
	// not the caller.
	data := make([]userJSON, 0, len(users))
	for _, u := range users {
		data = append(data, toUserJSON(u))
	}
	writeJSON(w, s.log, http.StatusOK, page[userJSON]{Data: data})
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.taxonomyID(w, r, "userId", s.userNotFound)
	if !ok {
		return
	}
	user, err := s.store.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.userNotFound(w, r)
		return
	} else if err != nil {
		s.internal(w, r, "get user", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, toUserJSON(user))
}

func (s *Server) getCurrentUser(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	writeJSON(w, s.log, http.StatusOK, toUserJSON(user))
}

// updateCurrentUser changes the caller's own profile.
//
// Changing an email address or a password requires the current password, and it
// is verified rather than merely present. A live session is ambient
// authentication — a borrowed laptop, a stolen cookie — and the two fields that
// would let somebody take the account over permanently are exactly the two worth
// a second proof.
func (s *Server) updateCurrentUser(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	var body currentUserUpdate
	if !s.readBody(w, r, 1<<14, &body) {
		return
	}

	var problems []ValidationItem
	bad := func(pointer, code, message string) {
		problems = append(problems, ValidationItem{Pointer: pointer, Code: code, Message: message})
	}

	changingEmail := body.Email != nil && !strings.EqualFold(*body.Email, user.Email)
	changingPassword := body.NewPassword != nil && *body.NewPassword != ""

	if changingEmail || changingPassword {
		if body.CurrentPassword == nil || *body.CurrentPassword == "" {
			bad("/current_password", "required",
				"current_password is required when changing your email address or password")
		} else {
			valid, err := auth.VerifyPassword(*body.CurrentPassword, user.PasswordHash)
			if err != nil {
				s.log.Error("verify current password", "error", err)
			}
			if !valid {
				bad("/current_password", "incorrect", "that is not your current password")
			}
		}
	}

	if changingEmail {
		address, err := mail.ParseAddress(*body.Email)
		if err != nil {
			bad("/email", "invalid", "email must be an email address")
		} else {
			lowered := strings.ToLower(address.Address)
			// Checked before the write so a collision is a 422 naming the field
			// rather than a unique-index failure.
			if existing, err := s.store.UserByEmail(r.Context(), s.orgID, lowered); err == nil && existing.ID != user.ID {
				bad("/email", "taken", "another account already uses that email address")
			}
			user.Email = lowered
		}
	}
	if changingPassword {
		if len(*body.NewPassword) < 12 {
			bad("/new_password", "too_short", "new_password must be at least 12 characters")
		} else {
			hash, err := auth.HashPassword(*body.NewPassword)
			if err != nil {
				s.internal(w, r, "hash password", err)
				return
			}
			user.PasswordHash = hash
		}
	}

	if body.Name != nil {
		if len(*body.Name) > 200 {
			bad("/name", "too_long", "name must be at most 200 characters")
		} else {
			user.Name = *body.Name
		}
	}
	if body.Timezone != nil {
		if *body.Timezone != "" {
			if _, err := time.LoadLocation(*body.Timezone); err != nil {
				bad("/timezone", "invalid", fmt.Sprintf("timezone %q is not an IANA zone name", *body.Timezone))
			} else {
				user.Timezone = *body.Timezone
			}
		} else {
			user.Timezone = ""
		}
	}
	if body.Locale != nil {
		user.Locale = *body.Locale
	}

	if len(problems) > 0 {
		writeProblem(w, r, s.log, http.StatusUnprocessableEntity, "validation-failed",
			"Validation failed", "Your account was not changed.", problems...)
		return
	}

	user.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	if err := s.store.UpdateUserProfile(r.Context(), user); err != nil {
		s.internal(w, r, "update user", err)
		return
	}

	// A password change ends every other session. Somebody who changes their
	// password because they think it was compromised has to be right about what
	// that accomplishes.
	if changingPassword {
		if err := s.store.DeleteUserSessions(r.Context(), user.ID); err != nil {
			s.log.Error("clear sessions after password change", "error", err)
		}
		clearSessionCookie(w, r)
	}

	writeJSON(w, s.log, http.StatusOK, toUserJSON(user))
}

func (s *Server) userNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "user-not-found",
		"User not found", "No user with that identifier exists.")
}
