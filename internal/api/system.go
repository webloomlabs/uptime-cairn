package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/live"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/notify"
	"github.com/webloomlabs/uptime-cairn/internal/store"
	"github.com/webloomlabs/uptime-cairn/internal/telemetry"
	"github.com/webloomlabs/uptime-cairn/internal/version"
)

// System endpoints: what this build is, what it is currently seeing, and the
// Prometheus exposition.
//
// /system/info is the mechanism progressive disclosure runs on. A dashboard that
// hardcodes the monitor-type list ships a dropdown containing a type the server
// cannot run; one that reads it here hides the surfaces this build does not
// have rather than showing dead controls.

// expirySoon is the horizon the overview counts against. Thirty days is the
// point at which a certificate is a task rather than an emergency, and it is
// also the shortest renewal window anybody sensible uses.
const expirySoon = 30 * 24 * time.Hour

func (s *Server) getSystemInfo(w http.ResponseWriter, r *http.Request) {
	info := systemInfoJSON{
		Version:       version.Version,
		Mode:          "solo",
		StorageEngine: "sqlite",
		APIVersion:    "v1",
		MonitorTypes:  append(s.registry.Types(), model.TypePush),
		NotificationChannelTypes: []string{
			model.ChannelEmail, model.ChannelWebhook, model.ChannelSlack,
			model.ChannelDiscord, model.ChannelTelegram, model.ChannelMatrix,
			model.ChannelGotify, model.ChannelNtfy, model.ChannelMSTeams,
			model.ChannelPagerDuty, model.ChannelOpsgenie, model.ChannelTwilio,
			model.ChannelApprise,
		},
		Capabilities: s.capabilities(r),
	}
	sort.Strings(info.MonitorTypes)
	writeJSON(w, s.log, http.StatusOK, info)
}

// capabilities is what this build and this configuration can actually do.
//
// Every entry is a fact about the running process rather than a constant. apprise
// is the clearest case: the meta-provider is compiled in and useless without the
// binary, so a dashboard offering it on a host without apprise installed is
// offering a channel that fails on first use.
func (s *Server) capabilities(r *http.Request) map[string]bool {
	caps := map[string]bool{
		"monitors":           true,
		"groups_and_tags":    true,
		"maintenance":        true,
		"incidents":          true,
		"status_pages":       true,
		"outbound_webhooks":  s.outbound != nil,
		"notifications":      true,
		"api_keys":           true,
		"totp":               true,
		"metrics":            true,
		"push_monitors":      true,
		"manual_check":       true,
		"bulk_operations":    true,
		"kuma_import":        false,
		"certificate_detail": true,

		// Three facts about this process, not a setting. A status page can offer
		// a subscribe box only if there is somewhere to deliver from, a relay to
		// deliver through, and a base URL to put in the confirmation link — and
		// a form that takes an address the install cannot write to is worse than
		// no form, because the person who used it believes they will be told.
		"subscriber_delivery": s.relay != nil && s.baseURL != "" && notify.InstanceSMTPConfigured(),
	}
	if _, ok := s.registry.Lookup(model.TypeICMP); ok {
		caps["icmp"] = true
	}
	if s.alerts != nil {
		caps["apprise"] = s.alerts.AppriseAvailable()
	}
	return caps
}

