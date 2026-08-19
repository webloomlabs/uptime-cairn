// Package app is the composition root: the one place that knows how every other
// package fits together.
//
// The import rule that keeps the seams honest, stated here because this is the
// package that would break it first (see docs/development/repo-layout.md):
//
//   - Only app constructs concrete implementations. Nothing else imports
//     internal/store/sqlite.
//   - internal/probe never imports internal/store or internal/controlplane. It
//     reaches the control plane over gRPC and nothing else, in solo mode as much
//     as in Phase 4 (ADR-001). The day the scheduler reaches around that
//     interface is the day the design stops being true, and it will not announce
//     itself.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/webloomlabs/uptime-cairn/internal/api"
	"github.com/webloomlabs/uptime-cairn/internal/config"
	"github.com/webloomlabs/uptime-cairn/internal/controlplane"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/probe"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	"github.com/webloomlabs/uptime-cairn/internal/rollup"
	"github.com/webloomlabs/uptime-cairn/internal/secrets"
	"github.com/webloomlabs/uptime-cairn/internal/store/sqlite"
	"github.com/webloomlabs/uptime-cairn/internal/version"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// shutdownGrace bounds how long a stop waits for in-flight work. Long enough for
// a check to finish, short enough that a container stop does not hang.
const shutdownGrace = 10 * time.Second

// Run starts the process and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, out io.Writer) error {
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("starting", "version", version.Version, "mode", cfg.Mode, "data_dir", cfg.DataDir)

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	store, err := sqlite.Open(ctx, filepath.Join(cfg.DataDir, "cairn.db"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	applied, err := store.Migrate(ctx)
	if err != nil {
		return err
	}
	for _, m := range applied {
		log.Info("migration applied", "version", m.Version, "name", m.Name)
	}

	keeper, err := openKeeper(ctx, store, cfg, log)
	if err != nil {
		return err
	}

	// Sessions expire on their own; sweeping keeps the table from accumulating
	// rows nobody can use. Hourly is often enough for a table this small and
	// rare enough to be invisible.
	go sweepSessions(ctx, store, log)

	// Every monitor type this build can run. push is absent by design: it is
	// evaluated by the control plane against the clock and is never assigned to
	// a probe at all (ADR-005 decision 6).
	registry := check.NewRegistry()
	registry.Register(check.NewHTTP())
	registry.Register(check.NewTCP())
	registry.Register(check.NewICMP())
	registry.Register(check.NewDNS())
	registry.Register(check.NewTLSExpiry())
	registry.Register(check.NewDomainExpiry())
	registry.Register(check.NewDocker())
	registry.Register(check.NewGRPC())

	// Monitors written before credentials were encrypted still hold them in
	// config. Moving them now, rather than when the monitor is next edited,
	// means the guarantee is true of the database rather than true of new rows.
	configVault := secrets.NewVault(keeper, "monitors", "config")
	if resealed, err := resealCredentials(ctx, store, registry, configVault); err != nil {
		return err
	} else if resealed > 0 {
		log.Info("moved monitor credentials out of plaintext configuration", "monitors", resealed)
	}

	// Alerting. Started before the control plane because the control plane
	// publishes into it: a transition that happens in the first second of a
	// process's life is as real as any other.
	//
	// The dispatcher is fire-and-forget by design. Ingest must never block on a
	// mail server, because the moment alerting is under strain is exactly the
	// moment heartbeats matter most.
	alerts := notify.NewDispatcher(store, notify.NewVault(keeper), notify.NewSender(),
		notify.Instance{Name: cfg.InstanceName, BaseURL: cfg.BaseURL, Version: version.Version},
		log.With("component", "notify"))
	alerts.Start(ctx)
	if !alerts.AppriseAvailable() {
		log.Info("apprise is not installed: the meta-provider channel type is unavailable, " +
			"the twelve native channel types are unaffected")
	}

	publisher := controlplane.NewPublisher()
	cp := controlplane.New(store, publisher, alerts, configVault, log.With("component", "controlplane"),
		model.EmbeddedProbeID, model.SentinelOrgID)

	// The rollup pipeline. Raw heartbeats are kept for seven days, so every
	// history range beyond that — the 90-day uptime bar a status page shows, the
	// year Phase 2's reports quote — is made of rows this job writes and nothing
	// else writes. It also enforces retention, which is what stops a Pi filling
	// its card, and drains the purge queue left behind by deleted monitors.
	rollups := rollup.NewRunner(store, rollup.DefaultRetention(), log.With("component", "rollup"))
	go runRollups(ctx, rollups)

	// Push monitors are evaluated here rather than by a probe: a dead-man's
	// switch measures silence, and only the side holding the clock and the last
	// heartbeat can see it. The interval is the resolution of the answer, not
	// its cadence — a monitor is only touched once its own deadline has passed.
	go sweepPush(ctx, cp, log)

	// The probe talks to the control plane over gRPC even though both are in
	// this process, across an in-memory listener with real serialisation
	// (ADR-005 decision 14). The cost is microseconds per result. The return is
	// that every solo install exercises the identical code path a remote probe
	// uses, which makes solo mode the protocol's integration test — run by every
	// user, every day.
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	probev1.RegisterProbeServiceServer(grpcServer, cp)

	go func() {
		if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Error("probe service stopped", "error", err)
		}
	}()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		// No TLS: this connection never leaves the process address space. A
		// remote probe dials a real endpoint with real credentials, which is the
		// same client code with a different dialler.
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial embedded probe transport: %w", err)
	}
	defer func() { _ = conn.Close() }()

	session := probe.NewSession(probev1.NewProbeServiceClient(conn), probe.Config{
		Name:          "embedded",
		AgentVersion:  version.Version,
		MaxConcurrent: 2000,
		Registry:      registry,
		Logger:        log.With("component", "probe"),
	})
	go func() {
		if err := session.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("probe session stopped", "error", err)
		}
	}()

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.New(store, publisher, cp, alerts, registry, keeper, log.With("component", "api"), cfg.InstanceName).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if required, err := setupRequired(ctx, store); err == nil && required {
		log.Info("first-run setup is required: POST /api/v1/setup to create the administrator account")
	}
	log.Info("listening", "addr", cfg.ListenAddr)

	serverErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := server.Shutdown(stopCtx); err != nil {
		log.Error("http shutdown", "error", err)
	}
	grpcServer.GracefulStop()

	// Deliveries already in flight finish; scheduled retries are cancelled. A
	// retry that has not fired yet has nothing durable behind it, and pretending
	// otherwise would mean blocking shutdown on a mail server.
	alerts.Wait()
	if dropped := alerts.Dropped(); dropped > 0 {
		log.Warn("notifications were dropped because the queue was full", "count", dropped)
	}

	// Buffered results that were never acknowledged are lost here: the probe
	// holds them in memory and nothing persists them (ADR-005 decision 9). In
	// solo mode that window is one flush interval, and the honest cost is at
	// most a second of heartbeats on a deliberate stop.
	return nil
}

