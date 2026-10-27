package demo

import (
	"math"
	"testing"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/model"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

func TestScenarioByName(t *testing.T) {
	for _, name := range []string{ScenarioRealDemo, ScenarioTiny} {
		sc, err := ScenarioByName(name)
		if err != nil || sc.Name != name {
			t.Fatalf("%s: %v %q", name, err, sc.Name)
		}
		if err := sc.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := ScenarioByName("huge"); err == nil {
		t.Fatal("unknown scenario accepted")
	}
}

// TestTinyScenario checks the tiny trace keeps the real-demo's shape: a
// few seconds, C spiking strictly inside the run, a few dozen requests.
func TestTinyScenario(t *testing.T) {
	sc := TinyScenario()
	roles := sim.ScenarioRoles(sc)
	if sc.Duration != 6*time.Second || roles.Noisy != "C" || len(roles.Victims) != 2 {
		t.Fatalf("duration %v roles %+v", sc.Duration, roles)
	}
	if roles.SpikeStart <= 0 || roles.SpikeEnd >= sc.Duration || roles.SpikeEnd <= roles.SpikeStart {
		t.Fatalf("spike window [%v, %v) not inside the %v run", roles.SpikeStart, roles.SpikeEnd, sc.Duration)
	}
	for seed := range int64(5) {
		if n := len(workload.Generate(sc, seed)); n < 15 || n > 120 {
			t.Errorf("seed %d: %d requests, want a few dozen", seed, n)
		}
	}
	if rd := workload.RealDemoScenario().Tenants; rd[0].BaseRPS*tinyRateFactor != sc.Tenants[0].BaseRPS {
		t.Errorf("tiny A rate %v, want %d x the real-demo's %v", sc.Tenants[0].BaseRPS, tinyRateFactor, rd[0].BaseRPS)
	}
}

func TestLiveSample(t *testing.T) {
	smp := eval.Sample{T: 2500 * time.Millisecond, Ready: 2, Starting: 1, Desired: 3,
		Queued: map[string]int{"A": 1}, InFlight: map[string]int{"A": 4, "B": 2}, CounterGap: math.NaN()}
	gs := &gateway.Stats{Tenants: []model.TenantObservation{
		{Tenant: "A", ArrivalRequestsPerSec: 0.5, HasSLOData: true, SLOAttainment: 0.75, P99TTFTSeconds: 1.2, Completed: 4, Shed: 1},
		{Tenant: "B", ArrivalRequestsPerSec: 0.2},
	}}
	ls := liveSample(smp, gs)
	if ls.T != 2.5 || ls.Ready != 2 || ls.Starting != 1 || ls.Desired != 3 || len(ls.Tenants) != 2 {
		t.Fatalf("sample %+v", ls)
	}
	a, b := ls.Tenants["A"], ls.Tenants["B"]
	if a.P99TTFTSeconds == nil || *a.P99TTFTSeconds != 1.2 || a.Attainment == nil || *a.Attainment != 0.75 ||
		a.RPS != 0.5 || a.Shed != 1 || a.Completed != 4 || a.Queued != 1 || a.InFlight != 4 {
		t.Errorf("tenant A %+v", a)
	}
	if b.P99TTFTSeconds != nil || b.Attainment != nil || b.InFlight != 2 {
		t.Errorf("tenant B without completions %+v: want nil p99 and attainment", b)
	}
	if none := liveSample(smp, nil); none.Tenants != nil || none.Ready != 2 {
		t.Errorf("sample without gateway stats %+v", none)
	}
}

func TestTenantResults(t *testing.T) {
	slo := model.SLO{TTFTSeconds: 1, TPOTMillis: 100, TargetFraction: 0.99}
	slos := map[string]model.SLO{"A": slo, "C": slo}
	ok := func(id uint64, tenant string) eval.Record {
		return eval.Record{ID: id, Tenant: tenant, Arrival: 0, FirstToken: 500 * time.Millisecond, Done: time.Second, OutputTokens: 6}
	}
	late := ok(3, "A")
	late.FirstToken = 3 * time.Second
	late.Done = 4 * time.Second
	recs := []eval.Record{
		ok(1, "A"), ok(2, "A"), late,
		{ID: 4, Tenant: "A", Failed: true},
		{ID: 5, Tenant: "C", Shed: true, ShedReason: "over-share"},
		ok(6, "C"),
	}
	m := eval.RunMetrics{P99TTFT: map[string]float64{"A": 3, "C": 0.5}}
	got := tenantResults([]string{"A", "B", "C"}, recs, slos, m)
	if len(got) != 3 {
		t.Fatalf("results %+v", got)
	}
	a, b, c := got[0], got[1], got[2]
	if a.Requests != 4 || a.Completed != 3 || a.Failed != 1 || a.Shed != 0 || *a.AttainmentPct != 50 || *a.ShedPct != 0 || *a.P99TTFTSeconds != 3 {
		t.Errorf("A %+v", a)
	}
	if b.Requests != 0 || b.AttainmentPct != nil || b.ShedPct != nil || b.P99TTFTSeconds != nil {
		t.Errorf("B without requests %+v", b)
	}
	if c.Requests != 2 || c.Shed != 1 || *c.AttainmentPct != 100 || *c.ShedPct != 50 {
		t.Errorf("C %+v", c)
	}
}