// getOverview is the dashboard's headline figures.
func (s *Server) getOverview(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.StatusCounts(r.Context())
	if err != nil {
		s.internal(w, r, "status counts", err)
		return
	}

	out := overviewJSON{
		Monitors: overviewCounts{
			Up:          counts[model.MonitorStatusUp],
			Down:        counts[model.MonitorStatusDown],
			Pending:     counts[model.MonitorStatusPending],
			Paused:      counts[model.MonitorStatusPaused],
			Maintenance: counts[model.MonitorStatusMaintenance],
		},
		GeneratedAt: time.Now().UTC(),
	}
	for _, count := range counts {
		out.Monitors.Total += count
	}

	if out.ActiveIncidents, err = s.store.CountOpenIncidents(r.Context()); err != nil {
		s.internal(w, r, "count incidents", err)
		return
	}
	windows, err := s.store.DueMaintenanceWindows(r.Context(), time.Now().UTC())
	if err != nil {
		s.internal(w, r, "count maintenance windows", err)
		return
	}
	out.ActiveMaintenanceWindows = len(windows)

	certificates, domains, err := s.store.ExpiringSoon(r.Context(), time.Now().UTC().Add(expirySoon))
	if err != nil {
		s.internal(w, r, "count expiring", err)
		return
	}
	out.CertificatesExpiringSoon = certificates
	out.DomainsExpiringSoon = domains

	writeJSON(w, s.log, http.StatusOK, out)
}

// getPrometheusMetrics writes the text exposition format.
//
// Hand-written rather than pulled from the Prometheus client library, and that
// is a deliberate trade rather than an oversight. The exposition format is a
// dozen lines of text; the client library brings a registry, a collector
// abstraction, and a dependency tree onto a binary whose whole pitch is that it
// is one static file you drop on a Raspberry Pi. When this needs histograms it
// will need the library, and that is the moment to take the dependency.
//
// What is exposed is chosen by the same rule the telemetry package states:
// the numbers that matter are the ones that reveal quiet failure. Per-monitor
// status and response time, so an alert can be built on the data rather than on
// this process being up; and the counters for shed work, because a queue that
// silently drops is indistinguishable from one nobody is using.
func (s *Server) getPrometheusMetrics(w http.ResponseWriter, r *http.Request) {
	monitors, _, err := s.store.ListMonitors(r.Context(), nil, maxMetricsMonitors, store.MonitorFilter{})
	if err != nil {
		s.internal(w, r, "list monitors for metrics", err)
		return
	}

	var b strings.Builder

	b.WriteString("# HELP cairn_build_info Build information for the running instance.\n")
	b.WriteString("# TYPE cairn_build_info gauge\n")
	fmt.Fprintf(&b, "cairn_build_info{version=%q,mode=\"solo\"} 1\n", version.Version)

	counts, err := s.store.StatusCounts(r.Context())
	if err != nil {
		s.internal(w, r, "status counts for metrics", err)
		return
	}
	b.WriteString("\n# HELP cairn_monitors Configured monitors by current status.\n")
	b.WriteString("# TYPE cairn_monitors gauge\n")
	for _, status := range []string{
		model.MonitorStatusUp, model.MonitorStatusDown, model.MonitorStatusPending,
		model.MonitorStatusPaused, model.MonitorStatusMaintenance,
	} {
		fmt.Fprintf(&b, "cairn_monitors{status=%q} %d\n", status, counts[status])
	}

	// Per-monitor series. The value vocabulary is the stored one and not a
	// zero/one "is it up": pending and maintenance are distinct from down, and
	// flattening them would make a Prometheus alert fire during a maintenance
	// window the operator declared.
	b.WriteString("\n# HELP cairn_monitor_status Monitor status: 0 down, 1 up, 2 pending, 3 maintenance, 4 unknown, 5 paused.\n")
	b.WriteString("# TYPE cairn_monitor_status gauge\n")
	for _, m := range monitors {
		fmt.Fprintf(&b, "cairn_monitor_status{monitor_id=%q,monitor=%q,type=%q} %d\n",
			m.Monitor.ID.String(), escapeLabel(m.Monitor.Name), m.Monitor.Type, statusValue(m.State.Status))
	}

	b.WriteString("\n# HELP cairn_monitor_response_time_seconds Response time of the most recent check.\n")
	b.WriteString("# TYPE cairn_monitor_response_time_seconds gauge\n")
	for _, m := range monitors {
		if m.State.LastResponseTimeMs == nil {
			// Absent rather than zero. Zero is a measurement of zero, which is a
			// different claim from "nothing has been measured", and a dashboard
			// averaging the two is wrong in the direction that looks good.
			continue
		}
		fmt.Fprintf(&b, "cairn_monitor_response_time_seconds{monitor_id=%q,monitor=%q} %g\n",
			m.Monitor.ID.String(), escapeLabel(m.Monitor.Name), *m.State.LastResponseTimeMs/1000.0)
	}

	b.WriteString("\n# HELP cairn_monitor_last_check_timestamp_seconds When the monitor was last checked.\n")
	b.WriteString("# TYPE cairn_monitor_last_check_timestamp_seconds gauge\n")
	for _, m := range monitors {
		if m.State.LastCheckAt == nil {
			continue
		}
		fmt.Fprintf(&b, "cairn_monitor_last_check_timestamp_seconds{monitor_id=%q} %d\n",
			m.Monitor.ID.String(), m.State.LastCheckAt.Unix())
	}

	if s.outbound != nil {
		if dropper, ok := s.outbound.(interface{ Dropped() uint64 }); ok {
			b.WriteString("\n# HELP cairn_webhook_events_dropped_total Events shed because the outbound queue was full.\n")
			b.WriteString("# TYPE cairn_webhook_events_dropped_total counter\n")
			fmt.Fprintf(&b, "cairn_webhook_events_dropped_total %d\n", dropper.Dropped())
		}
	}

	writeEngineMetrics(&b)
	writeProbeMetrics(&b)
	s.writePoolMetrics(&b)
	s.writeLiveMetrics(&b)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(b.String())); err != nil {
		s.log.Error("write metrics", "error", err)
	}
}

