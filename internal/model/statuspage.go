package model

import (
	"strings"
	"time"
)

// Status pages: the only part of this system a customer ever sees.
//
// Everything here answers to one constraint the rest of the API does not have —
// the read path is unauthenticated and public, so it is the endpoint that gets
// hit hardest exactly when the instance is least healthy. That is why the public
// projection is a separate shape rather than a filter over the private one: a
// field cannot leak through a projection that has no place to put it.

// Status page visibility.
const (
	VisibilityPublic   = "public"
	VisibilityPassword = "password"
)

// Overall status values a public page reports. Deliberately not the monitor
// status vocabulary: a page says how the service looks to a customer, and
// "pending" is an implementation detail of the checker.
const (
	OverallOperational   = "operational"
	OverallDegraded      = "degraded"
	OverallPartialOutage = "partial_outage"
	OverallMajorOutage   = "major_outage"
	OverallMaintenance   = "maintenance"
)

// Subscriber channels.
const (
	SubscriberEmail   = "email"
	SubscriberWebhook = "webhook"
)

// StatusPage is one published view over a chosen set of monitors.
type StatusPage struct {
	ID    ID
	OrgID ID

	// Slug is the public path segment, supplied rather than derived: it appears
	// in a URL a customer may have bookmarked, so the operator has to be able to
	// choose it and keep it.
	Slug  string
	Title string

	Description string
	Published   bool

	// CustomDomain is a hostname this page answers on. Unique across every
	// organisation, because a request arrives with nothing but a Host header to
	// route on.
	CustomDomain string

	Visibility string

	// PasswordHash is hashed rather than encrypted: it is verified against and
	// never replayed (data model §12.1).
	PasswordHash string

	Theme        string
	LogoURL      string
	FaviconURL   string
	PrimaryColor string
	FooterText   string
	CustomCSS    string
	Timezone     string

	ShowUptimePercentage  bool
	ShowResponseTimeChart bool
	UptimeBarDays         int
	ShowPoweredBy         bool
	SubscriptionsEnabled  bool
	GoogleAnalyticsID     string

	CreatedAt time.Time
	UpdatedAt time.Time

	// Sections are the visitor-facing groupings, in display order.
	Sections []StatusPageSection
}

// StatusPageSection is one heading on a page and the monitors under it.
type StatusPageSection struct {
	ID           ID
	StatusPageID ID
	OrgID        ID
	Name         string
	Description  string
	Position     int

	// MonitorIDs in display order. A monitor appears in at most one section per
	// page, which the schema enforces rather than the handler.
	MonitorIDs []ID
}

// ValidSlug reports whether s matches the spec's slug pattern: lower-case
// alphanumerics and interior hyphens, 1–64 characters.
//
// Checked here rather than with the regexp the spec publishes, so that the rule
// travels with the type that carries the field.
func ValidSlug(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// Subscriber is somebody who asked to be told when a page changes.
type Subscriber struct {
	ID           ID
	StatusPageID ID
	OrgID        ID
	Channel      string

	// Target is the plaintext address. Populated only where a caller has opened
	// the envelope below; the store never fills it, because the store holds no
	// key.
	Target string

	// SealedTarget is the address as stored: encrypted rather than hashed,
	// because it is replayed on every notification (data model §12.1).
	SealedTarget []byte

	// TargetHash carries the uniqueness constraint without an index over every
	// subscriber address on the instance in plaintext (§12.5).
	TargetHash []byte

	ConfirmTokenHash []byte
	ConfirmedAt      *time.Time

	// The unsubscribe token, stored twice, and the duplication is the point.
	//
	// "Hash what you verify, encrypt what you replay" decides this one twice
	// over, because the token is both: it is verified when somebody follows the
	// link, and replayed at the foot of every message this page ever sends
	// them. The hash carries the unique index the lookup probes; the envelope
	// carries the value, which nothing else can reproduce and without which a
	// notification would go out with no way out of it.
	UnsubscribeTokenHash   []byte
	SealedUnsubscribeToken []byte

	CreatedAt time.Time
}

// Confirmed reports whether this subscriber has completed double opt-in.
func (s Subscriber) Confirmed() bool { return s.ConfirmedAt != nil }

// MaskTarget hides most of an address while leaving enough to recognise one's
// own. A status page's subscriber list is an export of somebody's customers, so
// it is not handed back in full even to an authenticated operator.
func MaskTarget(channel, target string) string {
	if channel == SubscriberWebhook {
		if i := strings.Index(target, "://"); i >= 0 {
			rest := target[i+3:]
			if j := strings.IndexAny(rest, "/?"); j >= 0 {
				return target[:i+3] + rest[:j] + "/…"
			}
		}
		return target
	}

	local, domain, found := strings.Cut(target, "@")
	if !found {
		return maskLocalPart(local)
	}
	return maskLocalPart(local) + "@" + domain
}

func maskLocalPart(s string) string {
	switch {
	case s == "":
		return ""
	case len(s) <= 2:
		return s[:1] + "…"
	default:
		return s[:2] + "…"
	}
}

// OverallStatus reduces the monitor statuses on a page to the one line a
// visitor reads.
//
// The thresholds are the ones a customer would recognise: anything down is at
// least a partial outage, and it becomes major when it is most of the page.
// Maintenance only wins when nothing is actually down — a real outage during a
// maintenance window is still an outage, and reporting it as scheduled work is
// the sort of thing that ends up on a screenshot.
func OverallStatus(statuses []string) string {
	if len(statuses) == 0 {
		return OverallOperational
	}

	var down, maintenance, pending int
	for _, s := range statuses {
		switch s {
		case MonitorStatusDown:
			down++
		case MonitorStatusMaintenance:
			maintenance++
		case MonitorStatusPending:
			pending++
		}
	}

	switch {
	case down == 0 && maintenance > 0:
		return OverallMaintenance
	case down == 0 && pending > 0:
		return OverallDegraded
	case down == 0:
		return OverallOperational
	case down*2 >= len(statuses):
		return OverallMajorOutage
	default:
		return OverallPartialOutage
	}
}
