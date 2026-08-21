package controlplane

import (
	"os/exec"
	"strings"
	"testing"
)

// The ADR-001 seam, checked mechanically rather than by convention.
//
// The rule the doc comments state is that the control plane must not import the
// checkers: a control plane serving remote probes has no business linking
// checker binaries it does not run, and once it does, the split stops being a
// deployment choice and becomes a comment. Convention held it until check-now
// needed the certificate a check saw, at which point the shortest path was an
// import and the correct answer was a package boundary — internal/observation,
// which the probe and the API import and this package does not.
//
// So the boundary is asserted where it can fail: the transitive import graph.
// A test that only checks this file's own imports would pass the day somebody
// reaches for the mapper through a helper package.
func TestControlPlaneDoesNotLinkTheCheckers(t *testing.T) {
	t.Parallel()

	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to resolve the import graph with")
	}

	out, err := exec.CommandContext(t.Context(), goTool, "list", "-deps",
		"github.com/webloomlabs/uptime-cairn/internal/controlplane").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}

	forbidden := []string{
		// The checkers themselves, and their monitor-type registry.
		"github.com/webloomlabs/uptime-cairn/internal/probe",
		// The mapping from a checker's observation onto the wire. It is a
		// separate package precisely so that this assertion can be made.
		"github.com/webloomlabs/uptime-cairn/internal/observation",
	}
	for _, line := range strings.Split(string(out), "\n") {
		pkg := strings.TrimSpace(line)
		for _, bad := range forbidden {
			if pkg == bad || strings.HasPrefix(pkg, bad+"/") {
				t.Errorf("the control plane links %s, which ADR-001 puts on the probe's side of the seam", pkg)
			}
		}
	}
}
