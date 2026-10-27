package sim

import (
	"math"
	"testing"
	"time"

	"tenantscaler/internal/autoscale"
	"tenantscaler/internal/config"
	"tenantscaler/internal/fairq"
	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
	"tenantscaler/internal/workload"
)

// newOneReplica builds a simulator of setup with one warm replica. Tests
// drive it by hand (the event loop never runs), so only the requests they
// inject arrive.
func newOneReplica(t *testing.T, setup string) *simulator {
	t.Helper()
	o := Options{Setup: setup, Config: config.Default(), Scenario: steadyScenario([]string{"A"}, 0.01, time.Minute), Seed: 1, InitialReplicas: 1}
	s, err := newSimulator(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.reps) != 1 {
		t.Fatalf("%d warm replicas, want 1", len(s.reps))
	}
	return s
}

// arriveN sends n identical requests of tenant at s.now through the
// gateway.
func arriveN(s *simulator, n int, sp workload.Spec) []*request {
	var out []*request
	for range n {
		sp.Offset = s.now
		req := injectRequest(s, sp)
		s.arrive(req)
		out = append(out, req)
	}
	return out
}

// observe must build each tenant's observation like the live gateway's
// /stats over the window since the last tick: every arrival (shed ones
// included) as requests/s and weighted tokens (in + 2 x max out)/s, the
// current gateway queue as QueueLength, dispatched unfinished requests
// (running or in a replica queue) as InFlight, and the window's sheds,
// completions, attainment and p99 TTFT.
func TestObserveFair(t *testing.T) {
	s := newOneReplica(t, SetupFull)
	s.now = time.Second
	a := arriveN(s, 9, workload.Spec{Tenant: "A", InputTokens: 100, OutputTokens: 20})
	p := policy.Equal(s.cfg.TenantNames())
	p.Version = 1
	p.Tenants["B"] = policy.TenantPolicy{Weight: 1, Shed: true, RetryAfterSeconds: 1}
	if !s.sched.SetPolicy(p) {
		t.Fatal("policy rejected")
	}
	arriveN(s, 2, workload.Spec{Tenant: "B", Priority: model.PrioritySheddable, InputTokens: 50, OutputTokens: 10})
	s.now = a[0].done
	s.onDone(a[0]) // frees a window slot: the queued A request is dispatched
	s.now = max(s.now, 4*time.Second)
	win := s.now.Seconds()

	rec := a[0].record()
	slo := s.byName["A"].slo
	if !rec.MetSLO(slo) {
		t.Fatalf("the completed request missed its SLO (TTFT %v, TPOT %v): pick lighter requests", rec.TTFT(), rec.TPOT())
	}
	want := map[string]model.TenantObservation{
		"A": {Tenant: "A", ArrivalWeightedTokensPerSec: 9 * model.WeightedTokens(100, 20) / win,
			AdmittedWeightedTokensPerSec: 9 * model.WeightedTokens(100, 20) / win, ArrivalRequestsPerSec: 9 / win,
			QueueLength: 0, InFlight: 8, SLOAttainment: 1, HasSLOData: true, P99TTFTSeconds: rec.TTFT().Seconds(), Completed: 1},
		// Both of B's requests were shed: offered, but nothing admitted.
		"B": {Tenant: "B", ArrivalWeightedTokensPerSec: 2 * model.WeightedTokens(50, 10) / win, ArrivalRequestsPerSec: 2 / win, Shed: 2},
		"C": {Tenant: "C"},
	}
	checkObservation(t, s, want, model.PoolObservation{ReadyReplicas: 1, DesiredReplicas: 1})

	// One more A arrival fills the window: it waits at the gateway.
	arriveN(s, 1, workload.Spec{Tenant: "A", InputTokens: 100, OutputTokens: 20})
	if o := s.observe().Tenants[0]; o.QueueLength != 1 || o.InFlight != 8 {
		t.Fatalf("A queue %d in flight %d, want 1 and 8", o.QueueLength, o.InFlight)
	}
	s.resetWindow()
	if o := s.observe().Tenants[0]; o.ArrivalRequestsPerSec != 0 || o.Completed != 0 || o.HasSLOData || o.QueueLength != 1 {
		t.Fatalf("after a tick the window restarts but the live counts stay: %+v", o)
	}
}

