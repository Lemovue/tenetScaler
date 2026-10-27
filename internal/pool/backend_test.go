package pool

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenantscaler/internal/replica"
)

func TestPickSlot(t *testing.T) {
	m := func(slot int, draining bool) *member { return &member{slot: slot, draining: draining} }
	tests := []struct {
		name    string
		members []*member
		max     int
		want    int
	}{
		{"empty pool", nil, 4, 0},
		{"lowest free slot", []*member{m(0, false), m(2, false)}, 4, 1},
		{"freed slot is reused", []*member{m(1, false), m(2, false), m(3, false)}, 4, 0},
		{"free slot preferred over a draining one", []*member{m(0, true), m(1, false)}, 3, 2},
		{"share a draining slot when none is free", []*member{m(0, false), m(1, true), m(2, false)}, 3, 1},
		{"skip a draining slot already shared", []*member{m(0, true), m(0, false), m(1, true)}, 2, 1},
		{"zero max still yields slot 0", nil, 0, 0},
	}
	for _, tc := range tests {
		if got := pickSlot(tc.members, tc.max); got != tc.want {
			t.Errorf("%s: pickSlot = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestSlotsReusedAndShared drives the slot rules through Scale: freed
// slots are reused and, with every slot held, a new replica shares the
// slot of a draining one until that one stops.
func TestSlotsReusedAndShared(t *testing.T) {
	p, b := newFakePool(t, 3)
	if err := p.Scale(3); err != nil {
		t.Fatal(err)
	}
	if err := p.Scale(1); err != nil { // drains p-3 then p-2 (newest ready first)
		t.Fatal(err)
	}
	if got, want := slotsOf(p.Status()), "p-1@0:ready,p-2@1:draining,p-3@2:draining"; got != want {
		t.Fatalf("after Scale(1): %s, want %s", got, want)
	}
	b.inst("p-2").finishDrain()
	waitFor(t, 2*time.Second, "p-2 removed", func() bool { return len(p.Status().Replicas) == 2 })
	if b.inst("p-2").closeCount() != 1 {
		t.Fatal("drained p-2 was not closed exactly once")
	}

	if err := p.Scale(3); err != nil { // p-4 takes freed slot 1, p-5 shares draining p-3's slot 2
		t.Fatal(err)
	}
	if got, want := slotsOf(p.Status()), "p-1@0:ready,p-3@2:draining,p-4@1:ready,p-5@2:ready"; got != want {
		t.Fatalf("after re-scale-up: %s, want %s", got, want)
	}
	b.inst("p-3").finishDrain()
	waitFor(t, 2*time.Second, "p-3 removed", func() bool { return len(p.Status().Replicas) == 3 })
	if got, want := b.launched(), "p-1@0,p-2@1,p-3@2,p-4@1,p-5@2"; got != want {
		t.Fatalf("launches %s, want %s", got, want)
	}
}

func TestStatusReportsBackendAndColdStart(t *testing.T) {
	p, _ := newFakePool(t, 2)
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	st := p.Status()
	if st.Backend != "fake" || st.Simulated || st.Ready != 2 || st.Desired != 2 {
		t.Fatalf("status %+v", st)
	}
	for _, r := range st.Replicas {
		if r.ColdStartSeconds != 1.5 || r.Running != 1 {
			t.Fatalf("replica %+v, want cold start 1.5 s and running 1", r)
		}
	}
	if eps := p.Endpoints(); strings.Join(eps, ",") != "http://fake/p-1,http://fake/p-2" {
		t.Fatalf("endpoints %v", eps)
	}
}

// TestStoppedReplicaReplaced checks that a replica which stopped on its own
// (a crashed process) is closed, leaves the counts, and is replaced on its
// slot by the next Scale.
func TestStoppedReplicaReplaced(t *testing.T) {
	p, b := newFakePool(t, 3)
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	b.inst("p-1").setState(replica.StateStopped)
	if st := p.Status(); st.Ready != 1 || len(st.Replicas) != 2 || p.Endpoints()[0] != "http://fake/p-2" {
		t.Fatalf("with p-1 stopped: %+v", st)
	}
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	if got, want := slotsOf(p.Status()), "p-2@1:ready,p-3@0:ready"; got != want {
		t.Fatalf("after replacement: %s, want %s", got, want)
	}
	waitFor(t, 2*time.Second, "stopped p-1 closed", func() bool { return b.inst("p-1").closeCount() == 1 })
}

func TestLaunchErrorKeepsSequence(t *testing.T) {
	p, b := newFakePool(t, 2)
	b.failErr = errors.New("no binary")
	err := p.Scale(1)
	if err == nil || !strings.Contains(err.Error(), "launch replica p-1: no binary") {
		t.Fatalf("Scale with a failing backend: %v", err)
	}
	if st := p.Status(); st.Desired != 1 || len(st.Replicas) != 0 {
		t.Fatalf("after failed launch: %+v", st)
	}
	if err := p.Scale(1); err != nil {
		t.Fatal(err)
	}
	if got := b.launched(); got != "p-1@0" {
		t.Fatalf("launches %s, want the failed ID reused: p-1@0", got)
	}
}

// TestCloseIsParallel checks that Close stops every instance, draining
// ones included, concurrently: each fake Close waits until all of them
// have been entered, which a sequential Close would never reach.
func TestCloseIsParallel(t *testing.T) {
	b := newFakeBackend()
	var entered sync.WaitGroup
	entered.Add(3)
	all := make(chan struct{})
	go func() { entered.Wait(); close(all) }()
	var failed atomic.Bool
	b.onClose = func() {
		entered.Done()
		select {
		case <-all:
		case <-time.After(2 * time.Second):
			failed.Store(true)
		}
	}
	p := New(Config{Name: "p", MaxReplicas: 3, Backend: b})
	if err := p.Scale(3); err != nil {
		t.Fatal(err)
	}
	if err := p.Scale(2); err != nil { // p-3 drains and never finishes
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if failed.Load() {
		t.Fatal("Close closed the instances one at a time")
	}
	for _, id := range []string{"p-1", "p-2", "p-3"} {
		if n := b.inst(id).closeCount(); n != 1 {
			t.Fatalf("%s closed %d times, want 1", id, n)
		}
	}
}

// TestSimulatedBackendStatus checks the default backend's status fields
// while its replicas start: distinct slots and no cold start yet. The
// hour-long StartupDuration keeps them starting however slow the machine.
func TestSimulatedBackendStatus(t *testing.T) {
	p := newPool(t, time.Hour, 3)
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	st := p.Status()
	if st.Backend != "simulated" || !st.Simulated {
		t.Fatalf("backend %q simulated %v", st.Backend, st.Simulated)
	}
	if got := slotsOf(st); got != "p-1@0:starting,p-2@1:starting" {
		t.Fatalf("slots %s", got)
	}
	for _, r := range st.Replicas {
		if r.ColdStartSeconds != 0 {
			t.Fatalf("cold start %v reported while starting", r.ColdStartSeconds)
		}
	}
}

// TestSimulatedBackendColdStart checks that a ready simulated replica
// reports the configured StartupDuration as its cold start.
func TestSimulatedBackendColdStart(t *testing.T) {
	const startup = 20 * time.Millisecond
	p := newPool(t, startup, 3)
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "2 ready", func() bool { return p.Status().Ready == 2 })
	for _, r := range p.Status().Replicas {
		if r.ColdStartSeconds != startup.Seconds() {
			t.Fatalf("cold start %v, want %v", r.ColdStartSeconds, startup.Seconds())
		}
	}
}

func TestSimulatedBackendRejectsInvalidParams(t *testing.T) {
	b := NewSimulatedBackend(testParams(0), 1)
	inst, err := b.Launch("ok", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if inst.State() != replica.StateStopped {
		t.Fatalf("closed simulated replica state %q, want stopped", inst.State())
	}
	bad := NewSimulatedBackend(testParams(0), 1)
	bad.(*simulatedBackend).params.MaxNumSeqs = 0
	if _, err := bad.Launch("bad", 0); err == nil {
		t.Fatal("Launch with invalid params succeeded")
	}
}
