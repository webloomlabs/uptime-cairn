package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// Store is what the control plane needs from persistence, declared here rather
// than imported from the store package: the consumer names the interface, so
// nothing forces this package to know which backend is underneath (ADR-002).
type Store interface {
	ListAssignable(ctx context.Context) ([]model.Monitor, error)
	ListPushMonitors(ctx context.Context) ([]store.MonitorWithState, error)
	LoadMonitor(ctx context.Context, id model.ID) (model.Monitor, error)
	GetState(ctx context.Context, id model.ID) (model.MonitorState, error)
	SaveState(ctx context.Context, state model.MonitorState) error
	WriteBatch(ctx context.Context, beats []model.Heartbeat) error
}

// Tuning the control plane hands the probe at registration. Every value has a
// probe-side default, so a control plane that sets none of them is valid; these
// exist so a fleet can be retuned without a probe release.
const (
	healthInterval        = 30 * time.Second
	reconcileInterval     = 15 * time.Minute
	resultBatchMaxResults = 500
	resultFlushInterval   = time.Second
	assignmentChunkSize   = 500
)

// Server implements the probe-facing gRPC service.
//
// It is the same server in solo mode and in Phase 4; solo mode differs only in
// that the connection is a bufconn and no enrolment ever happens.
type Server struct {
	probev1.UnimplementedProbeServiceServer

	store   Store
	pub     *Publisher
	log     *slog.Logger
	probeID model.ID
	orgID   model.ID
}

// New returns a control-plane server bound to one store.
func New(store Store, pub *Publisher, log *slog.Logger, probeID, orgID model.ID) *Server {
	return &Server{store: store, pub: pub, log: log, probeID: probeID, orgID: orgID}
}

// Enrol is Phase 4. Solo mode performs no enrolment and holds no credentials:
// the embedded probe row seeded by migration 0001 is the identity
// (ADR-005 decision 14).
func (s *Server) Enrol(context.Context, *probev1.EnrolRequest) (*probev1.EnrolResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"enrolment is Phase 4: this control plane serves only its embedded probe")
}

// IssueToken is Phase 4, for the same reason as Enrol.
func (s *Server) IssueToken(context.Context, *probev1.IssueTokenRequest) (*probev1.IssueTokenResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"token exchange is Phase 4: the embedded probe authenticates by being in the same process")
}

// Register records what the probe can do and hands back its identity and the
// tuning above.
func (s *Server) Register(ctx context.Context, req *probev1.RegisterRequest) (*probev1.RegisterResponse, error) {
	if req.GetProtocolVersion() != probev1Version {
		return nil, status.Errorf(codes.FailedPrecondition,
			"probe speaks protocol version %d, this control plane speaks %d: upgrade the probe",
			req.GetProtocolVersion(), probev1Version)
	}

	// Clock skew is reported, never repaired: rewriting a result's timestamp
	// would change the natural key of a row that may already exist, turning a
	// deduplicated replay into a second row (protocol.md §8).
	now := time.Now().UTC()
	if probeTime := req.GetProbeTimeUnixMicros(); probeTime != 0 {
		skew := time.Duration(now.UnixMicro()-probeTime) * time.Microsecond
		if skew < 0 {
			skew = -skew
		}
		if skew > 5*time.Second {
			s.log.Warn("probe clock skew", "skew", skew, "probe", req.GetName())
		}
	}

	unavailable := 0
	for _, c := range req.GetCapabilities() {
		switch {
		case !c.GetAvailable():
			unavailable++
			s.log.Info("probe capability unavailable", "type", c.GetType(), "reason", c.GetReason())
		case c.GetReason() != "":
			// A reason alongside available is a degradation rather than an
			// absence: ICMP on a host that refuses raw sockets still runs the
			// monitors configured to fall back to TCP, so withholding the
			// assignment would take that away. Surfacing it here is the only
			// way the operator learns without reading heartbeats one at a time.
			s.log.Info("probe capability degraded", "type", c.GetType(), "reason", c.GetReason())
		}
	}
	s.log.Info("probe registered",
		"name", req.GetName(),
		"agent", req.GetAgentVersion(),
		"platform", req.GetPlatform(),
		"capabilities", len(req.GetCapabilities())-unavailable,
		"unavailable", unavailable,
		"max_concurrent", req.GetMaxConcurrentChecks())

	return &probev1.RegisterResponse{
		ProbeId:                   s.probeID[:],
		ServerTimeUnixMicros:      now.UnixMicro(),
		ProtocolVersion:           probev1Version,
		HealthIntervalSeconds:     uint32(healthInterval.Seconds()),
		ReconcileIntervalSeconds:  uint32(reconcileInterval.Seconds()),
		ResultBatchMaxResults:     resultBatchMaxResults,
		ResultFlushIntervalMillis: uint32(resultFlushInterval.Milliseconds()),
	}, nil
}

