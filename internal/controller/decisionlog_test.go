package controller

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"tenantscaler/internal/model"
)

// recs returns one record per cycle in [from, to], alternating tenants A, B.
func recs(from, to int64) []model.DecisionRecord {
	var out []model.DecisionRecord
	for c := from; c <= to; c++ {
		tenant := "A"
		if c%2 == 0 {
			tenant = "B"
		}
		out = append(out, model.DecisionRecord{Cycle: c, Tenant: tenant, Reason: "r"})
	}
	return out
}

func cycles(rs []model.DecisionRecord) []int64 {
	out := []int64{}
	for _, r := range rs {
		out = append(out, r.Cycle)
	}
	return out
}

func TestDecisionLogRingLimitAndFilter(t *testing.T) {
	l, err := NewDecisionLog(5, "")
	if err != nil {
		t.Fatal(err)
	}
	l.Add(recs(1, 3)...)
	l.Add(recs(4, 7)...) // wraps: cycles 1 and 2 are dropped
	tests := []struct {
		limit  int
		tenant string
		want   []int64
	}{
		{0, "", []int64{7, 6, 5, 4, 3}},
		{2, "", []int64{7, 6}},
		{10, "", []int64{7, 6, 5, 4, 3}},
		{0, "A", []int64{7, 5, 3}},
		{1, "B", []int64{6}},
		{0, "nobody", []int64{}},
	}
	for _, tt := range tests {
		if got := cycles(l.List(tt.limit, tt.tenant)); !equalInts(got, tt.want) {
			t.Errorf("List(%d, %q) = %v, want %v", tt.limit, tt.tenant, got, tt.want)
		}
	}
	if l.Len() != 5 {
		t.Errorf("Len = %d, want 5", l.Len())
	}
	if got := l.List(0, "nobody"); got == nil {
		t.Error("List returned nil, want an empty slice (JSON [])")
	}
}

func TestDecisionLogDefaultCapacity(t *testing.T) {
	l, err := NewDecisionLog(0, "")
	if err != nil {
		t.Fatal(err)
	}
	l.Add(recs(1, DefaultLogCapacity+10)...)
	got := l.List(0, "")
	if len(got) != DefaultLogCapacity || got[0].Cycle != DefaultLogCapacity+10 || got[len(got)-1].Cycle != 11 {
		t.Errorf("held %d records, newest %d, oldest %d; want %d, %d, 11",
			len(got), got[0].Cycle, got[len(got)-1].Cycle, DefaultLogCapacity, DefaultLogCapacity+10)
	}
}

func TestDecisionLogJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "decisions.jsonl")
	l, err := NewDecisionLog(2, path)
	if err != nil {
		t.Fatal(err)
	}
	l.Add(recs(1, 3)...)
	if err := l.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Reopening appends; the file keeps every record even after the ring
	// dropped some.
	l2, err := NewDecisionLog(2, path)
	if err != nil {
		t.Fatal(err)
	}
	l2.Add(recs(4, 4)...)
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	l2.Add(recs(5, 5)...) // after Close: memory only
	if l2.Err() != nil || l2.Len() != 2 {
		t.Errorf("Add after Close: err %v, len %d; want nil and 2", l2.Err(), l2.Len())
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r model.DecisionRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		got = append(got, r.Cycle)
	}
	if !equalInts(got, []int64{1, 2, 3, 4}) {
		t.Errorf("file cycles %v, want [1 2 3 4]", got)
	}
}

func TestDecisionLogBadPath(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewDecisionLog(1, dir); err == nil {
		t.Error("NewDecisionLog on a directory succeeded")
	}
}

func TestDecisionLogConcurrent(t *testing.T) {
	l, err := NewDecisionLog(100, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				l.Add(recs(int64(i), int64(i))...)
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				_ = l.List(10, "A")
			}
		}()
	}
	wg.Wait()
	if l.Len() != 100 {
		t.Errorf("Len = %d, want 100", l.Len())
	}
}

func equalInts(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