// openKeeper resolves the root key and the current data key, creating the first
// data key on a fresh install (data model §12.3).
//
// The order matters: whether encrypted data already exists decides what a
// missing key means. With data present it is fatal, because generating a
// replacement would render every stored secret permanently unreadable while
// appearing to work.
func openKeeper(ctx context.Context, store *sqlite.Store, cfg config.Config, log *slog.Logger) (*secrets.Keeper, error) {
	encrypted, err := store.HasEncryptedData(ctx)
	if err != nil {
		return nil, err
	}

	root, err := secrets.LoadRootKey(cfg.EncryptionKeyFile, cfg.DataDir, encrypted)
	if err != nil {
		return nil, err
	}
	if root.Generated {
		log.Warn("generated a new encryption key — back it up separately from the database, "+
			"because without it every stored secret is unrecoverable", "path", root.Description)
	}

	wrapped, err := store.EncryptionKeys(ctx)
	if err != nil {
		return nil, err
	}

	keys := make(map[uint32][]byte, len(wrapped)+1)
	var current uint32
	for _, k := range wrapped {
		dek, err := secrets.Unwrap(root.Key, k.Wrapped)
		if err != nil {
			return nil, fmt.Errorf("data key %d does not unwrap with the key at %s: %w", k.Version, root.Description, err)
		}
		keys[k.Version] = dek
		current = max(current, k.Version)
	}

	if len(keys) == 0 {
		dek, err := secrets.NewDataKey()
		if err != nil {
			return nil, err
		}
		sealed, err := secrets.Wrap(root.Key, dek)
		if err != nil {
			return nil, err
		}
		current = 1
		if err := store.InsertEncryptionKey(ctx,
			sqlite.WrappedKey{Version: current, Wrapped: sealed}, secrets.Algorithm, time.Now().UTC()); err != nil {
			return nil, err
		}
		keys[current] = dek
	}

	return secrets.NewKeeper(current, keys)
}