// probev1Version is the protocol major this build speaks. Compatibility within
// v1 is handled by capability negotiation, not by gating on this — it exists so
// a v2 probe against a v1 control plane fails with an explanation rather than a
// decode error.
const probev1Version = 1

// WatchAssignments sends the full set, then deltas as monitors change, then a
// reconciliation digest on a timer. Same diff-plus-reconcile pattern ADR-004
// chose for the browser (ADR-005 decision 11).
func (s *Server) WatchAssignments(req *probev1.WatchAssignmentsRequest, stream probev1.ProbeService_WatchAssignmentsServer) error {
	ctx := stream.Context()

	changed, unsubscribe := s.pub.Subscribe()
	defer unsubscribe()

	current, rev, err := s.assignments(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "load assignments: %v", err)
	}
	if err := s.sendFullSet(stream, current, rev); err != nil {
		return err
	}
	s.log.Info("assignment set sent", "monitors", len(current), "set_version", setVersion(rev))

	reconcile := time.NewTicker(reconcileInterval)
	defer reconcile.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-changed:
			next, nextRev, err := s.assignments(ctx)
			if err != nil {
				s.log.Error("reload assignments", "error", err)
				continue
			}
			delta := diff(current, next, nextRev)
			if delta == nil {
				continue
			}
			if err := stream.Send(&probev1.AssignmentUpdate{
				Update: &probev1.AssignmentUpdate_Delta{Delta: delta},
			}); err != nil {
				return err
			}
			s.log.Info("assignment delta sent",
				"added", len(delta.GetAdded()),
				"updated", len(delta.GetUpdated()),
				"removed", len(delta.GetRemovedMonitorIds()))
			current, rev = next, nextRev

		case <-reconcile.C:
			values := make([]*probev1.Assignment, 0, len(current))
			for _, a := range current {
				values = append(values, a)
			}
			if err := stream.Send(&probev1.AssignmentUpdate{
				Update: &probev1.AssignmentUpdate_Reconcile{
					Reconcile: &probev1.Reconcile{
						SetVersion:       setVersion(rev),
						AssignmentDigest: digest(values),
					},
				},
			}); err != nil {
				return err
			}
		}
	}
}

// assignments builds the current set, keyed by monitor id in hex.
func (s *Server) assignments(ctx context.Context) (map[string]*probev1.Assignment, uint64, error) {
	monitors, err := s.store.ListAssignable(ctx)
	if err != nil {
		return nil, 0, err
	}
	rev := s.pub.Revision()

	out := make(map[string]*probev1.Assignment, len(monitors))
	for _, m := range monitors {
		out[m.ID.String()] = toAssignment(m)
	}
	return out, rev, nil
}

