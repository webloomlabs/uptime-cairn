// Package protocol holds the parts of the probe protocol both sides must compute
// identically.
//
// One definition, imported by the control plane and the probe alike. Two
// implementations of a hash that must agree is a bug waiting for the first
// reconciliation, and it would present as a permanent full-set resend rather
// than as an error.
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

// AssignmentDigest is specified byte-for-byte in docs/probe/protocol.md §5.3:
// for each assignment, monitor_id ‖ 0x00 ‖ config_version ‖ 0x00; sort those
// byte strings; sha256 the concatenation; lowercase hex.
//
// An empty set digests as sha256 of the empty string, which is what makes "the
// probe has nothing and the control plane has nothing" agree rather than
// resync forever.
func AssignmentDigest(assignments []*probev1.Assignment) string {
	entries := make([][]byte, 0, len(assignments))
	for _, a := range assignments {
		e := make([]byte, 0, len(a.GetMonitorId())+len(a.GetConfigVersion())+2)
		e = append(e, a.GetMonitorId()...)
		e = append(e, 0x00)
		e = append(e, a.GetConfigVersion()...)
		e = append(e, 0x00)
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return string(entries[i]) < string(entries[j]) })

	h := sha256.New()
	for _, e := range entries {
		h.Write(e)
	}
	return hex.EncodeToString(h.Sum(nil))
}
