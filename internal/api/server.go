package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	"github.com/webloomlabs/uptime-cairn/internal/ui"
	"github.com/webloomlabs/uptime-cairn/internal/version"
)

// Store is what the API needs from persistence. Named by the consumer, so no
// handler can reach a backend-specific method by accident.
type Store interface {
	CreateMonitor(ctx context.Context, m model.Monitor) error
	GetMonitor(ctx context.Context, id model.ID) (store.MonitorWithState, error)
	ListMonitors(ctx context.Context, after *store.Cursor, limit int) ([]store.MonitorWithState, bool, error)
	DeleteMonitor(ctx context.Context, id model.ID) error
	ListHeartbeats(ctx context.Context, id model.ID, before *time.Time, limit int, importantOnly bool) ([]model.Heartbeat, bool, error)
}

// Notifier is the control plane's assignment publisher. The API calls it after
// any write, which is what turns "the user created a monitor" into "the probe is
// checking it" without a poll in between.
type Notifier interface{ Notify() }

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
	registry *check.Registry
	log      *slog.Logger

	// insecureNoAuth is temporary scaffolding, not a feature. Authentication —
	// first-run setup, sessions, TOTP, scoped API keys — is Phase 1 Month 1 work
	// specified in the OpenAPI spec and not built yet, and an API that quietly
	// accepted everything would be worse than one that says why it will not.
	insecureNoAuth bool
}

// New returns a server.
func New(s Store, notify Notifier, registry *check.Registry, log *slog.Logger, insecureNoAuth bool) *Server {
	return &Server{store: s, notify: notify, registry: registry, log: log, insecureNoAuth: insecureNoAuth}
}

// Handler builds the routing table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Operational endpoints sit outside the versioned surface and outside auth,
	// because a health check that needs a credential is a health check that
	// stops working at the worst moment.
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.health)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/monitors", s.listMonitors)
	api.HandleFunc("POST /api/v1/monitors", s.createMonitor)
	api.HandleFunc("GET /api/v1/monitors/{monitorId}", s.getMonitor)
	api.HandleFunc("DELETE /api/v1/monitors/{monitorId}", s.deleteMonitor)
	api.HandleFunc("GET /api/v1/monitors/{monitorId}/heartbeats", s.listHeartbeats)
	api.HandleFunc("/api/v1/", s.notImplemented)

	mux.Handle("/api/v1/", s.authenticate(api))

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

// authenticate is a placeholder that refuses rather than pretends.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.insecureNoAuth {
			next.ServeHTTP(w, r)
			return
		}
		writeProblem(w, r, s.log, http.StatusUnauthorized, "authentication-unavailable",
			"Authentication is not implemented in this build",
			"Scoped API keys and session login are specified in docs/api/openapi.yaml and are the next thing to build. "+
				"To run this build anyway, start it with --insecure-no-auth and do not expose the port.")
	})
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
