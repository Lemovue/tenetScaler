package realrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/stack"
	"tenantscaler/internal/workload"
)

// tinyScenario is a 2 s SIMULATED trace: A steady, B periodic, C spiking
// from 0.7 s to 1.5 s.
func tinyScenario() workload.Scenario {
	in := workload.LengthDist{Median: 12, Sigma: 0.3, Min: 4, Max: 24}
	out := workload.LengthDist{Median: 6, Sigma: 0.3, Min: 2, Max: 10}
	mix := workload.PriorityMix{Standard: 0.6, Sheddable: 0.4}
	return workload.Scenario{Name: "tiny", Duration: 2 * time.Second, Tenants: []workload.TenantPattern{
		{Tenant: "A", Kind: workload.KindSteady, BaseRPS: 4, PeakRPS: 4, CV: 1, Mix: mix, In: in, Out: out},
		{Tenant: "B", Kind: workload.KindPeriodic, BaseRPS: 3, PeakRPS: 6, PeriodSeconds: 1, BurstSeconds: 0.3, CV: 1, Mix: mix, In: in, Out: out},
		{Tenant: "C", Kind: workload.KindSpike, BaseRPS: 3, PeakRPS: 40, SpikeStartSeconds: 0.7, SpikeDurationSeconds: 0.8, CV: 1, Mix: mix, In: in, Out: out},
	}}
}

func tinyPlan(t *testing.T, setups []string, runs int) (Plan, *atomic.Int32) {
	guards := &atomic.Int32{}
	return Plan{
		Config: fastConfig(), Setups: setups, Runs: runs, Scenario: tinyScenario(), InitialReplicas: 1,
		OutDir: t.TempDir(), BaseSeed: 11, SampleEvery: 200 * time.Millisecond, DrainTimeout: 10 * time.Second,
		Guard:  func(context.Context) error { guards.Add(1); return nil },
		Logger: quietLogger(),
	}, guards
}

// TestRunEndToEndSimulated replays the tiny scenario on every supported
// setup twice and checks the written results.
func TestRunEndToEndSimulated(t *testing.T) {
	plan, guards := tinyPlan(t, stack.Supported, 2)
	sum, err := Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	n := len(stack.Supported) * 2
	if len(sum.Results) != n || int(guards.Load()) != n {
		t.Fatalf("%d results, %d guard checks, want %d", len(sum.Results), guards.Load(), n)
	}
	if !sum.Simulated || !strings.Contains(sum.Label, "SIMULATED") || strings.Contains(sum.Label, "REAL") {
		t.Errorf("label %q of a simulated run", sum.Label)
	}
	for _, r := range sum.Results {
		if r.Seed != plan.BaseSeed+int64(r.Run) {
			t.Errorf("%s run %d seed %d, want %d (same seed per run index)", r.Setup, r.Run, r.Seed, plan.BaseSeed+int64(r.Run))
		}
		if r.Requests == 0 || r.Metrics.Completed+r.Metrics.Shed+r.Metrics.Failed != r.Requests {
			t.Errorf("%s run %d: %d requests but %+v", r.Setup, r.Run, r.Requests, r.Metrics)
		}
		if len(r.data.Timeline) < 5 || r.DrainTimedOut {
			t.Errorf("%s run %d: %d samples, drain timed out %v", r.Setup, r.Run, len(r.data.Timeline), r.DrainTimedOut)
		}
		wantWarm := plan.InitialReplicas
		if r.Setup == sim.SetupStaticPartition {
			wantWarm = plan.Config.Pool.CapacityReplicas
		}
		if r.InitialReplicas != wantWarm {
			t.Errorf("%s run %d: warm pool %d, want %d", r.Setup, r.Run, r.InitialReplicas, wantWarm)
		}
		// Every replica the pool brought up has its (configured 50 ms) cold
		// start, including the warm pool.
		if len(r.ColdStartSeconds) < wantWarm {
			t.Errorf("%s run %d: %d cold starts, want at least the %d warm replicas", r.Setup, r.Run, len(r.ColdStartSeconds), wantWarm)
		}
		for _, c := range r.ColdStartSeconds {
			if c != 0.05 {
				t.Errorf("%s run %d: cold start %v, want the configured 0.05 s", r.Setup, r.Run, c)
			}
		}
		// The control phase is seeded per run index: the same for every
		// controlled setup of a run, within [0, cycle).
		if r.Setup == sim.SetupStaticPartition {
			if r.ControlPhaseSeconds != nil {
				t.Errorf("static partition has a control phase %v", *r.ControlPhaseSeconds)
			}
			continue
		}
		var ref RunResult // S2 of the same run index (setups run in a seeded order)
		for _, o := range sum.Results {
			if o.Run == r.Run && o.Setup == sim.SetupKEDAQueue {
				ref = o
			}
		}
		if r.ControlPhaseSeconds == nil || ref.ControlPhaseSeconds == nil || *r.ControlPhaseSeconds != *ref.ControlPhaseSeconds ||
			*r.ControlPhaseSeconds < 0 || *r.ControlPhaseSeconds >= plan.Config.Controller.CycleSeconds {
			t.Errorf("%s run %d: control phase %v, want %v in [0, cycle)", r.Setup, r.Run, r.ControlPhaseSeconds, ref.ControlPhaseSeconds)
		}
	}
	if len(sum.Stats) != len(stack.Supported) || sum.Stats[0].Runs != 2 || sum.Stats[0].Metrics["victim_violation_pct"].N == 0 {
		t.Fatalf("stats %+v", sum.Stats)
	}
	files := []string{"runs.csv", "summary.json", "summary.md", "victim_violation.svg", "timeline_replicas.svg",
		"timeline_victim_ttft.svg", "records/S2-keda-queue-run0.csv", "timeline/S7-ts-full-run1.csv",
		"decisions/S7-ts-full-run0.jsonl", "decisions/S5-ts-no-gateway-run1.jsonl"}
	for _, f := range files {
		if fi, err := os.Stat(filepath.Join(plan.OutDir, f)); err != nil || fi.Size() == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
	var decoded Summary
	data, err := os.ReadFile(filepath.Join(plan.OutDir, "summary.json"))
	if err != nil || json.Unmarshal(data, &decoded) != nil || len(decoded.Results) != n {
		t.Fatalf("summary.json does not round-trip: %v", err)
	}
	md, _ := os.ReadFile(filepath.Join(plan.OutDir, "summary.md"))
	for _, want := range []string{"SIMULATED", "What was NOT measured", "S7-ts-full (N=2)", "victim_violation_pct", "S1-cpu-hpa",
		"control phase s", "replicas started", "warm replicas"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("summary.md lacks %q", want)
		}
	}
}

