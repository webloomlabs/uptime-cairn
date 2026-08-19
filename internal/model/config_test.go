package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func object(t *testing.T, raw string) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return out
}

const httpWithAuth = `{
	"url": "https://api.example.com/health",
	"method": "GET",
	"auth": {"type": "basic", "username": "cairn", "password": "hunter2"}
}`

var httpSecrets = []string{"auth.password", "auth.token"}

func TestSplitMovesOnlyTheNamedPaths(t *testing.T) {
	t.Parallel()

	public, secret, err := SplitConfig(json.RawMessage(httpWithAuth), httpSecrets)
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	if strings.Contains(string(public), "hunter2") {
		t.Errorf("the password is still in the stored config:\n%s", public)
	}

	got := object(t, string(public))
	auth, ok := got["auth"].(map[string]any)
	if !ok {
		// The husk stays: an auth object stripped of its password still says
		// which scheme the monitor uses and as whom, and deleting it would
		// change what the config means.
		t.Fatalf("the auth object was removed entirely: %v", got)
	}
	if auth["type"] != "basic" || auth["username"] != "cairn" {
		t.Errorf("auth = %v, want type and username intact", auth)
	}
	if _, present := auth["password"]; present {
		t.Error("password survived in the public half")
	}

	held := object(t, string(secret))
	heldAuth, ok := held["auth"].(map[string]any)
	if !ok || heldAuth["password"] != "hunter2" {
		t.Errorf("secret half = %v", held)
	}
	// A path that was not set is not invented.
	if _, present := heldAuth["token"]; present {
		t.Error("an unset path was carried into the secret half")
	}
}

// The sealed half keeps the shape it was cut from, which is what lets the
// control plane put it back without being told what is in it.
func TestMergeIsTheInverseOfSplit(t *testing.T) {
	t.Parallel()

	public, secret, err := SplitConfig(json.RawMessage(httpWithAuth), httpSecrets)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	merged, err := MergeConfig(public, secret)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	if !reflect.DeepEqual(object(t, string(merged)), object(t, httpWithAuth)) {
		t.Errorf("round trip changed the config:\n%s\n%s", merged, httpWithAuth)
	}
}

func TestSplitLeavesAConfigWithNoSecretsAlone(t *testing.T) {
	t.Parallel()

	const plain = `{"hostname":"example.com","port":443}`
	public, secret, err := SplitConfig(json.RawMessage(plain), httpSecrets)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(secret) != 0 {
		t.Errorf("secret = %s, want nothing to seal", secret)
	}
	if !reflect.DeepEqual(object(t, string(public)), object(t, plain)) {
		t.Errorf("public = %s", public)
	}
}

func TestRedactMarksWhatIsSetAndNothingElse(t *testing.T) {
	t.Parallel()

	public, secret, err := SplitConfig(json.RawMessage(httpWithAuth), httpSecrets)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	redacted, err := RedactConfig(public, secret, httpSecrets)
	if err != nil {
		t.Fatalf("redact: %v", err)
	}

	got := object(t, string(redacted))
	auth := got["auth"].(map[string]any)
	if auth["password"] != Redacted {
		t.Errorf("password = %v, want the marker", auth["password"])
	}
	if _, present := auth["token"]; present {
		t.Error("an unset secret was reported as set")
	}
	if auth["username"] != "cairn" {
		t.Errorf("a non-secret field was masked: %v", auth)
	}
	if strings.Contains(string(redacted), "hunter2") {
		t.Errorf("the read shape contains the password:\n%s", redacted)
	}
}

// "Which gRPC metadata headers are set" is configuration the operator needs to
// see; the values are the part that is not.
func TestRedactingAMapKeepsItsKeys(t *testing.T) {
	t.Parallel()

	const grpc = `{"address":"api.example.com:443","metadata":{"authorization":"Bearer abc","x-tenant":"acme"}}`
	fields := []string{"metadata"}

	public, secret, err := SplitConfig(json.RawMessage(grpc), fields)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if strings.Contains(string(public), "Bearer abc") {
		t.Errorf("metadata is stored in plaintext:\n%s", public)
	}

	redacted, err := RedactConfig(public, secret, fields)
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	metadata := object(t, string(redacted))["metadata"].(map[string]any)
	if len(metadata) != 2 {
		t.Fatalf("metadata = %v, want both keys", metadata)
	}
	for key, value := range metadata {
		if value != Redacted {
			t.Errorf("%s = %v, want the marker", key, value)
		}
	}
}

func TestFindRedactedNamesEchoedMarkers(t *testing.T) {
	t.Parallel()

	const echoed = `{"url":"https://x","auth":{"type":"basic","username":"u","password":"__redacted__"}}`
	found := FindRedacted(json.RawMessage(echoed), httpSecrets)
	if len(found) != 1 || found[0] != "auth.password" {
		t.Errorf("found = %v, want auth.password", found)
	}

	if found := FindRedacted(json.RawMessage(httpWithAuth), httpSecrets); len(found) != 0 {
		t.Errorf("a real credential was mistaken for a marker: %v", found)
	}
}

func TestStripRedactedLeavesRealValuesAlone(t *testing.T) {
	t.Parallel()

	const echoed = `{"url":"https://x","auth":{"type":"bearer","token":"__redacted__"}}`
	stripped, err := StripRedacted(json.RawMessage(echoed), httpSecrets)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	auth := object(t, string(stripped))["auth"].(map[string]any)
	if _, present := auth["token"]; present {
		t.Error("the marker survived and would be stored as a credential")
	}
	if auth["type"] != "bearer" {
		t.Errorf("auth = %v", auth)
	}

	unchanged, err := StripRedacted(json.RawMessage(httpWithAuth), httpSecrets)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if !reflect.DeepEqual(object(t, string(unchanged)), object(t, httpWithAuth)) {
		t.Error("a real credential was stripped")
	}
}

// A path whose parent is missing must not create the parent, or a Docker monitor
// with no TLS block would grow an empty one on every read.
func TestAbsentParentIsNotInvented(t *testing.T) {
	t.Parallel()

	const docker = `{"container":"web"}`
	fields := []string{"tls.client_key"}

	public, secret, err := SplitConfig(json.RawMessage(docker), fields)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(secret) != 0 {
		t.Errorf("secret = %s", secret)
	}
	if _, present := object(t, string(public))["tls"]; present {
		t.Errorf("a tls block was invented: %s", public)
	}

	redacted, err := RedactConfig(public, secret, fields)
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if _, present := object(t, string(redacted))["tls"]; present {
		t.Errorf("a tls block was invented on read: %s", redacted)
	}
}

func TestMalformedConfigIsAnErrorNotAPanic(t *testing.T) {
	t.Parallel()

	if _, _, err := SplitConfig(json.RawMessage(`not json`), httpSecrets); err == nil {
		t.Error("malformed config was accepted")
	}
	// A config whose "auth" is a string rather than an object is nonsense, but
	// walking into it must fail quietly rather than assert its way to a panic.
	public, secret, err := SplitConfig(json.RawMessage(`{"auth":"nonsense"}`), httpSecrets)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(secret) != 0 || !strings.Contains(string(public), "nonsense") {
		t.Errorf("public = %s, secret = %s", public, secret)
	}
}
