package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// TLSExpiry implements the tls_expiry monitor type.
//
// The handshake is deliberately made with verification disabled and the chain
// verified afterwards by hand. Letting crypto/tls verify would abort the
// handshake on an already-expired certificate, and this monitor would then
// report a generic TLS error at the exact moment its entire purpose is to say
// "the certificate expired". Separating the two means expiry is always measured
// and chain problems are still reported, each with its own message.
type TLSExpiry struct{}

// NewTLSExpiry builds the checker.
func NewTLSExpiry() *TLSExpiry { return &TLSExpiry{} }

// Type implements Checker.
func (t *TLSExpiry) Type() string { return model.TypeTLSExpiry }

// Version implements Checker.
func (t *TLSExpiry) Version() uint32 { return 1 }

// tlsExpiryConfig mirrors TlsExpiryConfig in docs/api/openapi.yaml. Pointers
// where the spec's default is non-zero, so "unset" and "explicitly zero" stay
// distinguishable — a zero threshold means "only tell me once it has actually
// expired", which is a real choice.
type tlsExpiryConfig struct {
	Hostname               string  `json:"hostname"`
	Port                   *int    `json:"port"`
	ServerName             *string `json:"server_name"`
	DaysRemainingThreshold *int    `json:"days_remaining_threshold"`
	VerifyChain            *bool   `json:"verify_chain"`
}

const (
	defaultTLSPort      = 443
	defaultTLSThreshold = 14
)

// Validate implements Checker.
func (t *TLSExpiry) Validate(config []byte) error {
	cfg, err := decodeTLSExpiryConfig(config)
	if err != nil {
		return err
	}
	if err := validateHostname(cfg.Hostname); err != nil {
		return err
	}
	if cfg.Port != nil {
		if err := validatePort(*cfg.Port); err != nil {
			return err
		}
	}
	if cfg.DaysRemainingThreshold != nil {
		if d := *cfg.DaysRemainingThreshold; d < 0 || d > 3650 {
			return fmt.Errorf("days_remaining_threshold %d is outside 0-3650", d)
		}
	}
	if cfg.ServerName != nil && *cfg.ServerName != "" {
		if err := validateHostname(*cfg.ServerName); err != nil {
			return fmt.Errorf("server_name: %w", err)
		}
	}
	return nil
}

// Check implements Checker.
func (t *TLSExpiry) Check(ctx context.Context, config []byte) Observation {
	cfg, err := decodeTLSExpiryConfig(config)
	if err != nil {
		return Observation{Status: model.StatusUnknown, Class: ClassConfig, Message: err.Error()}
	}

	port := defaultTLSPort
	if cfg.Port != nil {
		port = *cfg.Port
	}
	threshold := defaultTLSThreshold
	if cfg.DaysRemainingThreshold != nil {
		threshold = *cfg.DaysRemainingThreshold
	}
	serverName := cfg.Hostname
	if cfg.ServerName != nil && *cfg.ServerName != "" {
		serverName = *cfg.ServerName
	}

	address := net.JoinHostPort(cfg.Hostname, strconv.Itoa(port))
	dialer := &tls.Dialer{
		Config: &tls.Config{
			// Verified by hand below; see the type comment.
			InsecureSkipVerify: true, //nolint:gosec // the chain is verified explicitly so expiry is measurable on an expired certificate
			ServerName:         serverName,
		},
	}

	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", address)
	elapsed := time.Since(start)
	if err != nil {
		return classify(err, elapsed)
	}
	defer func() { _ = conn.Close() }()

	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		// A completed handshake with no certificate is possible in principle
		// (PSK) and means this monitor has nothing to measure.
		return Observation{
			Status:       model.StatusUnknown,
			Class:        ClassTLS,
			ResponseTime: &elapsed,
			Message:      "handshake presented no certificate to inspect",
		}
	}
	leaf := state.PeerCertificates[0]

	now := time.Now()
	remaining := leaf.NotAfter.Sub(now)
	days := int(math.Floor(remaining.Hours() / 24))

	obs := Observation{
		Status:       model.StatusUp,
		ResponseTime: &elapsed,
		Code:         strconv.Itoa(days),
	}

	// Expiry first: it is what this monitor is for, and an expired certificate
	// also fails chain verification, so checking the chain first would report
	// the vaguer of the two causes.
	switch {
	case remaining <= 0:
		obs.Status = model.StatusDown
		obs.Class = ClassTLS
		obs.Message = fmt.Sprintf("certificate for %s expired %s ago, on %s",
			serverName, humaniseDays(-days), leaf.NotAfter.UTC().Format(time.RFC3339))
		return obs
	case days <= threshold:
		obs.Status = model.StatusDown
		obs.Class = ClassAssertion
		obs.Message = fmt.Sprintf("certificate for %s expires in %s, on %s — inside the %d-day threshold",
			serverName, humaniseDays(days), leaf.NotAfter.UTC().Format(time.RFC3339), threshold)
		return obs
	}

	if cfg.VerifyChain == nil || *cfg.VerifyChain {
		if err := verifyChain(state, serverName, now); err != nil {
			obs.Status = model.StatusDown
			obs.Class = ClassTLS
			obs.Message = "certificate chain verification failed: " + err.Error()
			return obs
		}
	}

	obs.Message = fmt.Sprintf("certificate valid for %s, until %s",
		humaniseDays(days), leaf.NotAfter.UTC().Format(time.RFC3339))
	return obs
}

// verifyChain runs the verification the handshake skipped, against the system
// roots and the intermediates the server actually sent.
func verifyChain(state tls.ConnectionState, serverName string, now time.Time) error {
	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
		DNSName:       serverName,
		Intermediates: intermediates,
		CurrentTime:   now,
	})
	return err
}

// humaniseDays keeps the message readable at both ends of the range: "0 days"
// on the day of expiry reads as a bug, and "1 days" reads as one too.
func humaniseDays(days int) string {
	switch {
	case days == 1:
		return "1 day"
	case days <= 0:
		return "under a day"
	default:
		return strconv.Itoa(days) + " days"
	}
}

func decodeTLSExpiryConfig(config []byte) (tlsExpiryConfig, error) {
	var cfg tlsExpiryConfig
	dec := json.NewDecoder(strings.NewReader(string(config)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}
