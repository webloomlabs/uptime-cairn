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
		Handler:           api.New(store, publisher, registry, log.With("component", "api"), cfg.InsecureNoAuth).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.InsecureNoAuth {
		log.Warn("authentication is disabled: this build has none, and --insecure-no-auth says run anyway. Do not expose this port.")
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
