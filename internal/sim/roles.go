package sim

import (
	"time"

	"tenantscaler/internal/workload"
)

// Roles says who is who in a scenario, for the evaluation metrics.
type Roles struct {
	// Noisy is the spike tenant with the largest rate increase
	// (PeakRPS - BaseRPS; the first such tenant on ties); empty when the
	// scenario has no spike tenant.
	Noisy string
	// Victims are all other scenario tenants, in scenario order.
	Victims []string
	// SpikeStart and SpikeEnd are the noisy tenant's spike window.
	SpikeStart, SpikeEnd time.Duration
	// Owner is another spike tenant whose demand returns at or after the
	// noisy spike starts (the reclaim scenario's returning owner), and
	// OwnerReturn is when it returns; empty and zero when there is none.
	Owner       string
	OwnerReturn time.Duration
}

// ScenarioRoles derives the roles of sc.
func ScenarioRoles(sc workload.Scenario) Roles {
	var r Roles
	best := -1.0
	for _, p := range sc.Tenants {
		if p.Kind == workload.KindSpike && p.PeakRPS-p.BaseRPS > best {
			best, r.Noisy = p.PeakRPS-p.BaseRPS, p.Tenant
		}
	}
	if r.Noisy != "" {
		r.SpikeStart, r.SpikeEnd, _ = sc.SpikeWindow(r.Noisy)
	}
	for _, p := range sc.Tenants {
		if p.Tenant == r.Noisy {
			continue
		}
		r.Victims = append(r.Victims, p.Tenant)
		if r.Owner != "" || r.Noisy == "" || p.Kind != workload.KindSpike {
			continue
		}
		if start, _, ok := sc.SpikeWindow(p.Tenant); ok && start >= r.SpikeStart && start > 0 {
			r.Owner, r.OwnerReturn = p.Tenant, start
		}
	}
	return r
}
