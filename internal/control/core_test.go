package control

import (
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/model"
)

var t0 = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// rateFor returns a weighted token rate that the default config (C = 400,
// headroom 0.2) turns into exactly the given replica demand.
func rateFor(demand int) float64 { return (float64(demand) - 0.5) * 400 / 1.2 }

func obsAt(sec int, demands map[string]int, p99 map[string]float64, pool int) model.Observation {
	o := model.Observation{Time: t0.Add(time.Duration(sec) * time.Second), OK: true, Pool: model.PoolObservation{DesiredReplicas: pool, ReadyReplicas: pool}}
	for _, name := range []string{"A", "B", "C"} {
		to := model.TenantObservation{Tenant: name, ArrivalWeightedTokensPerSec: rateFor(demands[name])}
		if v, ok := p99[name]; ok {
			to.HasSLOData, to.Completed, to.P99TTFTSeconds = true, 10, v
		}
		o.Tenants = append(o.Tenants, to)
	}
	return o
}

func grants(d Decision) map[string]int {
	g := map[string]int{}
	for _, r := range d.Records {
		if r.Tenant != PoolRecordTenant {
			g[r.Tenant] = r.Final.Grant
		}
	}
	return g
}

func newCore(t *testing.T, mutate func(*config.Config)) *Core {
	t.Helper()
	cfg := config.Default()
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := NewCore(cfg, 30*time.Second)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	return c
}

// The problem statement's worked example, run through the full cycle:
// demand 2/4/8 -> 2/4/4 with C shedding; then A returns -> 3/4/3 (reclaim).
func TestWorkedExampleThroughStep(t *testing.T) {
	c := newCore(t, nil)
	slack := map[string]float64{"A": 1.0, "B": 1.8, "C": 1.0} // B closest to its 2 s SLO
	d := c.Step(obsAt(15, map[string]int{"A": 2, "B": 4, "C": 8}, slack, 6))

	if g := grants(d); g["A"] != 2 || g["B"] != 4 || g["C"] != 4 {
		t.Fatalf("grants = %v, want A2 B4 C4", g)
	}
	if d.DesiredReplicas != 10 || d.PoolReplicas != 10 {
		t.Fatalf("desired/pool = %d/%d, want 10/10 (6 + max step 4)", d.DesiredReplicas, d.PoolReplicas)
	}
	if d.Policy == nil || d.Policy.Version != 1 {
		t.Fatalf("expected policy v1, got %+v", d.Policy)
	}
	if !d.Policy.Tenants["C"].Shed || d.Policy.Tenants["A"].Shed || d.Policy.Tenants["B"].Shed {
		t.Fatalf("only C should shed: %+v", d.Policy.Tenants)
	}
	if got := d.Policy.Tenants["B"]; got.Weight != 4 || got.CriticalSlots != 3*8 || got.RetryAfterSeconds != 2 ||
		got.Slots != 4*8 || got.GuaranteedSlots != 3*8 {
		t.Fatalf("B policy = %+v", got)
	}
	if !d.Policy.IssuedAt.Equal(t0.Add(15 * time.Second)) {
		t.Fatalf("IssuedAt = %v", d.Policy.IssuedAt)
	}

	d2 := c.Step(obsAt(30, map[string]int{"A": 3, "B": 4, "C": 8}, slack, 10))
	if g := grants(d2); g["A"] != 3 || g["B"] != 4 || g["C"] != 3 {
		t.Fatalf("reclaim grants = %v, want A3 B4 C3", g)
	}
	if d2.Policy.Version != 2 {
		t.Fatalf("policy version = %d, want 2", d2.Policy.Version)
	}
}

func TestRecordsExplainEveryTenantAndThePool(t *testing.T) {
	c := newCore(t, nil)
	d := c.Step(obsAt(15, map[string]int{"A": 2, "B": 4, "C": 8}, nil, 10))
	if len(d.Records) != 4 {
		t.Fatalf("want 3 tenant records + 1 pool record, got %d", len(d.Records))
	}
	for _, r := range d.Records {
		if r.Reason == "" || len(r.Steps) == 0 || r.Cycle != 1 || r.PolicyVersion != 1 {
			t.Errorf("incomplete record: %+v", r)
		}
	}
	c8 := d.Records[2]
	if c8.Tenant != "C" || c8.Inputs.Demand != 8 || c8.Inputs.Guaranteed != 3 || !c8.Final.Shedding {
		t.Errorf("C record = %+v", c8)
	}
	if !strings.Contains(c8.Reason, "429") {
		t.Errorf("C reason should mention 429 shedding: %q", c8.Reason)
	}
}

