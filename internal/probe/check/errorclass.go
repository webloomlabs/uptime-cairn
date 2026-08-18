package check

// ErrorClass says why a check failed, one level below the outcome. It drives the
// phrasing in the UI and the grouping in probe health; it never drives alerting,
// which branches on the outcome alone.
type ErrorClass string

const (
	ClassNone ErrorClass = ""

	// ClassAssertion is the target answering, and the answer being wrong.
	ClassAssertion ErrorClass = "assertion"

	ClassTimeout ErrorClass = "timeout"

	// ClassDNS pairs with down when the target's name does not resolve, and with
	// unknown when the probe's own resolver is broken. Telling those apart is the
	// checker's job and it must not guess.
	ClassDNS ErrorClass = "dns"

	ClassTLS     ErrorClass = "tls"
	ClassNetwork ErrorClass = "network"

	// ClassConfig is always unknown: the probe could not run the check at all.
	ClassConfig ErrorClass = "config"

	// ClassCapability is the probe having no checker for this type here — an
	// ICMP monitor on a host without CAP_NET_RAW, or a type this build does not
	// implement. Always unknown.
	ClassCapability ErrorClass = "capability"
)
