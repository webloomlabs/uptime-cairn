package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	"github.com/webloomlabs/uptime-cairn/internal/telemetry"
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
	WriteBatch(ctx context.Context, beats []model.Heartbeat) (int64, error)
}

// ConfigOpener decrypts the credential half of a monitor's configuration.
//
// The control plane is the only place other than the API that ever holds a
// monitor's config whole, and it holds it for exactly as long as it takes to put
// an assignment on the wire. That is the arrangement data model §12.1 describes
// as "decrypted only at delivery": at rest the credentials are an opaque blob,
// in flight they are protected by the transport the probe dialled — a bufconn
// inside this process in solo mode, mutual TLS in Phase 4.
type ConfigOpener interface {
	Open(orgID, rowID, envelope []byte) ([]byte, error)
}

// Alerter is where state changes go. Declared here by the consumer, and
// deliberately fire-and-forget: an alerting backlog must never become
// backpressure on heartbeat ingest, because the moment alerting is under strain
// is the moment heartbeats matter most.
type Alerter interface {
	Publish(ev notify.Event)
	Instance() notify.Instance
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

// assignmentSettle is how long a change signal waits before the assignment set
// is recomputed, so that a burst of writes produces one delta instead of one
// each.
//
// It exists because recomputing is not cheap: it reloads every assignable
// monitor, decrypts each one's credentials, and diffs the result against the
// previous set. One recompute per write makes creating N monitors O(N²), which
// the load-test harness found by creating 5,000 through the API — 2,116 full
// recomputations before it gave up. That is not only a seeding concern: the Kuma
// importer and any script driving the API in a loop take the same path.
//
// One second is chosen against what it costs. The spec promises only that a new
// monitor "begins checking on the next scheduler tick", and the finest interval
// this product offers is twenty seconds — so a delta arriving a second later
// changes nothing an operator can observe, while at 250ms the harness still
// measured 412 recomputations for one bulk creation.
//
// A fixed window rather than waiting for quiet: waiting for quiet would let a
// continuous stream of writes starve the probe of updates indefinitely, which is
// a worse failure than a second of latency.
//
// This does not make the recompute cheap, it makes it rarer. The underlying cost
// is that the reload holds the store's single connection while it scans every
// assignable monitor, so writes queue behind it — which is the open "reader pool
// alongside the single writer" item, now with a number against it.
const assignmentSettle = time.Second

// Server implements the probe-facing gRPC service.
//
// It is the same server in solo mode and in Phase 4; solo mode differs only in
// that the connection is a bufconn and no enrolment ever happens.
type Server struct {
	probev1.UnimplementedProbeServiceServer

	store   Store
	pub     *Publisher
	alerts  Alerter
	configs ConfigOpener
	log     *slog.Logger
	probeID model.ID
	orgID   model.ID
}

// New returns a control-plane server bound to one store.
//
// alerts may be nil, in which case transitions are recorded and nothing is sent.
// configs may be nil, in which case a monitor's stored config is passed through
// as-is; both are what a test that is not exercising those paths wants.
func New(store Store, pub *Publisher, alerts Alerter, configs ConfigOpener, log *slog.Logger, probeID, orgID model.ID) *Server {
	return &Server{store: store, pub: pub, alerts: alerts, configs: configs, log: log, probeID: probeID, orgID: orgID}
}

// raise publishes the events a batch produced, after the batch is durable.
func (s *Server) raise(pending []pendingAlert) {
	if s.alerts == nil || len(pending) == 0 {
		return
	}
	instance := s.alerts.Instance()
	for _, p := range pending {
		beat := p.beat
		status := statusFor(p.alert.eventType)
		s.alerts.Publish(notify.NewEvent(
			p.alert.eventType, instance, p.monitor, p.alert.previous, &beat, status, beat.Time))
	}
}

// statusFor is the monitor status the event describes. Taken from the event type
// rather than from the state, which by the time this runs has already been
// written and could have moved on within the same batch.
func statusFor(eventType string) string {
	switch eventType {
	case model.EventMonitorUp:
		return model.MonitorStatusUp
	case model.EventMonitorDown:
		return model.MonitorStatusDown
	default:
		return model.MonitorStatusPending
	}
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
			// Pause before recomputing, and swallow anything that arrives during
			// the pause: the reload below reads the store, so a signal from a
			// write that has already committed is a signal for a change this
			// recompute is about to pick up anyway.
			settle := time.NewTimer(assignmentSettle)
			select {
			case <-ctx.Done():
				settle.Stop()
				return nil
			case <-settle.C:
			}
			select {
			case <-changed:
			default:
			}

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
		assignment, err := s.toAssignment(m)
		if err != nil {
			// Withheld rather than sent with half a config. An HTTP monitor
			// missing its bearer token would authenticate as nobody and report
			// the target down, which is a lie about the target; leaving it
			// unassigned is at least visibly wrong.
			s.log.Error("cannot decrypt monitor credentials, monitor withheld from the probe",
				"monitor", m.ID.String(), "name", m.Name, "error", err)
			continue
		}
		out[m.ID.String()] = assignment
	}
	return out, rev, nil
}

