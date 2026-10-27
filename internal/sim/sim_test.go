package sim

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/fairq"
	"tenantscaler/internal/workload"
)

// shortScenario is the default scenario compressed to 240 s with the
// spike at 60-150 s, so tests stay fast.
func shortScenario() workload.Scenario {
	sc := workload.DefaultScenario()
	sc.Name, sc.Duration = "short", 240*time.Second
	for i := range sc.Tenants {
		p := &sc.Tenants[i]
		switch p.Kind {
		case workload.KindPeriodic:
			p.PeriodSeconds, p.BurstSeconds, p.PhaseSeconds = 60, 15, 30
		case workload.KindSpike:
			p.SpikeStartSeconds, p.SpikeDurationSeconds = 60, 90
		}
	}
	return sc
}

// everySetup is AllSetups followed by SensitivitySetups.
func everySetup() []string { return append(slices.Clone(AllSetups), SensitivitySetups...) }

func opts(setup string, sc workload.Scenario, seed int64) Options {
	return Options{Setup: setup, Config: config.Default(), Scenario: sc, Seed: seed, ColdStart: 30 * time.Second}
}

func mustSimulate(t *testing.T, o Options) Result {
	t.Helper()
	r, err := Simulate(o)
	if err != nil {
		t.Fatalf("Simulate(%v): %v", o, err)
	}
	return r
}

func TestDeterminism(t *testing.T) {
	for _, setup := range []string{SetupKEDAQueue, SetupFull, SetupFullForecast} {
		a := mustSimulate(t, opts(setup, shortScenario(), 7))
		b := mustSimulate(t, opts(setup, shortScenario(), 7))
		// %+v prints maps sorted and NaN as "NaN", so equal runs print equal.
		if fmt.Sprintf("%+v", a.Data) != fmt.Sprintf("%+v", b.Data) {
			t.Fatalf("%s: same options gave different RunData", setup)
		}
		c := mustSimulate(t, opts(setup, shortScenario(), 8))
		if fmt.Sprintf("%+v", a.Data.Records) == fmt.Sprintf("%+v", c.Data.Records) {
			t.Fatalf("%s: different seeds gave identical records", setup)
		}
	}
}

func TestEveryRequestRecordedExactlyOnce(t *testing.T) {
	sc := shortScenario()
	specs := workload.Generate(sc, 3)
	for _, setup := range everySetup() {
		d := mustSimulate(t, opts(setup, sc, 3)).Data
		if len(d.Records) != len(specs) {
			t.Fatalf("%s: %d records for %d requests", setup, len(d.Records), len(specs))
		}
		for i, r := range d.Records {
			sp := specs[i]
			if r.ID != uint64(i) || r.Tenant != sp.Tenant || r.Arrival != sp.Offset || r.OutputTokens != sp.OutputTokens {
				t.Fatalf("%s: record %d = %+v does not match spec %+v", setup, i, r, sp)
			}
			outcomes := 0
			for _, b := range []bool{r.Shed, r.Failed, r.Completed()} {
				if b {
					outcomes++
				}
			}
			if outcomes != 1 {
				t.Fatalf("%s: record %d has %d outcomes: %+v", setup, i, outcomes, r)
			}
			if r.Completed() && (r.FirstToken < r.Arrival || r.Done < r.FirstToken) {
				t.Fatalf("%s: record %d has inconsistent times: %+v", setup, i, r)
			}
		}
	}
}

// TestPoolInvariants checks the budget and the cold start on every setup:
// no more than capacity replicas hold a GPU, every new replica loads for
// exactly the cold start, and nothing runs on a replica before it is ready.
func TestPoolInvariants(t *testing.T) {
	for _, setup := range everySetup() {
		for _, cs := range []time.Duration{10 * time.Second, 60 * time.Second} {
			o := opts(setup, workload.DefaultScenario(), 1)
			o.ColdStart = cs
			s, err := newSimulator(o)
			if err != nil {
				t.Fatal(err)
			}
			s.run()
			capacity := o.Config.Pool.CapacityReplicas
			if s.diag.PeakPhysicalReplicas > capacity {
				t.Fatalf("%s: %d replicas held a GPU, capacity %d", setup, s.diag.PeakPhysicalReplicas, capacity)
			}
			for _, smp := range s.timeline {
				if smp.Ready+smp.Starting > capacity || smp.Desired > capacity {
					t.Fatalf("%s: sample %+v exceeds capacity %d", setup, smp, capacity)
				}
			}
			initial := s.st.InitialReplicas
			for _, r := range s.history[initial:] {
				if err := checkLoad(r, cs); err != nil {
					t.Fatalf("%s: %v", setup, err)
				}
			}
			for _, req := range s.reqs {
				if req.rep != nil && req.state == stDone && req.started < req.rep.readyAt {
					t.Fatalf("%s: request %d started at %v on replica %d ready at %v", setup, req.id, req.started, req.rep.id, req.rep.readyAt)
				}
			}
		}
	}
}

