package experiment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

// shortScenario is the default scenario compressed to 240 s (spike at
// 60-150 s) so tests stay fast.
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

func tinyPlan(dir string) Plan {
	return Plan{
		Setups: []string{sim.SetupKEDAQueue, sim.SetupFull}, Runs: 2,
		ColdStarts: []time.Duration{10 * time.Second}, DefaultColdStart: 20 * time.Second,
		Scenario: shortScenario(), BaseSeed: 5, OutDir: dir, Config: config.Default(),
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestRunWritesAllFiles(t *testing.T) {
	dir := t.TempDir()
	sum, err := Run(tinyPlan(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Simulated || len(sum.Results) != 2 || len(sum.Results[0].Setups) != 2 || sum.Results[0].ColdStartSeconds != 10 {
		t.Fatalf("summary = %+v", sum)
	}
	if got := sum.Seeds; len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("seeds = %v, want [5 6]", got)
	}
	victim := sum.Results[1].Setups[0].Metrics["victim_violation_pct"]
	if victim.N != 2 {
		t.Fatalf("S2 victim violation stat %+v, want N=2", victim)
	}

	csvLines := strings.Split(strings.TrimSpace(readFile(t, dir, "runs.csv")), "\n")
	if len(csvLines) != 1+2*2*2 || !strings.HasPrefix(csvLines[0], "cold_start_s,setup,run,seed,victim_violation_pct") {
		t.Fatalf("runs.csv has %d lines, header %q", len(csvLines), csvLines[0])
	}

	var decoded Summary
	if err := json.Unmarshal([]byte(readFile(t, dir, "summary.json")), &decoded); err != nil {
		t.Fatalf("summary.json: %v", err)
	}
	if decoded.Parameters.CapacityReplicas != 10 || decoded.Parameters.InitialReplicas != sim.DefaultInitialReplicas || len(decoded.Setups) != 2 ||
		decoded.Parameters.DispatchTimeoutSeconds != 120 {
		t.Fatalf("decoded summary parameters %+v", decoded.Parameters)
	}

	md := readFile(t, dir, "summary.md")
	for _, want := range []string{"SIMULATED", "Main results (cold start 20s", "Cold-start sweep", "cold start 10s", "cold start 20s",
		"| victim_violation_pct |", "| runs | 2 | 2 |", "S2-keda-queue", "S7-ts-full", "## Parameters", "victim_violation.svg", "Threats to validity",
		"dispatch timeout 120s"} {
		if !strings.Contains(md, want) {
			t.Fatalf("summary.md lacks %q:\n%s", want, md)
		}
	}
	if !strings.Contains(md, "VTC fairness never engaged") && !strings.Contains(md, "max_counter_gap defined") {
		t.Fatalf("summary.md does not say whether VTC ordering engaged:\n%s", md)
	}
	for _, name := range []string{"victim_violation.svg", "cold_start_sweep.svg", "timeline_replicas.svg", "timeline_victim_ttft.svg"} {
		svg := readFile(t, dir, name)
		if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "simulated") {
			t.Fatalf("%s is not a labelled SVG", name)
		}
	}
}

// The default plan (every setup, sensitivity variants included) with a
// single run: every plot renders, the sensitivity variant gets its own
// sweep chart, and no cell claims a CI it does not have.
func TestRunAllSetupsOneRun(t *testing.T) {
	dir := t.TempDir()
	p := tinyPlan(dir)
	p.Setups, p.Runs = nil, 1
	sum, err := Run(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(sum.Results[0].Setups); n != len(sim.AllSetups)+len(sim.SensitivitySetups) {
		t.Fatalf("%d setups ran", n)
	}
	for _, name := range []string{"cold_start_sweep.svg", "cold_start_sweep_sensitivity.svg"} {
		if svg := readFile(t, dir, name); !strings.Contains(svg, "simulated") {
			t.Fatalf("%s is not a labelled SVG", name)
		}
	}
	sens := readFile(t, dir, "cold_start_sweep_sensitivity.svg")
	for _, s := range []string{sim.SetupFlowControl, sim.SetupFull, sim.SetupFlowControlOutstanding} {
		if !strings.Contains(sens, s) {
			t.Fatalf("sensitivity chart lacks %s", s)
		}
	}
	md := readFile(t, dir, "summary.md")
	if strings.Contains(md, "± n/a") || !strings.Contains(md, sim.SetupFlowControlOutstanding) || !strings.Contains(md, "weak by construction") {
		t.Fatalf("summary.md for one run:\n%s", md)
	}
}

func TestRunIsDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		if _, err := Run(tinyPlan(dir)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"runs.csv", "summary.json", "summary.md"} {
		if readFile(t, a, name) != readFile(t, b, name) {
			t.Fatalf("%s differs between identical plans", name)
		}
	}
}

func TestPlanValidation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Plan)
		want string
	}{
		{"no runs", func(p *Plan) { p.Runs = 0 }, "runs"},
		{"unknown setup", func(p *Plan) { p.Setups = []string{"S9"} }, "unknown setup"},
		{"duplicate setup", func(p *Plan) { p.Setups = []string{sim.SetupFull, sim.SetupFull} }, "twice"},
		{"no out dir", func(p *Plan) { p.OutDir = "" }, "output directory"},
		{"no default cold start", func(p *Plan) { p.DefaultColdStart = 0 }, "default cold start"},
		{"bad cold start", func(p *Plan) { p.ColdStarts = []time.Duration{-time.Second} }, "cold starts"},
		{"bad scenario", func(p *Plan) { p.Scenario.Tenants[0].Tenant = "Z" }, "not in the config"},
	}
	for _, c := range cases {
		p := tinyPlan(t.TempDir())
		c.edit(&p)
		if _, err := Run(p); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	p, err := tinyPlan("x").normalize()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ColdStarts) != 2 || p.ColdStarts[0] != 10*time.Second || p.ColdStarts[1] != 20*time.Second {
		t.Fatalf("cold starts = %v, want the sweep plus the default, sorted", p.ColdStarts)
	}
	p.Setups = nil
	want := append(slices.Clone(sim.AllSetups), sim.SensitivitySetups...)
	if p, _ = p.normalize(); !slices.Equal(p.Setups, want) {
		t.Fatalf("empty setups should mean all plus the sensitivity variants, got %v", p.Setups)
	}
}

func TestTranspose(t *testing.T) {
	in := eval.Table{Header: []string{"setup", "runs", "m"}, Rows: [][]string{{"S1", "5", "1.0"}, {"S2", "5", "2.0"}}}
	got := transpose(in)
	if strings.Join(got.Header, ",") != "metric,S1,S2" || len(got.Rows) != 2 ||
		strings.Join(got.Rows[0], ",") != "runs,5,5" || strings.Join(got.Rows[1], ",") != "m,1.0,2.0" {
		t.Fatalf("transpose = %+v", got)
	}
}
