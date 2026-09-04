package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/importer/kuma"
	"github.com/webloomlabs/uptime-cairn/internal/live"
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
	RecentHeartbeats(ctx context.Context, ids []model.ID, limit int) (map[model.ID][]model.Heartbeat, error)
	UptimeRatios(ctx context.Context, ids []model.ID, window string) (map[model.ID]float64, error)

	// Probes are read-only here: enrolment is Phase 4. What Phase 1 needs is
	// the ability to name one, so a monitor that only one host can answer for
	// can say which host.
	ListProbes(ctx context.Context) ([]model.Probe, error)
	GetProbe(ctx context.Context, id model.ID) (model.Probe, error)
	CountEnabledProbes(ctx context.Context) (int, error)

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
	ReportStore
	ReportShareStore
	ReportScheduleStore
	BrandStore
	ExpiryStore
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

	// shares throttles the unauthenticated share-link read. Its own limiter
	// rather than the login one: five attempts in fifteen minutes is right for
	// credential guessing and absurd for a document somebody was sent.
	shares  *shareLimiter
	checks  *checkLimiter
	retuner Retuner

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

	// reportDrops seals an s3 delivery target's secret access key, and
	// reportStorage seals the artifact mirror's. Each has its own vault rather
	// than sharing one, because the AAD binds a ciphertext to its table, column
	// and row: without that separation, somebody who can write to the database
	// can move a credential from a row they control onto one they do not, and
	// GCM would open it happily. The mirror's is bound to the settings row's
	// report_storage column specifically, so an SMTP password cannot be
	// relocated into it either.
	reportDrops   *secrets.Vault
	reportStorage *secrets.Vault

	// reportShares seals a share token so the operator can be shown the link
	// again. Bound to the share row, so a ciphertext lifted from one link's row
	// fails to open against another rather than opening as somebody else's
	// credential.
	reportShares *secrets.Vault

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

	// reports queues a generation onto the bounded worker pool, and artifacts is
	// the local read path for what it produced. Both nil in a build that is not
	// running reporting, which /generate reports as 501 rather than queueing a
	// run into a void — a row stuck at `queued` forever reads as a hung report
	// rather than as a missing feature.
	//
	// reportZone is the zone an ad-hoc run's boundaries are cut in. A schedule
	// carries its own, defaulted from the instance zone at write time so that
	// changing the instance zone does not silently move the boundaries of a
	// report somebody has been receiving for a year; a "run now" has no schedule
	// to take one from, so it takes this.
	reports    Reporter
	artifacts  ArtifactFiles
	reportZone *time.Location

	// imports is the store the Kuma importer writes through, and running is what
	// keeps two imports from both deciding the same name was free. Nil in a
	// build that has no importer wired in, which /imports/kuma reports as 501.
	imports kuma.Store
	running *importRunner

	// domains resolves a request's Host to a custom-domain status page.
	domains *domainCache

	// live is the browser-facing update bus (ADR-004). Nil in a build or a test
	// that is not running one, which /api/v1/live reports as 501 rather than
	// panicking on — a dashboard without it still works, bounded by the
	// membership poll instead of being near-instant.
	live    live.Bus
	streams *liveStreams

	// trusted is the set of peers permitted to speak for somebody else through
	// X-Forwarded-For. Empty by default and empty in every test that does not
	// name one, which is what keeps the header untrusted unless an operator has
	// said otherwise.
	trusted *trustedProxies

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
	server := &Server{
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
		reportDrops:   secrets.NewVault(keeper, "report_schedule_deliveries", "secrets"),
		reportStorage: secrets.NewVault(keeper, "settings", "report_storage"),
		reportShares:  secrets.NewVault(keeper, "report_share_links", "token_encrypted"),
		log:           log,
		limiter:       newLoginLimiter(),
		shares:        newShareLimiter(),
		streams:       newLiveStreams(),
		running:       &importRunner{},

		checks:       newCheckLimiter(),
		instanceName: instanceName,
		orgID:        model.SentinelOrgID,
	}
	// After construction, because the cache reads through the store this server
	// was just given.
	server.domains = newDomainCache(s.CustomDomains)
	return server
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

// WithImports attaches the store the Kuma importer writes through.
//
// Separate from Store even though it is the same object, because the importer
// declares its own interface and handing it this one is what proves the two
// agree — the CLI constructs the same Target from the same concrete store.
// WithReporting attaches the report worker pool and the artifact directory.
//
// Both together, because a queue with no read path produces artifacts nobody can
// download and a read path with no queue has nothing to serve. The zone is the
// instance's, and is what an ad-hoc run's period boundaries are cut in.
func (s *Server) WithReporting(reports Reporter, files ArtifactFiles, zone *time.Location) *Server {
	s.reports, s.artifacts, s.reportZone = reports, files, zone
	return s
}

