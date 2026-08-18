package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	probev1 "github.com/webloomlabs/uptime-cairn/proto/cairn/probe/v1"
)

func assignment(id byte, version string) *probev1.Assignment {
	return &probev1.Assignment{MonitorId: []byte{id}, ConfigVersion: version}
}

// The digest must not depend on the order the assignments happen to be in: the
// control plane builds its set from a map and the probe from a stream, and if
// ordering leaked in they would resync forever without ever erroring.
func TestAssignmentDigestIsOrderIndependent(t *testing.T) {
	t.Parallel()

	forward := AssignmentDigest([]*probev1.Assignment{assignment(1, "a"), assignment(2, "b")})
	backward := AssignmentDigest([]*probev1.Assignment{assignment(2, "b"), assignment(1, "a")})

	if forward != backward {
		t.Errorf("digest depends on order: %s != %s", forward, backward)
	}
}

func TestAssignmentDigestDetectsChange(t *testing.T) {
	t.Parallel()

	base := AssignmentDigest([]*probev1.Assignment{assignment(1, "a")})

	if changed := AssignmentDigest([]*probev1.Assignment{assignment(1, "b")}); changed == base {
		t.Error("a changed config_version produced the same digest")
	}
	if added := AssignmentDigest([]*probev1.Assignment{assignment(1, "a"), assignment(2, "a")}); added == base {
		t.Error("an added assignment produced the same digest")
	}
}

// An empty set on both sides must agree, or a probe with no monitors would ask
// for a full set every reconciliation forever.
func TestAssignmentDigestEmptyIsSHA256OfNothing(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256(nil)
	if got, want := AssignmentDigest(nil), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("empty digest = %s, want %s", got, want)
	}
}
