package sim

import (
	"slices"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/model"
	"tenantscaler/internal/workload"
)

func steadyScenario(tenants []string, rps float64, d time.Duration) workload.Scenario {
	base := workload.DefaultScenario().Tenants[0]
	sc := workload.Scenario{Name: "steady", Duration: d}
	for _, name := range tenants {
		p := base
		p.Tenant, p.BaseRPS, p.PeakRPS = name, rps, rps
		sc.Tenants = append(sc.Tenants, p)
	}
	return sc
}

// A passthrough gateway with no ready replica holds requests; KEDA counts
// them, scales up from zero, and they run once the cold start is over.
func TestPassthroughHoldsUntilReplicaReady(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.MinReplicas = 0
	o := Options{Setup: SetupKEDAQueue, Config: cfg, Scenario: steadyScenario([]string{"A"}, 1, 60*time.Second), Seed: 1, ColdStart: 20 * time.Second}
	s, err := newSimulator(o)
	if err != nil {
		t.Fatal(err)
	}
	s.scaleTo(0) // every warm replica is idle, so all go at once
	if len(s.reps) != 0 {
		t.Fatalf("%d replicas left after scaling to 0", len(s.reps))
	}
	s.run()
	firstReady := s.cycle() + o.ColdStart
	completed := 0
	for _, req := range s.reqs {
		if req.state != stDone {
			t.Fatalf("request %d ended in state %d", req.id, req.state)
		}
		if req.started < firstReady {
			t.Fatalf("request %d started at %v, before the first replica was ready at %v", req.id, req.started, firstReady)
		}
		completed++
	}
	if completed == 0 {
		t.Fatal("no requests generated")
	}
}

// A busy replica removed by scale-down drains; a replica added meanwhile
// waits (pending) for its GPU slot and starts when the drain finishes.
func TestDrainThenPendingStart(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.CapacityReplicas, cfg.Pool.MinReplicas = 3, 1
	for i := range cfg.Tenants {
		cfg.Tenants[i].GuaranteedReplicas, cfg.Tenants[i].MaxReplicas = 1, 3
	}
	o := Options{Setup: SetupKEDAQueue, Config: cfg, Scenario: steadyScenario([]string{"A"}, 0.01, 10*time.Second), Seed: 1, ColdStart: 5 * time.Second, InitialReplicas: 3}
	s, err := newSimulator(o)
	if err != nil {
		t.Fatal(err)
	}
	var busy *request
	for i, r := range s.reps { // every replica busy, so scale-down must drain
		busy = &request{id: 1000 + i, spec: workload.Spec{Tenant: "A", InputTokens: 10, OutputTokens: 100}, ts: s.byName["A"]}
		s.accept(r, busy)
	}
	s.scaleTo(2)
	if s.reps[2].state != repDraining {
		t.Fatalf("busy newest replica state %d, want draining", s.reps[2].state)
	}
	s.scaleTo(3)
	added := s.history[len(s.history)-1]
	if added.state != repPending || s.physical() != 3 {
		t.Fatalf("new replica state %d with %d physical, want pending at capacity", added.state, s.physical())
	}
	s.now = busy.done
	s.onDone(busy)
	if added.state != repStarting || added.startedAt != busy.done || added.readyAt != busy.done+o.ColdStart {
		t.Fatalf("pending replica after drain: %+v", added)
	}
	if s.physical() > cfg.Pool.CapacityReplicas {
		t.Fatalf("%d physical replicas exceed capacity", s.physical())
	}
}

// A replica removed while still pending never loads, holds no GPU slot
// and passes the cold-start invariant of TestPoolInvariants.
func TestRemovePendingReplica(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.CapacityReplicas, cfg.Pool.MinReplicas = 3, 1
	for i := range cfg.Tenants {
		cfg.Tenants[i].GuaranteedReplicas, cfg.Tenants[i].MaxReplicas = 1, 3
	}
	o := Options{Setup: SetupKEDAQueue, Config: cfg, Scenario: steadyScenario([]string{"A"}, 0.01, 10*time.Second), Seed: 1, ColdStart: 5 * time.Second, InitialReplicas: 3}
	s, err := newSimulator(o)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range s.reps {
		s.accept(r, &request{id: 1000 + i, spec: workload.Spec{Tenant: "A", InputTokens: 10, OutputTokens: 100}, ts: s.byName["A"]})
	}
	s.now = time.Second
	s.scaleTo(2) // the newest busy replica drains
	s.scaleTo(3) // no free GPU slot: the new replica waits, pending
	added := s.history[len(s.history)-1]
	s.scaleTo(2) // pending replicas go first
	if added.state != repGone || slices.Contains(s.reps, added) || s.physical() != 3 {
		t.Fatalf("removed pending replica: state %d, %d physical", added.state, s.physical())
	}
	if err := checkLoad(added, o.ColdStart); err != nil {
		t.Fatalf("a replica that never loaded failed the load check: %v", err)
	}
	bad := &replica{id: 99, startedAt: time.Second, readyAt: 3 * time.Second}
	if checkLoad(bad, o.ColdStart) == nil {
		t.Fatal("a replica that loaded for 2 s passed a 5 s cold-start check")
	}
}

