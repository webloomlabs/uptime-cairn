// Package check is the monitor-type registry.
//
// Adding a monitor type is one file implementing Checker, one line in a
// registry, and a config schema in the OpenAPI spec. It never changes the probe
// protocol, never bumps a protocol version, and never invalidates a deployed
// probe — which is why config crosses the wire as opaque bytes (ADR-005
// decision 6).
package check

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// Observation is what one check attempt saw. It is not a heartbeat: the control
// plane decides what a run of these means.
type Observation struct {
	// Status is up, down, unknown, or skipped. Never pending or maintenance —
	// those require control-plane knowledge.
	//
	// The distinction that carries the whole taxonomy: a DNS failure resolving
	// the target is down; a DNS failure because this probe's resolver is broken
	// is unknown. A checker that cannot tell them apart must report unknown.
	// Over-reporting down is how one broken probe pages an entire on-call
	// rotation.
	Status model.Status

	// ResponseTime is nil when nothing was measured.
	ResponseTime *time.Duration

	// Code is the protocol-level result: HTTP status, DNS rcode, gRPC health
	// status.
	Code string

	// Class says why, one level below Status. For humans and for grouping in
	// probe health; never for alerting logic, which branches on Status alone.
	Class ErrorClass

	// Message is user-facing and must have every credential redacted — including
	// on the timeout path, which is where a URL with embedded credentials
	// usually escapes (ADR-005 decision 15).
	Message string

	// Certificate is what the far end presented, when this check completed a TLS
	// handshake. Nil everywhere else, and nil on the failure paths too: a
	// handshake that did not finish observed nothing, and reporting the previous
	// certificate again would let the expiry page claim a certificate is being
	// served when nothing has served it for a week.
	//
	// It is separate from Status because the two answer different questions. An
	// http monitor is up and its certificate expires on Thursday, and only one
	// of those fits in an outcome.
	Certificate *Certificate

	// Domain is the registration a domain_expiry check read. Nil for every other
	// type, and nil when the registry could not be read.
	Domain *Domain
}

// Certificate is the leaf certificate a check was presented with.
//
// It carries the certificate rather than a days-remaining figure on purpose:
// the figure is a function of the clock, and a result buffered through a
// twenty-minute outage would arrive carrying arithmetic from before it. The
// control plane counts the days itself, from NotAfter, when it reads the row.
type Certificate struct {
	Subject      string
	Issuer       string
	SerialNumber string

	NotBefore time.Time
	NotAfter  time.Time

	// FingerprintSHA256 is the raw digest over the DER, not hex. The control
	// plane compares it to decide whether the certificate changed; the API
	// hex-encodes it on the way out.
	FingerprintSHA256 []byte

	SANs []string

	// ChainValid is nil when the chain was not evaluated — verify_tls off, or a
	// type that does not verify. Distinct from false, which is a finding:
	// collapsing the two would report every deliberately unverified monitor as
	// having a broken chain.
	ChainValid *bool
	ChainError string

	// DaysRemainingThreshold is the line the operator drew in this monitor's
	// config, and nil where the type has no such setting — tls_expiry has one,
	// http does not.
	//
	// It is reported rather than left for the control plane to look up because
	// the config crosses the wire as opaque bytes (ADR-005 decision 6): the
	// checker is the only side that parsed it. The control plane still decides
	// what to do about it, which is the half that is not the probe's.
	DaysRemainingThreshold *int
}

// Domain is the registration behind a domain_expiry check.
type Domain struct {
	Domain    string
	ExpiresAt time.Time

	// Registrar is empty where the registry's answer did not name one, which
	// thin WHOIS records regularly do not.
	Registrar string

	// Source is "rdap" or "whois", lowercase, matching the values the schema
	// accepts. Which one answered is worth keeping: WHOIS is the fallback and
	// its dates are the less trustworthy of the two.
	Source string

	DaysRemainingThreshold *int
}

// Checker executes one monitor type. The interface is deliberately this small:
// it is the extension point, and every method a checker gains is a method nine
// implementations have to answer for.
type Checker interface {
	// Type is the monitors.type string this checker implements.
	Type() string

	// Version is the checker's capability version, advertised at registration so
	// a control plane can withhold a monitor whose config uses a feature an
	// older checker would silently ignore.
	Version() uint32

	// Validate runs at assignment time, not check time. A monitor whose config
	// cannot be parsed is reported once, immediately, as a configuration error
	// the user can see — not discovered 250 times a second.
	Validate(config []byte) error

	// Check performs one attempt. It must respect ctx, which carries the
	// monitor's timeout, and must never panic: one bad response body must not
	// take down the 4,999 other monitors sharing this process.
	Check(ctx context.Context, config []byte) Observation
}

