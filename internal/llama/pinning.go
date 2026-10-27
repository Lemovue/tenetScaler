package llama

import (
	"fmt"
	"math/bits"
	"runtime"
	"strconv"
	"strings"

	"tenantscaler/internal/config"
	"tenantscaler/internal/topology"
)

// Pinning describes where a run's processes execute (decision Q13).
type Pinning struct {
	// Replicas are the CPU masks of replica slots 0..capacity-1.
	Replicas []uint64
	// Services is every other logical processor: where the gateway,
	// controller and load generator of a run (all in this process) go.
	Services uint64
}

// String is the one-line description used in run labels.
func (p Pinning) String() string {
	parts := make([]string, len(p.Replicas))
	for i, m := range p.Replicas {
		parts[i] = topology.MaskString(m)
	}
	return fmt.Sprintf("replicas on CPUs %s (one physical core each); gateway, controller and load generator on CPUs %s",
		strings.Join(parts, " | "), topology.MaskString(p.Services))
}

// PlanPinning computes the masks of the first capacity replica slots (as
// Launch pins them) and the service CPUs left over.
func PlanPinning(spec config.LlamaSpec, capacity int) (Pinning, error) {
	l := &Launcher{Spec: spec}
	var p Pinning
	var used uint64
	for slot := range capacity {
		hex, err := l.cpuMask(slot)
		if err != nil {
			return Pinning{}, err
		}
		m, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(hex), "0x"), 16, 64)
		if err != nil {
			return Pinning{}, fmt.Errorf("llama: CPU mask %q of slot %d: %w", hex, slot, err)
		}
		p.Replicas = append(p.Replicas, m)
		used |= m
	}
	all := ^uint64(0)
	if n := runtime.NumCPU(); n < 64 {
		all = uint64(1)<<n - 1
	}
	if topo, err := detectTopology(); err == nil {
		all = topo.LogicalMask()
	}
	if p.Services = all &^ used; p.Services == 0 {
		return Pinning{}, fmt.Errorf("llama: %d replicas use every CPU; none is left for the gateway, controller and load generator", capacity)
	}
	return p, nil
}

// PinServices pins this process to the service CPUs of PlanPinning and
// sets GOMAXPROCS to their number. Replicas launched afterwards still run
// on their own cores (Launch sets each child's affinity). Where pinning is
// not implemented it returns an error wrapping topology.ErrUnsupported and
// changes nothing.
func PinServices(spec config.LlamaSpec, capacity int) (Pinning, error) {
	p, err := PlanPinning(spec, capacity)
	if err != nil {
		return Pinning{}, err
	}
	if err := topology.PinCurrentProcess(p.Services); err != nil {
		return Pinning{}, err
	}
	runtime.GOMAXPROCS(bits.OnesCount64(p.Services))
	return p, nil
}
