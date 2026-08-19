package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
)

// The control plane is the only place other than the API that holds a monitor's
// configuration whole, and it holds it for exactly as long as it takes to put an
// assignment on the wire.

func testConfigVault(t *testing.T) *secrets.Vault {
	t.Helper()

	key, err := secrets.NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := secrets.NewKeeper(1, map[uint32][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	return secrets.NewVault(keeper, "monitors", "config")
}

// sealedMonitor is a monitor in the state the store holds it in: public config,
// credentials in an opaque blob beside it.
func sealedMonitor(t *testing.T, vault *secrets.Vault) model.Monitor {
	t.Helper()

	m := model.Monitor{
		ID: model.NewID(), OrgID: model.SentinelOrgID,
		Name: "API gateway", Type: model.TypeHTTP,
		Config:   json.RawMessage(`{"url":"https://api.example.com/health","auth":{"type":"bearer"}}`),
		Interval: time.Minute, Timeout: 30 * time.Second,
	}
	sealed, err := vault.Seal(m.OrgID[:], m.ID[:], []byte(`{"auth":{"token":"xoxb-secret"}}`))
	if err != nil {
		t.Fatal(err)
	}
	m.ConfigSecrets = sealed
	return m
}

func TestAssignmentCarriesTheDecryptedConfig(t *testing.T) {
	t.Parallel()

	vault := testConfigVault(t)
	monitor := sealedMonitor(t, vault)

	store := &fakeStore{monitor: monitor, state: model.MonitorState{MonitorID: monitor.ID, OrgID: monitor.OrgID}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(store, NewPublisher(), nil, vault, log, model.EmbeddedProbeID, model.SentinelOrgID)

	set, _, err := server.assignments(context.Background())
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	assignment, ok := set[monitor.ID.String()]
	if !ok {
		t.Fatalf("the monitor was not assigned: %v", set)
	}

	var config struct {
		URL  string `json:"url"`
		Auth struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(assignment.GetConfig(), &config); err != nil {
		t.Fatalf("assignment config is not JSON: %v", err)
	}
	if config.Auth.Token != "xoxb-secret" {
		t.Errorf("token = %q — the probe cannot authenticate without it", config.Auth.Token)
	}
	if config.URL != "https://api.example.com/health" || config.Auth.Type != "bearer" {
		t.Errorf("the public half was lost: %+v", config)
	}
}

// An HTTP monitor missing its bearer token would authenticate as nobody and
// report the target down, which is a lie about the target. Withholding it is
// visibly wrong instead.
func TestMonitorIsWithheldWhenItsCredentialsCannotBeRead(t *testing.T) {
	t.Parallel()

	monitor := sealedMonitor(t, testConfigVault(t))

	// A different key: what a restored database without its key file looks like.
	store := &fakeStore{monitor: monitor, state: model.MonitorState{MonitorID: monitor.ID, OrgID: monitor.OrgID}}
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	server := New(store, NewPublisher(), nil, testConfigVault(t), log, model.EmbeddedProbeID, model.SentinelOrgID)

	set, _, err := server.assignments(context.Background())
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	if len(set) != 0 {
		t.Errorf("a monitor with unreadable credentials was assigned anyway: %v", set)
	}
	if !strings.Contains(logged.String(), monitor.ID.String()) {
		t.Errorf("the withheld monitor was not named in the log:\n%s", logged.String())
	}
}

// A monitor with nothing sealed is passed through untouched, which is every
// monitor of the five types that check anonymously.
func TestMonitorWithoutCredentialsNeedsNoKey(t *testing.T) {
	t.Parallel()

	monitor := model.Monitor{
		ID: model.NewID(), OrgID: model.SentinelOrgID, Type: model.TypeTCP,
		Config: json.RawMessage(`{"hostname":"example.com","port":443}`), Interval: time.Minute,
	}
	store := &fakeStore{monitor: monitor, state: model.MonitorState{MonitorID: monitor.ID, OrgID: monitor.OrgID}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(store, NewPublisher(), nil, nil, log, model.EmbeddedProbeID, model.SentinelOrgID)

	set, _, err := server.assignments(context.Background())
	if err != nil {
		t.Fatalf("assignments: %v", err)
	}
	assignment := set[monitor.ID.String()]
	if assignment == nil || string(assignment.GetConfig()) != string(monitor.Config) {
		t.Errorf("config = %s, want it verbatim", assignment.GetConfig())
	}
}

// The AAD is not decoration: without it a blob can be relocated onto another
// monitor's row without ever being decrypted.
func TestSealedCredentialsDoNotOpenOnAnotherMonitor(t *testing.T) {
	t.Parallel()

	vault := testConfigVault(t)
	monitor := sealedMonitor(t, vault)

	other := model.NewID()
	if _, err := vault.Open(monitor.OrgID[:], other[:], monitor.ConfigSecrets); err == nil {
		t.Fatal("a relocated ciphertext opened against the wrong monitor")
	} else if !errors.Is(err, secrets.ErrWrongKey) {
		t.Errorf("err = %v, want ErrWrongKey", err)
	}
}