// maxMetricsMonitors bounds one scrape. At the 5,000-monitor target this is
// three series per monitor and well inside what a Prometheus scrape handles;
// the bound exists so an install past the supported size degrades to a partial
// scrape rather than to a timeout.
const maxMetricsMonitors = 10000

// statusValue maps the status vocabulary to the numbers the HELP line
// documents. Written out rather than cast from model.Status, whose integers are
// the heartbeat schema's and mean something else.
func statusValue(status string) int {
	switch status {
	case model.MonitorStatusDown:
		return 0
	case model.MonitorStatusUp:
		return 1
	case model.MonitorStatusPending:
		return 2
	case model.MonitorStatusMaintenance:
		return 3
	case model.MonitorStatusPaused:
		return 5
	default:
		return 4
	}
}

// escapeLabel makes a monitor name safe in a label value. %q handles quotes and
// backslashes; newlines are what is left, and a monitor named with one would
// otherwise split a scrape into two malformed lines.
func escapeLabel(value string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(value)
}

// metricsAuth gates the scrape.
//
// The spec allows either an unauthenticated bound metrics address or an API key
// holding metrics:read. This build has one listener, so it takes the second
// route — with one exception that matters more than the purity: a scrape from
// loopback is allowed unauthenticated, because the overwhelmingly common
// deployment is a Prometheus on the same host, and a metrics endpoint that needs
// a credential is a metrics endpoint somebody turns off.
func (s *Server) metricsAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The exemption is for a scraper on this host, and "on this host" is a
		// claim about the connection rather than about a header. resolveClient
		// is what separates the two: a request forwarded by a same-host reverse
		// proxy also arrives from 127.0.0.1, and without this it inherited an
		// exemption meant for a local Prometheus while carrying the whole
		// monitor inventory out to the internet.
		if ip, known := resolveClient(r, s.trusted); known && isLoopback(ip) {
			next(w, r)
			return
		}

		token, ok := auth.BearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeProblem(w, r, s.log, http.StatusUnauthorized, "unauthorized",
				"Authentication required",
				"Scraping from a remote host needs an API key holding metrics:read.")
			return
		}
		principal, err := s.resolveAPIKey(r.Context(), token, time.Now())
		if err != nil || !principal.Scopes.Grants(auth.ScopeMetricsRead) {
			writeProblem(w, r, s.log, http.StatusForbidden, "forbidden",
				"Insufficient scope", "This endpoint requires the metrics:read scope.")
			return
		}
		next(w, r)
	}
}