// resealCredentials moves any credential still sitting in a monitor's plaintext
// config into its encrypted column, and reports how many it moved.
//
// Idempotent, and a no-op on every start after the first: a monitor whose config
// no longer holds any of its type's secret fields splits to an empty secret half
// and is left alone. It runs before anything reads a config, so no probe ever
// sees the half-migrated state.
//
// Failing here is fatal rather than logged. A pass that gives up partway leaves
// some monitors encrypted and some not, and the difference is invisible from the
// outside — which is the property that makes a security guarantee worthless.
func resealCredentials(ctx context.Context, store *sqlite.Store, registry *check.Registry, vault *secrets.Vault) (int, error) {
	monitors, err := store.AllMonitors(ctx)
	if err != nil {
		return 0, fmt.Errorf("reseal monitor credentials: %w", err)
	}

	resealed := 0
	for _, m := range monitors {
		fields := registry.SecretFields(m.Type)
		if len(fields) == 0 {
			continue
		}

		public, secret, err := model.SplitConfig(m.Config, fields)
		if err != nil {
			return resealed, fmt.Errorf("reseal monitor %s: %w", m.ID, err)
		}
		if len(secret) == 0 {
			continue
		}

		// Merged with whatever is already sealed, so a monitor that has some
		// credentials encrypted and one added later in plaintext ends up with
		// both rather than with only the newer one.
		existing, err := vault.Open(m.OrgID[:], m.ID[:], m.ConfigSecrets)
		if err != nil {
			return resealed, fmt.Errorf("reseal monitor %s: %w", m.ID, err)
		}
		combined, err := model.MergeConfig(existing, secret)
		if err != nil {
			return resealed, fmt.Errorf("reseal monitor %s: %w", m.ID, err)
		}
		sealed, err := vault.Seal(m.OrgID[:], m.ID[:], combined)
		if err != nil {
			return resealed, fmt.Errorf("reseal monitor %s: %w", m.ID, err)
		}
		if err := store.SetMonitorConfig(ctx, m.ID, public, sealed); err != nil {
			return resealed, fmt.Errorf("reseal monitor %s: %w", m.ID, err)
		}
		resealed++
	}
	return resealed, nil
}

func setupRequired(ctx context.Context, store *sqlite.Store) (bool, error) {
	n, err := store.CountUsers(ctx)
	return n == 0, err
}

// rollupInterval is how often the pipeline runs. A minute matches the finest
// tier: running more often would recompute the same open bucket repeatedly, and
// running less often would leave the newest minute missing from history for
// longer than the tier it belongs to.
const rollupInterval = time.Minute

func runRollups(ctx context.Context, runner *rollup.Runner) {
	// One pass at startup rather than waiting out the first tick: a process that
	// was down for a day has a day of buckets to build, and the sooner it starts
	// the sooner history is whole again.
	runner.Run(ctx, time.Now().UTC())

	ticker := time.NewTicker(rollupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			runner.Run(ctx, now.UTC())
		}
	}
}

// pushSweepInterval bounds how late a push outage is noticed. Five seconds is
// well inside the 20-second floor every other monitor type runs at, and the
// query behind it is one indexed read over a table holding only push monitors.
const pushSweepInterval = 5 * time.Second

func sweepPush(ctx context.Context, cp *controlplane.Server, log *slog.Logger) {
	ticker := time.NewTicker(pushSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			moved, err := cp.SweepPush(ctx, now.UTC())
			if err != nil {
				log.Warn("sweep push monitors", "error", err)
				continue
			}
			if moved > 0 {
				log.Info("push monitors went overdue", "count", moved)
			}
		}
	}
}

func sweepSessions(ctx context.Context, store *sqlite.Store, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed, err := store.DeleteExpiredSessions(ctx, time.Now().UTC())
			if err != nil {
				log.Warn("sweep expired sessions", "error", err)
				continue
			}
			if removed > 0 {
				log.Debug("swept expired sessions", "removed", removed)
			}
		}
	}
}
