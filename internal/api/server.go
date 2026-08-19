package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	"github.com/webloomlabs/uptime-cairn/internal/ui"
	"github.com/webloomlabs/uptime-cairn/internal/version"
)

// MonitorStore is the monitoring half of what the API needs from persistence.
type MonitorStore interface {
	CreateMonitor(ctx context.Context, m model.Monitor) error
	GetMonitor(ctx context.Context, id model.ID) (store.MonitorWithState, error)
	ListMonitors(ctx context.Context, after *store.Cursor, limit int) ([]store.MonitorWithState, bool, error)
	DeleteMonitor(ctx context.Context, id model.ID) error
	MonitorByPushToken(ctx context.Context, hash []byte) (model.Monitor, error)
	ListHeartbeats(ctx context.Context, id model.ID, before *time.Time, limit int, importantOnly bool) ([]model.Heartbeat, bool, error)

	// History and uptime read the rollup tiers, or raw heartbeats where those
	// still cover the range. RawCovers is what decides between them.
	RawCovers(ctx context.Context, id model.ID, from time.Time, tier string) (bool, error)
	HistoryFromRaw(ctx context.Context, id model.ID, from, to time.Time, interval time.Duration) ([]store.HistoryBucket, error)
	HistoryFromTier(ctx context.Context, id model.ID, from, to time.Time, tier string) ([]store.HistoryBucket, error)
	UptimeFromRaw(ctx context.Context, id model.ID, from, to time.Time) (store.HistoryBucket, error)
	UptimeFromTier(ctx context.Context, id model.ID, from, to time.Time, tier string) (store.HistoryBucket, error)
}

// IdentityStore is the credentials half.
type IdentityStore interface {
	CountUsers(ctx context.Context) (int, error)
	CreateUser(ctx context.Context, u model.User) error
	UserByEmail(ctx context.Context, orgID model.ID, email string) (model.User, error)
	UserByID(ctx context.Context, id model.ID) (model.User, error)
	SetUserTOTP(ctx context.Context, id model.ID, secret []byte, enabledAt *time.Time) error
	TouchUserLogin(ctx context.Context, id model.ID, at time.Time) error
	ReplaceRecoveryCodes(ctx context.Context, userID model.ID, hashes [][]byte) error
	ConsumeRecoveryCode(ctx context.Context, userID model.ID, hash []byte) (bool, error)

	CreateSession(ctx context.Context, s model.Session) error
	SessionByTokenHash(ctx context.Context, hash []byte, now time.Time) (model.Session, error)
	TouchSession(ctx context.Context, id model.ID, at time.Time) error
	DeleteSession(ctx context.Context, id model.ID) error
	DeleteUserSessions(ctx context.Context, userID model.ID) error

	InstanceName(ctx context.Context, orgID model.ID) (string, error)
	SetInstanceName(ctx context.Context, orgID model.ID, name string) error

	CreateAPIKey(ctx context.Context, k model.APIKey) error
	APIKeyByHash(ctx context.Context, hash []byte) (model.APIKey, error)
	GetAPIKey(ctx context.Context, id model.ID) (model.APIKey, error)
	ListAPIKeys(ctx context.Context, after *store.Cursor, limit int) ([]model.APIKey, bool, error)
	UpdateAPIKey(ctx context.Context, k model.APIKey) error
	RevokeAPIKey(ctx context.Context, id model.ID, at time.Time) error
	TouchAPIKey(ctx context.Context, id model.ID, at time.Time) error
}

// Store is everything the API touches. Named by the consumer, so no handler can
// reach a backend-specific method by accident.
type Store interface {
	MonitorStore
	IdentityStore
	ChannelStore
	MaintenanceStore
}

// Page sizes. The server caps rather than rejects, per the spec: a client
// asking for 10,000 rows gets 100, not a 400.
const (
	defaultLimit = 25
	maxLimit     = 100
)

