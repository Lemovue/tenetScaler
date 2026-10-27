package realrun

import (
	"bytes"
	"cmp"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/loadgen"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/stack"
)

// Summary is the outcome of Run, written as summary.json.
type Summary struct {
	// Label is the results' headline: REAL inference with engine, model,
	// replicas, CPU and date, or SIMULATED.
	Label     string `json:"label"`
	Simulated bool   `json:"simulated"`
	// Pinning says which CPUs the replicas and the services used.
	Pinning string `json:"pinning,omitempty"`
	// Claim is the pre-registered S7 vs S4 comparison (nil without both).
	Claim *Claim `json:"claim,omitempty"`
	// ReadinessHoldSeconds is the injected load delay (pool.HoldLabel).
	ReadinessHoldSeconds float64          `json:"readinessHoldSeconds"`
	Engine               stack.EngineInfo `json:"engine"`
	CPU                  string           `json:"cpu"`
	Date                 string           `json:"date"`
	Scenario             ScenarioInfo     `json:"scenario"`
	Setups               []SetupInfo      `json:"setups"`
	// Runs is the planned runs per setup; Seeds are their seeds.
	Runs              int     `json:"runs"`
	Seeds             []int64 `json:"seeds"`
	InitialReplicas   int     `json:"initialReplicas"`
	CapacityReplicas  int     `json:"capacityReplicas"`
	ThreadsPerReplica int     `json:"threadsPerReplica,omitempty"`
	SlotsPerReplica   int     `json:"slotsPerReplica"`
	CycleSeconds      float64 `json:"cycleSeconds"`
	// Results holds every completed run; Stats the per-setup summaries.
	Results []RunResult  `json:"results"`
	Stats   []SetupStats `json:"stats"`
}

// ScenarioInfo describes the replayed scenario.
type ScenarioInfo struct {
	Name              string   `json:"name"`
	DurationSeconds   float64  `json:"durationSeconds"`
	Noisy             string   `json:"noisy"`
	Victims           []string `json:"victims"`
	SpikeStartSeconds float64  `json:"spikeStartSeconds"`
	SpikeEndSeconds   float64  `json:"spikeEndSeconds"`
}

// SetupInfo describes one evaluated setup.
type SetupInfo struct {
	Name         string `json:"name"`
	Gateway      string `json:"gateway"`
	ControlPlane string `json:"controlPlane"`
}

// SetupStats is one setup's metrics over its runs (mean, std, 95% CI, N).
type SetupStats struct {
	Setup   string               `json:"setup"`
	Runs    int                  `json:"runs"`
	Metrics map[string]eval.Stat `json:"metrics"`
}

// buildSummary describes the plan and summarizes the completed runs.
func buildSummary(p Plan, results []RunResult) Summary {
	roles := sim.ScenarioRoles(p.Scenario)
	sum := Summary{
		CPU: CPUModel(), Date: time.Now().Format("2006-01-02"), Runs: p.Runs,
		Scenario: ScenarioInfo{Name: p.Scenario.Name, DurationSeconds: p.Scenario.Duration.Seconds(),
			Noisy: roles.Noisy, Victims: roles.Victims,
			SpikeStartSeconds: roles.SpikeStart.Seconds(), SpikeEndSeconds: roles.SpikeEnd.Seconds()},
		InitialReplicas: p.InitialReplicas, CapacityReplicas: p.Config.Pool.CapacityReplicas,
		SlotsPerReplica: p.Config.SlotsPerReplica(), CycleSeconds: p.Config.Controller.CycleSeconds,
		Results: results, Engine: stack.EngineInfo{Backend: p.Config.Backend.Kind}, Pinning: p.pinning,
		ReadinessHoldSeconds: p.ReadinessHold.Seconds(),
	}
	sum.Engine.Simulated = p.Config.Backend.Kind != config.BackendLlama
	if !sum.Engine.Simulated {
		sum.ThreadsPerReplica = p.Config.Backend.Llama.Threads
	}
	for k := range p.Runs {
		sum.Seeds = append(sum.Seeds, p.BaseSeed+int64(k))
	}
	for _, s := range p.Setups {
		gw, ctl, _ := stack.Describe(s)
		sum.Setups = append(sum.Setups, SetupInfo{Name: s, Gateway: gw, ControlPlane: ctl})
	}
	if len(results) > 0 {
		sum.Engine = results[0].Engine
	}
	for _, r := range results {
		sum.Simulated = sum.Simulated || r.Simulated
	}
	sum.Simulated = sum.Simulated || sum.Engine.Simulated
	sum.Label = summaryLabel(sum)
	sum.Claim = buildClaim(results)
	metrics := reportMetrics(p)
	for _, s := range eval.SummarizeRuns(evalResults(results), metrics) {
		ss := SetupStats{Setup: s.Setup, Runs: s.Runs, Metrics: map[string]eval.Stat{}}
		for i, m := range metrics {
			ss.Metrics[m.Name] = s.Stats[i]
		}
		sum.Stats = append(sum.Stats, ss)
	}
	return sum
}

