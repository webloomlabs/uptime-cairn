package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/store/sqlite"
)

// Monitor credentials, from the outside and from the row.
//
// The assertion that matters is the one about the stored row rather than the
// response: a serialiser can be made to hide a value, and the point of this
// design is that there is no value in the column being serialised to hide.

func secretsClient(t *testing.T, extra ...check.Checker) (*client, *sqlite.Store) {
	t.Helper()

	server, store := testServerWithStore(t, extra...)
	c := newClient(t, server)
	c.setup()
	return c, store
}

func storedMonitor(t *testing.T, store *sqlite.Store, id string) model.Monitor {
	t.Helper()

	parsed, ok := model.ParseID(id)
	if !ok {
		t.Fatalf("unparseable id %q", id)
	}
	m, err := store.GetMonitor(t.Context(), parsed)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return m.Monitor
}

func TestHTTPCredentialsAreNotStoredInConfig(t *testing.T) {
	t.Parallel()

	c, store := secretsClient(t)
	const password = "correct-horse-battery-staple"

	resp, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "API gateway", "type": "http",
		"config": map[string]any{
			"url":  "https://api.example.com/health",
			"auth": map[string]any{"type": "basic", "username": "cairn", "password": password},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v)", resp.StatusCode, created)
	}

	stored := storedMonitor(t, store, created["id"].(string))
	if strings.Contains(string(stored.Config), password) {
		t.Errorf("the password is in monitors.config:\n%s", stored.Config)
	}
	if len(stored.ConfigSecrets) == 0 {
		t.Fatal("nothing was sealed, so the password went somewhere else or nowhere")
	}
	if strings.Contains(string(stored.ConfigSecrets), password) {
		t.Error("the sealed column holds the password in the clear")
	}

	// The non-secret half is untouched, and still queryable as JSON.
	if !strings.Contains(string(stored.Config), "api.example.com") {
		t.Errorf("the stored config lost its url:\n%s", stored.Config)
	}
	if !strings.Contains(string(stored.Config), "cairn") {
		t.Errorf("the username was swept up with the password:\n%s", stored.Config)
	}
}

func TestCredentialsAreRedactedOnEveryRead(t *testing.T) {
	t.Parallel()

	c, _ := secretsClient(t)
	const token = "xoxb-not-a-real-token"

	resp, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "API gateway", "type": "http",
		"config": map[string]any{
			"url":  "https://api.example.com/health",
			"auth": map[string]any{"type": "bearer", "token": token},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v)", resp.StatusCode, created)
	}
	id := created["id"].(string)

	// The create response, the single read, and the list all go through the
	// same assembly — and all three are checked, because "redacted on read"
	// that holds for two of three is not a property.
	for _, body := range []map[string]any{created,
		fetchOne(t, c, "/api/v1/monitors/"+id),
		firstOfList(t, c, "/api/v1/monitors"),
	} {
		config := body["config"].(map[string]any)
		auth, ok := config["auth"].(map[string]any)
		if !ok {
			t.Fatalf("config = %v", config)
		}
		if auth["token"] != model.Redacted {
			t.Errorf("token = %v, want the marker", auth["token"])
		}
		if auth["type"] != "bearer" {
			t.Errorf("auth type was masked too: %v", auth)
		}
	}
}

// An echoed marker is refused rather than stored. Accepting it produces a
// monitor that looks configured and authenticates as nobody, and the failure
// arrives hours later looking like the target's fault.
func TestEchoedRedactionIsRefused(t *testing.T) {
	t.Parallel()

	c, _ := secretsClient(t)
	resp, body := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "API gateway", "type": "http",
		"config": map[string]any{
			"url":  "https://api.example.com/health",
			"auth": map[string]any{"type": "basic", "username": "cairn", "password": model.Redacted},
		},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("create = %d, want 422 (%v)", resp.StatusCode, body)
	}

	errors, _ := body["errors"].([]any)
	first, _ := errors[0].(map[string]any)
	if first["code"] != "redacted" {
		t.Errorf("code = %v", first["code"])
	}
	if first["pointer"] != "/config/auth/password" {
		t.Errorf("pointer = %v, want the field that is wrong", first["pointer"])
	}
}