func TestFailStaticHoldsReplicasAndPolicy(t *testing.T) {
	c := newCore(t, nil)
	first := c.Step(obsAt(15, map[string]int{"A": 2, "B": 4, "C": 8}, nil, 10))
	d := c.Step(model.Observation{Time: t0.Add(30 * time.Second), OK: false, Error: "prometheus down"})
	if !d.FailStatic || d.PoolReplicas != first.PoolReplicas || d.Policy != first.Policy {
		t.Fatalf("fail-static must hold pool and policy: %+v", d)
	}
	for _, r := range d.Records {
		if !r.FailStatic || !strings.Contains(r.Reason, "prometheus down") {
			t.Errorf("record not marked fail-static: %+v", r)
		}
	}
	// A failed first cycle must not scale to zero either.
	c2 := newCore(t, nil)
	d2 := c2.Step(model.Observation{Time: t0, OK: false, Pool: model.PoolObservation{DesiredReplicas: 7}})
	if d2.PoolReplicas != 7 {
		t.Fatalf("fail-static on first cycle should hold the observed pool size 7, got %d", d2.PoolReplicas)
	}
}

// A first cycle without a pool reading must not stop the core from adopting
// the pool size once it is reported.
func TestAdoptsPoolSizeWhenFirstReported(t *testing.T) {
	c := newCore(t, nil)
	c.Step(model.Observation{Time: t0, OK: false})
	d := c.Step(obsAt(15, map[string]int{"A": 1, "B": 1, "C": 1}, nil, 9))
	if d.PoolReplicas != 9 {
		t.Fatalf("pool = %d, want the adopted 9 held by the scale-down window", d.PoolReplicas)
	}
	d = c.Step(obsAt(30, map[string]int{"A": 1, "B": 1, "C": 1}, nil, 4))
	if d.PoolReplicas != 9 {
		t.Fatalf("adoption must happen once; pool = %d, want 9", d.PoolReplicas)
	}
}

func TestAblations(t *testing.T) {
	noCoupling := newCore(t, func(c *config.Config) { c.Controller.DisableGatewayCoupling = true })
	d := noCoupling.Step(obsAt(15, map[string]int{"A": 2, "B": 4, "C": 8}, nil, 10))
	if d.Policy != nil {
		t.Fatal("coupling disabled: expected no policy")
	}
	if !strings.Contains(d.Records[0].Reason, "coupling is off") {
		t.Fatalf("reason should state the ablation: %q", d.Records[0].Reason)
	}

	noBorrow := newCore(t, func(c *config.Config) { c.Controller.DisableBorrowing = true })
	d = noBorrow.Step(obsAt(15, map[string]int{"A": 2, "B": 4, "C": 8}, nil, 10))
	if g := grants(d); g["A"] != 2 || g["B"] != 3 || g["C"] != 3 {
		t.Fatalf("no-borrowing grants = %v, want A2 B3 C3", g)
	}
}

func TestStabilizationDelaysScaleDown(t *testing.T) {
	c := newCore(t, nil) // 120 s scale-down window
	c.Step(obsAt(15, map[string]int{"A": 3, "B": 3, "C": 4}, nil, 10))
	d := c.Step(obsAt(30, map[string]int{"A": 1, "B": 1, "C": 1}, nil, 10))
	if d.DesiredReplicas != 3 || d.PoolReplicas != 10 {
		t.Fatalf("within window: desired/pool = %d/%d, want 3/10", d.DesiredReplicas, d.PoolReplicas)
	}
	d = c.Step(obsAt(200, map[string]int{"A": 1, "B": 1, "C": 1}, nil, 10))
	if d.PoolReplicas != 6 {
		t.Fatalf("after window: pool = %d, want 10-4 = 6 (max step)", d.PoolReplicas)
	}
}

