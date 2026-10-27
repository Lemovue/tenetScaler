package demo

import (
	"fmt"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

// Scenario names accepted by ScenarioByName (the demo's -scenario flag).
const (
	// ScenarioRealDemo is workload.RealDemoScenario (120 s).
	ScenarioRealDemo = "real-demo"
	// ScenarioTiny is TinyScenario (6 s), for tests and smoke runs.
	ScenarioTiny = "tiny"
)

// tinyCompression and tinyRateFactor derive TinyScenario from the
// real-demo trace: 20 times shorter, with 4 times the rates so that a few
// seconds still hold a few dozen requests.
const (
	tinyCompression = 20
	tinyRateFactor  = 4
)

// ScenarioByName returns the named demo scenario.
func ScenarioByName(name string) (workload.Scenario, error) {
	switch name {
	case ScenarioRealDemo:
		return workload.RealDemoScenario(), nil
	case ScenarioTiny:
		return TinyScenario(), nil
	}
	return workload.Scenario{}, fmt.Errorf("demo: scenario must be %q or %q, got %q", ScenarioRealDemo, ScenarioTiny, name)
}

// TinyScenario is a 6 s smoke-test trace with the real-demo's shape (A
// steady, B periodic, C spiking in the middle): workload.RealDemoScenario
// with every timing divided by 20 and every rate multiplied by 4. It checks
// that the A/B pipeline works end to end; its numbers measure nothing of
// interest.
func TinyScenario() workload.Scenario {
	sc := workload.RealDemoScenario()
	sc.Name = ScenarioTiny
	sc.Duration /= tinyCompression
	tenants := make([]workload.TenantPattern, len(sc.Tenants))
	for i, p := range sc.Tenants {
		p.BaseRPS *= tinyRateFactor
		p.PeakRPS *= tinyRateFactor
		p.PeriodSeconds /= tinyCompression
		p.BurstSeconds /= tinyCompression
		p.PhaseSeconds /= tinyCompression
		p.SpikeStartSeconds /= tinyCompression
		p.SpikeDurationSeconds /= tinyCompression
		tenants[i] = p
	}
	sc.Tenants = tenants
	return sc
}

// ScenarioInfo describes the replayed scenario for the dashboard.
type ScenarioInfo struct {
	Name            string  `json:"name"`
	DurationSeconds float64 `json:"durationSeconds"`
	// Noisy is the spiking tenant, Victims the others; the spike window is
	// [SpikeStartSeconds, SpikeEndSeconds) (zero without a spike).
	Noisy             string       `json:"noisy"`
	Victims           []string     `json:"victims"`
	SpikeStartSeconds float64      `json:"spikeStartSeconds"`
	SpikeEndSeconds   float64      `json:"spikeEndSeconds"`
	Tenants           []TenantInfo `json:"tenants"`
}

// TenantInfo is one tenant of the scenario with its configured SLO and
// replica guarantee.
type TenantInfo struct {
	Name string `json:"name"`
	// Kind is the traffic pattern: steady, periodic or spike.
	Kind string `json:"kind"`
	// TTFTSeconds, TPOTMillis and TargetFraction are the tenant's SLO.
	TTFTSeconds    float64 `json:"ttftSeconds"`
	TPOTMillis     float64 `json:"tpotMillis"`
	TargetFraction float64 `json:"targetFraction"`
	// GuaranteedReplicas and MaxReplicas are the tenant's share of the
	// budget.
	GuaranteedReplicas int `json:"guaranteedReplicas"`
	MaxReplicas        int `json:"maxReplicas"`
}

// describeScenario builds the ScenarioInfo of sc under cfg.
func describeScenario(sc workload.Scenario, roles sim.Roles, cfg config.Config) ScenarioInfo {
	info := ScenarioInfo{
		Name: sc.Name, DurationSeconds: sc.Duration.Seconds(), Noisy: roles.Noisy, Victims: roles.Victims,
		SpikeStartSeconds: roles.SpikeStart.Seconds(), SpikeEndSeconds: roles.SpikeEnd.Seconds(),
	}
	if info.Victims == nil {
		info.Victims = []string{}
	}
	for _, p := range sc.Tenants {
		ti := TenantInfo{Name: p.Tenant, Kind: p.Kind}
		for _, t := range cfg.Tenants {
			if t.Name == p.Tenant {
				ti.TTFTSeconds, ti.TPOTMillis, ti.TargetFraction = t.SLO.TTFTSeconds, t.SLO.TPOTMillis, t.SLO.TargetFraction
				ti.GuaranteedReplicas, ti.MaxReplicas = t.GuaranteedReplicas, t.MaxReplicas
			}
		}
		info.Tenants = append(info.Tenants, ti)
	}
	return info
}

// tenantNames lists the scenario's tenants in order.
func tenantNames(sc workload.Scenario) []string {
	out := make([]string, len(sc.Tenants))
	for i, p := range sc.Tenants {
		out[i] = p.Tenant
	}
	return out
}

// seconds converts a duration to float seconds rounded to milliseconds,
// for compact JSON.
func seconds(d time.Duration) float64 { return float64(d.Milliseconds()) / 1000 }