func TestGRPCMetadataKeepsItsKeys(t *testing.T) {
	t.Parallel()

	c, store := secretsClient(t, check.NewGRPC())
	resp, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "Orders", "type": "grpc",
		"config": map[string]any{
			"address":  "orders.internal:443",
			"metadata": map[string]any{"authorization": "Bearer abc123", "x-tenant": "acme"},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v)", resp.StatusCode, created)
	}

	stored := storedMonitor(t, store, created["id"].(string))
	if strings.Contains(string(stored.Config), "Bearer abc123") {
		t.Errorf("metadata is stored in plaintext:\n%s", stored.Config)
	}

	metadata := created["config"].(map[string]any)["metadata"].(map[string]any)
	if len(metadata) != 2 {
		t.Fatalf("metadata = %v, want both keys back", metadata)
	}
	for key, value := range metadata {
		if value != model.Redacted {
			t.Errorf("%s = %v, want the marker", key, value)
		}
	}
}

func TestDockerTLSMaterialIsSealed(t *testing.T) {
	t.Parallel()

	c, store := secretsClient(t, check.NewDocker())
	cert, key := clientKeypair(t)

	resp, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "web container", "type": "docker",
		"config": map[string]any{
			"container":   "web",
			"docker_host": "tcp://docker.internal:2376",
			// The checker requires the pair, which is correct: half a client
			// certificate authenticates nothing.
			"tls": map[string]any{"client_key": key, "client_cert": cert, "verify": true},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v)", resp.StatusCode, created)
	}

	stored := storedMonitor(t, store, created["id"].(string))
	if strings.Contains(string(stored.Config), "PRIVATE KEY") {
		t.Errorf("the client key is in monitors.config:\n%s", stored.Config)
	}
	if strings.Contains(string(stored.Config), "CERTIFICATE") {
		t.Errorf("the client certificate is in monitors.config:\n%s", stored.Config)
	}

	tlsConfig := created["config"].(map[string]any)["tls"].(map[string]any)
	if tlsConfig["client_key"] != model.Redacted {
		t.Errorf("client_key = %v", tlsConfig["client_key"])
	}
	// verify is not a credential and must survive, or the read cannot be used
	// to reconstruct the monitor.
	if tlsConfig["verify"] != true {
		t.Errorf("tls = %v, want verify intact", tlsConfig)
	}
}

// A type with nothing to hide must round-trip byte for byte, or every read of
// every TCP monitor would be reassembling JSON for no reason.
func TestConfigWithoutCredentialsIsUntouched(t *testing.T) {
	t.Parallel()

	c, store := secretsClient(t)
	resp, created := c.do(http.MethodPost, "/api/v1/monitors", map[string]any{
		"name": "Plain", "type": "http",
		"config": map[string]any{"url": "https://example.com", "method": "HEAD"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v)", resp.StatusCode, created)
	}

	stored := storedMonitor(t, store, created["id"].(string))
	if len(stored.ConfigSecrets) != 0 {
		t.Error("an empty secret half was sealed rather than left null")
	}
	config := created["config"].(map[string]any)
	if config["url"] != "https://example.com" || config["method"] != "HEAD" {
		t.Errorf("config = %v", config)
	}
}

// clientKeypair generates a real one, because the Docker checker parses what it
// is given — correctly, since half a client certificate authenticates nothing.
func clientKeypair(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "cairn-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}))
}

func fetchOne(t *testing.T, c *client, path string) map[string]any {
	t.Helper()

	resp, body := c.do(http.MethodGet, path, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, resp.StatusCode)
	}
	return body
}

func firstOfList(t *testing.T, c *client, path string) map[string]any {
	t.Helper()

	body := fetchOne(t, c, path)
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatalf("GET %s returned nothing", path)
	}
	return data[0].(map[string]any)
}
