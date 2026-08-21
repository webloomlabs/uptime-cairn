package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/protocol"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The push ingest is the only unauthenticated write in the API, and it is
// unauthenticated on purpose: the callers are cron jobs and shell scripts, and
// `curl <url>` with no flags has to work or the feature does not get used. The
// token in the path is the credential.
//
// Which makes two properties load-bearing. The token is looked up by hash
// through a unique index, so an attacker guessing tokens costs one index probe
// rather than a scan — and a stolen database yields no working tokens. And the
// answer is 404 for both "no such token" and a malformed one, so the endpoint
// cannot be used to confirm that a token exists.

// pushBody is the optional POST body, PushHeartbeat in the spec.
type pushBody struct {
	Status         string   `json:"status"`
	Message        string   `json:"message"`
	ResponseTimeMs *float64 `json:"response_time_ms"`
}

const maxPushMessage = 1024

// PushIngest is the control-plane calls the API makes with a result of its own.
// Declared here so the API does not import the control plane's concrete server.
type PushIngest interface {
	PushHeartbeat(ctx context.Context, monitor model.Monitor, up bool, message string, responseTime *time.Duration) error

	// RecordCheck ingests a check the API ran itself and returns the heartbeat.
	// The API runs the checker because the control plane must not import
	// probe/check (ADR-001); what crosses this interface is an observation, and
	// from there a manual check is treated exactly like a scheduled one.
	RecordCheck(ctx context.Context, monitor model.Monitor, c protocol.Check) (model.Heartbeat, error)
}

func (s *Server) pushHeartbeat(w http.ResponseWriter, r *http.Request) {
	// Never empty: Go's router does not match an empty path segment, so
	// /api/v1/push/ falls through to the authenticated catch-all rather than
	// arriving here.
	token := r.PathValue("pushToken")

	monitor, err := s.store.MonitorByPushToken(r.Context(), auth.HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		s.pushNotFound(w, r)
		return
	}
	if err != nil {
		s.internal(w, r, "resolve push token", err)
		return
	}

	up, message, responseTime, problem := s.readPush(r)
	if problem != "" {
		writeProblem(w, r, s.log, http.StatusBadRequest, "malformed-json", "Malformed request body", problem)
		return
	}

	if !monitor.Enabled {
		// Accepted and discarded rather than refused: a disabled monitor is the
		// operator's choice, and a cron job that starts failing because someone
		// paused a monitor in the UI is a support ticket nobody wants.
		writeJSON(w, s.log, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	if err := s.push.PushHeartbeat(r.Context(), monitor, up, message, responseTime); err != nil {
		s.internal(w, r, "record push heartbeat", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, map[string]bool{"ok": true})
}

// readPush accepts the query parameters the GET form uses and the JSON body the
// POST form uses, with the body winning where both are present.
func (s *Server) readPush(r *http.Request) (up bool, message string, responseTime *time.Duration, problem string) {
	up = true
	query := r.URL.Query()

	if v := query.Get("status"); v != "" {
		if v != "up" && v != "down" {
			return false, "", nil, "status must be up or down"
		}
		up = v == "up"
	}
	message = query.Get("msg")
	if v := query.Get("ping"); v != "" {
		ms, err := strconv.ParseFloat(v, 64)
		if err != nil || ms < 0 {
			return false, "", nil, "ping must be a non-negative number of milliseconds"
		}
		d := time.Duration(ms * float64(time.Millisecond))
		responseTime = &d
	}

	if r.Method == http.MethodPost && r.Body != nil {
		var body pushBody
		dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16))
		switch err := dec.Decode(&body); {
		case errors.Is(err, io.EOF):
			// An empty body is explicitly allowed: the spec marks the request
			// body optional so `curl -X POST <url>` works.
		case err != nil:
			return false, "", nil, err.Error()
		default:
			if body.Status != "" {
				if body.Status != "up" && body.Status != "down" {
					return false, "", nil, "status must be up or down"
				}
				up = body.Status == "up"
			}
			if body.Message != "" {
				message = body.Message
			}
			if body.ResponseTimeMs != nil {
				if *body.ResponseTimeMs < 0 {
					return false, "", nil, "response_time_ms must not be negative"
				}
				d := time.Duration(*body.ResponseTimeMs * float64(time.Millisecond))
				responseTime = &d
			}
		}
	}

	if len(message) > maxPushMessage {
		message = message[:maxPushMessage]
	}
	return up, message, responseTime, ""
}

// pushNotFound is the single answer for every unusable token, so the endpoint
// says nothing about which tokens exist.
func (s *Server) pushNotFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, s.log, http.StatusNotFound, "not-found",
		"Not found", "No push monitor holds that token.")
}

// pushURL assembles the URL the caller should hit.
//
// The configured base URL wins, because it is the operator saying how this
// install is reached from outside. Without one it is derived from the request
// that created the monitor — right whenever the creator and the pusher reach the
// install the same way, and visibly wrong rather than silently wrong when they
// do not.
func (s *Server) pushURL(r *http.Request, token string) string {
	if token == "" {
		return ""
	}
	if s.baseURL != "" {
		return s.baseURL + "/api/v1/push/" + token
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/v1/push/" + token
}
