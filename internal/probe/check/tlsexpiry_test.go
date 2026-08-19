package check

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
)

// serveTLS starts a listener presenting a certificate with the given validity
// window and returns its port. Real certificates rather than a mock: the whole
// question this checker answers is what crypto/tls saw on the wire.
func serveTLS(t *testing.T, notBefore, notAfter time.Time) int {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				// Force the handshake so the client gets the certificate even
				// though no application data ever flows.
				_ = conn.(*tls.Conn).Handshake()
				_ = conn.Close()
			}()
		}
	}()

	return listener.Addr().(*net.TCPAddr).Port
}

func TestTLSExpiry(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tests := []struct {
		name      string
		notBefore time.Time
		notAfter  time.Time
		threshold string
		want      model.Status
		class     ErrorClass
	}{
		{
			name:      "plenty of time left",
			notBefore: now.Add(-24 * time.Hour),
			notAfter:  now.Add(365 * 24 * time.Hour),
			want:      model.StatusUp,
		},
		{
			name:      "inside the threshold",
			notBefore: now.Add(-24 * time.Hour),
			notAfter:  now.Add(5 * 24 * time.Hour),
			want:      model.StatusDown,
			class:     ClassAssertion,
		},
		{
			// The case the whole design is arranged around: crypto/tls would
			// abort the handshake here, and this monitor would report a generic
			// TLS error at the exact moment its job is to say "it expired".
			name:      "already expired",
			notBefore: now.Add(-365 * 24 * time.Hour),
			notAfter:  now.Add(-24 * time.Hour),
			want:      model.StatusDown,
			class:     ClassTLS,
		},
		{
			name:      "expired but the threshold is zero",
			notBefore: now.Add(-365 * 24 * time.Hour),
			notAfter:  now.Add(-24 * time.Hour),
			threshold: `,"days_remaining_threshold":0`,
			want:      model.StatusDown,
			class:     ClassTLS,
		},
	}

	checker := NewTLSExpiry()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			port := serveTLS(t, tc.notBefore, tc.notAfter)
			// verify_chain is off: these are self-signed, and chain trust is a
			// separate assertion tested below.
			config := `{"hostname":"127.0.0.1","port":` + strconv.Itoa(port) +
				`,"server_name":"localhost","verify_chain":false` + tc.threshold + `}`

			if err := checker.Validate([]byte(config)); err != nil {
				t.Fatalf("validate: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			obs := checker.Check(ctx, []byte(config))
			if obs.Status != tc.want {
				t.Errorf("status = %s, want %s (%s)", obs.Status, tc.want, obs.Message)
			}
			if tc.class != "" && obs.Class != tc.class {
				t.Errorf("class = %q, want %q (%s)", obs.Class, tc.class, obs.Message)
			}
			if obs.Code == "" {
				t.Error("no days-remaining code recorded")
			}
		})
	}
}

// With verify_chain on, an untrusted chain is down — but expiry is still
// measured, which is why the code carries a days figure on the failure too.
func TestTLSExpiryChainVerification(t *testing.T) {
	t.Parallel()

	now := time.Now()
	port := serveTLS(t, now.Add(-24*time.Hour), now.Add(365*24*time.Hour))
	config := `{"hostname":"127.0.0.1","port":` + strconv.Itoa(port) + `,"server_name":"localhost"}`

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obs := NewTLSExpiry().Check(ctx, []byte(config))
	if obs.Status != model.StatusDown {
		t.Errorf("status = %s, want down for a self-signed chain", obs.Status)
	}
	if obs.Class != ClassTLS {
		t.Errorf("class = %q, want %q", obs.Class, ClassTLS)
	}
	if obs.Code == "" {
		t.Error("chain failure lost the days-remaining figure; both facts are useful")
	}
}

func TestTLSExpiryValidate(t *testing.T) {
	t.Parallel()

	checker := NewTLSExpiry()
	rejected := map[string]string{
		"no hostname":        `{"port":443}`,
		"port out of range":  `{"hostname":"example.com","port":0}`,
		"threshold too big":  `{"hostname":"example.com","days_remaining_threshold":4000}`,
		"negative threshold": `{"hostname":"example.com","days_remaining_threshold":-1}`,
		"url in hostname":    `{"hostname":"https://example.com"}`,
		"unknown field":      `{"hostname":"example.com","nope":1}`,
	}
	for name, config := range rejected {
		if err := checker.Validate([]byte(config)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := checker.Validate([]byte(`{"hostname":"example.com"}`)); err != nil {
		t.Errorf("minimal config rejected: %v", err)
	}
}

func TestHumaniseDays(t *testing.T) {
	t.Parallel()

	// "0 days" and "1 days" both read as bugs in a status message someone is
	// looking at during an incident.
	for days, want := range map[int]string{0: "under a day", 1: "1 day", 2: "2 days", -3: "under a day"} {
		if got := humaniseDays(days); got != want {
			t.Errorf("humaniseDays(%d) = %q, want %q", days, got, want)
		}
	}
}
