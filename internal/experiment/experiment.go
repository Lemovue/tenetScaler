// Package experiment runs TenantScaler's SIMULATED evaluation: every setup
// of internal/sim over several seeded runs and a sweep of replica cold
// starts, summarised as mean ± 95% confidence interval and written as CSV,
// JSON, Markdown and SVG plots. It also evaluates the demand forecaster
// (Forecast) and measures the per-replica capacity C of the simulated
// engine (Profile).
//
// Every number written here comes from the discrete-event simulator, which
// drives the real gateway scheduler, controller and baseline autoscalers in
// virtual time but replaces replica compute with the simulated engine
// latency model; all outputs say so.
package experiment

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

// Disclaimer heads every report this package writes.
const Disclaimer = "SIMULATED: these numbers come from the discrete-event simulator (internal/sim), " +
	"which runs the real fairq gateway scheduler, control.Core controller and autoscale HPA/KEDA code " +
	"in virtual time, with replica compute replaced by the simulated engine latency model " +
	"(the config's engine parameters: illustrative unless calibrated by `tenantscaler profile`; not measured inference). " +
	"Real-replica results are reported separately."

// Plan describes one evaluation.
type Plan struct {
	// Setups to run, in report order; empty means sim.AllSetups followed
	// by sim.SensitivitySetups.
	Setups []string
	// Runs per setup and cold start (>= 1); run r uses seed BaseSeed+r for
	// every setup, so setups see identical traces.
	Runs int
	// ColdStarts is the cold-start sweep; DefaultColdStart (> 0) is added
	// when missing and is the one the main table reports.
	ColdStarts       []time.Duration
	DefaultColdStart time.Duration
	Scenario         workload.Scenario
	BaseSeed         int64
	// InitialReplicas is the warm pool of every scaling setup; 0 means
	// sim.DefaultInitialReplicas.
	InitialReplicas int
	// OutDir receives runs.csv, summary.json, summary.md and the SVG plots.
	OutDir string
	Config config.Config
	// Logger reports progress; nil discards it.
	Logger *slog.Logger
}

// Summary is what summary.json holds.
type Summary struct {
	Simulated               bool              `json:"simulated"`
	Disclaimer              string            `json:"disclaimer"`
	Scenario                string            `json:"scenario"`
	ScenarioSeconds         float64           `json:"scenarioSeconds"`
	Noisy                   string            `json:"noisy"`
	Victims                 []string          `json:"victims"`
	SpikeStartSeconds       float64           `json:"spikeStartSeconds"`
	SpikeEndSeconds         float64           `json:"spikeEndSeconds"`
	Runs                    int               `json:"runs"`
	Seeds                   []int64           `json:"seeds"`
	DefaultColdStartSeconds float64           `json:"defaultColdStartSeconds"`
	ColdStartsSeconds       []float64         `json:"coldStartsSeconds"`
	Setups                  []SetupInfo       `json:"setups"`
	Parameters              Parameters        `json:"parameters"`
	Results                 []ColdStartResult `json:"results"`
}

// SetupInfo names a setup's gateway and control plane.
type SetupInfo struct {
	Name         string `json:"name"`
	Gateway      string `json:"gateway"`
	ControlPlane string `json:"controlPlane"`
}

// ColdStartResult holds every setup's statistics at one cold start.
type ColdStartResult struct {
	ColdStartSeconds float64       `json:"coldStartSeconds"`
	Setups           []SetupResult `json:"setups"`
}

// SetupResult is one setup's statistics, keyed by eval.Metrics name.
type SetupResult struct {
	Setup   string               `json:"setup"`
	Runs    int                  `json:"runs"`
	Metrics map[string]eval.Stat `json:"metrics"`
}

// sweepPoint is the raw outcome at one cold start.
type sweepPoint struct {
	coldStart time.Duration
	results   []eval.RunResult
	sums      []eval.SetupSummary
}

// outcome is everything Run produced, for the writers.
type outcome struct {
	plan     Plan
	metrics  []eval.Metric
	points   []sweepPoint
	main     sweepPoint
	examples map[string]eval.RunData // run 0 at the default cold start
	summary  Summary
}

// Run executes the plan and writes its outputs to OutDir.
func Run(p Plan) (Summary, error) {
	p, err := p.normalize()
	if err != nil {
		return Summary{}, err
	}
	out := outcome{plan: p, metrics: eval.Metrics(tenantNames(p.Scenario)), examples: map[string]eval.RunData{}}
	for _, cs := range p.ColdStarts {
		pt := sweepPoint{coldStart: cs}
		for _, setup := range p.Setups {
			for run := range p.Runs {
				o := sim.Options{Setup: setup, Config: p.Config, Scenario: p.Scenario, Seed: p.BaseSeed + int64(run), ColdStart: cs,
					InitialReplicas: p.InitialReplicas}
				d, err := sim.Run(o)
				if err != nil {
					return Summary{}, fmt.Errorf("simulate %v: %w", o, err)
				}
				d.Run = run
				pt.results = append(pt.results, eval.Evaluate(d))
				if cs == p.DefaultColdStart && run == 0 {
					out.examples[setup] = d
				}
			}
		}
		pt.sums = eval.SummarizeRuns(pt.results, out.metrics)
		vi := metricIndex(out.metrics, metricVictim)
		for _, s := range pt.sums {
			p.Logger.Info("simulated setup done", "coldStart", cs, "setup", s.Setup, "runs", s.Runs,
				"victimViolationPct", eval.FormatValue(s.Stats[vi].Mean))
		}
		out.points = append(out.points, pt)
		if cs == p.DefaultColdStart {
			out.main = pt
		}
	}
	if out.summary, err = buildSummary(out); err != nil {
		return Summary{}, err
	}
	if err := writeOutputs(out); err != nil {
		return Summary{}, err
	}
	return out.summary, nil
}

