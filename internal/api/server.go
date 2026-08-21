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
	ListMonitors(ctx context.Context, after *store.Cursor, limit int, filter store.MonitorFilter) ([]store.MonitorWithState, bool, error)
	DeleteMonitor(ctx context.Context, id model.ID) error
	MonitorByPushToken(ctx context.Context, hash []byte) (model.Monitor, error)
	ListHeartbeats(ctx context.Context, id model.ID, before *time.Time, limit int, importantOnly bool) ([]model.Heartbeat, bool, error)

	UpdateMonitor(ctx context.Context, m model.Monitor) error
	SetMonitorEnabled(ctx context.Context, id model.ID, enabled bool, at time.Time) error

	// Membership is ADR-004's reconciliation signal: a version and a count for a
	// filter, cheap enough to poll every few seconds per open view.
	Membership(ctx context.Context, filter store.MonitorFilter) (store.Membership, error)

	// The include= embeds. Each takes the whole page's ids, because the
	// alternative is a query per row on the endpoint the load gate covers.
	LastHeartbeats(ctx context.Context, ids []model.ID) (map[model.ID]model.Heartbeat, error)
	UptimeRatios(ctx context.Context, ids []model.ID, window string) (map[model.ID]float64, error)

	StatusCounts(ctx context.Context) (map[string]int, error)
	GetCertificate(ctx context.Context, id model.ID) (model.Certificate, error)
	ExpiringSoon(ctx context.Context, before time.Time) (certificates, domains int, err error)
	DailyUptime(ctx context.Context, ids []model.ID, from, to time.Time) (map[model.ID][]store.DailyUptime, error)

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

	ListUsers(ctx context.Context, orgID model.ID) ([]model.User, error)
	UpdateUserProfile(ctx context.Context, u model.User) error

	GetSettings(ctx context.Context, orgID model.ID) (model.Settings, error)
	SaveSettings(ctx context.Context, set model.Settings) error

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
	TaxonomyStore
	IncidentStore
	StatusPageStore
	WebhookStore
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
	checks   *checkLimiter
	retuner  Retuner

	// vault seals and opens notification-channel secrets, and configs does the
	// same for the credential half of a monitor's configuration. Both are built
	// from the keeper the TOTP path uses, so there is one key hierarchy rather
	// than three.
	vault   *notify.Vault
	configs *secrets.Vault

	// webhooks seals a webhook's signing secret and headers; settingsVault seals
	// the instance SMTP password; subscribers seals a status page subscriber's
	// address. Three vaults rather than one because the AAD binds an envelope to
	// its table and column, which is what makes a blob moved from one row to
	// another fail to open (data model §12.2).
	webhooks      *secrets.Vault
	settingsVault *secrets.Vault
	subscribers   *secrets.Vault

	// outbound is the webhook delivery engine. Nil in a build or a test that is
	// not running one, which the redeliver endpoint reports rather than panics
	// on.
	outbound Redeliverer

	// relay delivers to status page subscribers: the confirmation a new
	// subscription needs, and the incident updates that are the whole reason
	// somebody subscribed. Nil in a test that is not exercising it, which every
	// call site checks — a status page must still work on an install with no
	// mail relay, it just cannot promise anybody anything.
	relay SubscriberRelay

	// instanceName is the issuer shown in an authenticator app.
	instanceName string

	// defaults are the monitoring defaults a newly created monitor inherits,
	// cached from settings so the create path does not read them per request.
	defaults model.MonitoringSettings

	// baseURL is the public URL of this install, from settings. Empty means
	// "derive it from the request", which is right whenever the creator and the
	// caller reach the install the same way and visibly wrong rather than
	// silently wrong when they do not.
	baseURL string
	orgID   model.ID
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
		store:         s,
		notify:        publisher,
		sweeps:        sweeps,
		push:          push,
		alerts:        alerts,
		registry:      registry,
		keeper:        keeper,
		vault:         notify.NewVault(keeper),
		configs:       secrets.NewVault(keeper, "monitors", "config"),
		webhooks:      secrets.NewVault(keeper, "webhooks", "secret_encrypted"),
		settingsVault: secrets.NewVault(keeper, "settings", "smtp"),
		subscribers:   secrets.NewVault(keeper, "subscribers", "target"),
		log:           log,
		limiter:       newLoginLimiter(),
		checks:        newCheckLimiter(),
		instanceName:  instanceName,
		orgID:         model.SentinelOrgID,
	}
}

// WithOutbound attaches the webhook delivery engine.
//
// A setter rather than another positional argument to New, which already takes
// nine: past a certain count a constructor's arguments stop being read and start
// being counted, and the two things below are optional in a way the other nine
// are not — a test exercising the monitor list has no reason to build a
// dispatcher.
func (s *Server) WithOutbound(d Redeliverer) *Server {
	s.outbound = d
	return s
}