func TestRunGuardFailureStops(t *testing.T) {
	plan, _ := tinyPlan(t, []string{"S7-ts-full"}, 1)
	plan.Guard = func(context.Context) error { return errors.New("RAM too high") }
	sum, err := Run(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "RAM too high") || len(sum.Results) != 0 {
		t.Fatalf("err %v, results %d", err, len(sum.Results))
	}
	if _, err := os.Stat(filepath.Join(plan.OutDir, "summary.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("summary.md written without any run: %v", err)
	}
}

func TestPlanValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(p *Plan)
		want string
	}{
		{"unsupported setup", func(p *Plan) { p.Setups = []string{"S1-cpu-hpa"} }, "unsupported setup"},
		{"duplicate setup", func(p *Plan) { p.Setups = []string{"S7-ts-full", "S7-ts-full"} }, "twice"},
		{"no setups", func(p *Plan) { p.Setups = nil }, "no setups"},
		{"zero runs", func(p *Plan) { p.Runs = 0 }, "runs must be"},
		{"no out dir", func(p *Plan) { p.OutDir = "" }, "output directory"},
		{"unknown tenant", func(p *Plan) { p.Scenario.Tenants[0].Tenant = "Z" }, "not in the config"},
		{"negative cooldown", func(p *Plan) { p.Cooldown = -time.Second }, ">= 0"},
		{"initial above capacity", func(p *Plan) { p.InitialReplicas = 9 }, "initial replicas 9 outside"},
		{"initial below min", func(p *Plan) { p.Config.Pool.MinReplicas, p.InitialReplicas = 2, 1 }, "initial replicas 1 outside"},
		{"gpu offload", func(p *Plan) {
			p.Config.Backend.Kind = config.BackendLlama
			p.Config.Backend.Llama.GPULayers = 20
		}, "gpuLayers is 20"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := tinyPlan(t, []string{"S7-ts-full"}, 1)
			tc.edit(&p)
			if _, err := p.normalize(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want %q", err, tc.want)
			}
		})
	}
	p, _ := tinyPlan(t, []string{"S7-ts-full"}, 1)
	p.InitialReplicas, p.DrainTimeout, p.SampleEvery, p.Guard, p.Logger = 0, 0, 0, nil, nil
	got, err := p.normalize()
	if err != nil || got.InitialReplicas != DefaultInitialReplicas || got.DrainTimeout != DefaultDrainTimeout ||
		got.SampleEvery != DefaultSampleEvery || got.Guard == nil || got.Logger == nil {
		t.Errorf("defaults not applied: %+v, %v", got, err)
	}
	// The default warm pool is clamped to the pool's range.
	p.Config.Pool.MinReplicas = 3
	if got, err := p.normalize(); err != nil || got.InitialReplicas != 3 {
		t.Errorf("default initial replicas with minReplicas 3: %d, %v", got.InitialReplicas, err)
	}
}