func (s *Server) WithImports(store kuma.Store) *Server {
	s.imports = store
	return s
}

// WithLive attaches the live-update bus.
//
// A setter, like the others, because a server built for a test that never opens
// a stream has no reason to construct one — and because the bus is the seam
// ADR-004's solo/scaled split runs through, so what gets passed here is exactly
// what changes between the two modes.
func (s *Server) WithLive(b live.Bus) *Server {
	s.live = b
	return s
}

// WithTrustedProxies declares which peers may set X-Forwarded-For on behalf of
// a client. This is the only place in the server where that header is believed,
// and it is believed nowhere unless an operator has named the proxy.
//
// It returns an error rather than panicking because the value comes from a
// flag, and a flag typed wrong should stop the process with the flag's name in
// the message rather than in a stack trace.
func (s *Server) WithTrustedProxies(values []string) error {
	t, err := parseTrustedProxies(values)
	if err != nil {
		return err
	}
	s.trusted = t
	return nil
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
	// /setup/status is the path the frozen spec names, and it is the only one.
	//
	// A `GET /api/v1/setup` alias answering the same question used to sit beside
	// it, kept for "anything already written against it". The contract test
	// found it, which is what that test is for: an endpoint the spec does not
	// describe is an endpoint no other client knows exists, whatever the
	// intention was. Nothing has been released, so nothing was written against
	// it, and the spec is the contract.
	public.HandleFunc("GET /api/v1/setup/status", s.setupStatus)
	public.HandleFunc("POST /api/v1/setup", s.completeSetup)
	public.HandleFunc("POST /api/v1/auth/login", s.login)
	// The dead-man's-switch ingest. Unauthenticated by design — see push.go.
	public.HandleFunc("GET /api/v1/push/{pushToken}", s.pushHeartbeat)
	public.HandleFunc("POST /api/v1/push/{pushToken}", s.pushHeartbeat)

	// The status page read path. Unauthenticated because a status page whose
	// audience needs a credential is not a status page.
	// The share pair: unauthenticated, rate limited, noindex, and answering on a
	// separate public projection. The token in the path is the whole of the
	// authorisation, which is why it is bounded and hashed before it is looked
	// up and why the read path has no place to put a monitor identifier.
	public.HandleFunc("GET /api/v1/public/reports/{shareToken}", s.getPublicReport)
	public.HandleFunc("GET /api/v1/public/reports/{shareToken}/download", s.downloadPublicReport)

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
	// Probes read under monitors:read: the endpoint exists so a caller can fill
	// in monitor.probe_id, it returns no credential, and in solo mode it answers
	// with one row.
	authed.HandleFunc("GET /api/v1/probes", s.require(auth.ScopeMonitorsRead, s.listProbes))

	// The guided import flow. Same importer as `cairn import kuma`, through the
	// same seam: two write paths would be two sets of rules, and the one that
	// drifts is the one nobody exercises.
	authed.HandleFunc("POST /api/v1/imports/kuma", s.require(auth.ScopeImportsWrite, s.importFromKuma))
	authed.HandleFunc("GET /api/v1/imports/{importJobId}", s.require(auth.ScopeImportsWrite, s.getImportJob))

	// ADR-004's live half. Ordinary API surface under an ordinary scope: the
	// dashboard gets no privileged channel, so anything holding monitors:read
	// can open a stream and watch the same diffs the dashboard watches.
	authed.HandleFunc("GET /api/v1/live", s.require(auth.ScopeMonitorsRead, s.streamUpdates))
	authed.HandleFunc("PUT /api/v1/live/{streamId}/scope", s.require(auth.ScopeMonitorsRead, s.setStreamScope))

	authed.HandleFunc("GET /api/v1/overview", s.require(auth.ScopeMonitorsRead, s.getOverview))

	authed.HandleFunc("GET /api/v1/users", s.require(auth.ScopeUsersRead, s.listUsers))
	authed.HandleFunc("GET /api/v1/users/me", s.getCurrentUser)
	authed.HandleFunc("PATCH /api/v1/users/me", s.updateCurrentUser)
	authed.HandleFunc("GET /api/v1/users/{userId}", s.require(auth.ScopeUsersRead, s.getUser))

	// Reporting. The scopes are the ones the spec names on each operation.
	authed.HandleFunc("GET /api/v1/report-templates", s.require(auth.ScopeReportsRead, s.listReportTemplates))
	authed.HandleFunc("POST /api/v1/report-templates", s.require(auth.ScopeReportsWrite, s.createReportTemplate))
	authed.HandleFunc("GET /api/v1/report-templates/{reportTemplateId}", s.require(auth.ScopeReportsRead, s.getReportTemplate))
	authed.HandleFunc("PATCH /api/v1/report-templates/{reportTemplateId}", s.require(auth.ScopeReportsWrite, s.updateReportTemplate))
	authed.HandleFunc("DELETE /api/v1/report-templates/{reportTemplateId}", s.require(auth.ScopeReportsWrite, s.deleteReportTemplate))
	authed.HandleFunc("POST /api/v1/report-templates/{reportTemplateId}/generate", s.require(auth.ScopeReportsWrite, s.generateReport))

	authed.HandleFunc("GET /api/v1/brand-profiles", s.require(auth.ScopeBrandProfilesRead, s.listBrandProfiles))
	authed.HandleFunc("POST /api/v1/brand-profiles", s.require(auth.ScopeBrandProfilesWrite, s.createBrandProfile))
	authed.HandleFunc("GET /api/v1/brand-profiles/{brandProfileId}", s.require(auth.ScopeBrandProfilesRead, s.getBrandProfile))
	authed.HandleFunc("PATCH /api/v1/brand-profiles/{brandProfileId}", s.require(auth.ScopeBrandProfilesWrite, s.updateBrandProfile))
	authed.HandleFunc("DELETE /api/v1/brand-profiles/{brandProfileId}", s.require(auth.ScopeBrandProfilesWrite, s.deleteBrandProfile))
	authed.HandleFunc("PUT /api/v1/brand-profiles/{brandProfileId}/logo", s.require(auth.ScopeBrandProfilesWrite, s.uploadBrandProfileLogo))

	authed.HandleFunc("GET /api/v1/report-schedules", s.require(auth.ScopeReportsRead, s.listReportSchedules))
	authed.HandleFunc("POST /api/v1/report-schedules", s.require(auth.ScopeReportsWrite, s.createReportSchedule))
	authed.HandleFunc("GET /api/v1/report-schedules/{reportScheduleId}", s.require(auth.ScopeReportsRead, s.getReportSchedule))
	authed.HandleFunc("PATCH /api/v1/report-schedules/{reportScheduleId}", s.require(auth.ScopeReportsWrite, s.updateReportSchedule))
	authed.HandleFunc("DELETE /api/v1/report-schedules/{reportScheduleId}", s.require(auth.ScopeReportsWrite, s.deleteReportSchedule))

	authed.HandleFunc("GET /api/v1/report-runs", s.require(auth.ScopeReportsRead, s.listReportRuns))
	authed.HandleFunc("GET /api/v1/report-runs/{reportRunId}", s.require(auth.ScopeReportsRead, s.getReportRun))
	authed.HandleFunc("GET /api/v1/report-runs/{reportRunId}/download", s.require(auth.ScopeReportsRead, s.downloadReportArtifact))
	authed.HandleFunc("GET /api/v1/report-runs/{reportRunId}/artifacts/{artifactId}", s.require(auth.ScopeReportsRead, s.downloadReportArtifactByID))
	authed.HandleFunc("POST /api/v1/report-runs/{reportRunId}/share", s.require(auth.ScopeReportsWrite, s.createReportShareLink))
	authed.HandleFunc("DELETE /api/v1/report-runs/{reportRunId}/share", s.require(auth.ScopeReportsWrite, s.revokeReportShareLink))

	// monitors:read rather than reports:read, which is the spec's choice: the
	// rows are facts about monitors, and a key that can see a monitor can
	// already read its certificate one at a time.
	authed.HandleFunc("GET /api/v1/expiries", s.require(auth.ScopeMonitorsRead, s.listUpcomingExpiries))

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
		mux.Handle("/", newSPAHandler(assets, s.domains))
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

// Flush passes through, because embedding http.ResponseWriter hides every
// optional interface the wrapped writer implements.
//
// This is the classic decorator bug and it fails in a specific, confusing way:
// the streaming endpoint's type assertion to http.Flusher stops holding, so
// /api/v1/live reported "response writer cannot flush" on a stdlib server that
// can. Without it an SSE stream would either 500 or, worse on a wrapper that
// buffers silently, deliver nothing until the handler returned — which for a
// long-lived stream is never.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