// Server serves /api/v1 and the embedded UI on one port. The dashboard is an
// ordinary API client and gets no privileged channel (PHASE-1-PLAN.md §2).
type Server struct {
	store    Store
	notify   Notifier
	sweeps   Notifier
	push     PushIngest
	alerts   Alerts
	registry *check.Registry
	keeper   *secrets.Keeper
	log      *slog.Logger
	limiter  *loginLimiter

	// vault seals and opens notification-channel secrets, and configs does the
	// same for the credential half of a monitor's configuration. Both are built
	// from the keeper the TOTP path uses, so there is one key hierarchy rather
	// than three.
	vault   *notify.Vault
	configs *secrets.Vault

	// instanceName is the issuer shown in an authenticator app.
	instanceName string
	orgID        model.ID
}

// Notifier is the control plane's assignment publisher. The API calls it after
// any write, which is what turns "the user created a monitor" into "the probe is
// checking it" without a poll in between.
type Notifier interface{ Notify() }

// New returns a server.
func New(s Store, publisher Notifier, sweeps Notifier, push PushIngest, alerts Alerts, registry *check.Registry, keeper *secrets.Keeper, log *slog.Logger, instanceName string) *Server {
	if instanceName == "" {
		instanceName = "Uptime Cairn"
	}
	return &Server{
		store:        s,
		notify:       publisher,
		sweeps:       sweeps,
		push:         push,
		alerts:       alerts,
		registry:     registry,
		keeper:       keeper,
		vault:        notify.NewVault(keeper),
		configs:      secrets.NewVault(keeper, "monitors", "config"),
		log:          log,
		limiter:      newLoginLimiter(),
		instanceName: instanceName,
		orgID:        model.SentinelOrgID,
	}
}