// Requests that never get a replica are Failed.
func TestUnservedRequestsFail(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.CapacityReplicas, cfg.Pool.MinReplicas = 1, 1
	slo := cfg.Tenants[0].SLO
	cfg.Tenants = []model.TenantSpec{
		{Name: "A", GuaranteedReplicas: 1, MaxReplicas: 1, SLO: slo},
		{Name: "B", GuaranteedReplicas: 0, MaxReplicas: 1, SLO: slo},
	}
	o := Options{Setup: SetupStaticPartition, Config: cfg, Scenario: steadyScenario([]string{"A", "B"}, 0.2, 60*time.Second), Seed: 4}
	d, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range d.Records {
		switch {
		case r.Tenant == "B" && (!r.Failed || r.FirstToken != 0 || r.Done != 0):
			t.Fatalf("B owns no replica, so its request should fail: %+v", r)
		case r.Tenant == "A" && !r.Completed():
			t.Fatalf("A's request did not complete: %+v", r)
		}
	}
}

func TestResolve(t *testing.T) {
	base := Options{Setup: SetupFull, Config: config.Default(), Scenario: workload.DefaultScenario()}
	st, err := Resolve(base)
	if err != nil {
		t.Fatal(err)
	}
	if st.InitialReplicas != DefaultInitialReplicas || st.SampleEvery != time.Second || st.PolicyDelay != DefaultPolicyDelay ||
		st.DispatchTimeout != 120*time.Second {
		t.Fatalf("defaults not applied: %+v", st)
	}
	if st.ColdStart != 30*time.Second || st.SlotsPerReplica != 8 || st.Gateway != GatewayFair {
		t.Fatalf("cold start %v slots %d gateway %s", st.ColdStart, st.SlotsPerReplica, st.Gateway)
	}
	if st.Controller.Forecast.Enabled || st.Controller.DisableBorrowing || st.Controller.DisableGatewayCoupling || st.Controller.DisableSlotReservation {
		t.Fatalf("S7 controller switches: %+v", st.Controller)
	}

	cases := []struct {
		name  string
		edit  func(*Options)
		check func(Settings) bool
	}{
		{"S3 at capacity", func(o *Options) { o.Setup = SetupStaticPartition }, func(s Settings) bool { return s.InitialReplicas == 10 }},
		{"initial clamped", func(o *Options) { o.InitialReplicas = 50 }, func(s Settings) bool { return s.InitialReplicas == 10 }},
		{"S5 uncoupled", func(o *Options) { o.Setup = SetupNoGateway }, func(s Settings) bool { return s.Controller.DisableGatewayCoupling }},
		{"S6 no borrowing", func(o *Options) { o.Setup = SetupNoBorrowing }, func(s Settings) bool { return s.Controller.DisableBorrowing }},
		{"S4b roundrobin", func(o *Options) { o.Setup = SetupFlowControlOutstanding }, func(s Settings) bool { return s.Gateway == GatewayRoundRobin }},
		{"S7nr no reservation", func(o *Options) { o.Setup = SetupNoReservation }, func(s Settings) bool {
			return s.Gateway == GatewayFair && s.Controller.DisableSlotReservation && !s.Controller.DisableBorrowing
		}},
		{"dispatch timeout", func(o *Options) { o.DispatchTimeout = 7 * time.Second }, func(s Settings) bool { return s.DispatchTimeout == 7*time.Second }},
		{"S7F forecast", func(o *Options) { o.Setup = SetupFullForecast }, func(s Settings) bool {
			f := s.Controller.Forecast
			return f.Enabled && f.BucketSeconds == 15 && f.SeasonBuckets == 8 && f.Alpha == 0.5 && f.Beta == 0.1 && f.Gamma == 0.3
		}},
	}
	for _, c := range cases {
		o := base
		c.edit(&o)
		st, err := Resolve(o)
		if err != nil || !c.check(st) {
			t.Fatalf("%s: settings %+v, err %v", c.name, st, err)
		}
	}

	bad := []struct {
		name string
		edit func(*Options)
		want string
	}{
		{"unknown setup", func(o *Options) { o.Setup = "S9" }, "unknown setup"},
		{"unknown tenant", func(o *Options) { o.Scenario.Tenants[0].Tenant = "Z" }, "not in the config"},
		{"negative cold start", func(o *Options) { o.ColdStart = -time.Second }, "cold start"},
		{"negative dispatch timeout", func(o *Options) { o.DispatchTimeout = -time.Second }, "dispatch timeout"},
		{"invalid config", func(o *Options) { o.Config.Pool.CapacityReplicas = 0 }, "config"},
		{"invalid scenario", func(o *Options) { o.Scenario.Duration = 0 }, "duration"},
	}
	for _, c := range bad {
		o := base
		o.Scenario = workload.DefaultScenario()
		c.edit(&o)
		if _, err := Resolve(o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestScenarioRoles(t *testing.T) {
	r := ScenarioRoles(workload.DefaultScenario())
	if r.Noisy != "C" || strings.Join(r.Victims, ",") != "A,B" || r.SpikeStart != 180*time.Second || r.SpikeEnd != 360*time.Second || r.Owner != "" {
		t.Fatalf("default roles = %+v", r)
	}
	r = ScenarioRoles(workload.ReclaimScenario())
	if r.Noisy != "C" || r.Owner != workload.ReclaimOwner || r.OwnerReturn != workload.ReclaimReturn() || strings.Join(r.Victims, ",") != "A,B" {
		t.Fatalf("reclaim roles = %+v", r)
	}
	if r := ScenarioRoles(steadyScenario([]string{"A"}, 1, time.Minute)); r.Noisy != "" || r.SpikeEnd != 0 {
		t.Fatalf("steady roles = %+v", r)
	}
}
