// Package topology detects the CPU's physical cores and their performance
// class (P-cores and E-cores on hybrid CPUs such as the i5-14450HX) and
// builds the affinity masks that pin each llama-server replica to one
// physical P-core and the TenantScaler services to the remaining CPUs
// (PLAN.md decision Q13). Detection uses GetLogicalProcessorInformationEx
// on Windows; other platforms report ErrUnsupported.
package topology

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"
)

// ErrUnsupported is returned by Detect where no detection is implemented.
var ErrUnsupported = errors.New("topology: CPU core detection is implemented for Windows only")

// Core is one physical core.
type Core struct {
	// Mask holds the core's logical processors (processor group 0).
	Mask uint64
	// EfficiencyClass orders cores by performance: higher is faster
	// (P-cores above E-cores on hybrid CPUs); all equal otherwise.
	EfficiencyClass int
	// SMT reports two or more logical processors on the core.
	SMT bool
}

// Logical returns the core's logical processor numbers, ascending.
func (c Core) Logical() []int { return maskBits(c.Mask) }

// Topology is the detected set of physical cores, ordered by their lowest
// logical processor.
type Topology struct {
	Cores []Core
}

// Hybrid reports cores of more than one efficiency class.
func (t Topology) Hybrid() bool {
	for _, c := range t.Cores {
		if c.EfficiencyClass != t.Cores[0].EfficiencyClass {
			return true
		}
	}
	return false
}

// PCores returns the cores of the highest efficiency class: the
// performance cores of a hybrid CPU, every core otherwise.
func (t Topology) PCores() []Core {
	top := slices.MaxFunc(t.Cores, func(a, b Core) int { return a.EfficiencyClass - b.EfficiencyClass }).EfficiencyClass
	return slices.DeleteFunc(slices.Clone(t.Cores), func(c Core) bool { return c.EfficiencyClass != top })
}

// ECores returns the cores below the highest efficiency class.
func (t Topology) ECores() []Core {
	top := t.PCores()[0].EfficiencyClass
	return slices.DeleteFunc(slices.Clone(t.Cores), func(c Core) bool { return c.EfficiencyClass == top })
}

// LogicalMask is the mask of every logical processor.
func (t Topology) LogicalMask() uint64 {
	var m uint64
	for _, c := range t.Cores {
		m |= c.Mask
	}
	return m
}

// ReplicaMasks returns the masks of the first n P-cores (both hardware
// threads of each), one per replica slot.
func (t Topology) ReplicaMasks(n int) ([]uint64, error) {
	p := t.PCores()
	if n < 0 || n > len(p) {
		return nil, fmt.Errorf("topology: %d replicas need %d P-cores, this CPU has %d", n, n, len(p))
	}
	out := make([]uint64, n)
	for i := range n {
		out[i] = p[i].Mask
	}
	return out, nil
}

// ServiceMask is every logical processor outside the first n P-cores: the
// CPUs left for the gateway, controller and load generator.
func (t Topology) ServiceMask(n int) (uint64, error) {
	masks, err := t.ReplicaMasks(n)
	if err != nil {
		return 0, err
	}
	m := t.LogicalMask()
	for _, r := range masks {
		m &^= r
	}
	if m == 0 {
		return 0, fmt.Errorf("topology: %d replicas leave no CPU for the services", n)
	}
	return m, nil
}

// Describe is a human-readable summary, one line per core class.
func (t Topology) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d physical cores, %d logical processors, hybrid: %t\n", len(t.Cores), bits.OnesCount64(t.LogicalMask()), t.Hybrid())
	line := func(name string, cores []Core) {
		parts := make([]string, len(cores))
		for i, c := range cores {
			parts[i] = fmt.Sprintf("%v", c.Logical())
		}
		fmt.Fprintf(&b, "%s: %d cores, logical processors %s\n", name, len(cores), strings.Join(parts, " "))
	}
	line("P-cores", t.PCores())
	if t.Hybrid() {
		line("E-cores", t.ECores())
	}
	return b.String()
}

// HexMask formats a mask as llama-server's -C option expects (hex, no 0x).
func HexMask(m uint64) string { return strconv.FormatUint(m, 16) }

// MaskString lists a mask's logical processors, e.g. "0,1,8-15".
func MaskString(m uint64) string {
	cpus := maskBits(m)
	var parts []string
	for i := 0; i < len(cpus); {
		j := i
		for j+1 < len(cpus) && cpus[j+1] == cpus[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(cpus[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", cpus[i], cpus[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

func maskBits(m uint64) []int {
	var out []int
	for m != 0 {
		i := bits.TrailingZeros64(m)
		out = append(out, i)
		m &^= 1 << i
	}
	return out
}

// Layout of the RelationProcessorCore records GetLogicalProcessorInformationEx
// returns on 64-bit Windows (winnt.h): SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX
// {Relationship uint32, Size uint32, PROCESSOR_RELATIONSHIP{Flags byte,
// EfficiencyClass byte, Reserved [20]byte, GroupCount uint16,
// GroupMask [GroupCount]GROUP_AFFINITY{Mask uint64, Group uint16,
// Reserved [3]uint16}}}.
const (
	relationProcessorCore = 0
	ltpPcSMT              = 1
	offFlags              = 8
	offEfficiency         = 9
	offGroupCount         = 30
	offGroupMask          = 32
	groupAffinitySize     = 16
)

// parseCores decodes a RelationProcessorCore buffer into a Topology. Only
// processor group 0 is supported (up to 64 logical processors).
func parseCores(buf []byte) (Topology, error) {
	var t Topology
	for off := 0; off < len(buf); {
		if len(buf)-off < 8 {
			return Topology{}, fmt.Errorf("topology: truncated record at byte %d", off)
		}
		rel := binary.LittleEndian.Uint32(buf[off:])
		size := int(binary.LittleEndian.Uint32(buf[off+4:]))
		if size < 8 || off+size > len(buf) {
			return Topology{}, fmt.Errorf("topology: bad record size %d at byte %d", size, off)
		}
		rec := buf[off : off+size]
		off += size
		if rel != relationProcessorCore {
			continue
		}
		if len(rec) < offGroupMask+groupAffinitySize {
			return Topology{}, fmt.Errorf("topology: processor record of %d bytes is too short", len(rec))
		}
		c := Core{EfficiencyClass: int(rec[offEfficiency]), SMT: rec[offFlags]&ltpPcSMT != 0}
		groups := int(binary.LittleEndian.Uint16(rec[offGroupCount:]))
		for g := range groups {
			base := offGroupMask + g*groupAffinitySize
			if base+groupAffinitySize > len(rec) {
				return Topology{}, fmt.Errorf("topology: processor record lists %d groups but holds fewer", groups)
			}
			if group := binary.LittleEndian.Uint16(rec[base+8:]); group != 0 {
				return Topology{}, fmt.Errorf("topology: processor group %d found; only machines with one group (up to 64 logical processors) are supported", group)
			}
			c.Mask |= binary.LittleEndian.Uint64(rec[base:])
		}
		if c.Mask != 0 {
			t.Cores = append(t.Cores, c)
		}
	}
	if len(t.Cores) == 0 {
		return Topology{}, errors.New("topology: no processor cores reported")
	}
	slices.SortFunc(t.Cores, func(a, b Core) int { return bits.TrailingZeros64(a.Mask) - bits.TrailingZeros64(b.Mask) })
	return t, nil
}