// normalize validates p and fills defaults.
func (p Plan) normalize() (Plan, error) {
	if len(p.Setups) == 0 {
		p.Setups = append(slices.Clone(sim.AllSetups), sim.SensitivitySetups...)
	}
	seen := map[string]bool{}
	for _, s := range p.Setups {
		if _, _, ok := sim.Info(s); !ok {
			return p, fmt.Errorf("unknown setup %q (want one of %v or %v)", s, sim.AllSetups, sim.SensitivitySetups)
		}
		if seen[s] {
			return p, fmt.Errorf("setup %q listed twice", s)
		}
		seen[s] = true
	}
	switch {
	case p.Runs < 1:
		return p, fmt.Errorf("runs must be >= 1, got %d", p.Runs)
	case p.DefaultColdStart <= 0:
		return p, fmt.Errorf("default cold start must be > 0, got %v", p.DefaultColdStart)
	case p.OutDir == "":
		return p, fmt.Errorf("output directory is required")
	}
	css := []time.Duration{p.DefaultColdStart}
	for _, cs := range p.ColdStarts {
		if cs <= 0 {
			return p, fmt.Errorf("cold starts must be > 0, got %v", cs)
		}
		css = append(css, cs)
	}
	slices.Sort(css)
	p.ColdStarts = slices.Compact(css)
	// Validate once up front (config, scenario, tenants) instead of failing
	// in the middle of a long sweep.
	if _, err := sim.Resolve(sim.Options{Setup: p.Setups[0], Config: p.Config, Scenario: p.Scenario}); err != nil {
		return p, err
	}
	if p.Logger == nil {
		p.Logger = slog.New(slog.DiscardHandler)
	}
	return p, nil
}

// tenantNames lists the scenario's tenants in scenario order.
func tenantNames(sc workload.Scenario) []string {
	names := make([]string, len(sc.Tenants))
	for i, t := range sc.Tenants {
		names[i] = t.Tenant
	}
	return names
}

func buildSummary(out outcome) (Summary, error) {
	p := out.plan
	roles := sim.ScenarioRoles(p.Scenario)
	params, err := parametersFor(p)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{
		Simulated: true, Disclaimer: Disclaimer,
		Scenario: p.Scenario.Name, ScenarioSeconds: p.Scenario.Duration.Seconds(),
		Noisy: roles.Noisy, Victims: roles.Victims,
		SpikeStartSeconds: roles.SpikeStart.Seconds(), SpikeEndSeconds: roles.SpikeEnd.Seconds(),
		Runs: p.Runs, DefaultColdStartSeconds: p.DefaultColdStart.Seconds(),
		Parameters: params,
	}
	for r := range p.Runs {
		s.Seeds = append(s.Seeds, p.BaseSeed+int64(r))
	}
	for _, name := range p.Setups {
		gw, ctl, _ := sim.Info(name)
		s.Setups = append(s.Setups, SetupInfo{Name: name, Gateway: gw, ControlPlane: ctl})
	}
	for _, pt := range out.points {
		s.ColdStartsSeconds = append(s.ColdStartsSeconds, pt.coldStart.Seconds())
		cr := ColdStartResult{ColdStartSeconds: pt.coldStart.Seconds()}
		for _, sum := range pt.sums {
			sr := SetupResult{Setup: sum.Setup, Runs: sum.Runs, Metrics: map[string]eval.Stat{}}
			for i, m := range out.metrics {
				sr.Metrics[m.Name] = sum.Stats[i]
			}
			cr.Setups = append(cr.Setups, sr)
		}
		s.Results = append(s.Results, cr)
	}
	return s, nil
}

// Metric names (eval.Metrics) the report and plots single out.
const (
	metricVictim          = "victim_violation_pct"
	metricVictimShedAware = "victim_shed_aware_violation_pct"
	metricCounterGap      = "max_counter_gap"
)

// metricIndex returns the position of the named metric.
func metricIndex(metrics []eval.Metric, name string) int {
	return slices.IndexFunc(metrics, func(m eval.Metric) bool { return m.Name == name })
}

// writeFile writes data to OutDir/name.
func writeFile(dir, name string, data []byte) error {
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}