func isLoopback(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1" || strings.HasPrefix(ip, "127.")
}

// parseBool is the shared reader for boolean query parameters, so "1" and "true"
// mean the same thing everywhere rather than in whichever handler remembered.
func parseBool(raw string) (bool, bool) {
	v, err := strconv.ParseBool(raw)
	return v, err == nil
}

// writeEngineMetrics is the process watching itself.
//
// Every series here answers a question about quiet failure rather than about
// volume, which is the rule the telemetry package states: an install that is
// losing heartbeats, shedding alerts, or falling behind its schedule looks
// exactly like a healthy one from the outside until somebody goes looking.
//
// cairn_heartbeats_written_total is the load-test gate's primary measurement. At
// N monitors on interval I the engine's steady-state rate is N/I by
// construction — there is nothing more to check — so the assertion is not "go
// faster" but "achieve the rate the schedule implies", which is what falling
// behind destroys.
func writeEngineMetrics(b *strings.Builder) {
	b.WriteString("\n# HELP cairn_heartbeats_written_total Heartbeats durably written, counted after the write returned.\n")
	b.WriteString("# TYPE cairn_heartbeats_written_total counter\n")
	fmt.Fprintf(b, "cairn_heartbeats_written_total %d\n", telemetry.Engine.HeartbeatsWritten.Load())

	b.WriteString("\n# HELP cairn_results_ingested_total Results offered to the write path; exceeds rows written when a probe redelivers.\n")
	b.WriteString("# TYPE cairn_results_ingested_total counter\n")
	fmt.Fprintf(b, "cairn_results_ingested_total %d\n", telemetry.Engine.ResultsIngested.Load())

	b.WriteString("\n# HELP cairn_results_rejected_total Results that could not be attributed to a live monitor.\n")
	b.WriteString("# TYPE cairn_results_rejected_total counter\n")
	fmt.Fprintf(b, "cairn_results_rejected_total %d\n", telemetry.Engine.ResultsRejected.Load())

	b.WriteString("\n# HELP cairn_checks_run_inline_total Checks run by the API for POST /monitors/{id}/check.\n")
	b.WriteString("# TYPE cairn_checks_run_inline_total counter\n")
	fmt.Fprintf(b, "cairn_checks_run_inline_total %d\n", telemetry.Engine.ChecksRunInline.Load())

	b.WriteString("\n# HELP cairn_alerts_published_total Events handed to the notification dispatcher.\n")
	b.WriteString("# TYPE cairn_alerts_published_total counter\n")
	fmt.Fprintf(b, "cairn_alerts_published_total %d\n", telemetry.Engine.AlertsPublished.Load())

	b.WriteString("\n# HELP cairn_alerts_dropped_total Events shed because the notification queue was full.\n")
	b.WriteString("# TYPE cairn_alerts_dropped_total counter\n")
	fmt.Fprintf(b, "cairn_alerts_dropped_total %d\n", telemetry.Engine.AlertsDropped.Load())

	b.WriteString("\n# HELP cairn_process_uptime_seconds Seconds since this process started.\n")
	b.WriteString("# TYPE cairn_process_uptime_seconds gauge\n")
	fmt.Fprintf(b, "cairn_process_uptime_seconds %d\n", int64(telemetry.Uptime(time.Now()).Seconds()))
}

