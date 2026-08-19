package model

import "time"

// Outbound webhooks: the machine-readable half of alerting.
//
// A notification channel is aimed at a person — it renders a sentence and posts
// it to Slack. A webhook is aimed at a program, so it delivers the event
// envelope verbatim, signed, and logs every attempt. The two share the event
// stream and nothing else; collapsing them would mean either the program gets
// prose or the person gets JSON.

// DeliveryPending is the outcome a webhook delivery carries while a retry is
// still owed. The succeeded and failed values are shared with notification
// deliveries and declared alongside them.
const DeliveryPending = "pending"

// Webhook is one subscription to the event stream.
type Webhook struct {
	ID    ID
	OrgID ID
	Name  string
	URL   string

	// Events the subscription wants. Empty means every monitor state change,
	// matching the notification channel's rule so an operator does not have to
	// learn two.
	Events []string

	Enabled bool

	// Headers are additional request headers. Stored encrypted, because an
	// operator putting an Authorization header here is the expected case rather
	// than a misuse.
	Headers map[string]string

	// SecretEncrypted holds the HMAC key. Encrypted rather than hashed: every
	// delivery recomputes a signature with it, so it has to be recoverable
	// (data model §12.1). The distinction only surfaces at the first delivery,
	// which is why it is worth stating in the type.
	SecretEncrypted []byte

	// SecretPrefix is enough to recognise which secret a receiver was given and
	// not enough to sign with.
	SecretPrefix string

	VerifyTLS bool

	ConsecutiveFailures int

	// DisabledAt is set when a run of failures auto-disabled the subscription,
	// so "why did this stop" has an answer in the row rather than in the logs.
	DisabledAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time

	// LastDeliveryAt and LastDeliveryOutcome come from the delivery log on read.
	// A webhook that has silently stopped working is the failure mode this
	// feature cannot be allowed to have, and it is invisible without these.
	LastDeliveryAt      *time.Time
	LastDeliveryOutcome string
}

// WebhookDelivery is one attempt, logged whether it worked or not.
type WebhookDelivery struct {
	ID        ID
	WebhookID ID
	OrgID     ID

	// EventID is stable across retries, so a receiver can deduplicate.
	EventID   ID
	EventType string

	Outcome     string
	Attempt     int
	RequestBody string

	ResponseStatus *int

	// ResponseBody is truncated on write: a receiver returning a megabyte of
	// HTML on error must not turn one failing webhook into a disk-space
	// incident.
	ResponseBody string

	Error       string
	DurationMs  *float64
	NextRetryAt *time.Time
	CreatedAt   time.Time
}

// WantsEvent reports whether this webhook subscribes to an event type.
//
// An empty list means every monitor state change rather than literally every
// event, so a webhook created without a selection does not start receiving
// report-generated notices it has no idea what to do with.
func (h Webhook) WantsEvent(eventType string) bool {
	if len(h.Events) == 0 {
		switch eventType {
		case EventMonitorUp, EventMonitorDown:
			return true
		}
		return false
	}
	for _, e := range h.Events {
		if e == eventType {
			return true
		}
	}
	return false
}
