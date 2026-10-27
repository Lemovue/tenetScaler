package workload

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestRateAt(t *testing.T) {
	periodic := TenantPattern{Kind: KindPeriodic, BaseRPS: 1, PeakRPS: 2,
		PeriodSeconds: 120, BurstSeconds: 30, PhaseSeconds: 90}
	negPhase := TenantPattern{Kind: KindPeriodic, BaseRPS: 1, PeakRPS: 3,
		PeriodSeconds: 100, BurstSeconds: 20, PhaseSeconds: -10}
	alwaysPeak := TenantPattern{Kind: KindPeriodic, BaseRPS: 1, PeakRPS: 4,
		PeriodSeconds: 10, BurstSeconds: 10}
	spike := TenantPattern{Kind: KindSpike, BaseRPS: 1, PeakRPS: 5,
		SpikeStartSeconds: 180, SpikeDurationSeconds: 180}
	tests := []struct {
		name string
		p    TenantPattern
		t    float64
		want float64
	}{
		{"steady", TenantPattern{Kind: KindSteady, BaseRPS: 1.5, PeakRPS: 9}, 100, 1.5},
		{"periodic before first burst", periodic, 0, 1},
		{"periodic t=60 is base", periodic, 60, 1},
		{"periodic burst start", periodic, 90, 2},
		{"periodic inside burst", periodic, 119.9, 2},
		{"periodic burst end is exclusive", periodic, 120, 1},
		{"periodic next burst", periodic, 210, 2},
		{"periodic negative phase, t=5", negPhase, 5, 3},
		{"periodic negative phase, t=10", negPhase, 10, 1},
		{"periodic negative phase, t=95", negPhase, 95, 3},
		{"burst covering the period", alwaysPeak, 7, 4},
		{"periodic with zero period stays base", TenantPattern{Kind: KindPeriodic, BaseRPS: 1, PeakRPS: 2, BurstSeconds: 5}, 1, 1},
		{"spike before", spike, 179.9, 1},
		{"spike start", spike, 180, 5},
		{"spike inside", spike, 359.9, 5},
		{"spike end is exclusive", spike, 360, 1},
		{"unknown kind uses base", TenantPattern{Kind: "weird", BaseRPS: 2, PeakRPS: 7}, 1, 2},
		{"negative rate is zero", TenantPattern{Kind: KindSteady, BaseRPS: -1}, 1, 0},
		{"NaN rate is zero", TenantPattern{Kind: KindSteady, BaseRPS: math.NaN()}, 1, 0},
		{"infinite rate is zero", TenantPattern{Kind: KindSteady, BaseRPS: math.Inf(1)}, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RateAt(tc.p, tc.t); got != tc.want {
				t.Errorf("RateAt(t=%v) = %v, want %v", tc.t, got, tc.want)
			}
		})
	}
}

func TestSpikeWindow(t *testing.T) {
	tests := []struct {
		name       string
		s          Scenario
		tenant     string
		start, end time.Duration
		ok         bool
	}{
		{"default C", DefaultScenario(), "C", 180 * time.Second, 360 * time.Second, true},
		{"periodic tenant", DefaultScenario(), "B", 0, 0, false},
		{"missing tenant", DefaultScenario(), "Z", 0, 0, false},
		{"reclaim owner", ReclaimScenario(), ReclaimOwner, 240 * time.Second, 480 * time.Second, true},
		{"clamped to duration", Scenario{Duration: 100 * time.Second, Tenants: []TenantPattern{
			{Tenant: "X", Kind: KindSpike, SpikeStartSeconds: 50, SpikeDurationSeconds: 500}}},
			"X", 50 * time.Second, 100 * time.Second, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ok := tc.s.SpikeWindow(tc.tenant)
			if start != tc.start || end != tc.end || ok != tc.ok {
				t.Errorf("SpikeWindow(%q) = %v, %v, %v; want %v, %v, %v", tc.tenant, start, end, ok, tc.start, tc.end, tc.ok)
			}
		})
	}
}

func TestReclaimReturn(t *testing.T) {
	start, _, ok := ReclaimScenario().SpikeWindow(ReclaimOwner)
	if !ok || ReclaimReturn() != start || start != 240*time.Second {
		t.Fatalf("ReclaimReturn() = %v, owner window start %v (ok=%v); want 4m0s for both", ReclaimReturn(), start, ok)
	}
	// Before the return the owner is nearly idle; afterwards it needs its share.
	var owner TenantPattern
	for _, p := range ReclaimScenario().Tenants {
		if p.Tenant == ReclaimOwner {
			owner = p
		}
	}
	if before, after := RateAt(owner, 239), RateAt(owner, 240); before > 0.1 || after < 1 {
		t.Errorf("owner rate before/after return = %v/%v, want <= 0.1 then >= 1", before, after)
	}
	specs := Generate(ReclaimScenario(), 7)
	early, late := 0, 0
	for _, s := range specs {
		if s.Tenant != ReclaimOwner {
			continue
		}
		if s.Offset < ReclaimReturn() {
			early++
		} else {
			late++
		}
	}
	if early >= late/10 {
		t.Errorf("owner requests before/after return = %d/%d, want the owner nearly idle first", early, late)
	}
}

