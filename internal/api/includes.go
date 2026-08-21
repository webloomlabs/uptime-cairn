package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// The include= parameter.
//
// Everything here is opt-in for one reason: cost. The dashboard's list view
// wants a monitor's last heartbeat and its uptime under every row; an export
// wants neither, and asking for them anyway is twenty-five extra range scans a
// page. The spec makes that the client's decision, and this is where the
// decision is honoured — one extra query per requested embed, never one per row.

// includes is the parsed include= set.
type includes struct {
	LastHeartbeat bool
	Heartbeats    bool
	Uptime        bool
	Tags          bool
	Group         bool
	Certificate   bool

	// HeartbeatLimit is how long a run `heartbeats` resolves. Set from
	// heartbeats_limit=, clamped rather than rejected, in the same spirit as
	// the page limit: a client asking for a thousand beats a row gets the
	// ceiling, not a 400.
	HeartbeatLimit int
}

func (i includes) any() bool {
	return i.LastHeartbeat || i.Heartbeats || i.Uptime || i.Tags || i.Group || i.Certificate
}

// parseIncludes reads the comma-separated list. An unrecognised value is a 400
// rather than being ignored: a client asking for data it will not get should
// find out at development time, not by wondering why a field is missing.
//
// `certificate` is accepted only on the single-monitor read, which is where the
// spec offers it — embedding it in a list would be a per-row primary-key read
// for a field almost no row has.
func (s *Server) parseIncludes(w http.ResponseWriter, r *http.Request, allowCertificate bool) (includes, bool) {
	var out includes

	raw := r.URL.Query().Get("include")
	if raw == "" {
		return out, true
	}

	for _, name := range strings.Split(raw, ",") {
		switch strings.TrimSpace(name) {
		case "":
		case "last_heartbeat":
			out.LastHeartbeat = true
		case "heartbeats":
			out.Heartbeats = true
		case "uptime":
			out.Uptime = true
		case "tags":
			out.Tags = true
		case "group":
			out.Group = true
		case "certificate":
			if !allowCertificate {
				writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-include", "Invalid include",
					"certificate can only be included on a single monitor, not on a list")
				return includes{}, false
			}
			out.Certificate = true
		default:
			writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-include", "Invalid include",
				fmt.Sprintf("include %q is not one of last_heartbeat, heartbeats, uptime, tags, group, certificate", strings.TrimSpace(name)))
			return includes{}, false
		}
	}

	if out.Heartbeats {
		out.HeartbeatLimit = defaultHeartbeatEmbed
		if raw := r.URL.Query().Get("heartbeats_limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				writeProblem(w, r, s.log, http.StatusBadRequest, "invalid-parameter", "Invalid parameter",
					"heartbeats_limit must be a positive integer")
				return includes{}, false
			}
			out.HeartbeatLimit = min(n, maxHeartbeatEmbed)
		}
	}
	return out, true
}

// The bounds on the heartbeats embed.
//
// A run of checks under a row is a fixed-width strip on screen, so the useful
// range is small and the ceiling is what keeps the embed's cost bounded by the
// viewport rather than by whatever a caller types — which is the ADR-004
// invariant this endpoint is on the wrong side of the moment the ceiling is
// removed. Fifty beats across a hundred rows is five thousand rows, which is
// the largest page this embed can be asked to produce.
const (
	defaultHeartbeatEmbed = 20
	maxHeartbeatEmbed     = 50
)