func toAssignment(m model.Monitor) *probev1.Assignment {
	retryInterval := m.RetryInterval
	if retryInterval <= 0 {
		retryInterval = m.Interval
	}
	return &probev1.Assignment{
		MonitorId:            append([]byte(nil), m.ID[:]...),
		Type:                 m.Type,
		Config:               append([]byte(nil), m.Config...),
		IntervalSeconds:      uint32(m.Interval.Seconds()),
		TimeoutSeconds:       uint32(m.Timeout.Seconds()),
		Retries:              uint32(m.Retries),
		RetryIntervalSeconds: uint32(retryInterval.Seconds()),
		UpsideDown:           m.UpsideDown,
		// updated_at is the config version: the row's timestamp moves on every
		// write, so it changes whenever config or scheduling does. It costs
		// nothing to compute and nothing to store, and the probe only ever
		// compares it for equality.
		ConfigVersion: strconv.FormatInt(m.UpdatedAt.UnixMilli(), 10),
	}
}

// sendFullSet chunks the set. gRPC's default 4 MiB receive limit is smaller than
// a 5,000-monitor set with ~1 KB of config each, so chunking is required rather
// than defensive; the probe applies atomically on final = true.
func (s *Server) sendFullSet(stream probev1.ProbeService_WatchAssignmentsServer, set map[string]*probev1.Assignment, rev uint64) error {
	all := make([]*probev1.Assignment, 0, len(set))
	for _, a := range set {
		all = append(all, a)
	}

	version := setVersion(rev)
	for start := 0; ; start += assignmentChunkSize {
		end := min(start+assignmentChunkSize, len(all))
		final := end >= len(all)

		if err := stream.Send(&probev1.AssignmentUpdate{
			Update: &probev1.AssignmentUpdate_Set{
				Set: &probev1.AssignmentSet{
					SetVersion:  version,
					Assignments: all[start:end],
					Final:       final,
				},
			},
		}); err != nil {
			return err
		}
		if final {
			return nil
		}
	}
}

// diff produces the delta between two sets, or nil when nothing moved.
func diff(old, next map[string]*probev1.Assignment, rev uint64) *probev1.AssignmentDelta {
	delta := &probev1.AssignmentDelta{SetVersion: setVersion(rev)}

	for id, a := range next {
		prev, existed := old[id]
		switch {
		case !existed:
			delta.Added = append(delta.Added, a)
		case prev.GetConfigVersion() != a.GetConfigVersion():
			delta.Updated = append(delta.Updated, a)
		}
	}
	for id, a := range old {
		if _, still := next[id]; !still {
			delta.RemovedMonitorIds = append(delta.RemovedMonitorIds, a.GetMonitorId())
		}
	}

	if len(delta.GetAdded()) == 0 && len(delta.GetUpdated()) == 0 && len(delta.GetRemovedMonitorIds()) == 0 {
		return nil
	}
	return delta
}

// StreamResults ingests batches and acknowledges them. The probe frees its
// buffer only on the acknowledgement, never on send, so anything not
// acknowledged here is resent — which is why ingest is idempotent.
func (s *Server) StreamResults(stream probev1.ProbeService_StreamResultsServer) error {
	ctx := stream.Context()

	for {
		batch, err := stream.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			return err
		}

		for _, r := range batch.GetRejections() {
			var id model.ID
			copy(id[:], r.GetMonitorId())
			s.log.Error("probe rejected assignment",
				"monitor", id.String(),
				"reason", r.GetReason().String(),
				"detail", r.GetMessage())
		}

		ack, err := s.ingest(ctx, batch.GetResults())
		if err != nil {
			// Do not acknowledge what was not written. The probe keeps the batch
			// and resends it; a failed write that returned an ack would be a
			// silent hole in the history.
			s.log.Error("ingest batch", "error", err, "results", len(batch.GetResults()))
			if err := stream.Send(&probev1.ResultAck{Message: err.Error()}); err != nil {
				return err
			}
			continue
		}

		if health := batch.GetHealth(); health != nil && health.GetShedResultsTotal() > 0 {
			s.log.Warn("probe is shedding results",
				"shed_total", health.GetShedResultsTotal(),
				"buffered", health.GetBufferedResults())
		}

		if err := stream.Send(ack); err != nil {
			return err
		}
	}
}
