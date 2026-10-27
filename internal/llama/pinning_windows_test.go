//go:build windows

package llama

import (
	"testing"

	"tenantscaler/internal/topology"
)

// A launched replica runs with its slot's mask as its process affinity,
// whatever this process's own affinity is.
func TestLaunchPinsReplicaProcess(t *testing.T) {
	l := fakeLauncher(t)
	l.Spec.CPUMasks = []string{"3", "c"}
	p := launch(t, l, "pinned-1", 1)
	got, err := topology.ProcessAffinity(p.cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0xc {
		t.Fatalf("replica affinity %s, want CPUs 2-3 (slot 1)", topology.MaskString(got))
	}
}
