package fairq

import (
	"slices"
	"testing"
	"time"

	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
)

func arrivingAt(d time.Duration) func(*Request) {
	return func(r *Request) { r.Arrival = t0.Add(d) }
}

// Strict bands (critical > standard > sheddable), one request per tenant per
// turn in name order regardless of request size, FIFO inside a tenant; the
// policy's weights, shedding and critical quota are ignored.
func TestRoundRobinBandsAndRotation(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeRoundRobin})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{
		"A": {Weight: 1, Shed: true, CriticalSlots: 1},
		"B": {Weight: 3},
	})
	big := withTokens(10000, 4000)
	h.add("A", model.PrioritySheddable) // 1: first to arrive, lowest band
	h.add("A", model.PriorityStandard, big)
	h.add("A", model.PriorityStandard, big)
	h.add("A", model.PriorityStandard, big)
	h.add("B", model.PriorityStandard)  // 5
	h.add("C", model.PriorityStandard)  // 6
	h.add("C", model.PriorityStandard)  // 7
	h.add("B", model.PrioritySheddable) // 8
	h.add("C", model.PriorityCritical)  // 9
	h.add("A", model.PriorityCritical)  // 10
	k := h.add("A", model.PriorityCritical)
	if k.EffectivePriority != model.PriorityCritical {
		t.Fatal("roundrobin applied the critical quota")
	}
	got := ids(drain(h))
	want := []uint64{
		10, 9, 11, // critical: A, C, A
		2, 5, 6, 3, 7, 4, // standard: A, B, C, A, C, A
		1, 8, // sheddable: A, B
	}
	if !slices.Equal(got, want) {
		t.Fatalf("dispatch order = %v, want %v", got, want)
	}
	for _, ts := range h.s.Snapshot() {
		if ts.Counter != 0 {
			t.Fatalf("tenant %s counter = %v, want 0 (no VTC in roundrobin)", ts.Tenant, ts.Counter)
		}
	}
}

// The rotation resumes after the tenant served last, also across Next calls
// interleaved with new arrivals.
func TestRoundRobinRotationResumes(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeRoundRobin})
	h.add("A", model.PriorityStandard) // 1
	h.add("B", model.PriorityStandard) // 2
	if r := h.next(); r.ID != 1 {
		t.Fatalf("first dispatch %d, want 1", r.ID)
	}
	h.add("A", model.PriorityStandard) // 3
	h.add("C", model.PriorityStandard) // 4
	if got := ids(drain(h)); !slices.Equal(got, []uint64{2, 4, 3}) {
		t.Fatalf("dispatch order = %v, want [2 4 3] (B, C, then back to A)", got)
	}
}

func TestRoundRobinSaturatedShedsOnlySheddable(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeRoundRobin, Seed: 5})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 1, Shed: true, RetryAfterSeconds: 4}})
	cases := []struct {
		p         model.Priority
		saturated bool
		deadline  time.Duration
		reason    string
	}{
		{model.PrioritySheddable, true, 0, ReasonSaturated},
		{model.PriorityStandard, true, 0, ""},
		{model.PriorityCritical, true, 0, ""},
		// Not saturated: policy Shed and an unreachable deadline are ignored.
		{model.PrioritySheddable, false, -time.Second, ""},
	}
	for _, tc := range cases {
		r := h.req("A", tc.p)
		if tc.deadline != 0 {
			r.Deadline = h.now.Add(tc.deadline)
		}
		v := h.s.Admit(r, h.now, tc.saturated)
		if v.Admitted != (tc.reason == "") || v.Reason != tc.reason {
			t.Errorf("%s saturated=%v: verdict %+v, want reason %q", tc.p.Class(), tc.saturated, v, tc.reason)
		}
		if !v.Admitted && (v.RetryAfter < 4*time.Second || v.RetryAfter >= 6*time.Second) {
			t.Errorf("RetryAfter = %v, want in [4s, 6s)", v.RetryAfter)
		}
	}
	if st := state(t, h.s, "A"); st.Rejected[ReasonSaturated] != 1 || st.Admitted != 3 {
		t.Fatalf("A state = %+v, want 1 saturated rejection and 3 admitted", st)
	}
}

// FCFS serves one global queue by (Arrival, ID), ignoring tenant, priority,
// policy and saturation.
func TestFCFSOrder(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFCFS})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 5, Shed: true}})
	h.add("A", model.PriorityStandard, arrivingAt(2*time.Second))                          // 1
	h.add("A", model.PrioritySheddable, arrivingAt(time.Second), withDeadline(-time.Hour)) // 2
	h.add("B", model.PriorityCritical, arrivingAt(time.Second))                            // 3
	h.add("C", model.PriorityStandard, arrivingAt(0))                                      // 4
	if v := h.s.Admit(h.req("A", model.PrioritySheddable, arrivingAt(3*time.Second)), h.now, true); !v.Admitted {
		t.Fatalf("fcfs rejected a sheddable request while saturated: %+v", v)
	}
	if got := ids(drain(h)); !slices.Equal(got, []uint64{4, 2, 3, 1, 5}) {
		t.Fatalf("dispatch order = %v, want [4 2 3 1 5]", got)
	}
}
