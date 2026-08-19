package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
	"github.com/webloomlabs/uptime-cairn/internal/store/sqlite"
)

// The re-sealing pass is the migration path, and a migration path nobody runs
// twice is a migration path nobody has tested. These do.

func testStore(t *testing.T) *sqlite.Store {
	t.Helper()

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "cairn.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func testRegistry() *check.Registry {
	registry := check.NewRegistry()
	registry.Register(check.NewHTTP())
	registry.Register(check.NewTCP())
	registry.Register(check.NewGRPC())
	return registry
}

func testVault(t *testing.T) *secrets.Vault {
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

// legacyMonitor is a row as it was written before credentials were encrypted:
// everything in config, nothing sealed.
func legacyMonitor(t *testing.T, store *sqlite.Store, name, monitorType, config string, enabled bool) model.ID {
	t.Helper()

	now := time.Now().UTC().Truncate(time.Millisecond)
	m := model.Monitor{
		ID: model.NewID(), OrgID: model.SentinelOrgID, Name: name, Type: monitorType,
		Config: json.RawMessage(config), Enabled: enabled,
		Interval: time.Minute, Timeout: 30 * time.Second,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateMonitor(t.Context(), m); err != nil {
		t.Fatalf("create: %v", err)
	}
	return m.ID
}

func TestResealMovesPlaintextCredentials(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	registry := testRegistry()
	vault := testVault(t)

	withAuth := legacyMonitor(t, store, "API", model.TypeHTTP,
		`{"url":"https://api.example.com","auth":{"type":"basic","username":"cairn","password":"hunter2"}}`, true)
	// Disabled, and paused monitors have passwords too.
	disabled := legacyMonitor(t, store, "Retired", model.TypeHTTP,
		`{"url":"https://old.example.com","auth":{"type":"bearer","token":"tok"}}`, false)
	plain := legacyMonitor(t, store, "Port", model.TypeTCP, `{"hostname":"example.com","port":443}`, true)

	resealed, err := resealCredentials(context.Background(), store, registry, vault)
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if resealed != 2 {
		t.Errorf("resealed %d, want the two with credentials", resealed)
	}

	for _, id := range []model.ID{withAuth, disabled} {
		m, err := store.GetMonitor(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(m.Monitor.Config), "hunter2") || strings.Contains(string(m.Monitor.Config), `"tok"`) {
			t.Errorf("%s still holds its credential in config:\n%s", m.Monitor.Name, m.Monitor.Config)
		}
		if len(m.Monitor.ConfigSecrets) == 0 {
			t.Errorf("%s sealed nothing", m.Monitor.Name)
		}
		// What the probe will be handed has to be what it was before.
		secret, err := vault.Open(m.Monitor.OrgID[:], m.Monitor.ID[:], m.Monitor.ConfigSecrets)
		if err != nil {
			t.Fatal(err)
		}
		merged, err := model.MergeConfig(m.Monitor.Config, secret)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(merged), "hunter2") && !strings.Contains(string(merged), `"tok"`) {
			t.Errorf("%s: the credential did not survive the round trip:\n%s", m.Monitor.Name, merged)
		}
	}

	untouched, err := store.GetMonitor(t.Context(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(untouched.Monitor.ConfigSecrets) != 0 {
		t.Error("a monitor with nothing to hide was given an envelope")
	}
}

// It runs on every start, so the second run has to be a no-op rather than a
// second layer of encryption or a lost credential.
func TestResealIsIdempotent(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	registry := testRegistry()
	vault := testVault(t)

	id := legacyMonitor(t, store, "API", model.TypeHTTP,
		`{"url":"https://api.example.com","auth":{"type":"bearer","token":"tok"}}`, true)

	if _, err := resealCredentials(context.Background(), store, registry, vault); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first, _ := store.GetMonitor(t.Context(), id)

	resealed, err := resealCredentials(context.Background(), store, registry, vault)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if resealed != 0 {
		t.Errorf("the second pass resealed %d monitors", resealed)
	}

	second, _ := store.GetMonitor(t.Context(), id)
	if string(first.Monitor.ConfigSecrets) != string(second.Monitor.ConfigSecrets) {
		t.Error("the second pass rewrote the envelope")
	}

	secret, err := vault.Open(second.Monitor.OrgID[:], second.Monitor.ID[:], second.Monitor.ConfigSecrets)
	if err != nil {
		t.Fatalf("open after two passes: %v", err)
	}
	if !strings.Contains(string(secret), "tok") {
		t.Errorf("the credential did not survive two passes: %s", secret)
	}
}

// A monitor written after this feature landed, then edited by hand to add a
// second credential in the clear, must end up with both rather than only one.
func TestResealMergesWithWhatIsAlreadySealed(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	registry := testRegistry()
	vault := testVault(t)

	id := legacyMonitor(t, store, "Orders", model.TypeGRPC,
		`{"address":"orders.internal:443","metadata":{"authorization":"Bearer abc"}}`, true)
	if _, err := resealCredentials(context.Background(), store, registry, vault); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// A plaintext credential reappears in config alongside the sealed one.
	sealed, _ := store.GetMonitor(t.Context(), id)
	if err := store.SetMonitorConfig(t.Context(), id,
		[]byte(`{"address":"orders.internal:443","metadata":{"x-tenant":"acme"}}`),
		sealed.Monitor.ConfigSecrets); err != nil {
		t.Fatal(err)
	}

	if _, err := resealCredentials(context.Background(), store, registry, vault); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	after, _ := store.GetMonitor(t.Context(), id)
	secret, err := vault.Open(after.Monitor.OrgID[:], after.Monitor.ID[:], after.Monitor.ConfigSecrets)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(secret), "Bearer abc") {
		t.Errorf("the earlier credential was lost: %s", secret)
	}
	if !strings.Contains(string(secret), "acme") {
		t.Errorf("the newer credential was not sealed: %s", secret)
	}
}

// updated_at is the probe's config version. Re-sealing changes where a
// credential is stored, not what the monitor checks, and bumping it would make
// every probe reload every monitor for a change none of them can see.
func TestResealDoesNotBumpTheConfigVersion(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	id := legacyMonitor(t, store, "API", model.TypeHTTP,
		`{"url":"https://api.example.com","auth":{"type":"bearer","token":"tok"}}`, true)

	before, _ := store.GetMonitor(t.Context(), id)
	if _, err := resealCredentials(context.Background(), store, testRegistry(), testVault(t)); err != nil {
		t.Fatalf("reseal: %v", err)
	}
	after, _ := store.GetMonitor(t.Context(), id)

	if !before.Monitor.UpdatedAt.Equal(after.Monitor.UpdatedAt) {
		t.Errorf("updated_at moved from %s to %s", before.Monitor.UpdatedAt, after.Monitor.UpdatedAt)
	}
}