// Handler builds the routing table. Scopes come from each operation's
// x-cairn-scopes in docs/api/openapi.yaml.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Operational endpoints sit outside the versioned surface and outside auth,
	// because a health check that needs a credential is a health check that
	// stops working at the worst moment.
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.health)

	// Unauthenticated by design, and each one says why in its handler: setup is
	// available only until an administrator exists, and login is how a caller
	// obtains a credential in the first place.
	public := http.NewServeMux()
	public.HandleFunc("GET /api/v1/setup", s.setupStatus)
	public.HandleFunc("POST /api/v1/setup", s.completeSetup)
	public.HandleFunc("POST /api/v1/auth/login", s.login)
	// The dead-man's-switch ingest. Unauthenticated by design — see push.go.
	public.HandleFunc("GET /api/v1/push/{pushToken}", s.pushHeartbeat)
	public.HandleFunc("POST /api/v1/push/{pushToken}", s.pushHeartbeat)

	authed := http.NewServeMux()
	authed.HandleFunc("POST /api/v1/auth/logout", s.logout)
	authed.HandleFunc("GET /api/v1/auth/session", s.describeSession)
	authed.HandleFunc("POST /api/v1/auth/totp", s.enrolTOTP)
	authed.HandleFunc("POST /api/v1/auth/totp/confirm", s.confirmTOTP)
	authed.HandleFunc("DELETE /api/v1/auth/totp", s.disableTOTP)

	authed.HandleFunc("GET /api/v1/api-keys", s.require(auth.ScopeAPIKeysRead, s.listAPIKeys))
	authed.HandleFunc("POST /api/v1/api-keys", s.require(auth.ScopeAPIKeysWrite, s.createAPIKey))
	authed.HandleFunc("GET /api/v1/api-keys/{apiKeyId}", s.require(auth.ScopeAPIKeysRead, s.getAPIKey))
	authed.HandleFunc("PATCH /api/v1/api-keys/{apiKeyId}", s.require(auth.ScopeAPIKeysWrite, s.updateAPIKey))
	authed.HandleFunc("DELETE /api/v1/api-keys/{apiKeyId}", s.require(auth.ScopeAPIKeysWrite, s.revokeAPIKey))

	authed.HandleFunc("GET /api/v1/monitors", s.require(auth.ScopeMonitorsRead, s.listMonitors))
	authed.HandleFunc("POST /api/v1/monitors", s.require(auth.ScopeMonitorsWrite, s.createMonitor))
	authed.HandleFunc("GET /api/v1/monitors/{monitorId}", s.require(auth.ScopeMonitorsRead, s.getMonitor))
	authed.HandleFunc("DELETE /api/v1/monitors/{monitorId}", s.require(auth.ScopeMonitorsWrite, s.deleteMonitor))
	authed.HandleFunc("GET /api/v1/monitors/{monitorId}/heartbeats", s.require(auth.ScopeHeartbeatsRead, s.listHeartbeats))
	authed.HandleFunc("GET /api/v1/monitors/{monitorId}/history", s.require(auth.ScopeHeartbeatsRead, s.getMonitorHistory))
	authed.HandleFunc("GET /api/v1/monitors/{monitorId}/uptime", s.require(auth.ScopeHeartbeatsRead, s.getMonitorUptime))

	// Notification channels. The two literal sub-paths are registered before the
	// wildcard for readability only — Go's mux resolves by specificity, so
	// /preview cannot be swallowed by /{channelId} whatever the order here.
	authed.HandleFunc("GET /api/v1/notification-channels", s.require(auth.ScopeNotificationsRead, s.listNotificationChannels))
	authed.HandleFunc("POST /api/v1/notification-channels", s.require(auth.ScopeNotificationsWrit, s.createNotificationChannel))
	authed.HandleFunc("GET /api/v1/notification-channels/template-variables", s.require(auth.ScopeNotificationsRead, s.listTemplateVariables))
	authed.HandleFunc("POST /api/v1/notification-channels/preview", s.require(auth.ScopeNotificationsRead, s.previewNotificationTemplate))
	authed.HandleFunc("GET /api/v1/notification-channels/{channelId}", s.require(auth.ScopeNotificationsRead, s.getNotificationChannel))
	authed.HandleFunc("PATCH /api/v1/notification-channels/{channelId}", s.require(auth.ScopeNotificationsWrit, s.updateNotificationChannel))
	authed.HandleFunc("DELETE /api/v1/notification-channels/{channelId}", s.require(auth.ScopeNotificationsWrit, s.deleteNotificationChannel))
	authed.HandleFunc("POST /api/v1/notification-channels/{channelId}/test", s.require(auth.ScopeNotificationsWrit, s.testNotificationChannel))

	authed.HandleFunc("GET /api/v1/maintenance-windows", s.require(auth.ScopeMaintenanceRead, s.listMaintenanceWindows))
	authed.HandleFunc("POST /api/v1/maintenance-windows", s.require(auth.ScopeMaintenanceWrite, s.createMaintenanceWindow))
	authed.HandleFunc("GET /api/v1/maintenance-windows/{maintenanceWindowId}", s.require(auth.ScopeMaintenanceRead, s.getMaintenanceWindow))
	authed.HandleFunc("PATCH /api/v1/maintenance-windows/{maintenanceWindowId}", s.require(auth.ScopeMaintenanceWrite, s.updateMaintenanceWindow))
	authed.HandleFunc("DELETE /api/v1/maintenance-windows/{maintenanceWindowId}", s.require(auth.ScopeMaintenanceWrite, s.deleteMaintenanceWindow))

	authed.HandleFunc("/api/v1/", s.notImplemented)

	// One dispatcher: public routes first, everything else behind
	// authentication. Registering both on one mux would make the difference a
	// matter of pattern precedence, which is exactly the kind of thing that
	// silently opens an endpoint during a refactor.
	protected := s.authenticate(authed)
	mux.Handle("/api/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := public.Handler(r); pattern != "" {
			public.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	}))

	// Registered without a method: "GET /" would be more specific in method and
	// less specific in path than "/api/v1/", which Go rejects as ambiguous.
	if assets, err := ui.FS(); err == nil {
		mux.Handle("/", http.FileServerFS(assets))
	} else {
		s.log.Error("embedded UI unavailable", "error", err)
	}

	return s.logRequests(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.log, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": version.Version,
	})
}

// notImplemented answers the rest of the specified surface honestly. A 404 would
// imply the endpoint does not exist; it does, in the frozen contract, and this
// build has not caught up.
func (s *Server) notImplemented(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotImplemented, "not-implemented",
		"Not implemented",
		"This endpoint is in the API contract but not in this build. See docs/api/openapi.yaml and docs/plans/PHASE-1-PLAN.md.")
}

// logRequests is deliberately minimal: method, path, status, duration. Query
// strings are not logged, because a push token or an API key ends up in one
// eventually and logs outlive the request that made them.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(rec, r)

		s.log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Microsecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