// checkLoad checks a replica created after t=0: if it ever began loading
// it loaded for exactly cs, and it served nothing before it was ready. A
// replica that never got a GPU slot (still pending, or removed while
// pending) never loaded: scale-ups happen at control ticks, so a loaded
// non-initial replica has readyAt > 0.
func checkLoad(r *replica, cs time.Duration) error {
	if r.readyAt == 0 {
		return nil
	}
	if r.readyAt-r.startedAt != cs {
		return fmt.Errorf("replica %d loaded for %v, want %v", r.id, r.readyAt-r.startedAt, cs)
	}
	if r.served > 0 && r.firstServe < r.readyAt {
		return fmt.Errorf("replica %d served at %v before ready at %v", r.id, r.firstServe, r.readyAt)
	}
	return nil
}

func TestStaticPartitionNeverScales(t *testing.T) {
	r := mustSimulate(t, opts(SetupStaticPartition, workload.DefaultScenario(), 1))
	capacity := config.Default().Pool.CapacityReplicas
	for _, smp := range r.Data.Timeline {
		if smp.Desired != capacity || smp.Ready != capacity || smp.Starting != 0 {
			t.Fatalf("static partition changed size: %+v", smp)
		}
	}
	if len(r.Data.ActionTimes) != 0 || r.Diagnostics.ControlCycles != 0 {
		t.Fatalf("static partition acted: actions %v, cycles %d", r.Data.ActionTimes, r.Diagnostics.ControlCycles)
	}
	if n := eval.Compute(r.Data).Shed; n != 0 {
		t.Fatalf("static partition shed %d requests", n)
	}
}

// TestS7ShedsNoisyTenantS2ShedsNothing: on a binding budget TenantScaler
// protects the victims by shedding the noisy tenant's sheddable traffic
// during the spike; on the default budget, where the pool is rarely
// contended, it sheds almost nothing (shedding is work-conserving). The
// passthrough KEDA baseline never sheds.
func TestS7ShedsNoisyTenantS2ShedsNothing(t *testing.T) {
	sc := workload.DefaultScenario()
	m := eval.Compute(mustSimulate(t, opts(SetupFull, sc, 1)).Data)
	if !(m.NoisyShedPct < 5) || m.VictimViolationPct != 0 {
		t.Fatalf("S7 on the default budget: noisy shed %.2f%%, victim violation %.2f%%; want < 5%% (41.5%% before shedding was work-conserving) and 0",
			m.NoisyShedPct, m.VictimViolationPct)
	}
	if !(m.ReactionTimeSec <= config.Default().Controller.CycleSeconds+0.1) {
		t.Fatalf("S7 reaction time %v s, want within one cycle", m.ReactionTimeSec)
	}
	o := opts(SetupFull, sc, 1)
	o.Config.Pool.CapacityReplicas = 6
	for i := range o.Config.Tenants {
		o.Config.Tenants[i].GuaranteedReplicas = 2
	}
	s7 := mustSimulate(t, o).Data
	// A victim's own sheddable requests may be shed over-share too (B's
	// periodic burst can exceed its guarantee while C holds the spare), so
	// require that the noisy tenant takes the bulk of it during the spike.
	noisy, victims := 0, 0
	for _, r := range s7.Records {
		if !r.Shed || r.ShedReason != fairq.ReasonOverShare || r.Arrival < s7.SpikeStart || r.Arrival >= s7.SpikeEnd {
			continue
		}
		if r.Tenant == s7.Noisy {
			noisy++
		} else {
			victims++
		}
	}
	if noisy == 0 || noisy < 10*victims {
		t.Fatalf("S7 over-share sheds during the spike: noisy %d, victims %d", noisy, victims)
	}
	if n := eval.Compute(mustSimulate(t, opts(SetupKEDAQueue, sc, 1)).Data).Shed; n != 0 {
		t.Fatalf("S2 shed %d requests, want 0", n)
	}
}

