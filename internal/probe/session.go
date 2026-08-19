package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/probe/check"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Session is one control plane: its assignments, its result buffer, its
// reconnect backoff, its capability negotiation.
//
// A probe process holds N of these and shares one scheduler and one worker pool
// between them. This build runs exactly one — solo mode, over bufconn — but the
// type is the seam Phase 4 opens rather than a wrapper invented later, and
// monitor identity is already (session, monitor_id) rather than monitor_id
// alone.
type Session struct {
	client   probev1.ProbeServiceClient
	registry *check.Registry
	log      *slog.Logger
	buf      *Buffer
	sched    *scheduler

	name          string
	agentVersion  string
	maxConcurrent int
	workers       chan struct{}

	mu          sync.Mutex
	assignments map[string]*probev1.Assignment
	lastOutcome map[string]probev1.Outcome
	rejections  []*probev1.AssignmentRejection

	tuning atomic.Pointer[tuning]

	started   atomic.Uint64
	completed atomic.Uint64
	skipped   atomic.Uint64
	inFlight  atomic.Int64
	clockSkew atomic.Int64
	startedAt time.Time
}

// tuning is what the control plane handed back at registration. Held in an
// atomic pointer because the scheduler and the result stream both read it while
// a re-registration may replace it.
type tuning struct {
	health          time.Duration
	batchMax        int
	flushInterval   time.Duration
	reconcileWindow time.Duration
}

func defaultTuning() *tuning {
	return &tuning{
		health:          30 * time.Second,
		batchMax:        500,
		flushInterval:   time.Second,
		reconcileWindow: 15 * time.Minute,
	}
}

// Config configures a session.
type Config struct {
	Name          string
	AgentVersion  string
	MaxConcurrent int
	Registry      *check.Registry
	Logger        *slog.Logger
}

// NewSession builds a session against an already-dialled control plane.
func NewSession(client probev1.ProbeServiceClient, cfg Config) *Session {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 2000
	}
	s := &Session{
		client:        client,
		registry:      cfg.Registry,
		log:           cfg.Logger,
		buf:           NewBuffer(),
		sched:         newScheduler(),
		name:          cfg.Name,
		agentVersion:  cfg.AgentVersion,
		maxConcurrent: cfg.MaxConcurrent,
		workers:       make(chan struct{}, cfg.MaxConcurrent),
		assignments:   make(map[string]*probev1.Assignment),
		lastOutcome:   make(map[string]probev1.Outcome),
		startedAt:     time.Now(),
	}
	s.tuning.Store(defaultTuning())
	return s
}

// Run registers, then runs the three loops until ctx is cancelled. It returns
// only when the session is finished; the caller runs it in a goroutine.
func (s *Session) Run(ctx context.Context) error {
	if err := s.register(ctx); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); s.retry(ctx, "assignments", s.watchOnce) }()
	go func() { defer wg.Done(); s.retry(ctx, "results", s.streamOnce) }()
	go func() { defer wg.Done(); s.schedule(ctx) }()
	wg.Wait()

	return ctx.Err()
}

// register declares what this probe can do and picks up the control plane's
// tuning. Capabilities are the compatibility mechanism: a type this build cannot
// run is advertised as unavailable with a reason, so "no probe can run this
// monitor" is a fact the control plane holds before a single check runs, rather
// than an error message repeated 250 times a second.
func (s *Session) register(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := s.client.Register(ctx, &probev1.RegisterRequest{
		AgentVersion:        s.agentVersion,
		ProtocolVersion:     protocolVersion,
		Name:                s.name,
		Capabilities:        s.capabilities(),
		MaxConcurrentChecks: uint32(s.maxConcurrent),
		//nolint:gosec // a check rate cannot exceed a uint32 in any configuration this supports
		MaxChecksPerSecond:  uint32(s.maxConcurrent),
		ProbeTimeUnixMicros: time.Now().UnixMicro(),
		Platform:            runtime.GOOS + "/" + runtime.GOARCH,
	})
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	if server := resp.GetServerTimeUnixMicros(); server != 0 {
		s.clockSkew.Store(time.Now().UnixMicro() - server)
	}

	t := defaultTuning()
	if v := resp.GetHealthIntervalSeconds(); v > 0 {
		t.health = time.Duration(v) * time.Second
	}
	if v := resp.GetResultBatchMaxResults(); v > 0 {
		t.batchMax = int(v)
	}
	if v := resp.GetResultFlushIntervalMillis(); v > 0 {
		t.flushInterval = time.Duration(v) * time.Millisecond
	}
	if v := resp.GetReconcileIntervalSeconds(); v > 0 {
		t.reconcileWindow = time.Duration(v) * time.Second
	}
	s.tuning.Store(t)

	var probeID model.ID
	copy(probeID[:], resp.GetProbeId())
	s.log.Info("registered with control plane", "probe", probeID.String(), "protocol", resp.GetProtocolVersion())
	return nil
}

// protocolVersion is the major this probe speaks. Feature differences within v1
// are handled by capabilities, never by this number.
const protocolVersion = 1

// capabilities reports every type the product defines, not only the ones this
// build implements — an unimplemented type is advertised as unavailable with a
// reason, which is what turns "we cannot run that here" into a user-visible fact.
func (s *Session) capabilities() []*probev1.Capability {
	// push is absent on purpose: it is evaluated by the control plane and is
	// never assigned to a probe at all (ADR-005 decision 6).
	all := []string{
		model.TypeHTTP, model.TypeTCP, model.TypeICMP, model.TypeDNS,
		model.TypeTLSExpiry, model.TypeDomainExpiry, model.TypeDocker, model.TypeGRPC,
	}

	out := make([]*probev1.Capability, 0, len(all))
	for _, t := range all {
		if checker, ok := s.registry.Lookup(t); ok {
			// A registered checker is available unless it says otherwise. ICMP
			// is the case that needs asking: the code is compiled in, but
			// whether the host will hand out an ICMP socket is a property of
			// the container, not of the build.
			available, reason := true, ""
			if reporter, ok := checker.(check.Availability); ok {
				available, reason = reporter.Availability()
			}
			out = append(out, &probev1.Capability{
				Type:      t,
				Version:   checker.Version(),
				Available: available,
				Reason:    reason,
			})
			continue
		}
		out = append(out, &probev1.Capability{
			Type:      t,
			Version:   0,
			Available: false,
			Reason:    "not implemented in this build",
		})
	}
	return out
}

// retry runs one long-lived stream, reconnecting with backoff and jitter when it
// drops. NATs and corporate firewalls drop idle connections silently, so
// reconnection is load-bearing rather than polish.
func (s *Session) retry(ctx context.Context, what string, once func(context.Context) error) {
	backoff := time.Second

	for ctx.Err() == nil {
		start := time.Now()
		err := once(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("stream dropped", "stream", what, "error", err, "retry_in", backoff)
		}

		// A stream that stayed healthy for a while starts its next backoff from
		// the bottom; one that fails immediately keeps climbing.
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}

		jitter := time.Duration(rand.Int64N(int64(backoff / 5)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff + jitter):
		}

		backoff = min(time.Duration(float64(backoff)*1.6), time.Minute)
	}
}
