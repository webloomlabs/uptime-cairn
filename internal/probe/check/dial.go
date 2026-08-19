package check

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// The dialling vocabulary every non-HTTP checker shares. Six checkers resolving
// ip_family six slightly different ways is how "auto" comes to mean three things
// depending on which monitor type you picked.

// networkFor maps an ip_family setting onto a Go network name. "auto" leaves the
// choice to the resolver, which is the behaviour the spec's default describes.
func networkFor(base, family string) string {
	switch family {
	case "ipv4":
		return base + "4"
	case "ipv6":
		return base + "6"
	default:
		return base
	}
}

// validateIPFamily is the shared spelling of the enum, so a typo is rejected
// identically on every type that offers it.
func validateIPFamily(family string) error {
	switch family {
	case "", "auto", "ipv4", "ipv6":
		return nil
	default:
		return fmt.Errorf("ip_family %q: want auto, ipv4, or ipv6", family)
	}
}

// validateHostname rejects the mistakes a form actually produces: a URL pasted
// into a hostname field, a "host:port" pasted into a field that has a separate
// port, and whitespace. Catching them here makes them a 422 the user reads
// rather than a DNS failure they have to interpret.
func validateHostname(host string) error {
	if host == "" {
		return errors.New("hostname is required")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("hostname %q contains whitespace", host)
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("hostname %q looks like a URL; give the host alone", host)
	}
	if strings.Contains(host, "/") {
		return fmt.Errorf("hostname %q contains a path; give the host alone", host)
	}
	// A bare IPv6 literal is legitimate and full of colons, so only complain
	// about a colon when what precedes it is not one.
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return fmt.Errorf("hostname %q contains a port; give the host alone", host)
	}
	return nil
}

// validatePort covers the whole 1-65535 range because monitoring a service on a
// privileged port is the normal case, not the exception.
func validatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d is outside 1-65535", port)
	}
	return nil
}

// splitHostPort is net.SplitHostPort with an error a user can act on. The
// standard message names neither the field nor what a correct value looks like.
func splitHostPort(address string) (string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", "", fmt.Errorf("address %q: want host:port", address)
	}
	if host == "" {
		return "", "", fmt.Errorf("address %q has no host", address)
	}
	if err := validatePort(atoiPort(port)); err != nil {
		return "", "", fmt.Errorf("address %q: %w", address, err)
	}
	return host, port, nil
}

func atoiPort(port string) int {
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}