// TestCoreClaim is the project's core claim in simulation: in the default
// scenario the victims miss their SLO during the spike far less often
// under TenantScaler (S7) than under the queue-KEDA baseline (S2), and the
// baseline reproduces the problem (R11).
func TestCoreClaim(t *testing.T) {
	sc := workload.DefaultScenario()
	s2 := eval.Compute(mustSimulate(t, opts(SetupKEDAQueue, sc, 1)).Data)
	s7 := eval.Compute(mustSimulate(t, opts(SetupFull, sc, 1)).Data)
	if !(s2.VictimViolationPct > 10) {
		t.Fatalf("S2 victim violation %.2f%%: the baseline should reproduce the noisy-neighbour problem", s2.VictimViolationPct)
	}
	if !(s7.VictimViolationPct < s2.VictimViolationPct) || !(s7.VictimShedAwareViolationPct < s2.VictimShedAwareViolationPct) {
		t.Fatalf("victim violation S7 %.2f%% (shed-aware %.2f%%) not below S2 %.2f%% (%.2f%%)",
			s7.VictimViolationPct, s7.VictimShedAwareViolationPct, s2.VictimViolationPct, s2.VictimShedAwareViolationPct)
	}
}

func TestPolicyLatencyAndCounterGap(t *testing.T) {
	o := opts(SetupFull, shortScenario(), 2)
	o.PolicyDelay = 35 * time.Millisecond
	r := mustSimulate(t, o)
	if len(r.Data.PolicyLatencies) != r.Diagnostics.ControlCycles {
		t.Fatalf("%d policy latencies for %d cycles", len(r.Data.PolicyLatencies), r.Diagnostics.ControlCycles)
	}
	for _, l := range r.Data.PolicyLatencies {
		if l != o.PolicyDelay {
			t.Fatalf("policy latency %v, want %v", l, o.PolicyDelay)
		}
	}
	s2 := mustSimulate(t, opts(SetupKEDAQueue, shortScenario(), 2)).Data
	if len(s2.PolicyLatencies) != 0 {
		t.Fatalf("S2 has policy latencies %v", s2.PolicyLatencies)
	}
	for _, smp := range s2.Timeline {
		if !math.IsNaN(smp.CounterGap) {
			t.Fatalf("S2 counter gap %v, want NaN outside fair mode", smp.CounterGap)
		}
	}
}

func TestCounterGap(t *testing.T) {
	states := []fairq.TenantState{{Tenant: "A", Queued: 2, Counter: 10}, {Tenant: "B", Counter: 500}, {Tenant: "C", Queued: 1, Counter: 40}}
	if g := counterGap(states); g != 30 {
		t.Fatalf("gap = %v, want 30 (B is not backlogged)", g)
	}
	if g := counterGap(states[1:2]); !math.IsNaN(g) {
		t.Fatalf("gap with no backlog = %v, want NaN", g)
	}
}

// TestFlowControlMetricBlindSpot documents why S4b exists: S4's KEDA reads
// only the roundrobin gateway's queue, which stays empty while the
// dispatch window is full (sheddable overflow is shed as "saturated"), so
// once the initial scale-down window expires S4 scales down in the middle
// of the spike. S4b, KEDA on outstanding requests, never does.
func TestFlowControlMetricBlindSpot(t *testing.T) {
	sc := workload.DefaultScenario()
	s4 := mustSimulate(t, opts(SetupFlowControl, sc, 1)).Data
	s4b := mustSimulate(t, opts(SetupFlowControlOutstanding, sc, 1)).Data
	desired := func(d eval.RunData) (start, low int) {
		start, low = -1, math.MaxInt
		for _, smp := range d.Timeline {
			if smp.T < d.SpikeStart || smp.T >= d.SpikeEnd {
				continue
			}
			if start < 0 {
				start = smp.Desired
			}
			low = min(low, smp.Desired)
		}
		return start, low
	}
	if start, low := desired(s4); !(low < start) {
		t.Fatalf("S4 desired replicas during the spike: start %d, low %d; expected the queue-only metric to scale down", start, low)
	}
	if start, low := desired(s4b); low < start {
		t.Fatalf("S4b scaled down during the spike: start %d, low %d", start, low)
	}
	if v4, v4b := eval.Compute(s4).VictimViolationPct, eval.Compute(s4b).VictimViolationPct; !(v4b < v4) {
		t.Fatalf("victim violation S4b %.2f%% not below S4 %.2f%%", v4b, v4)
	}
}

