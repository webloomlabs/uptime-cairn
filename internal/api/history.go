package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// /history and /uptime read the rollup tiers the pipeline builds — and read raw
// heartbeats instead whenever raw still covers the requested range.
//
// That fallback is not an optimisation. A rollup tier lags by its own bucket
// width plus the pipeline's grace period, so a 24-hour chart served from the 5m
// tier is missing its last few minutes: precisely the part someone watching an
// incident is looking at. When the raw rows are there, they are both fresher and
// exact, and the aggregation is the same one the pipeline performs.

// historyTier is one selectable resolution. raw is listed first and has no fixed
// interval — it means "one bucket per check", so its width is the monitor's own.
type historyTier struct {
	name     string
	interval time.Duration
}

// Ordered finest to coarsest. rollupTiers excludes raw, because auto never picks
// raw: a caller asking for a chart wants buckets, and someone who genuinely
// wants individual results has /heartbeats.
var (
	rawTier     = historyTier{name: "raw"}
	rollupTiers = []historyTier{
		{name: "1m", interval: time.Minute},
		{name: "5m", interval: 5 * time.Minute},
		{name: "1h", interval: time.Hour},
		{name: "1d", interval: 24 * time.Hour},
	}
)

const (
	// minHistoryBuckets is what makes a resolution "useful" for auto: below it a
	// chart has too few points to show anything, so auto steps finer.
	minHistoryBuckets = 120

	// maxHistoryBuckets is where a response stops being a chart and becomes a
	// download. The spec allows the server to answer at a coarser resolution
	// than asked for, which is exactly what this triggers.
	maxHistoryBuckets = 2000

	defaultHistorySpan = 24 * time.Hour
	maxHistorySpan     = 10 * 365 * 24 * time.Hour
)

// uptimeWindowTiers maps a named window onto the tier that answers it when raw
// no longer reaches back that far. Each is chosen to keep the read to a few
// hundred buckets — a primary-key range seek, not a scan.
var uptimeWindowTiers = map[string]struct {
	span time.Duration
	tier string
}{
	"1h":   {time.Hour, "1m"},
	"24h":  {24 * time.Hour, "5m"},
	"7d":   {7 * 24 * time.Hour, "1h"},
	"30d":  {30 * 24 * time.Hour, "1d"},
	"90d":  {90 * 24 * time.Hour, "1d"},
	"365d": {365 * 24 * time.Hour, "1d"},
}

var defaultUptimeWindows = []string{"24h", "30d"}

func (s *Server) getMonitorHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.monitorID(w, r)
	if !ok {
		return
	}
	monitor, err := s.store.GetMonitor(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.internal(w, r, "load monitor", err)
		return
	}

	from, to, problem := timeRange(r, defaultHistorySpan)
	if problem != "" {
		writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-range", "Invalid time range", problem)
		return
	}

	requested := r.URL.Query().Get("resolution")
	if requested == "" {
		requested = "auto"
	}
	tier, problem := resolveHistoryTier(requested, from, to, monitor.Monitor.Interval)
	if problem != "" {
		writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-resolution", "Invalid resolution", problem)
		return
	}

	buckets, err := s.readHistory(r, id, from, to, tier)
	if err != nil {
		s.internal(w, r, "read history", err)
		return
	}

	body := historyResponse{
		MonitorID:  id.String(),
		Resolution: tier.name,
		From:       from,
		To:         to,
		Data:       make([]historyBucketJSON, 0, len(buckets)),
	}
	for _, b := range buckets {
		body.Data = append(body.Data, toHistoryBucketJSON(b))
	}
	writeJSON(w, s.log, http.StatusOK, body)
}

// readHistory picks the source. Raw when it covers the range, the tier
// otherwise — and raw is the only source that can answer at raw resolution at
// all, so asking for it beyond retention answers with what is actually there.
func (s *Server) readHistory(r *http.Request, id model.ID, from, to time.Time, tier historyTier) ([]store.HistoryBucket, error) {
	covered, err := s.store.RawCovers(r.Context(), id, from, tier.name)
	if err != nil {
		return nil, err
	}
	if covered {
		return s.store.HistoryFromRaw(r.Context(), id, from, to, tier.interval)
	}
	return s.store.HistoryFromTier(r.Context(), id, from, to, tier.name)
}