// In passthrough mode InFlight includes requests waiting in a replica's
// own queue, QueueLength counts requests held at the gateway while no
// replica is ready, and pending/starting replicas count as starting.
func TestObservePassthrough(t *testing.T) {
	s := newOneReplica(t, SetupNoGateway)
	s.now = time.Second
	arriveN(s, 10, workload.Spec{Tenant: "A", InputTokens: 100, OutputTokens: 20})
	s.scaleTo(0) // the busy replica drains: nothing is ready
	s.scaleTo(1) // a new replica starts loading
	arriveN(s, 3, workload.Spec{Tenant: "B", InputTokens: 50, OutputTokens: 10})
	s.now = 2 * time.Second
	want := map[string]model.TenantObservation{
		"A": {Tenant: "A", ArrivalWeightedTokensPerSec: 10 * model.WeightedTokens(100, 20) / 2,
			AdmittedWeightedTokensPerSec: 10 * model.WeightedTokens(100, 20) / 2, ArrivalRequestsPerSec: 5, InFlight: 10},
		"B": {Tenant: "B", ArrivalWeightedTokensPerSec: 3 * model.WeightedTokens(50, 10) / 2,
			AdmittedWeightedTokensPerSec: 3 * model.WeightedTokens(50, 10) / 2, ArrivalRequestsPerSec: 1.5, QueueLength: 3},
		"C": {Tenant: "C"},
	}
	checkObservation(t, s, want, model.PoolObservation{ReadyReplicas: 0, StartingReplicas: 1, DesiredReplicas: 1})
}

// checkObservation compares s.observe() with the expected tenant and pool
// observations.
func checkObservation(t *testing.T, s *simulator, want map[string]model.TenantObservation, pool model.PoolObservation) {
	t.Helper()
	obs := s.observe()
	if !obs.OK || !obs.Time.Equal(s.clock()) || obs.Pool != pool {
		t.Fatalf("observation ok %v at %v pool %+v, want ok at %v and %+v", obs.OK, obs.Time, obs.Pool, s.clock(), pool)
	}
	if len(obs.Tenants) != len(want) {
		t.Fatalf("%d tenant observations, want %d", len(obs.Tenants), len(want))
	}
	for _, got := range obs.Tenants {
		w := want[got.Tenant]
		if math.Abs(got.ArrivalWeightedTokensPerSec-w.ArrivalWeightedTokensPerSec) > 1e-9 {
			t.Fatalf("%s weighted arrival rate %v, want %v", got.Tenant, got.ArrivalWeightedTokensPerSec, w.ArrivalWeightedTokensPerSec)
		}
		got.ArrivalWeightedTokensPerSec = w.ArrivalWeightedTokensPerSec
		if got != w {
			t.Fatalf("%s observation\n got %+v\nwant %+v", got.Tenant, got, w)
		}
	}
}

// Each baseline scaler receives its specified metric: S1 the mean slot
// occupancy of ready replicas since the last tick, S2 every queued request
// (gateway hold + replica queues), S4 the gateway queue only, S4b the
// gateway queue plus dispatched requests. Each case feeds a fresh scaler
// of the same configuration the expected value and compares the result;
// the values are chosen so that a wrong input changes the outcome.
func TestScalerInputs(t *testing.T) {
	cases := []struct {
		setup   string
		prepare func(s *simulator)
		value   float64
	}{
		{SetupCPUHPA, func(s *simulator) {
			for i, r := range s.reps {
				if i < 5 {
					r.running = s.params.MaxNumSeqs
				}
			}
			drain := s.newReplica()
			drain.state = repDraining // excluded from occupancy
			s.sample()
		}, 5.0 / 6},
		{SetupKEDAQueue, func(s *simulator) {
			s.byName["A"].gwQueued, s.byName["B"].waiting, s.byName["C"].running = 4, 30, 40
		}, 34},
		{SetupFlowControl, func(s *simulator) {
			fillGateway(s, 36)
			s.byName["A"].waiting, s.byName["B"].running = 7, 40
		}, 36},
		{SetupFlowControlOutstanding, func(s *simulator) {
			fillGateway(s, 12)
			s.byName["A"].running, s.byName["B"].waiting = 20, 4
		}, 36},
	}
	for _, c := range cases {
		o := Options{Setup: c.setup, Config: config.Default(), Scenario: steadyScenario([]string{"A"}, 0.0001, time.Hour), Seed: 1}
		s, err := newSimulator(o)
		if err != nil {
			t.Fatal(err)
		}
		c.prepare(s)
		initial := s.target
		s.now = s.cycle()
		var want int
		switch c.setup {
		case SetupCPUHPA:
			want = autoscale.NewHPA(s.st.HPA, initial).Step(s.clock(), initial, c.value)
		default:
			want = autoscale.NewKEDA(s.st.KEDA, initial).Step(s.clock(), initial, c.value)
		}
		if want == initial {
			t.Fatalf("%s: value %v does not move the scaler; the case cannot tell inputs apart", c.setup, c.value)
		}
		s.tick()
		if s.target != want {
			t.Fatalf("%s: scaled to %d, want %d (the scaler's answer for %v)", c.setup, s.target, want, c.value)
		}
	}
}

// fillGateway queues n requests in the gateway scheduler with every ready
// replica's dispatch window full, so they stay queued.
func fillGateway(s *simulator, n int) {
	for _, r := range s.reps {
		r.assigned = s.st.SlotsPerReplica
	}
	for i := range n {
		fq := &fairq.Request{ID: uint64(1_000_000 + i), Tenant: "A", Arrival: s.clock()}
		s.sched.Admit(fq, s.clock(), false)
	}
}
