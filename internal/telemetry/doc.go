// Package telemetry is logging, metrics, and the health endpoints.
//
// Prometheus /metrics and OpenTelemetry export are specified in the Phase 0 API
// contract; /healthz and /readyz sit outside the versioned surface. Probe
// self-metrics arrive on the result stream rather than by scraping, because a
// probe has no inbound port, and are republished here labelled by probe
// (docs/probe/protocol.md §8).
//
// "Who watches the watchman" starts here: the process monitors itself, and the
// numbers that matter are the ones that reveal quiet failure — shed results,
// buffer depth, scheduler lateness, notification delivery failures.
//
// No telemetry leaves the install by default. Opt-in only, per principle 7:
// your data is yours.
package telemetry