func TestSetTenantsValidatesAndKeepsOrder(t *testing.T) {
	c := newCore(t, nil)
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	if err := c.SetTenants([]model.TenantSpec{{Name: "X", GuaranteedReplicas: 11, MaxReplicas: 11, SLO: slo}}); err == nil {
		t.Fatal("expected over-promise rejection")
	}
	if err := c.SetTenants([]model.TenantSpec{{Name: "Z", GuaranteedReplicas: 2, MaxReplicas: 5, SLO: slo}, {Name: "A", GuaranteedReplicas: 1, MaxReplicas: 5, SLO: slo}}); err != nil {
		t.Fatal(err)
	}
	d := c.Step(model.Observation{Time: t0, OK: true})
	if d.Records[0].Tenant != "Z" || d.Records[1].Tenant != "A" {
		t.Fatalf("records not in spec order: %s, %s", d.Records[0].Tenant, d.Records[1].Tenant)
	}
}

// An idle tenant lends its guaranteed replicas but keeps its guaranteed
// weight at the gateway, so its first requests back are not starved; a
// borrower's weight follows its larger grant. Fail-static records report
// the last grants, not the weights.
func TestIdleTenantKeepsGuaranteedWeight(t *testing.T) {
	c := newCore(t, nil)
	d := c.Step(obsAt(15, map[string]int{"A": 0, "B": 2, "C": 8}, nil, 10))
	if g := grants(d); g["A"] != 0 || g["B"] != 2 || g["C"] != 8 {
		t.Fatalf("grants = %v, want A0 B2 C8 (C borrows A's idle guarantee)", g)
	}
	want := map[string]float64{"A": 3, "B": 3, "C": 8} // guarantees are 3/3/3
	for name, w := range want {
		if got := d.Policy.Tenants[name].Weight; got != w {
			t.Errorf("weight(%s) = %v, want %v", name, got, w)
		}
	}
	fs := c.Step(model.Observation{Time: t0.Add(30 * time.Second), OK: false, Error: "down"})
	for _, r := range fs.Records {
		if r.Final.Grant != grants(d)[r.Tenant] || r.Final.Weight != want[r.Tenant] {
			t.Errorf("fail-static record %s = %+v, want the last grant %d and weight %v", r.Tenant, r.Final, grants(d)[r.Tenant], want[r.Tenant])
		}
	}
	r := newCore(t, nil)
	r.Restore(c.State())
	if fs := r.Step(model.Observation{Time: t0.Add(45 * time.Second), OK: false}); fs.Records[0].Final.Grant != 0 || fs.Records[2].Final.Grant != 8 {
		t.Errorf("restored fail-static grants = %+v, want the saved A0 ... C8", fs.Records)
	}
}

func TestStateRestore(t *testing.T) {
	c := newCore(t, nil)
	c.Step(obsAt(15, map[string]int{"A": 2, "B": 4, "C": 8}, nil, 10))
	st := c.State()
	r := newCore(t, nil)
	r.Restore(st)
	d := r.Step(obsAt(30, map[string]int{"A": 2, "B": 4, "C": 8}, nil, 10))
	if d.Cycle != 2 || d.Policy.Version != 2 {
		t.Fatalf("restored core should continue at cycle 2 / v2, got %d / v%d", d.Cycle, d.Policy.Version)
	}
}

// A periodic tenant: once Holt-Winters has learned the season and proven
// accurate, demand is raised ahead of the burst (proactive pre-warming).
func TestForecastPreWarmsPeriodicTenant(t *testing.T) {
	c := newCore(t, func(cfg *config.Config) {
		cfg.Controller.Forecast = config.ForecastSpec{Enabled: true, BucketSeconds: 15, SeasonBuckets: 4, Alpha: 0.5, Beta: 0.1, Gamma: 0.3}
	})
	pattern := []int{1, 1, 1, 6} // a burst every 4th cycle
	sawForecast := false
	for cycle := 0; cycle < 40; cycle++ {
		dB := pattern[cycle%4]
		d := c.Step(obsAt(15*(cycle+1), map[string]int{"A": 1, "B": dB, "C": 1}, nil, 10))
		b := d.Records[1]
		if b.Inputs.ForecastDemand > b.Inputs.ReactiveDemand && dB == 1 {
			sawForecast = true
			if b.Inputs.Demand != b.Inputs.ForecastDemand {
				t.Fatalf("demand should take the forecast when it is higher: %+v", b.Inputs)
			}
		}
	}
	if !sawForecast {
		t.Fatal("forecaster never raised demand ahead of a burst")
	}
}