// Registry maps a type string to its checker.
//
// Safe for concurrent use because capability reporting reads it while the
// scheduler is running. Registration happens at startup, before either.
type Registry struct {
	mu       sync.RWMutex
	checkers map[string]Checker
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{checkers: make(map[string]Checker)}
}

// Register adds a checker. Registering a type twice is a programming error and
// panics at startup rather than leaving which one wins to map iteration order.
func (r *Registry) Register(c Checker) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t := c.Type()
	if _, exists := r.checkers[t]; exists {
		panic(fmt.Sprintf("check: duplicate checker registered for type %q", t))
	}
	r.checkers[t] = c
}

// Lookup returns the checker for a type. A missing checker is not an error to
// swallow: it becomes an assignment rejection, so the control plane can tell the
// user their monitor has nowhere to run rather than leaving it pending forever.
func (r *Registry) Lookup(monitorType string) (Checker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	c, ok := r.checkers[monitorType]
	return c, ok
}

// Types lists the registered types in a stable order, for capability reporting
// and for tests that would otherwise depend on map ordering.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	types := make([]string, 0, len(r.checkers))
	for t := range r.checkers {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// SecretFields returns the config paths a monitor type keeps encrypted, or nil
// when it has none. Asking the registry rather than the checker keeps the type
// switch in one place: a caller holding a monitor row has a type string, not a
// Checker.
func (r *Registry) SecretFields(monitorType string) []string {
	checker, ok := r.Lookup(monitorType)
	if !ok {
		return nil
	}
	confidential, ok := checker.(Confidential)
	if !ok {
		return nil
	}
	return confidential.SecretFields()
}

// Availability is an optional interface a Checker may implement when whether it
// can run is a property of the host rather than of the build.
//
// ICMP is the case that forces it to exist: the checker is compiled in, but raw
// sockets are unavailable in most container runtimes, and "this build has no
// ICMP" and "this host will not let me open the socket" are different facts that
// need different answers from the operator. A checker that does not implement
// this is available whenever it is registered.
type Availability interface {
	// Availability reports whether the checker can run here, and — whether or
	// not it can — anything degraded the operator should know. The reason is
	// carried to the control plane at registration, so it is read by a person,
	// not matched on.
	Availability() (available bool, reason string)
}

// Targeter is an optional interface a Checker may implement to name what a
// monitor points at, in one line, from its config.
//
// It exists because monitors.target is a promoted column: "what else points at
// this host?" has to be an indexed query rather than a JSON scan across 5,000
// rows (data model §4.1). Promoting it needs the config parsed, and the checker
// is the only thing that knows how — the alternative is a second table of field
// names in the API layer, which would make adding a monitor type two edits in
// two packages instead of one registration.
//
// It is also the line an alert leads with, so a checker that does not implement
// this costs its users the most useful sentence in the notification.
type Targeter interface {
	// Target is a short, human-readable identifier: a URL, a host:port, a
	// domain, a container name. Never a credential, because it is stored
	// unencrypted, indexed, and rendered into alerts.
	Target(config []byte) string
}

// Confidential is an optional interface a Checker may implement when its config
// carries values that must not be stored in plaintext.
//
// It is the checker's job because the checker owns the config schema: HTTP knows
// that a bearer token lives at auth.token, and nothing else should have to. A
// second list of field names in the storage layer would be a list that goes
// stale the first time a monitor type gains a credential — and it would go stale
// silently, because the symptom is a secret sitting in the clear rather than an
// error.
//
// A checker that does not implement this has no secrets in its config, which is
// true of TCP, ICMP, DNS, TLS expiry, and domain expiry: every one of them
// checks something anonymously.
type Confidential interface {
	// SecretFields returns dotted paths from the root of the config object —
	// "auth.password", "tls.client_key", "metadata". They are removed from the
	// stored config, sealed into their own column, and redacted on read.
	//
	// The list must match the writeOnly properties of this type's config schema
	// in docs/api/openapi.yaml. That correspondence is asserted by a test rather
	// than left to review.
	SecretFields() []string
}
