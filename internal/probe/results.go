package probe

import (
	"context"
	"fmt"
	"time"

	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// streamOnce opens the result stream and pumps it until it drops.
//
// Results go up in batches because the storage layer's primary operation is a
// batch write, so a received frame hands almost straight to it. Acknowledgements
// come back down the same stream, and only an acknowledgement frees buffer.
func (s *Session) streamOnce(ctx context.Context) error {
	stream, err := s.client.StreamResults(ctx)
	if err != nil {
		return fmt.Errorf("open result stream: %w", err)
	}

	// Anything sent on the previous stream and not acknowledged has to go again:
	// the control plane may or may not have written it, and the only safe
	// assumption is that it did not. Ingest is idempotent precisely so this
	// resend is free.
	s.buf.Rewind()

	acks := make(chan error, 1)
	go func() {
		for {
			ack, err := stream.Recv()
			if err != nil {
				acks <- err
				return
			}
			s.buf.Ack(ack.GetAcknowledgedThroughResultId())
			if ack.GetRejected() > 0 {
				s.log.Warn("control plane rejected results",
					"rejected", ack.GetRejected(), "detail", ack.GetMessage())
			}
			if pause := ack.GetPauseMillis(); pause > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(pause) * time.Millisecond):
				}
			}
		}
	}()

	t := s.tuning.Load()
	flush := time.NewTicker(t.flushInterval)
	defer flush.Stop()
	health := time.NewTicker(t.health)
	defer health.Stop()

	var sinceHealth time.Time

	for {
		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			return nil

		case err := <-acks:
			return err

		case <-health.C:
			// A probe with nothing to report is exactly when its health matters,
			// so this goes out on an otherwise empty batch.
			if err := stream.Send(&probev1.ResultBatch{Health: s.health()}); err != nil {
				return err
			}
			sinceHealth = time.Now()

		case <-flush.C:
			batch := s.buf.Next(t.batchMax)
			rejections := s.takeRejections()
			if len(batch) == 0 && len(rejections) == 0 {
				continue
			}

			frame := &probev1.ResultBatch{Results: batch, Rejections: rejections}
			if time.Since(sinceHealth) > t.health {
				frame.Health = s.health()
				sinceHealth = time.Now()
			}
			if err := stream.Send(frame); err != nil {
				return err
			}
		}
	}
}

// health is the self-metrics payload. A probe has no inbound port to scrape, so
// these ride the result stream and the control plane republishes them.
func (s *Session) health() *probev1.ProbeHealth {
	buffered, bufferedBytes, shed := s.buf.Stats()

	return &probev1.ProbeHealth{
		TimeUnixMicros:       time.Now().UnixMicro(),
		AssignedCount:        uint32(len(s.snapshot())),
		InFlightChecks:       uint32(max(s.inFlight.Load(), 0)),
		MaxConcurrentChecks:  uint32(s.maxConcurrent),
		DueQueueDepth:        uint32(s.sched.len()),
		BufferedResults:      uint64(buffered),
		BufferedBytes:        uint64(bufferedBytes),
		ShedResultsTotal:     shed,
		SkippedChecksTotal:   s.skipped.Load(),
		ChecksStartedTotal:   s.started.Load(),
		ChecksCompletedTotal: s.completed.Load(),
		ProcessUptimeSeconds: uint64(time.Since(s.startedAt).Seconds()),
		ClockOffsetMicros:    s.clockSkew.Load(),
	}
}