func TestHorizonBuckets(t *testing.T) {
	c := newCore(t, nil) // cold start 30 s, cycle 15 s, bucket 15 s
	if got := c.horizonBuckets(); got != 3 {
		t.Fatalf("horizon = %d, want ceil(45/15) = 3", got)
	}
}

// The slot-reservation ablation keeps everything but the slot shares.
func TestDisableSlotReservation(t *testing.T) {
	c := newCore(t, func(cfg *config.Config) { cfg.Controller.DisableSlotReservation = true })
	d := c.Step(obsAt(15, map[string]int{"A": 2, "B": 4, "C": 8}, nil, 10))
	for name, tp := range d.Policy.Tenants {
		if tp.Slots != 0 || tp.GuaranteedSlots != 0 || tp.CriticalSlots != 3*8 || tp.Weight < 2 {
			t.Errorf("%s: %+v, want no slot shares but weight and critical quota kept", name, tp)
		}
	}
	if !d.Policy.Tenants["C"].Shed {
		t.Error("C should still shed")
	}
}

// spiralLoop runs the core for cycles control cycles against a stylised
// gateway: A and B need one replica each, C offers enough weighted tokens
// for 12 replicas, 70% of them sheddable, and while the policy in force
// sheds C the gateway rejects C's sheddable share. Every observation carries
// both the offered rate (every arrival) and the admitted rate (arrivals not
// shed). It returns C's grant, the desired pool size and C's offered load
// that the grant cannot serve (replicas' worth), per cycle.
func spiralLoop(t *testing.T, naive bool, cycles int) (grantC, desired []int, unserved []float64) {
	t.Helper()
	c := newCore(t, func(cfg *config.Config) { cfg.Controller.NaiveDemand = naive })
	shedC := false
	for k := range cycles {
		o := obsAt(15*(k+1), map[string]int{"A": 1, "B": 1, "C": 12}, nil, 10)
		for i := range o.Tenants {
			o.Tenants[i].AdmittedWeightedTokensPerSec = o.Tenants[i].ArrivalWeightedTokensPerSec
		}
		if shedC {
			o.Tenants[2].AdmittedWeightedTokensPerSec *= 0.3
		}
		d := c.Step(o)
		g := grants(d)["C"]
		grantC, desired, unserved = append(grantC, g), append(desired, d.DesiredReplicas), append(unserved, 12-float64(g))
		shedC = d.Policy.Tenants["C"].Shed
	}
	return grantC, desired, unserved
}

// The death-spiral mechanism (BRIEF-v2 Q11) in the control core, with a
// gateway that sheds C's whole sheddable share while flagged (more than the
// excess): under naive demand the shed share is missing from C's measured
// demand, so C's grant and the pool's desired size fall while C's offered
// load is unchanged, and more of that load goes unserved. Offered demand
// (the default) counts shed requests, so the grant and the pool hold. The
// full system does not spiral (TestNaiveDemandDoesNotSpiralInSimulation).
func TestNaiveDemandSpirals(t *testing.T) {
	g, d, u := spiralLoop(t, true, 6)
	turned := false
	for k := 1; k < len(g); k++ {
		if g[k] < g[0] && d[k] < d[0] && u[k] > u[0] {
			turned = true
		}
	}
	if !turned {
		t.Fatalf("naive demand: C grants %v, desired %v, unserved %v; want a cycle where the grant and the pool fall and more goes unserved", g, d, u)
	}
	g, d, _ = spiralLoop(t, false, 6)
	for k := range g {
		if g[k] != g[0] || d[k] != d[0] {
			t.Fatalf("offered demand: C grants %v, desired %v; want them steady while C's load is steady", g, d)
		}
	}
}