func (s *Server) getMonitorUptime(w http.ResponseWriter, r *http.Request) {
	id, ok := s.monitorID(w, r)
	if !ok {
		return
	}
	monitor, err := s.store.GetMonitor(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.internal(w, r, "load monitor", err)
		return
	}

	windows := r.URL.Query()["window"]
	if len(windows) == 0 {
		windows = defaultUptimeWindows
	}
	for _, name := range windows {
		if _, ok := uptimeWindowTiers[name]; !ok {
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-window", "Invalid window",
				fmt.Sprintf("window %q is not one the spec defines: want 1h, 24h, 7d, 30d, 90d, or 365d", name))
			return
		}
	}

	handling := r.URL.Query().Get("maintenance")
	if handling == "" {
		handling = "exclude"
	}
	switch handling {
	case "exclude", "count_as_up", "count_as_down":
	default:
		writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-maintenance", "Invalid maintenance handling",
			fmt.Sprintf("maintenance %q: want exclude, count_as_up, or count_as_down", handling))
		return
	}

	now := time.Now().UTC()
	body := uptimeSummary{
		MaintenanceHandling: handling,
		Windows:             make(map[string]uptimeWindowJSON, len(windows)),
	}

	for _, name := range windows {
		spec := uptimeWindowTiers[name]
		from := now.Add(-spec.span)

		covered, err := s.store.RawCovers(r.Context(), id, from, spec.tier)
		if err != nil {
			s.internal(w, r, "read uptime", err)
			return
		}

		var totals store.HistoryBucket
		if covered {
			totals, err = s.store.UptimeFromRaw(r.Context(), id, from, now)
		} else {
			totals, err = s.store.UptimeFromTier(r.Context(), id, from, now, spec.tier)
		}
		if err != nil {
			s.internal(w, r, "read uptime", err)
			return
		}
		body.Windows[name] = toUptimeWindowJSON(totals, handling, monitor.Monitor.Interval)
	}

	writeJSON(w, s.log, http.StatusOK, body)
}

// resolveHistoryTier turns the requested resolution into the one actually used.
//
// The spec says the response reports "the tier actually used, which may be
// coarser than requested", and this is where that happens: a request for
// one-minute buckets over ninety days would be 130,000 of them, so it is served
// at a resolution that fits and says so, rather than refused or streamed.
func resolveHistoryTier(requested string, from, to time.Time, monitorInterval time.Duration) (historyTier, string) {
	span := to.Sub(from)

	if requested == "auto" {
		return autoHistoryTier(span), ""
	}

	start := -1
	if requested == rawTier.name {
		// One bucket per check: the monitor's own interval is the only width
		// that means "raw" without inventing a number.
		candidate := historyTier{name: rawTier.name, interval: monitorInterval}
		if monitorInterval > 0 && span/monitorInterval <= maxHistoryBuckets {
			return candidate, ""
		}
		start = 0
	} else {
		for i, tier := range rollupTiers {
			if tier.name == requested {
				start = i
				break
			}
		}
		if start < 0 {
			return historyTier{}, fmt.Sprintf(
				"resolution %q is not one the spec defines: want auto, raw, 1m, 5m, 1h, or 1d", requested)
		}
	}

	for i := start; i < len(rollupTiers); i++ {
		if span/rollupTiers[i].interval <= maxHistoryBuckets {
			return rollupTiers[i], ""
		}
	}
	return rollupTiers[len(rollupTiers)-1], ""
}

// autoHistoryTier picks the coarsest resolution that still yields a useful
// number of buckets, stepping finer until it does and stopping before the
// response stops being a chart.
func autoHistoryTier(span time.Duration) historyTier {
	best := rollupTiers[len(rollupTiers)-1]

	for i := len(rollupTiers) - 1; i >= 0; i-- {
		count := span / rollupTiers[i].interval
		if count > maxHistoryBuckets {
			// This tier and every finer one are too many; the previous one wins.
			break
		}
		best = rollupTiers[i]
		if count >= minHistoryBuckets {
			break
		}
	}
	return best
}

// timeRange reads from/to, defaulting to the last `span` and refusing the
// ranges that are mistakes rather than requests.
func timeRange(r *http.Request, span time.Duration) (from, to time.Time, problem string) {
	query := r.URL.Query()
	now := time.Now().UTC()

	to = now
	if v := query.Get("to"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, "to must be an RFC 3339 timestamp"
		}
		to = parsed.UTC()
	}

	from = to.Add(-span)
	if v := query.Get("from"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, "from must be an RFC 3339 timestamp"
		}
		from = parsed.UTC()
	}

	switch {
	case !from.Before(to):
		return time.Time{}, time.Time{}, "from must be before to"
	case to.Sub(from) > maxHistorySpan:
		// Ten years is past the point where the answer is history rather than a
		// range someone meant to type.
		return time.Time{}, time.Time{}, "the range must be at most ten years"
	}
	return from, to, ""
}
