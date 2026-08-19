package model

import "time"

// Certificate is the TLS certificate last seen on a monitor's target.
//
// One row per monitor, replaced on each observation rather than appended to:
// Phase 1 answers "what is on the wire now and when does it expire", and a
// history of certificates is a different question that nothing yet asks. Making
// it a primary-key read is what keeps the endpoint cheap enough to embed in a
// monitor detail view.
type Certificate struct {
	MonitorID ID
	OrgID     ID

	Subject      string
	Issuer       string
	SerialNumber string

	ValidFrom *time.Time
	ValidTo   time.Time

	FingerprintSHA256 []byte
	SANs              []string

	// ChainValid is nil when the chain was not evaluated. Distinct from false,
	// which is a finding.
	ChainValid *bool
	ChainError string

	ObservedAt time.Time
}

// DaysRemaining is the figure the API reports and an expiry alert fires on,
// counted from now. Negative for a certificate that has already expired, which
// is a fact worth rendering rather than clamping to zero.
func (c Certificate) DaysRemaining(now time.Time) int {
	return int(c.ValidTo.Sub(now).Hours() / 24)
}

// DomainExpiry is the registration behind a domain-expiry monitor. It gets its
// own shape rather than being forced into a certificate-shaped row: a
// registration has a registrar and a lookup source, and no issuer, subject, or
// chain (data model §4.6).
type DomainExpiry struct {
	MonitorID ID
	OrgID     ID
	Domain    string
	ExpiresAt time.Time
	Registrar string

	// Source is rdap or whois — which one answered matters, because WHOIS is the
	// fallback and its dates are the less trustworthy of the two.
	Source string

	ObservedAt time.Time
}
