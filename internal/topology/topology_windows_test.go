//go:build windows

package topology

import (
	"math/bits"
	"runtime"
	"testing"
)

// Detect on the machine running the tests: every logical processor belongs
// to exactly one core.
func TestDetectThisMachine(t *testing.T) {
	topo, err := Detect()
	if err != nil {
		t.Fatal(err)
	}
	var seen uint64
	for _, c := range topo.Cores {
		if seen&c.Mask != 0 {
			t.Fatalf("logical processors %s are in two cores", MaskString(seen&c.Mask))
		}
		seen |= c.Mask
	}
	if n := bits.OnesCount64(seen); n != runtime.NumCPU() {
		t.Fatalf("detected %d logical processors, runtime.NumCPU() = %d", n, runtime.NumCPU())
	}
	t.Log("\n" + topo.Describe())
}