func (s *Server) toAssignment(m model.Monitor) (*probev1.Assignment, error) {
	retryInterval := m.RetryInterval
	if retryInterval <= 0 {
		retryInterval = m.Interval
	}

	config, err := s.wholeConfig(m)
	if err != nil {
		return nil, err
	}

	return &probev1.Assignment{
		MonitorId:            append([]byte(nil), m.ID[:]...),
		Type:                 m.Type,
		Config:               config,
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
	}, nil
}

// wholeConfig merges the encrypted credentials back into the stored config.
//
// No field list is needed: the sealed half keeps the shape it was cut from, so
// putting it back is a deep merge rather than a lookup. That is why the control
// plane can do this without importing the checkers, which live on the other side
// of the ADR-001 seam.
func (s *Server) wholeConfig(m model.Monitor) ([]byte, error) {
	if len(m.ConfigSecrets) == 0 {
		return append([]byte(nil), m.Config...), nil
	}
	if s.configs == nil {
		return nil, fmt.Errorf("monitor holds encrypted credentials and this control plane has no key")
	}

	secret, err := s.configs.Open(m.OrgID[:], m.ID[:], m.ConfigSecrets)
	if err != nil {
		return nil, err
	}
	merged, err := model.MergeConfig(m.Config, secret)
	if err != nil {
		return nil, err
	}
	return merged, nil
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

		if health := batch.GetHealth(); health != nil {
			// Republished rather than only logged. A probe has no inbound port
			// to scrape, so this frame is the only way its numbers reach an
			// operator — and "the probe is shedding" is precisely the quiet
			// failure that has to be visible on a dashboard rather than found in
			// a log after the fact (docs/probe/protocol.md §8).
			s.recordHealth(health)

			if health.GetShedResultsTotal() > 0 {
				s.log.Warn("probe is shedding results",
					"shed_total", health.GetShedResultsTotal(),
					"buffered", health.GetBufferedResults())
			}
		}

		if err := stream.Send(ack); err != nil {
			return err
		}
	}
}

// recordHealth republishes a probe's self-report through the telemetry registry,
// where /metrics picks it up.
//
// Solo mode has one probe and its id is the sentinel; Phase 4's fleet will label
// each series by the probe that reported it, which is why the id travels rather
// than being assumed.
func (s *Server) recordHealth(h *probev1.ProbeHealth) {
	telemetry.RecordProbeHealth(telemetry.ProbeHealth{
		ProbeID:              s.probeID.String(),
		Name:                 "embedded",
		ReportedAt:           time.UnixMicro(h.GetTimeUnixMicros()).UTC(),
		Assigned:             h.GetAssignedCount(),
		InFlight:             h.GetInFlightChecks(),
		MaxConcurrent:        h.GetMaxConcurrentChecks(),
		DueQueueDepth:        h.GetDueQueueDepth(),
		BufferedResults:      h.GetBufferedResults(),
		BufferedBytes:        h.GetBufferedBytes(),
		ShedResultsTotal:     h.GetShedResultsTotal(),
		SkippedChecksTotal:   h.GetSkippedChecksTotal(),
		ChecksStartedTotal:   h.GetChecksStartedTotal(),
		ChecksCompletedTotal: h.GetChecksCompletedTotal(),
		UptimeSeconds:        h.GetProcessUptimeSeconds(),
		ClockOffsetMicros:    h.GetClockOffsetMicros(),
	})
}