// writePoolMetrics reports the store's connection pools.
//
// Asked for rather than required, the same way the outbound queue's drop counter
// is: the store arrives here as the consumer-defined interface ADR-002 asks for,
// which describes what the API needs from a store and says nothing about
// connections — a backend may not have a pool at all. A backend that has one
// reports it; one that does not stays silent, and neither has to change.
//
// The pair to read together is cairn_db_pool_wait_total on the writer and the
// latency of whatever endpoint is slow. SQLite takes one write lock per
// database, so the writer pool is one connection by construction and its wait
// count is the queue depth in front of it. Reads run on a separate read-only
// pool and no longer join that queue, which is the whole point: before, a scan
// of every assignable monitor held the one connection every write also needed,
// and monitor creation fell from 1,144/sec to 38/sec as the install grew — a
// number the load-test harness produced and this pool exists to answer.
//
// cairn_db_pool_in_use_connections on the reader is the other one worth an
// alert, for a failure the pool introduced. A result set that is never closed
// holds its connection and, in WAL, holds a read snapshot that stops the write
// ahead log being checkpointed — so the disk grows and nothing else complains.
// With one shared connection that mistake deadlocked the process on the next
// statement, which is unpleasant but impossible to miss. Now it leaks quietly,
// and this gauge sitting at a floor above zero while the instance is idle is
// what it looks like.
func (s *Server) writePoolMetrics(b *strings.Builder) {
	reporter, ok := s.store.(interface{ Pools() []telemetry.Pool })
	if !ok {
		return
	}
	pools := reporter.Pools()
	if len(pools) == 0 {
		return
	}

	for _, series := range []struct {
		name, help, kind string
		value            func(telemetry.Pool) float64
	}{
		{"cairn_db_pool_max_connections", "Configured connection ceiling for this pool.", "gauge",
			func(p telemetry.Pool) float64 { return float64(p.Max) }},
		{"cairn_db_pool_open_connections", "Connections currently open.", "gauge",
			func(p telemetry.Pool) float64 { return float64(p.Open) }},
		{"cairn_db_pool_in_use_connections", "Connections currently executing a statement.", "gauge",
			func(p telemetry.Pool) float64 { return float64(p.InUse) }},
		{"cairn_db_pool_idle_connections", "Connections open and idle.", "gauge",
			func(p telemetry.Pool) float64 { return float64(p.Idle) }},
		{"cairn_db_pool_wait_total", "Times a caller had to queue for a connection.", "counter",
			func(p telemetry.Pool) float64 { return float64(p.WaitCount) }},
		{"cairn_db_pool_wait_seconds_total", "Total time spent queued for a connection.", "counter",
			func(p telemetry.Pool) float64 { return p.WaitSeconds }},
	} {
		fmt.Fprintf(b, "\n# HELP %s %s\n# TYPE %s %s\n", series.name, series.help, series.name, series.kind)
		for _, p := range pools {
			fmt.Fprintf(b, "%s{pool=%q} %g\n", series.name, p.Name, series.value(p))
		}
	}
}

// writeLiveMetrics reports the cost of the live-update channel.
//
// It is here because it is the one cost in this system that scales with
// connected clients rather than with monitor count, and the 5,000-monitor gate
// does not exercise that dimension at all. ADR-004's first invariant is that
// server-side resource use stays flat as total monitor count grows regardless
// of how many are being viewed; the number that would falsify it is this one
// against a flat heartbeat rate.
//
// Absent rather than zero when no bus is wired in, on the same reasoning as the
// probe series: a series reporting 0 subscribers is a claim that nobody is
// watching, and "this build has no live channel" is a different statement.
func (s *Server) writeLiveMetrics(b *strings.Builder) {
	counter, ok := s.live.(live.Counter)
	if !ok {
		return
	}
	b.WriteString("\n# HELP cairn_live_subscribers Open browser update streams.\n")
	b.WriteString("# TYPE cairn_live_subscribers gauge\n")
	fmt.Fprintf(b, "cairn_live_subscribers %d\n", counter.Subscribers())
}

