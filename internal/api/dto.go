package api

import (
	"encoding/json"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The wire shapes, matching docs/api/openapi.yaml. Hand-written rather than
// generated: generating from the spec is the right answer once contract tests
// exist to prove these agree, and until then a small hand-written struct is
// easier to read against the spec than a generated one.

type monitorJSON struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	Description         *string         `json:"description"`
	Type                string          `json:"type"`
	Config              json.RawMessage `json:"config"`
	Enabled             bool            `json:"enabled"`
	IntervalSeconds     int             `json:"interval_seconds"`
	TimeoutSeconds      int             `json:"timeout_seconds"`
	Retries             int             `json:"retries"`
	RetryIntervalSecond *int            `json:"retry_interval_seconds"`
	ResendAfter         int             `json:"resend_after"`
	UpsideDown          bool            `json:"upside_down"`
	GroupID             *string         `json:"group_id"`
	ParentMonitorID     *string         `json:"parent_monitor_id"`
	NotifyOnRecovery    bool            `json:"notify_on_recovery"`
	Status              string          `json:"status"`
	LastCheckAt         *time.Time      `json:"last_check_at"`
	NextCheckAt         *time.Time      `json:"next_check_at"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// monitorWrite is the request body. Pointers everywhere a default exists, so the
// server can tell "unset, use the default" from "explicitly set to the zero
// value" — the difference between an unspecified interval and a nonsensical one.
type monitorWrite struct {
	Name                 *string         `json:"name"`
	Description          *string         `json:"description"`
	Type                 *string         `json:"type"`
	Config               json.RawMessage `json:"config"`
	Enabled              *bool           `json:"enabled"`
	IntervalSeconds      *int            `json:"interval_seconds"`
	TimeoutSeconds       *int            `json:"timeout_seconds"`
	Retries              *int            `json:"retries"`
	RetryIntervalSeconds *int            `json:"retry_interval_seconds"`
	ResendAfter          *int            `json:"resend_after"`
	UpsideDown           *bool           `json:"upside_down"`
	NotifyOnRecovery     *bool           `json:"notify_on_recovery"`
}

type heartbeatJSON struct {
	MonitorID      string    `json:"monitor_id"`
	Time           time.Time `json:"time"`
	Status         string    `json:"status"`
	ResponseTimeMs *float64  `json:"response_time_ms"`
	Message        *string   `json:"message"`
	Code           *string   `json:"code"`
	Attempt        int       `json:"attempt"`
	Important      bool      `json:"important"`
	Suppressed     bool      `json:"suppressed"`
	ProbeID        *string   `json:"probe_id"`
}

type pagination struct {
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

type page[T any] struct {
	Data       []T        `json:"data"`
	Pagination pagination `json:"pagination"`
}

func toMonitorJSON(m store.MonitorWithState) monitorJSON {
	out := monitorJSON{
		ID:               m.Monitor.ID.String(),
		Name:             m.Monitor.Name,
		Type:             m.Monitor.Type,
		Config:           json.RawMessage(m.Monitor.Config),
		Enabled:          m.Monitor.Enabled,
		IntervalSeconds:  int(m.Monitor.Interval.Seconds()),
		TimeoutSeconds:   int(m.Monitor.Timeout.Seconds()),
		Retries:          m.Monitor.Retries,
		ResendAfter:      m.Monitor.ResendAfter,
		UpsideDown:       m.Monitor.UpsideDown,
		NotifyOnRecovery: m.Monitor.NotifyOnRecovery,
		Status:           m.State.Status,
		LastCheckAt:      m.State.LastCheckAt,
		NextCheckAt:      m.State.NextCheckAt,
		CreatedAt:        m.Monitor.CreatedAt,
		UpdatedAt:        m.Monitor.UpdatedAt,
	}
	if m.Monitor.Description != "" {
		out.Description = &m.Monitor.Description
	}
	if m.Monitor.RetryInterval > 0 {
		seconds := int(m.Monitor.RetryInterval.Seconds())
		out.RetryIntervalSecond = &seconds
	}
	if m.Monitor.GroupID != nil {
		id := m.Monitor.GroupID.String()
		out.GroupID = &id
	}
	if m.Monitor.ParentMonitorID != nil {
		id := m.Monitor.ParentMonitorID.String()
		out.ParentMonitorID = &id
	}
	return out
}

func toHeartbeatJSON(b model.Heartbeat) heartbeatJSON {
	out := heartbeatJSON{
		MonitorID:  b.MonitorID.String(),
		Time:       b.Time,
		Status:     b.Status.String(),
		Attempt:    b.Attempt,
		Important:  b.Important,
		Suppressed: b.Suppressed,
	}
	if b.ResponseTime != nil {
		ms := float64(b.ResponseTime.Microseconds()) / 1000.0
		out.ResponseTimeMs = &ms
	}
	if b.Message != "" {
		out.Message = &b.Message
	}
	if b.Code != "" {
		out.Code = &b.Code
	}
	if !b.ProbeID.IsZero() {
		id := b.ProbeID.String()
		out.ProbeID = &id
	}
	return out
}
