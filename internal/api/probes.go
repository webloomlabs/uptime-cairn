package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/webloomlabs/uptime-cairn/internal/model"
	"github.com/webloomlabs/uptime-cairn/internal/store"
)

// Probes are readable and nothing else, in this build.
//
// Enrolment, revocation, and the operator screens that go with them are Phase 4
// behind the protocol's credential exchange (ADR-005 decision 8). What Phase 1
// needs is the ability to *name* one, because a monitor can be pinned to a probe
// and a client filling in that field has to be able to find out what the
// choices are.
//
// It reads under monitors:read rather than a scope of its own. The endpoint
// exists to let a caller fill in monitor.probe_id, it returns no credential, and
// in solo mode it answers with one row — inventing a scope for that would make
// every key issued today wrong the day Phase 4 needs one for real work.

// listProbes returns every probe. Unpaginated on purpose: the count is bounded
// by hosts an operator runs a probe on, which is a number in the tens even for
// the agency this product is sized for, and a cursor on a list that short is
// ceremony rather than protection.
func (s *Server) listProbes(w http.ResponseWriter, r *http.Request) {
	probes, err := s.store.ListProbes(r.Context())
	if err != nil {
		s.internal(w, r, "list probes", err)
		return
	}

	out := make([]probeJSON, 0, len(probes))
	for _, p := range probes {
		out = append(out, toProbeJSON(p))
	}
	writeJSON(w, s.log, http.StatusOK, map[string]any{"data": out})
}

// resolvePin validates and applies probe_id on a write.
//
// Three cases, and the third is the one worth having.
//
//   - Explicitly named: the probe must exist, or the caller gets a field error
//     pointing at /probe_id rather than a foreign-key failure surfacing as a 500
//     three layers away.
//   - Explicitly null: unpinned, run anywhere. Refused for a host-local type in
//     an install with more than one probe, for the same reason as below.
//   - Absent, on a host-local type: the install decides. With exactly one probe
//     — which is every solo install — the pin is implied and written, so the
//     response shows where the monitor actually landed rather than leaving the
//     field null and the placement to luck. With more than one, it is a
//     validation error naming the field, because guessing which host somebody
//     meant produces a monitor reporting a container missing that was never
//     meant to be there.
func (s *Server) resolvePin(ctx context.Context, m *model.Monitor, supplied *string, present bool) []ValidationItem {
	if present && supplied != nil && *supplied != "" {
		id, ok := model.ParseID(*supplied)
		if !ok {
			return []ValidationItem{{Pointer: "/probe_id", Code: "invalid",
				Message: "probe_id must be an identifier"}}
		}
		probe, err := s.store.GetProbe(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return []ValidationItem{{Pointer: "/probe_id", Code: "not_found",
				Message: "no probe with that identifier"}}
		} else if err != nil {
			return []ValidationItem{{Pointer: "/probe_id", Code: "invalid",
				Message: "probe_id could not be resolved"}}
		}
		if !probe.Enabled {
			return []ValidationItem{{Pointer: "/probe_id", Code: "unavailable",
				Message: "that probe is disabled, so a monitor pinned to it would never run"}}
		}
		m.ProbeID = &id
		return nil
	}

	if present {
		m.ProbeID = nil
	}
	if m.ProbeID != nil || !hostLocal(m.Type) {
		return nil
	}

	n, err := s.store.CountEnabledProbes(ctx)
	if err != nil {
		return []ValidationItem{{Pointer: "/probe_id", Code: "invalid",
			Message: "probe_id could not be resolved"}}
	}
	if n != 1 {
		return []ValidationItem{{Pointer: "/probe_id", Code: "required",
			Message: "a " + m.Type + " monitor asks a question only one host can answer, " +
				"so this install's " + plural(n, "probe") + " leave it ambiguous: name the probe to run it on"}}
	}

	probes, err := s.store.ListProbes(ctx)
	if err != nil || len(probes) == 0 {
		return []ValidationItem{{Pointer: "/probe_id", Code: "invalid",
			Message: "probe_id could not be resolved"}}
	}
	for _, p := range probes {
		if p.Enabled {
			id := p.ID
			m.ProbeID = &id
			break
		}
	}
	return nil
}

// hostLocal reports whether a type's answer depends on which host asks.
//
// Only docker today. It is a function rather than a field on the checker
// because the constraint is about placement and the checker is about checking —
// and because the control plane, which enforces the pin, must not import the
// checkers at all (ADR-001).
func hostLocal(monitorType string) bool {
	return monitorType == model.TypeDocker
}

// plural is the smallest possible version of the rule, for one message. English
// only, and deliberately not the i18n machinery: this string is a validation
// detail read by a developer, not a label read by a user.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