func TestParseCPUName(t *testing.T) {
	out := "\r\nHKEY_LOCAL_MACHINE\\HARDWARE\\DESCRIPTION\\System\\CentralProcessor\\0\r\n" +
		"    ProcessorNameString    REG_SZ    13th Gen Intel(R) Core(TM)   i5-14450HX  \r\n\r\n"
	if got := parseRegString(out); got != "13th Gen Intel(R) Core(TM) i5-14450HX" {
		t.Errorf("parseRegString = %q", got)
	}
	if got := parseRegString("nothing here"); got != "" {
		t.Errorf("parseRegString without a value = %q", got)
	}
	path := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(path, []byte("processor\t: 0\nmodel name\t: AMD EPYC 7B13\nflags\t: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cpuinfoModel(path); got != "AMD EPYC 7B13" {
		t.Errorf("cpuinfoModel = %q", got)
	}
	if got := CPUModel(); got == "" {
		t.Error("CPUModel is empty")
	}
}

// The setup order of a run index is a seeded shuffle: reproducible,
// different across run indexes, always a permutation.
func TestRunOrder(t *testing.T) {
	setups := []string{"S2", "S3", "S4", "S5", "S7"}
	a, b := runOrder(setups, 1, 0), runOrder(setups, 1, 0)
	if !slices.Equal(a, b) {
		t.Fatalf("not reproducible: %v vs %v", a, b)
	}
	differs := false
	for k := 1; k < 5; k++ {
		o := runOrder(setups, 1, k)
		if !slices.Equal(slices.Sorted(slices.Values(o)), setups) {
			t.Fatalf("run %d order %v is not a permutation", k, o)
		}
		differs = differs || !slices.Equal(o, a)
	}
	for k := range 5 {
		t.Logf("seed 1, run %d: %v", k, runOrder([]string{"S2", "S3", "S4", "S5", "S7"}, 1, k))
	}
	if !differs || !slices.Equal(setups, []string{"S2", "S3", "S4", "S5", "S7"}) {
		t.Fatalf("orders never differ across run indexes, or the input was modified: %v", setups)
	}
}

// The pre-registered claim pairs S7 and S4 by run index and decides from
// the 95% CI of the mean paired difference.
func TestBuildClaim(t *testing.T) {
	run := func(setup string, k int, violation float64) RunResult {
		return RunResult{Setup: setup, Run: k, Metrics: eval.RunMetrics{VictimViolationPct: violation, VictimShedAwareViolationPct: violation,
			NoisyShedPct: 30, ReplicaMinutes: 14}}
	}
	var rs []RunResult
	for k, v := range []float64{10, 12, 9, 11, 10} { // S7 misses 10% less than S4 in every run
		rs = append(rs, run(sim.SetupFull, k, v), run(sim.SetupFlowControl, k, v+10+float64(k%2)))
	}
	c := buildClaim(rs)
	if c == nil || len(c.Pairs) != 5 || c.Verdict != "supported" || !(c.Diff.Mean > 10 && c.Diff.Mean < 11) {
		t.Fatalf("claim %+v, want 5 pairs, a difference of 10.4 points, supported", c)
	}
	for i := range rs {
		if rs[i].Setup == sim.SetupFlowControl {
			rs[i].Metrics.VictimViolationPct -= 20 // now S4 is better
		}
	}
	c = buildClaim(rs)
	if c.Verdict != "contradicted" {
		t.Fatalf("verdict %q, want contradicted", c.Verdict)
	}
	if buildClaim(rs[:1]) != nil {
		t.Fatal("a claim without S4 runs")
	}
	if md := claimMarkdown(c, 30); !strings.Contains(md, "Verdict: contradicted") || !strings.Contains(md, "injected load delay") {
		t.Errorf("markdown:\n%s", md)
	}
	if _, err := json.Marshal(c); err != nil {
		t.Fatal(err)
	}
}
