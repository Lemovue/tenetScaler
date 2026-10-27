package sim

import (
	"math"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/fairq"
	"tenantscaler/internal/model"
	"tenantscaler/internal/workload"
)

// tenantSched returns tenant's scheduler state in sched.
func tenantSched(t *testing.T, sched *fairq.Scheduler, tenant string) fairq.TenantState {
	t.Helper()
	for _, st := range sched.Snapshot() {
		if st.Tenant == tenant {
			return st
		}
	}
	t.Fatalf("tenant %s unknown to the scheduler", tenant)
	return fairq.TenantState{}
}

// injectRequest appends a request the trace did not generate, so the
// dispatcher can find it by ID.
func injectRequest(s *simulator, sp workload.Spec) *request {
	req := &request{id: len(s.reqs), spec: sp, ts: s.byName[sp.Tenant]}
	s.reqs = append(s.reqs, req)
	return req
}

// In fair mode a tenant's counter follows the tokens a request has
// streamed, as the live gateway charges them chunk by chunk: the estimate
// at dispatch, then (in + 2*produced)/weight once produced exceeds it, at
// every point the scheduler reads its counters (Snapshot via sample, Admit
// via the lift), and (in + 2*out)/weight at completion.
func TestFairChargesStreamedTokens(t *testing.T) {
	o := Options{Setup: SetupFull, Config: config.Default(), Scenario: steadyScenario([]string{"A"}, 0.01, 30*time.Second), Seed: 1}
	s, err := newSimulator(o)
	if err != nil {
		t.Fatal(err)
	}
	const in, out = 100, 400
	req := injectRequest(s, workload.Spec{Tenant: "A", InputTokens: in, OutputTokens: out})
	s.admit(s.sched, req, false)
	if req.state != stRunning || len(s.charging) != 1 {
		t.Fatalf("request state %d, %d charging; want running and charged", req.state, len(s.charging))
	}
	w := tenantSched(t, s.sched, "A").Weight
	check := func(what string, produced int) {
		t.Helper()
		want := model.WeightedTokens(in, float64(produced)) / w
		if got := tenantSched(t, s.sched, "A").Counter; math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s: counter %v, want %v (%d tokens charged)", what, got, want, produced)
		}
	}
	est := int(min(out, fairq.DefaultOutputEstimate))
	check("at dispatch", est)

	s.now = req.firstToken + 250*req.interval + req.interval/2 // 251 tokens streamed
	if got := req.produced(s.now); got != 251 {
		t.Fatalf("produced = %d, want 251", got)
	}
	s.sample()
	check("partial decode, after a sample", 251)

	s.now = req.firstToken + 300*req.interval // 301 tokens streamed
	s.admit(s.sched, injectRequest(s, workload.Spec{Tenant: "B", InputTokens: 10, OutputTokens: 10}), false)
	check("partial decode, after another admission", 301)

	s.now = req.done
	s.onDone(req)
	check("completed", out)
	if len(s.charging) != 1 || s.charging[0] == req {
		t.Fatalf("charging list after completion: %d requests", len(s.charging))
	}
}

// Before a request runs it has produced nothing; while it runs one token
// per interval after the first; never more than its output length.
func TestProduced(t *testing.T) {
	req := &request{spec: workload.Spec{OutputTokens: 5}, state: stRunning, firstToken: 10 * time.Second, interval: time.Second}
	for _, c := range []struct {
		now  time.Duration
		want int
	}{{9 * time.Second, 0}, {10 * time.Second, 1}, {12500 * time.Millisecond, 3}, {14 * time.Second, 5}, {time.Hour, 5}} {
		if got := req.produced(c.now); got != c.want {
			t.Fatalf("produced(%v) = %d, want %d", c.now, got, c.want)
		}
	}
	req.state = stWaiting
	if got := req.produced(time.Hour); got != 0 {
		t.Fatalf("a waiting request produced %d tokens", got)
	}
}

// A request still queued at the gateway DispatchTimeout after it arrived
// is withdrawn from the scheduler and Failed (partition mode: tenant B
// owns no replica).
func TestDispatchTimeoutFairq(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.CapacityReplicas, cfg.Pool.MinReplicas = 1, 1
	slo := cfg.Tenants[0].SLO
	cfg.Tenants = []model.TenantSpec{
		{Name: "A", GuaranteedReplicas: 1, MaxReplicas: 1, SLO: slo},
		{Name: "B", GuaranteedReplicas: 0, MaxReplicas: 1, SLO: slo},
	}
	const timeout = 10 * time.Second
	o := Options{Setup: SetupStaticPartition, Config: cfg, Scenario: steadyScenario([]string{"A", "B"}, 0.5, 60*time.Second), Seed: 4, DispatchTimeout: timeout}
	s, err := newSimulator(o)
	if err != nil {
		t.Fatal(err)
	}
	s.run()
	var arrivals []time.Duration
	for _, req := range s.reqs {
		if req.ts.name != "B" {
			continue
		}
		arrivals = append(arrivals, req.spec.Offset)
		if req.state != stTimedOut || !req.record().Failed {
			t.Fatalf("B request %d ended in state %d, want timed out", req.id, req.state)
		}
	}
	if len(arrivals) == 0 || s.partSched["B"].Len() != 0 || s.byName["B"].gwQueued != 0 {
		t.Fatalf("%d B requests, %d still queued", len(arrivals), s.partSched["B"].Len())
	}
	// At every sample exactly the B requests of the last timeout wait.
	held := false
	for _, smp := range s.timeline {
		want := 0
		for _, a := range arrivals {
			if a <= smp.T && a > smp.T-timeout {
				want++
			}
		}
		if smp.Queued["B"] != want {
			t.Fatalf("t=%v: %d B requests queued, want %d", smp.T, smp.Queued["B"], want)
		}
		held = held || want > 0
	}
	if !held {
		t.Fatal("no B request ever waited")
	}
}

// Passthrough holds requests while no replica is ready; those held longer
// than DispatchTimeout fail, later ones are served once a replica is up.
func TestDispatchTimeoutPassthrough(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.MinReplicas = 0
	o := Options{Setup: SetupKEDAQueue, Config: cfg, Scenario: steadyScenario([]string{"A"}, 1, 120*time.Second), Seed: 2,
		ColdStart: 60 * time.Second, DispatchTimeout: 30 * time.Second}
	s, err := newSimulator(o)
	if err != nil {
		t.Fatal(err)
	}
	s.scaleTo(0)
	s.run()
	// KEDA scales up at the first tick (15 s); the replica is ready at
	// 75 s, so requests that arrived before 45 s time out first.
	cut := s.cycle() + o.ColdStart - o.DispatchTimeout
	failed, served := 0, 0
	for _, req := range s.reqs {
		a := req.spec.Offset
		switch {
		case a < cut-time.Second:
			if req.state != stTimedOut {
				t.Fatalf("request arriving at %v ended in state %d, want timed out", a, req.state)
			}
			failed++
		case a > cut+time.Second:
			if req.state != stDone {
				t.Fatalf("request arriving at %v ended in state %d, want done", a, req.state)
			}
			served++
		}
	}
	if failed == 0 || served == 0 || len(s.pending) != 0 {
		t.Fatalf("%d timed out, %d served, %d still held", failed, served, len(s.pending))
	}
}
