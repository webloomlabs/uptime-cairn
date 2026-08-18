// Package probe is the check-execution agent.
//
// It is a separate process in Phase 4 and an in-process goroutine tree in solo
// mode, and the code does not know which: it talks to the control plane over
// gRPC either way, in solo mode across an in-memory bufconn with real
// serialisation (ADR-001, ADR-005 decision 14). The cost is microseconds per
// result. The return is that every solo install continuously exercises the
// identical code path a remote probe uses, which makes solo mode the protocol's
// integration test — run by every user, every day.
//
// # The line this package must not cross
//
// The probe evaluates everything that requires the response payload. The control
// plane evaluates everything that requires knowledge of another check, another
// monitor, or another probe (ADR-005 decision 1).
//
// So: checks, assertions, upside_down, and retries at retry_interval_seconds
// happen here. consecutive_failures, the up/down transition, resend_after,
// dependency suppression, maintenance windows, incidents, notifications, and
// rollups do not, and no amount of convenience justifies moving one of them in.
//
// This package therefore imports internal/model and the generated protocol
// types, and never internal/store or internal/controlplane. That import
// restriction is the whole seam, expressed in a way a reviewer can check
// mechanically.
//
// # What lands here in Phase 1
//
//   - session: one control plane — its assignments, result buffer, backoff, and
//     capability negotiation. Monitor identity is (session, monitor_id)
//     throughout, never monitor_id alone.
//   - scheduler: one min-heap keyed on next-due, with each monitor's phase
//     offset at hash(monitor_id) mod interval. Without that dispersal, 5,000
//     monitors imported on a 60-second interval all fire in the same second.
//   - executor: a bounded worker pool that sheds rather than queues. Probe
//     overload must never look like target downtime.
//
// See docs/probe/protocol.md for what any of that puts on the wire.
package probe
