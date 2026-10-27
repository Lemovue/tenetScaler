package llama

import (
	"errors"
	"testing"

	"tenantscaler/internal/config"
	"tenantscaler/internal/topology"
)

func TestPlanPinningConfiguredMasks(t *testing.T) {
	p, err := PlanPinning(config.LlamaSpec{CPUMasks: []string{"3", "0xc"}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Replicas) != 2 || p.Replicas[0] != 0x3 || p.Replicas[1] != 0xc || p.Services&0xf != 0 || p.Services == 0 {
		t.Fatalf("pinning %+v: replicas 0-1 and 2-3, services elsewhere", p)
	}
	if _, err := PlanPinning(config.LlamaSpec{CPUMasks: []string{"zz"}}, 1); err == nil {
		t.Error("a bad configured mask was accepted")
	}
}

// Without configured masks every replica slot gets its own P-core and the
// services get every other CPU; on this machine's detected topology.
func TestPlanPinningDetectedPCores(t *testing.T) {
	topo, err := topology.Detect()
	if errors.Is(err, topology.ErrUnsupported) {
		t.Skip("topology detection is not implemented on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	n := min(4, len(topo.PCores()))
	p, err := PlanPinning(config.LlamaSpec{}, n)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := topo.ReplicaMasks(n)
	var used uint64
	for i, m := range p.Replicas {
		if m != want[i] {
			t.Errorf("slot %d mask %s, want P-core %s", i, topology.MaskString(m), topology.MaskString(want[i]))
		}
		used |= m
	}
	if p.Services != topo.LogicalMask()&^used {
		t.Errorf("services %s, want every CPU but the replicas'", topology.MaskString(p.Services))
	}
	t.Log(p)
}
