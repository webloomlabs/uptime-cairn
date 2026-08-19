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
	"github.com/webloomlabs/uptime-cairn/internal/probe"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
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

	publisher := controlplane.NewPublisher()
	cp := controlplane.New(store, publisher, log.With("component", "controlplane"),
		model.EmbeddedProbeID, model.SentinelOrgID)

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

	registry := check.NewRegistry()
	registry.Register(check.NewHTTP())

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
		Handler:           api.New(store, publisher, registry, keeper, log.With("component", "api"), cfg.InstanceName).Handler(),
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

func setupRequired(ctx context.Context, store *sqlite.Store) (bool, error) {
	n, err := store.CountUsers(ctx)
	return n == 0, err
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
