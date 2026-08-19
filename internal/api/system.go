package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/auth"
	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
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
		"certificate_detail": false,
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
		if isLoopback(clientIP(r)) {
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
