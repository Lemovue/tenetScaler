package llama

import (
	"context"
	"testing"
	"time"

	"tenantscaler/internal/pool"
)

// TestPoolWithLlamaBackend drives a pool of fake llama-server processes:
// slots, status fields, scale-down through drain, and Close.
func TestPoolWithLlamaBackend(t *testing.T) {
	t.Setenv(envFakeLoad, "100ms")
	l := fakeLauncher(t)
	p := pool.New(pool.Config{Name: "llama", MaxReplicas: 3, Backend: NewBackend(l)})
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("pool Close: %v", err)
		}
	})
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "2 ready replicas", func() bool { return p.Status().Ready == 2 })
	st := p.Status()
	if st.Backend != "llama" || st.Simulated || len(st.Replicas) != 2 {
		t.Fatalf("status %+v", st)
	}
	for i, r := range st.Replicas {
		if r.Slot != i || r.ColdStartSeconds < 0.1 {
			t.Fatalf("replica %d: %+v, want slot %d and cold start >= 0.1 s", i, r, i)
		}
	}
	pids, err := readPIDs(l.PIDFile)
	if err != nil || len(pids) != 2 {
		t.Fatalf("pid file: %v, %v", pids, err)
	}
	for _, ep := range p.Endpoints() {
		c, err := streamCompletion(context.Background(), ep, "through the pool", 4)
		if err != nil || c.CompletionTokens != 4 {
			t.Fatalf("completion via %s: %+v, %v", ep, c, err)
		}
	}

	if err := p.Scale(1); err != nil { // drains llama-2 (newest), which is idle
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "drained replica removed", func() bool { return len(p.Status().Replicas) == 1 })
	if got := p.Status().Replicas[0].ID; got != "llama-1" {
		t.Fatalf("remaining replica %s, want llama-1", got)
	}
	assertGone(t, pids[1])
	if err := p.Scale(2); err != nil { // the freed slot 1 is reused
		t.Fatal(err)
	}
	if r := p.Status().Replicas[1]; r.ID != "llama-3" || r.Slot != 1 {
		t.Fatalf("relaunched replica %+v, want llama-3 on slot 1", r)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	all, err := readPIDs(l.PIDFile)
	if err != nil {
		t.Fatal(err)
	}
	assertGone(t, all...)
}
