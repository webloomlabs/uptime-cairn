package check

import (
	"encoding/json"
	"net"
	"strconv"
)

// Target implementations, one per checker.
//
// Gathered in one file rather than scattered across eight because they are eight
// one-line answers to the same question, and reading them together is how you
// check that none of them returns something with a credential in it. Every one
// of these values is stored unencrypted, indexed, and rendered into alerts.

// Target implements Targeter.
func (h *HTTP) Target(config []byte) string {
	var cfg httpConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ""
	}
	// The URL verbatim, including any query string. Credentials in an HTTP
	// monitor live in the auth object, not in the URL — validation rejects
	// userinfo — so this is safe to store and to show.
	return cfg.URL
}

// Target implements Targeter.
func (t *TCP) Target(config []byte) string {
	var cfg tcpConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ""
	}
	return net.JoinHostPort(cfg.Hostname, strconv.Itoa(cfg.Port))
}

// Target implements Targeter.
func (i *ICMP) Target(config []byte) string {
	var cfg icmpConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ""
	}
	return cfg.Hostname
}

// Target implements Targeter.
func (d *DNS) Target(config []byte) string {
	var cfg dnsConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ""
	}
	// The record type is part of the identity: two monitors on the same name
	// asking for A and MX are not checking the same thing.
	if cfg.RecordType != "" {
		return cfg.Hostname + " " + cfg.RecordType
	}
	return cfg.Hostname
}

// Target implements Targeter.
func (t *TLSExpiry) Target(config []byte) string {
	var cfg tlsExpiryConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ""
	}
	port := 443
	if cfg.Port != nil {
		port = *cfg.Port
	}
	return net.JoinHostPort(cfg.Hostname, strconv.Itoa(port))
}

// Target implements Targeter.
func (d *DomainExpiry) Target(config []byte) string {
	var cfg domainExpiryConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ""
	}
	return cfg.Domain
}

// Target implements Targeter.
func (d *Docker) Target(config []byte) string {
	var cfg dockerConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ""
	}
	return cfg.Container
}

// Target implements Targeter.
func (g *GRPC) Target(config []byte) string {
	var cfg grpcConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ""
	}
	// The service name matters as much as the address: one server answers health
	// checks for several services, and an alert naming only the host would not
	// say which one is failing.
	if cfg.ServiceName != nil && *cfg.ServiceName != "" {
		return cfg.Address + "/" + *cfg.ServiceName
	}
	return cfg.Address
}
