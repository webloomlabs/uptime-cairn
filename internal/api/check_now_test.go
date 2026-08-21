package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
)

// observingChecker stands in for the TLS checker: it reports up and hands back
// a certificate, which is the pair of facts a real handshake produces. A real
// checker here would need a listener with a certificate on it, and what is
// under test is the seam rather than the handshake.
type observingChecker struct {
	notAfter time.Time
}

func (observingChecker) Type() string          { return "tls_expiry" }
func (observingChecker) Version() uint32       { return 1 }
func (observingChecker) Validate([]byte) error { return nil }

func (o observingChecker) Check(context.Context, []byte) check.Observation {
	elapsed := 12 * time.Millisecond
	valid := true
	return check.Observation{
		Status:       model.StatusUp,
		ResponseTime: &elapsed,
		Certificate: &check.Certificate{
			Subject:           "CN=example.test",
			Issuer:            "CN=Test CA",
			SerialNumber:      "01",
			NotBefore:         o.notAfter.AddDate(0, -3, 0),
			NotAfter:          o.notAfter,
			FingerprintSHA256: []byte("0123456789abcdef0123456789abcdef"),
			SANs:              []string{"example.test"},
			ChainValid:        &valid,
		},
	}
}

// Check-now used to refresh the verdict and drop the certificate, so pressing
// the button after installing a new certificate left the expiry panel showing
// the one that had just been replaced until the next scheduled check. The
// mapping that made that a seam problem rather than a patch now lives in
// internal/observation, and this is the behaviour it buys.
func TestCheckNowRecordsTheCertificateItObserved(t *testing.T) {
	t.Parallel()

	expiry := time.Now().UTC().AddDate(0, 2, 0).Truncate(time.Second)
	server := testServer2(t, observingChecker{notAfter: expiry})
	c := newClient(t, server)
	c.setup()

	resp, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "example.test certificate",
		"type": "tls_expiry",
		"config": map[string]any{
			"host": "example.test", "port": 443, "days_remaining_threshold": 14,
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v)", resp.StatusCode, created)
	}
	id := created["id"].(string)

	// Nothing observed yet: the endpoint is honest about that rather than
	// inventing a row.
	if resp, _ := c.do(http.MethodGet, "/api/v1/monitors/"+id+"/certificate", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("certificate before any check = %d, want 404", resp.StatusCode)
	}

	resp, beat := c.do(http.MethodPost, "/api/v1/monitors/"+id+"/check", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("check now = %d (%v)", resp.StatusCode, beat)
	}
	if beat["status"] != "up" {
		t.Errorf("status = %v, want up", beat["status"])
	}

	resp, certificate := c.do(http.MethodGet, "/api/v1/monitors/"+id+"/certificate", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("certificate after check now = %d (%v), want the row the check just observed",
			resp.StatusCode, certificate)
	}
	if certificate["subject"] != "CN=example.test" {
		t.Errorf("subject = %v", certificate["subject"])
	}
	if certificate["issuer"] != "CN=Test CA" {
		t.Errorf("issuer = %v", certificate["issuer"])
	}
	valid, ok := certificate["valid_to"].(string)
	if !ok {
		t.Fatalf("valid_to = %v, want a timestamp", certificate["valid_to"])
	}
	if got, err := time.Parse(time.RFC3339, valid); err != nil {
		t.Fatalf("valid_to %q does not parse: %v", valid, err)
	} else if !got.Equal(expiry) {
		t.Errorf("valid_to = %s, want %s", got, expiry)
	}
}