// embed fills in the requested include= data for a page of monitors.
//
// One query per embed for the whole page, never one per monitor. That is the
// entire reason this is a separate function rather than a few lines inside the
// render loop: written inline it would be correct and it would fan out, and the
// fan-out is what the 5,000-monitor gate exists to catch.
func (s *Server) embed(ctx context.Context, rendered []monitorJSON, monitors []store.MonitorWithState, want includes) error {
	if !want.any() || len(monitors) == 0 {
		return nil
	}

	ids := make([]model.ID, 0, len(monitors))
	for _, m := range monitors {
		ids = append(ids, m.Monitor.ID)
	}

	if want.LastHeartbeat {
		beats, err := s.store.LastHeartbeats(ctx, ids)
		if err != nil {
			return fmt.Errorf("last heartbeats: %w", err)
		}
		for i, m := range monitors {
			if beat, ok := beats[m.Monitor.ID]; ok {
				encoded := toHeartbeatJSON(beat)
				rendered[i].LastHeartbeat = &encoded
			}
		}
	}

	if want.Heartbeats {
		// One bounded seek per monitor stitched into one statement, not one
		// request per row — the strip under each row in the reference dashboard
		// is exactly the fan-out both ADR-004 and this parameter exist to
		// prevent, and drawing it from a single beat or from an uptime ratio
		// would be inventing a history the client was never told.
		runs, err := s.store.RecentHeartbeats(ctx, ids, want.HeartbeatLimit)
		if err != nil {
			return fmt.Errorf("recent heartbeats: %w", err)
		}
		for i, m := range monitors {
			run := runs[m.Monitor.ID]
			encoded := make([]heartbeatJSON, 0, len(run))
			for _, beat := range run {
				encoded = append(encoded, toHeartbeatJSON(beat))
			}
			rendered[i].Heartbeats = encoded
		}
	}

	if want.Uptime {
		// Read from the cache rather than computed: computing 24h and 30d per
		// row across a page is a fan-out of range scans, and at 5,000 monitors
		// that is exactly the convenience that fails the load gate (data model
		// §5.5). A monitor absent from the cache reports null, not zero.
		day, err := s.store.UptimeRatios(ctx, ids, "24h")
		if err != nil {
			return fmt.Errorf("uptime cache: %w", err)
		}
		month, err := s.store.UptimeRatios(ctx, ids, "30d")
		if err != nil {
			return fmt.Errorf("uptime cache: %w", err)
		}
		for i, m := range monitors {
			embed := uptimeEmbed{}
			if ratio, ok := day[m.Monitor.ID]; ok {
				embed.Ratio24h = &ratio
			}
			if ratio, ok := month[m.Monitor.ID]; ok {
				embed.Ratio30d = &ratio
			}
			rendered[i].Uptime = &embed
		}
	}

	if want.Tags {
		assignments, err := s.store.TagIDsForMonitors(ctx, ids)
		if err != nil {
			return fmt.Errorf("monitor tags: %w", err)
		}
		catalogue, err := s.tagCatalogue(ctx)
		if err != nil {
			return err
		}
		for i, m := range monitors {
			tags := make([]tagJSON, 0, len(assignments[m.Monitor.ID]))
			for _, id := range assignments[m.Monitor.ID] {
				if tag, ok := catalogue[id]; ok {
					tags = append(tags, tag)
				}
			}
			rendered[i].Tags = tags
		}
	}

	if want.Group {
		groups, err := s.groupCatalogue(ctx)
		if err != nil {
			return err
		}
		for i, m := range monitors {
			if m.Monitor.GroupID == nil {
				continue
			}
			if group, ok := groups[*m.Monitor.GroupID]; ok {
				embedded := group
				rendered[i].Group = &embedded
			}
		}
	}

	if want.Certificate {
		now := time.Now().UTC()
		for i, m := range monitors {
			certificate, err := s.store.GetCertificate(ctx, m.Monitor.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			} else if err != nil {
				return fmt.Errorf("certificate: %w", err)
			}
			encoded := toCertificateJSON(certificate, now)
			rendered[i].Certificate = &encoded
		}
	}
	return nil
}

// tagCatalogue and groupCatalogue read the whole taxonomy in one page.
//
// A join per monitor would be the obvious implementation and the wrong one: tags
// and groups are counted in dozens, not thousands, so one read of the lot beats
// a lookup per row by an order of magnitude — and the alternative is a query
// whose cost scales with the page rather than with the vocabulary.
func (s *Server) tagCatalogue(ctx context.Context) (map[model.ID]tagJSON, error) {
	tags, _, err := s.store.ListTags(ctx, nil, maxTaxonomyPage, "")
	if err != nil {
		return nil, fmt.Errorf("tag catalogue: %w", err)
	}
	out := make(map[model.ID]tagJSON, len(tags))
	for _, t := range tags {
		out[t.Tag.ID] = toTagJSON(t)
	}
	return out, nil
}

func (s *Server) groupCatalogue(ctx context.Context) (map[model.ID]groupJSON, error) {
	groups, _, err := s.store.ListGroups(ctx, nil, maxTaxonomyPage, "")
	if err != nil {
		return nil, fmt.Errorf("group catalogue: %w", err)
	}
	out := make(map[model.ID]groupJSON, len(groups))
	for _, g := range groups {
		out[g.Group.ID] = toGroupJSON(g)
	}
	return out, nil
}

// maxTaxonomyPage bounds the catalogue read. An install with more than a
// thousand tags has a taxonomy problem rather than an API problem, and the
// embed degrades to omitting the ones past the bound rather than to a slow
// query.
const maxTaxonomyPage = 1000