// summaryLabel is the results' headline.
func summaryLabel(s Summary) string {
	if s.Simulated {
		return fmt.Sprintf("SIMULATED replicas (%s): not real inference; %s, %s", s.Engine.Label(), s.CPU, s.Date)
	}
	return fmt.Sprintf("REAL inference — %s, budget of %d replicas x %d threads (no GPU); %s; %s; CPU %s, %s",
		s.Engine.Label(), s.CapacityReplicas, s.ThreadsPerReplica, cmp.Or(s.Pinning, "CPU-pinned replicas"),
		holdLabel(s.ReadinessHoldSeconds), s.CPU, s.Date)
}

// reportMetrics are the reported columns: eval's standard metrics for the
// scenario's tenants.
func reportMetrics(p Plan) []eval.Metric {
	tenants := make([]string, 0, len(p.Scenario.Tenants))
	for _, t := range p.Scenario.Tenants {
		tenants = append(tenants, t.Tenant)
	}
	return eval.Metrics(tenants)
}

// evalResults converts run results to eval's form.
func evalResults(results []RunResult) []eval.RunResult {
	out := make([]eval.RunResult, len(results))
	for i, r := range results {
		out[i] = eval.RunResult{Setup: r.Setup, Run: r.Run, Seed: r.Seed, Metrics: r.Metrics}
	}
	return out
}

// writeOutputs writes runs.csv, summary.json, summary.md and the plots.
func writeOutputs(p Plan, sum Summary) error {
	metrics := reportMetrics(p)
	var runs bytes.Buffer
	if err := eval.RunsTable(evalResults(sum.Results), metrics).WriteCSV(&runs); err != nil {
		return err
	}
	if err := writeFile(p.OutDir, runsCSVFile, runs.Bytes()); err != nil {
		return err
	}
	js, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return fmt.Errorf("encode summary.json: %w", err)
	}
	if err := writeFile(p.OutDir, summaryJSONFile, append(js, '\n')); err != nil {
		return err
	}
	plots, err := writePlots(p, sum)
	if err != nil {
		return err
	}
	md, err := summaryMarkdown(p, sum, metrics, plots)
	if err != nil {
		return err
	}
	return writeFile(p.OutDir, summaryMDFile, md)
}

// MainTable is the main results table: one row per metric, one column per
// setup, each cell "mean ± 95% CI" over the setup's runs, with "(n=k)" when
// only k of its N runs define the metric.
func MainTable(sum Summary, metrics []eval.Metric) eval.Table {
	t := eval.Table{Header: []string{"metric"}}
	for _, s := range sum.Stats {
		t.Header = append(t.Header, fmt.Sprintf("%s (N=%d)", s.Setup, s.Runs))
	}
	for _, m := range metrics {
		row := []string{m.Name}
		for _, s := range sum.Stats {
			row = append(row, formatStat(s.Metrics[m.Name], s.Runs))
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// formatStat renders "mean ± ci95", "(n=k)" when k of runs define it.
func formatStat(st eval.Stat, runs int) string {
	if st.N == 0 {
		return "n/a"
	}
	s := eval.FormatValue(st.Mean)
	if !math.IsNaN(st.CI95) {
		s += " ± " + eval.FormatValue(st.CI95)
	}
	if st.N != runs {
		s += fmt.Sprintf(" (n=%d)", st.N)
	}
	return s
}

// writeRunFiles writes one run's raw request records and timeline.
func writeRunFiles(dir string, rr RunResult, res loadgen.Result) error {
	var recs bytes.Buffer
	if err := loadgen.WriteRecordsCSV(&recs, res); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, recordsDir), runFile(rr.Setup, rr.Run, ".csv"), recs.Bytes()); err != nil {
		return err
	}
	tl, err := timelineCSV(rr.data)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, timelineDir), runFile(rr.Setup, rr.Run, ".csv"), tl)
}

// timelineCSV renders a run's timeline: time, replicas, counter gap and
// per-tenant queued and in-flight requests.
func timelineCSV(d eval.RunData) ([]byte, error) {
	tenants := slices.Sorted(maps.Keys(d.SLOs))
	header := []string{"t_s", "ready", "starting", "desired", "counter_gap"}
	for _, t := range tenants {
		header = append(header, "queued_"+t, "inflight_"+t)
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write(header)
	for _, s := range d.Timeline {
		row := []string{strconv.FormatFloat(s.T.Seconds(), 'f', 3, 64), strconv.Itoa(s.Ready),
			strconv.Itoa(s.Starting), strconv.Itoa(s.Desired), eval.FormatValue(s.CounterGap)}
		for _, t := range tenants {
			row = append(row, strconv.Itoa(s.Queued[t]), strconv.Itoa(s.InFlight[t]))
		}
		_ = w.Write(row)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("timeline csv: %w", err)
	}
	return b.Bytes(), nil
}

// writeFile writes name under dir, creating dir.
func writeFile(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
