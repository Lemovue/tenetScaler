package topology

import (
	"encoding/binary"
	"slices"
	"strings"
	"testing"
)

// record builds one RelationProcessorCore record as Windows lays it out.
func record(mask uint64, efficiency byte, smt bool) []byte {
	rec := make([]byte, offGroupMask+groupAffinitySize)
	binary.LittleEndian.PutUint32(rec[0:], relationProcessorCore)
	binary.LittleEndian.PutUint32(rec[4:], uint32(len(rec)))
	if smt {
		rec[offFlags] = ltpPcSMT
	}
	rec[offEfficiency] = efficiency
	binary.LittleEndian.PutUint16(rec[offGroupCount:], 1)
	binary.LittleEndian.PutUint64(rec[offGroupMask:], mask)
	return rec
}

// hybrid is a small hybrid CPU: 3 P-cores with 2 threads (0-5) and 2
// E-cores (6, 7), reported out of order.
func hybrid() []byte {
	return slices.Concat(record(0x30, 1, true), record(0x40, 0, false), record(0x3, 1, true), record(0xc, 1, true), record(0x80, 0, false))
}

func TestParseHybrid(t *testing.T) {
	topo, err := parseCores(hybrid())
	if err != nil {
		t.Fatal(err)
	}
	if !topo.Hybrid() || len(topo.Cores) != 5 || len(topo.PCores()) != 3 || len(topo.ECores()) != 2 {
		t.Fatalf("topology %+v", topo)
	}
	if got := topo.PCores()[0].Logical(); !slices.Equal(got, []int{0, 1}) {
		t.Errorf("first P-core = %v, want [0 1] (sorted by lowest logical processor)", got)
	}
	masks, err := topo.ReplicaMasks(2)
	if err != nil || !slices.Equal(masks, []uint64{0x3, 0xc}) {
		t.Fatalf("ReplicaMasks(2) = %x, %v", masks, err)
	}
	svc, err := topo.ServiceMask(2)
	if err != nil || svc != 0xf0 || MaskString(svc) != "4-7" {
		t.Fatalf("ServiceMask(2) = %x (%s), %v; want P-core 4-5 and E-cores 6-7", svc, MaskString(svc), err)
	}
	if HexMask(0xc) != "c" {
		t.Errorf("HexMask = %q", HexMask(0xc))
	}
	if _, err := topo.ReplicaMasks(4); err == nil {
		t.Error("4 replicas on 3 P-cores accepted")
	}
	if svc, err := topo.ServiceMask(3); err != nil || svc != 0xc0 {
		t.Errorf("ServiceMask(3) = %x, %v; want the E-cores 6-7", svc, err)
	}
	if d := topo.Describe(); !strings.Contains(d, "P-cores: 3 cores") || !strings.Contains(d, "E-cores: 2 cores") {
		t.Errorf("Describe = %q", d)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	other := record(0x1, 0, false)
	binary.LittleEndian.PutUint32(other[0:], 2) // another relationship: skipped
	bad := map[string][]byte{
		"empty":           nil,
		"truncated":       record(0x3, 1, true)[:20],
		"only other kind": other,
		"second group": func() []byte {
			r := record(0x3, 1, true)
			binary.LittleEndian.PutUint16(r[offGroupMask+8:], 1)
			return r
		}(),
	}
	for name, buf := range bad {
		if _, err := parseCores(buf); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNonHybridAllCoresArePCores(t *testing.T) {
	topo, err := parseCores(slices.Concat(record(0x3, 0, true), record(0xc, 0, true)))
	if err != nil {
		t.Fatal(err)
	}
	if topo.Hybrid() || len(topo.PCores()) != 2 || len(topo.ECores()) != 0 {
		t.Fatalf("non-hybrid: %+v", topo)
	}
	if _, err := topo.ServiceMask(2); err == nil || !strings.Contains(err.Error(), "no CPU") {
		t.Errorf("all cores used by replicas: err = %v, want 'no CPU' left for the services", err)
	}
}
