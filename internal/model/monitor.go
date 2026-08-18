package model

import (
	"encoding/json"
	"time"
)

// The nine monitor types, matching monitors.type in the schema and MonitorType
// in the OpenAPI spec. Adding one is a checker, a registry entry, and a config
// schema — it never changes the probe protocol (ADR-005 decision 6).
const (
	TypeHTTP         = "http"
	TypeTCP          = "tcp"
	TypeICMP         = "icmp"
	TypeDNS          = "dns"
	TypeTLSExpiry    = "tls_expiry"
	TypeDomainExpiry = "domain_expiry"
	TypePush         = "push"
	TypeDocker       = "docker"
	TypeGRPC         = "grpc"
)

// Monitor is the configured intent: what to check and how often. What it is
// currently doing lives in monitor_state, kept in a separate table on purpose
// (data model §4.2) — a status update must not rewrite a row that carries the
// user's configuration.
type Monitor struct {
	ID          ID
	OrgID       ID
	Name        string
	Description string

	// Type is one of the constants above. Immutable after creation: changing it
	// would make the monitor's own history incomparable with itself.
	Type string

	// Config is the type-specific JSON, stored and transmitted verbatim. The
	// probe treats it as opaque bytes; only the checker for Type parses it.
	//
	// It carries secrets. Everything in data model §12 belonging to this monitor
	// is here in plaintext once decrypted, which is why it is never logged.
	Config json.RawMessage

	// Target is promoted out of Config so "what else points at this host?" is an
	// indexed query rather than a JSON scan across 5,000 rows.
	Target string

	Enabled          bool
	Interval         time.Duration
	Timeout          time.Duration
	Retries          int
	RetryInterval    time.Duration
	ResendAfter      int
	UpsideDown       bool
	NotifyOnRecovery bool
	GroupID          *ID
	ParentMonitorID  *ID

	CreatedAt time.Time
	UpdatedAt time.Time
}

// MinInterval is the floor the schema enforces and the number every capacity
// calculation in the project is derived from: 5,000 monitors at 20 seconds is
// 250 checks per second, which is also 250 writes per second (data model §5.1).
const MinInterval = 20 * time.Second
