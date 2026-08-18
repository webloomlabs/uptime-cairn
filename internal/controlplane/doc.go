// Package controlplane owns everything a probe deliberately does not.
//
// It serves the probe-facing gRPC service (docs/probe/protocol.md), and it is
// the only place the following happen:
//
//   - Assignment: which monitors run on which probe, synchronised as a full set,
//     then deltas, then periodic reconciliation against a digest.
//   - Ingest: idempotent batch writes of incoming results, and the acknowledged
//     high-water mark that lets a probe free its buffer.
//   - State: consecutive_failures, the up/down transition, resend_after,
//     dependency suppression, and maintenance windows.
//   - Dispatch: notifications and outbound webhooks, after the transition is
//     decided and not before.
//   - Rollups: raw to 1m to 5m to hourly to daily.
//
// The honest limitation, which belongs in the operator documentation in these
// words: data collection survives a control-plane outage and alerting does not.
// A probe keeps checking and buffering, but nothing evaluates state transitions
// while this package is down, and the backlog arrives at once when it returns.
// Phase 4's HA control plane is the real answer (docs/probe/probe-plan.md §8.3).
package controlplane
