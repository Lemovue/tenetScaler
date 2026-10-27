package policy

import (
	"math"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := Policy{Version: 3, Tenants: map[string]TenantPolicy{"A": {Weight: 2, RetryAfterSeconds: 1, CriticalSlots: 8}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	bad := map[string]Policy{
		"zero version":              {Version: 0},
		"empty tenant":              {Version: 1, Tenants: map[string]TenantPolicy{"": {Weight: 1}}},
		"NaN weight":                {Version: 1, Tenants: map[string]TenantPolicy{"A": {Weight: math.NaN()}}},
		"infinite weight":           {Version: 1, Tenants: map[string]TenantPolicy{"A": {Weight: math.Inf(1)}}},
		"negative weight":           {Version: 1, Tenants: map[string]TenantPolicy{"A": {Weight: -1}}},
		"negative retry":            {Version: 1, Tenants: map[string]TenantPolicy{"A": {Weight: 1, RetryAfterSeconds: -1}}},
		"negative slots":            {Version: 1, Tenants: map[string]TenantPolicy{"A": {Weight: 1, CriticalSlots: -1}}},
		"negative share":            {Version: 1, Tenants: map[string]TenantPolicy{"A": {Weight: 1, Slots: -1}}},
		"negative guaranteed share": {Version: 1, Tenants: map[string]TenantPolicy{"A": {Weight: 1, GuaranteedSlots: -4}}},
	}
	for name, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestWeightFor(t *testing.T) {
	p := Policy{Version: 1, Tenants: map[string]TenantPolicy{"A": {Weight: 3}, "Z": {Weight: 0}}}
	cases := map[string]float64{"A": 3, "Z": MinWeight, "unknown": 1}
	for tenant, want := range cases {
		if got := p.WeightFor(tenant); got != want {
			t.Errorf("WeightFor(%q) = %v, want %v", tenant, got, want)
		}
	}
}

func TestEqual(t *testing.T) {
	p := Equal([]string{"A", "B"})
	if p.Version != 0 || len(p.Tenants) != 2 {
		t.Fatalf("Equal = %+v", p)
	}
	for name, tp := range p.Tenants {
		if tp.Weight != 1 || tp.Shed || tp.RetryAfterSeconds != 1 {
			t.Errorf("tenant %s: %+v, want weight 1, no shedding", name, tp)
		}
	}
}