func TestDefaultScenarioMatchesProblemStatement(t *testing.T) {
	s := DefaultScenario()
	if s.Duration != 600*time.Second || len(s.Tenants) != 3 {
		t.Fatalf("duration %v with %d tenants, want 600 s and 3 tenants", s.Duration, len(s.Tenants))
	}
	kinds := map[string]string{"A": KindSteady, "B": KindPeriodic, "C": KindSpike}
	for _, p := range s.Tenants {
		if kinds[p.Tenant] != p.Kind {
			t.Errorf("tenant %s kind %q, want %q", p.Tenant, p.Kind, kinds[p.Tenant])
		}
		if p.CV <= 1 {
			t.Errorf("tenant %s CV %v, want > 1 (burstier than Poisson)", p.Tenant, p.CV)
		}
		m := p.Mix
		if p.Tenant == "C" {
			if m.Sheddable <= m.Standard+m.Critical {
				t.Errorf("C mix %+v: want mostly sheddable batch work", m)
			}
			if p.PeakRPS < 3*p.BaseRPS {
				t.Errorf("C peak %v vs base %v: want a spike several times the base rate", p.PeakRPS, p.BaseRPS)
			}
		} else if m.Standard <= 0.5*(m.Critical+m.Standard+m.Sheddable) || m.Critical <= 0 {
			t.Errorf("tenant %s mix %+v: want mostly standard with some critical", p.Tenant, m)
		}
	}
	if math.Abs(RateAt(s.Tenants[0], 300)-1) > 1e-12 {
		t.Errorf("A rate = %v, want 1 req/s", RateAt(s.Tenants[0], 300))
	}
}

func TestValidate(t *testing.T) {
	withZeroOut := DefaultScenario()
	withZeroOut.Name = "zero-output sample" // as LoadAzure may return
	withZeroOut.Samples = []LengthSample{{In: 374, Out: 0}, {In: 0, Out: 7}}
	for _, s := range []Scenario{DefaultScenario(), ReclaimScenario(), withZeroOut} {
		if err := s.Validate(); err != nil {
			t.Errorf("%s scenario: unexpected error %v", s.Name, err)
		}
	}
	tests := []struct {
		name   string
		mutate func(*Scenario)
		want   string
	}{
		{"zero duration", func(s *Scenario) { s.Duration = 0 }, "duration"},
		{"no tenants", func(s *Scenario) { s.Tenants = nil }, "at least one tenant"},
		{"empty name", func(s *Scenario) { s.Tenants[0].Tenant = "" }, "empty name"},
		{"duplicate", func(s *Scenario) { s.Tenants[1].Tenant = "A" }, "duplicate"},
		{"bad kind", func(s *Scenario) { s.Tenants[0].Kind = "bursty" }, "kind must be"},
		{"negative rate", func(s *Scenario) { s.Tenants[0].BaseRPS = -1 }, "baseRPS"},
		{"NaN peak", func(s *Scenario) { s.Tenants[0].PeakRPS = math.NaN() }, "baseRPS"},
		{"short period", func(s *Scenario) { s.Tenants[1].PeriodSeconds = 0.5 }, "periodSeconds"},
		{"burst longer than period", func(s *Scenario) { s.Tenants[1].BurstSeconds = 200 }, "burstSeconds"},
		{"negative spike start", func(s *Scenario) { s.Tenants[2].SpikeStartSeconds = -1 }, "spikeStartSeconds"},
		{"zero spike duration", func(s *Scenario) { s.Tenants[2].SpikeDurationSeconds = 0 }, "spikeDurationSeconds"},
		{"zero CV", func(s *Scenario) { s.Tenants[0].CV = 0 }, "cv"},
		{"negative mix weight", func(s *Scenario) { s.Tenants[0].Mix.Critical = -0.1 }, "mix weights"},
		{"empty mix", func(s *Scenario) { s.Tenants[0].Mix = PriorityMix{} }, "positive weight"},
		{"bad input median", func(s *Scenario) { s.Tenants[0].In.Median = 0 }, "input lengths"},
		{"bad output range", func(s *Scenario) { s.Tenants[0].Out.Max = 1 }, "output lengths"},
		{"negative sample output", func(s *Scenario) { s.Samples = []LengthSample{{In: 5, Out: 0}, {In: 5, Out: -1}} }, "sample 1"},
		{"negative sample input", func(s *Scenario) { s.Samples = []LengthSample{{In: -1, Out: 3}} }, "sample 0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultScenario()
			tc.mutate(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}