// The reclaim scenario: the returning owner and its return time reach
// RunData, and the reclaim latency is defined.
func TestReclaimScenario(t *testing.T) {
	sc := workload.ReclaimScenario()
	for _, setup := range []string{SetupKEDAQueue, SetupFull} {
		d := mustSimulate(t, opts(setup, sc, 1)).Data
		if d.Owner != workload.ReclaimOwner || d.OwnerReturn != workload.ReclaimReturn() {
			t.Fatalf("%s: owner %q returning at %v, want %q at %v", setup, d.Owner, d.OwnerReturn, workload.ReclaimOwner, workload.ReclaimReturn())
		}
		if l := eval.Compute(d).ReclaimLatencySec; math.IsNaN(l) || l < 0 || l > eval.ReclaimHorizon.Seconds() {
			t.Fatalf("%s: reclaim latency %v, want a value in [0, horizon]", setup, l)
		}
	}
}

// spikeStats returns, over C's spike, the mean desired replicas and the
// share of the noisy tenant's requests that were shed or failed.
func spikeStats(d eval.RunData) (desired, lostPct float64) {
	n := 0
	for _, smp := range d.Timeline {
		if smp.T >= d.SpikeStart && smp.T < d.SpikeEnd {
			desired += float64(smp.Desired)
			n++
		}
	}
	lost, total := 0, 0
	for _, r := range d.Records {
		if r.Tenant != d.Noisy || r.Arrival < d.SpikeStart || r.Arrival >= d.SpikeEnd {
			continue
		}
		total++
		if r.Shed || r.Failed {
			lost++
		}
	}
	return desired / float64(n), 100 * float64(lost) / float64(total)
}

// The death spiral predicted for naive demand (BRIEF-v2 Q11) does NOT occur
// in the full system, and this test records what happens instead
// (PLAN.md decision Q11-R): it was first written to require that S8 run
// fewer replicas than S7 during C's spike and lose more of C (commit
// c3d68df), and S8 matched S7 exactly. TenantScaler's shed decision comes
// from the same demand estimate (demand > grant), so a lower admitted-only
// reading switches shedding off and the reading recovers next cycle; and
// shedding removes only the excess while the pool is contended, so the
// admitted load stays at or above the budget-capped grant. The spiral needs
// shedding that does not depend on the scaling signal, as in S4
// (TestFlowControlMetricBlindSpot); TestNaiveDemandSpirals shows the
// mechanism in the control core with a gateway that sheds more.
func TestNaiveDemandDoesNotSpiralInSimulation(t *testing.T) {
	sc := workload.DefaultScenario()
	run := func(setup string) (float64, float64) {
		o := opts(setup, sc, 1)
		o.Config.Pool.CapacityReplicas = 6
		for i := range o.Config.Tenants {
			o.Config.Tenants[i].GuaranteedReplicas = 2
		}
		return spikeStats(mustSimulate(t, o).Data)
	}
	d7, lost7 := run(SetupFull)
	d8, lost8 := run(SetupNaiveDemand)
	if math.Abs(d8-d7) > 0.25 || math.Abs(lost8-lost7) > 5 {
		t.Fatalf("spike: S7 %.2f desired replicas, %.1f%% of C lost; S8 %.2f, %.1f%%; S8 departed from S7: re-examine Q11-R",
			d7, lost7, d8, lost8)
	}
	t.Logf("spike: S7 %.2f desired replicas, %.1f%% of C lost; S8 %.2f, %.1f%%", d7, lost7, d8, lost8)
}