// writeProbeMetrics republishes what each probe reported about itself.
//
// A probe has no inbound port to scrape — it dials out and never listens — so
// these arrive on the result stream and are re-emitted here, labelled by probe
// (docs/probe/protocol.md §8). In solo mode there is one probe in this process
// and the path is identical, which is the point: what an operator reads is
// produced by the same code a remote probe will run.
//
// cairn_probe_shed_results_total and cairn_probe_skipped_checks_total are the
// two that matter most. A probe under overload sheds rather than queueing, and
// shedding is invisible from the monitor's side by design — the whole taxonomy
// exists so that probe overload never looks like target downtime, which means it
// has to look like something *here* instead.
func writeProbeMetrics(b *strings.Builder) {
	reports := telemetry.Probes()
	if len(reports) == 0 {
		return
	}

	for _, series := range []struct {
		name, help, kind string
		value            func(telemetry.ProbeHealth) uint64
	}{
		{"cairn_probe_assigned_monitors", "Monitors currently assigned to this probe.", "gauge",
			func(h telemetry.ProbeHealth) uint64 { return uint64(h.Assigned) }},
		{"cairn_probe_checks_in_flight", "Checks executing right now.", "gauge",
			func(h telemetry.ProbeHealth) uint64 { return uint64(h.InFlight) }},
		{"cairn_probe_max_concurrent_checks", "Worker pool size.", "gauge",
			func(h telemetry.ProbeHealth) uint64 { return uint64(h.MaxConcurrent) }},
		{"cairn_probe_due_queue_depth", "Checks waiting to start.", "gauge",
			func(h telemetry.ProbeHealth) uint64 { return uint64(h.DueQueueDepth) }},
		{"cairn_probe_buffered_results", "Results held pending acknowledgement.", "gauge",
			func(h telemetry.ProbeHealth) uint64 { return h.BufferedResults }},
		{"cairn_probe_buffered_bytes", "Approximate memory held by buffered results.", "gauge",
			func(h telemetry.ProbeHealth) uint64 { return h.BufferedBytes }},
		{"cairn_probe_shed_results_total", "Results dropped from a full buffer.", "counter",
			func(h telemetry.ProbeHealth) uint64 { return h.ShedResultsTotal }},
		{"cairn_probe_skipped_checks_total", "Checks that never started: past the lateness budget or the pool was full.", "counter",
			func(h telemetry.ProbeHealth) uint64 { return h.SkippedChecksTotal }},
		{"cairn_probe_checks_started_total", "Checks begun.", "counter",
			func(h telemetry.ProbeHealth) uint64 { return h.ChecksStartedTotal }},
		{"cairn_probe_checks_completed_total", "Checks finished.", "counter",
			func(h telemetry.ProbeHealth) uint64 { return h.ChecksCompletedTotal }},
		{"cairn_probe_uptime_seconds", "Seconds since the probe session started.", "gauge",
			func(h telemetry.ProbeHealth) uint64 { return h.UptimeSeconds }},
	} {
		fmt.Fprintf(b, "\n# HELP %s %s\n# TYPE %s %s\n", series.name, series.help, series.name, series.kind)
		for _, h := range reports {
			fmt.Fprintf(b, "%s{probe_id=%q,probe=%q} %d\n",
				series.name, h.ProbeID, escapeLabel(h.Name), series.value(h))
		}
	}

	// Signed, so it is a gauge of its own rather than folded into the loop
	// above. A probe whose clock has drifted writes heartbeats stamped from the
	// past or the future, and every ordering guarantee in the system is built on
	// that timestamp.
	b.WriteString("\n# HELP cairn_probe_clock_offset_seconds Probe clock offset from the control plane, signed.\n")
	b.WriteString("# TYPE cairn_probe_clock_offset_seconds gauge\n")
	for _, h := range reports {
		fmt.Fprintf(b, "cairn_probe_clock_offset_seconds{probe_id=%q} %g\n",
			h.ProbeID, float64(h.ClockOffsetMicros)/1e6)
	}
}
