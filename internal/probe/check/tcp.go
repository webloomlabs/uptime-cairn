package check

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// TCP implements the tcp monitor type: a completed handshake within the timeout
// is up, anything else is down.
//
// Deliberately no read or write after connecting. Sending a probe byte would
// make this monitor type protocol-aware, and half the services worth watching
// close the connection on unexpected input — which would report a healthy
// service as broken.
type TCP struct{}

// NewTCP builds the checker. It holds no state: a TCP connect has nothing worth
// pooling, and pooling is the thing that makes a check lie (protocol.md §6.1).
func NewTCP() *TCP { return &TCP{} }

// Type implements Checker.
func (t *TCP) Type() string { return model.TypeTCP }

// Version implements Checker.
func (t *TCP) Version() uint32 { return 1 }

// tcpConfig mirrors TcpConfig in docs/api/openapi.yaml.
type tcpConfig struct {
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
	IPFamily string `json:"ip_family"`
}

// Validate implements Checker.
func (t *TCP) Validate(config []byte) error {
	cfg, err := decodeTCPConfig(config)
	if err != nil {
		return err
	}
	if err := validateHostname(cfg.Hostname); err != nil {
		return err
	}
	if err := validatePort(cfg.Port); err != nil {
		return err
	}
	return validateIPFamily(cfg.IPFamily)
}

// Check implements Checker.
func (t *TCP) Check(ctx context.Context, config []byte) Observation {
	cfg, err := decodeTCPConfig(config)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}
	return tcpConnect(ctx, cfg.Hostname, cfg.Port, cfg.IPFamily)
}

// tcpConnect is shared with the ICMP checker's TCP fallback, so both spell a
// connect check exactly one way.
func tcpConnect(ctx context.Context, host string, port int, family string) Observation {
	address := net.JoinHostPort(host, strconv.Itoa(port))

	var dialer net.Dialer
	start := time.Now()
	conn, err := dialer.DialContext(ctx, networkFor("tcp", family), address)
	elapsed := time.Since(start)
	if err != nil {
		obs := classify(err, elapsed)
		// A refused connection is the one network error that is unambiguously
		// about the target rather than the path to it, and saying so beats
		// "connect: connection refused" with a Go type name attached.
		if obs.Class == ClassNetwork && strings.Contains(err.Error(), "connection refused") {
			obs.Message = fmt.Sprintf("connection to %s refused", address)
		}
		return obs
	}
	_ = conn.Close()

	return Observation{Status: model.StatusUp, ResponseTime: &elapsed}
}

func decodeTCPConfig(config []byte) (tcpConfig, error) {
	var cfg tcpConfig
	dec := json.NewDecoder(strings.NewReader(string(config)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