// WithRetuner attaches the rollup runner, so a retention change through
// /settings takes effect on the next sweep rather than on the next restart.
func (s *Server) WithRetuner(r Retuner) *Server {
	s.retuner = r
	return s
}

// WithSubscribers attaches the status page relay.
func (s *Server) WithSubscribers(r SubscriberRelay) *Server {
	s.relay = r
	return s
}

// SubscriberRelay is what the API needs from status page delivery. Both methods
// are fire-and-forget: a public subscribe request and an incident update are
// both HTTP requests somebody is waiting on, and neither should be held open for
// an SMTP conversation with a stranger's mail server.
type SubscriberRelay interface {
	Confirm(c notify.Confirmation)
	Announce(a notify.Announcement)
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
	mux.HandleFunc("GET /metrics", s.metricsAuth(s.getPrometheusMetrics))

	// Unauthenticated by design, and each one says why in its handler: setup is
	// available only until an administrator exists, and login is how a caller
	// obtains a credential in the first place.
	public := http.NewServeMux()
	// Both spellings: /setup/status is the path the frozen spec names, and the
	// bare /setup answered the same question in earlier builds. Keeping the old
	// one costs a line and avoids breaking anything already written against it.
	public.HandleFunc("GET /api/v1/setup/status", s.setupStatus)
	public.HandleFunc("GET /api/v1/setup", s.setupStatus)
	public.HandleFunc("POST /api/v1/setup", s.completeSetup)
	public.HandleFunc("POST /api/v1/auth/login", s.login)
	// The dead-man's-switch ingest. Unauthenticated by design — see push.go.
	public.HandleFunc("GET /api/v1/push/{pushToken}", s.pushHeartbeat)
	public.HandleFunc("POST /api/v1/push/{pushToken}", s.pushHeartbeat)

	// The status page read path. Unauthenticated because a status page whose
	// audience needs a credential is not a status page.
	public.HandleFunc("GET /api/v1/public/status-pages/{slug}", s.getPublicStatusPage)
	public.HandleFunc("POST /api/v1/public/status-pages/{slug}/authenticate", s.authenticatePublicStatusPage)
	public.HandleFunc("POST /api/v1/public/status-pages/{slug}/subscribers", s.subscribeToStatusPage)
	public.HandleFunc("POST /api/v1/public/subscriptions/{token}", s.confirmSubscription)
	public.HandleFunc("DELETE /api/v1/public/subscriptions/{token}", s.unsubscribe)

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
	authed.HandleFunc("PATCH /api/v1/monitors/{monitorId}", s.require(auth.ScopeMonitorsWrite, s.updateMonitor))
	// Registered before the wildcard for readability only — Go's mux resolves by
	// specificity, so /membership and /bulk cannot be swallowed by /{monitorId}
	// whatever the order here.
	authed.HandleFunc("GET /api/v1/monitors/membership", s.require(auth.ScopeMonitorsRead, s.getMonitorMembership))
	authed.HandleFunc("POST /api/v1/monitors/bulk", s.require(auth.ScopeMonitorsWrite, s.bulkUpdateMonitors))
	authed.HandleFunc("POST /api/v1/monitors/{monitorId}/pause", s.require(auth.ScopeMonitorsWrite, s.pauseMonitor))
	authed.HandleFunc("POST /api/v1/monitors/{monitorId}/resume", s.require(auth.ScopeMonitorsWrite, s.resumeMonitor))
	authed.HandleFunc("POST /api/v1/monitors/{monitorId}/check", s.require(auth.ScopeMonitorsWrite, s.runMonitorCheck))
	authed.HandleFunc("GET /api/v1/monitors/{monitorId}/certificate", s.require(auth.ScopeMonitorsRead, s.getMonitorCertificate))
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

	authed.HandleFunc("GET /api/v1/groups", s.require(auth.ScopeGroupsRead, s.listGroups))
	authed.HandleFunc("POST /api/v1/groups", s.require(auth.ScopeGroupsWrite, s.createGroup))
	authed.HandleFunc("GET /api/v1/groups/{groupId}", s.require(auth.ScopeGroupsRead, s.getGroup))
	authed.HandleFunc("PATCH /api/v1/groups/{groupId}", s.require(auth.ScopeGroupsWrite, s.updateGroup))
	authed.HandleFunc("DELETE /api/v1/groups/{groupId}", s.require(auth.ScopeGroupsWrite, s.deleteGroup))

	authed.HandleFunc("GET /api/v1/tags", s.require(auth.ScopeTagsRead, s.listTags))
	authed.HandleFunc("POST /api/v1/tags", s.require(auth.ScopeTagsWrite, s.createTag))
	authed.HandleFunc("GET /api/v1/tags/{tagId}", s.require(auth.ScopeTagsRead, s.getTag))
	authed.HandleFunc("PATCH /api/v1/tags/{tagId}", s.require(auth.ScopeTagsWrite, s.updateTag))
	authed.HandleFunc("DELETE /api/v1/tags/{tagId}", s.require(auth.ScopeTagsWrite, s.deleteTag))

	authed.HandleFunc("GET /api/v1/system/info", s.getSystemInfo)
	authed.HandleFunc("GET /api/v1/overview", s.require(auth.ScopeMonitorsRead, s.getOverview))

	authed.HandleFunc("GET /api/v1/users", s.require(auth.ScopeUsersRead, s.listUsers))
	authed.HandleFunc("GET /api/v1/users/me", s.getCurrentUser)
	authed.HandleFunc("PATCH /api/v1/users/me", s.updateCurrentUser)
	authed.HandleFunc("GET /api/v1/users/{userId}", s.require(auth.ScopeUsersRead, s.getUser))

	authed.HandleFunc("GET /api/v1/settings", s.require(auth.ScopeSettingsRead, s.getSettings))
	authed.HandleFunc("PATCH /api/v1/settings", s.require(auth.ScopeSettingsWrite, s.updateSettings))

	authed.HandleFunc("GET /api/v1/incidents", s.require(auth.ScopeIncidentsRead, s.listIncidents))
	authed.HandleFunc("POST /api/v1/incidents", s.require(auth.ScopeIncidentsWrite, s.createIncident))
	authed.HandleFunc("GET /api/v1/incidents/{incidentId}", s.require(auth.ScopeIncidentsRead, s.getIncident))
	authed.HandleFunc("PATCH /api/v1/incidents/{incidentId}", s.require(auth.ScopeIncidentsWrite, s.updateIncident))
	authed.HandleFunc("DELETE /api/v1/incidents/{incidentId}", s.require(auth.ScopeIncidentsWrite, s.deleteIncident))
	authed.HandleFunc("GET /api/v1/incidents/{incidentId}/updates", s.require(auth.ScopeIncidentsRead, s.listIncidentUpdates))
	authed.HandleFunc("POST /api/v1/incidents/{incidentId}/updates", s.require(auth.ScopeIncidentsWrite, s.createIncidentUpdate))

	authed.HandleFunc("GET /api/v1/status-pages", s.require(auth.ScopeStatusPagesRead, s.listStatusPages))
	authed.HandleFunc("POST /api/v1/status-pages", s.require(auth.ScopeStatusPagesWrite, s.createStatusPage))
	authed.HandleFunc("GET /api/v1/status-pages/{statusPageId}", s.require(auth.ScopeStatusPagesRead, s.getStatusPage))
	authed.HandleFunc("PATCH /api/v1/status-pages/{statusPageId}", s.require(auth.ScopeStatusPagesWrite, s.updateStatusPage))
	authed.HandleFunc("DELETE /api/v1/status-pages/{statusPageId}", s.require(auth.ScopeStatusPagesWrite, s.deleteStatusPage))
	authed.HandleFunc("GET /api/v1/status-pages/{statusPageId}/subscribers", s.require(auth.ScopeStatusPagesRead, s.listStatusPageSubscribers))
	authed.HandleFunc("DELETE /api/v1/status-pages/{statusPageId}/subscribers/{subscriberId}", s.require(auth.ScopeStatusPagesWrite, s.deleteStatusPageSubscriber))

	authed.HandleFunc("GET /api/v1/webhooks", s.require(auth.ScopeWebhooksRead, s.listWebhooks))
	authed.HandleFunc("POST /api/v1/webhooks", s.require(auth.ScopeWebhooksWrite, s.createWebhook))
	authed.HandleFunc("GET /api/v1/webhooks/{webhookId}", s.require(auth.ScopeWebhooksRead, s.getWebhook))
	authed.HandleFunc("PATCH /api/v1/webhooks/{webhookId}", s.require(auth.ScopeWebhooksWrite, s.updateWebhook))
	authed.HandleFunc("DELETE /api/v1/webhooks/{webhookId}", s.require(auth.ScopeWebhooksWrite, s.deleteWebhook))
	authed.HandleFunc("GET /api/v1/webhooks/{webhookId}/deliveries", s.require(auth.ScopeWebhooksRead, s.listWebhookDeliveries))
	authed.HandleFunc("POST /api/v1/webhooks/{webhookId}/deliveries/{deliveryId}/redeliver", s.require(auth.ScopeWebhooksWrite, s.redeliverWebhook))

	// The Kuma import endpoints stay unimplemented, and deliberately so: the
	// endpoint without the importer behind it would accept a file and report
	// success for an import that never happened. The importer is its own
	// deliverable (docs/plans/PHASE-1-TODO.md, "Kuma migration").
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
	//
	// newSPAHandler rather than a plain file server: the dashboard resolves its
	// own routes, so an unknown document path has to return the shell. The three
	// URLs subscriber mail carries are client-side routes, and a file server
	// answers all of them with 404 (see ui.go).
	if assets, err := ui.FS(); err == nil {
		mux.Handle("/", newSPAHandler(assets))
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
