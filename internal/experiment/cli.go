package experiment

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/workload"
)

// ScenarioByName returns a named workload scenario, "default" or
// "reclaim" (case and surrounding spaces ignored).
func ScenarioByName(name string) (workload.Scenario, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "default":
		return workload.DefaultScenario(), nil
	case "reclaim":
		return workload.ReclaimScenario(), nil
	case "real":
		return workload.RealScenario(), nil
	case "real-demo":
		return workload.RealDemoScenario(), nil
	}
	return workload.Scenario{}, fmt.Errorf("unknown scenario %q: want default|reclaim|real|real-demo", name)
}

// ParseDurations parses a comma-separated list of Go durations such as
// "10s,30s,2m"; empty items are skipped.
func ParseDurations(s string) ([]time.Duration, error) {
	var out []time.Duration
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", part, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// WithCapacity returns cfg with a replica budget of capacity replicas, to
// evaluate a binding budget. When the tenants' guarantees add up to more
// than capacity, each guarantee g becomes floor(g*capacity/sum), so they
// fit in their original proportions; MinReplicas is capped at capacity.
// cfg itself is not modified, and the result is validated.
func WithCapacity(cfg config.Config, capacity int) (config.Config, error) {
	if capacity < 1 {
		return cfg, fmt.Errorf("capacity must be >= 1, got %d", capacity)
	}
	cfg.Pool.CapacityReplicas = capacity
	cfg.Pool.MinReplicas = min(cfg.Pool.MinReplicas, capacity)
	cfg.Tenants = slices.Clone(cfg.Tenants)
	sum := 0
	for _, t := range cfg.Tenants {
		sum += t.GuaranteedReplicas
	}
	if sum > capacity {
		for i := range cfg.Tenants {
			cfg.Tenants[i].GuaranteedReplicas = cfg.Tenants[i].GuaranteedReplicas * capacity / sum
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("capacity %d: %w", capacity, err)
	}
	return cfg, nil
}

// MetricsTable formats the named metrics (eval.Metrics names) of every
// setup at one cold start exactly like summary.md's main table
// (eval.SummaryTable): "mean ± ci95", without the CI when it is undefined
// (one run), "(n=k)" when only k runs define the metric, "n/a" when none.
func MetricsTable(sum Summary, coldStartSeconds float64, names ...string) (eval.Table, error) {
	i := slices.IndexFunc(sum.Results, func(r ColdStartResult) bool { return r.ColdStartSeconds == coldStartSeconds })
	if i < 0 {
		return eval.Table{}, fmt.Errorf("no results at cold start %gs", coldStartSeconds)
	}
	metrics := make([]eval.Metric, len(names))
	for k, name := range names {
		metrics[k] = eval.Metric{Name: name}
	}
	var sums []eval.SetupSummary
	for _, sr := range sum.Results[i].Setups {
		ss := eval.SetupSummary{Setup: sr.Setup, Runs: sr.Runs}
		for _, name := range names {
			st, ok := sr.Metrics[name]
			if !ok {
				return eval.Table{}, fmt.Errorf("unknown metric %q", name)
			}
			ss.Stats = append(ss.Stats, st)
		}
		sums = append(sums, ss)
	}
	return eval.SummaryTable(sums, metrics), nil
}
