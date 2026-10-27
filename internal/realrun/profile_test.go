package realrun

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/model"
	"tenantscaler/internal/workload"
)

// fastConfig is a SIMULATED config whose replicas start in 50 ms and
// answer in milliseconds, for tests.
func fastConfig() config.Config {
	cfg := config.Default()
	cfg.Pool.CapacityReplicas, cfg.Pool.MinReplicas = 3, 1
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	cfg.Tenants = []model.TenantSpec{
		{Name: "A", GuaranteedReplicas: 1, MaxReplicas: 3, Priority: 100, SLO: slo},
		{Name: "B", GuaranteedReplicas: 1, MaxReplicas: 3, Priority: 100, SLO: slo},
		{Name: "C", GuaranteedReplicas: 1, MaxReplicas: 3, Priority: 100, SLO: slo},
	}
	cfg.Controller.CycleSeconds = 0.2
	cfg.Controller.ScaleDownWindowSeconds = 1
	cfg.Engine = config.EngineFromParams(engine.Params{
		PrefillOverhead: 2 * time.Millisecond, PrefillPerToken: 50 * time.Microsecond,
		InterTokenLatency: 2 * time.Millisecond, MaxNumSeqs: 2, TimeFactorUnderLoad: 1.5,
		StartupDuration: 50 * time.Millisecond, JitterFraction: 0.05,
	})
	return cfg
}

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestProfileSimulated(t *testing.T) {
	plan := ProfilePlan{
		Config: fastConfig(),
		In:     workload.LengthDist{Median: 20, Sigma: 0.3, Min: 4, Max: 40},
		Out:    workload.LengthDist{Median: 8, Sigma: 0.3, Min: 2, Max: 12},
		// Levels 1..4 of 300 ms each: slots (2) + 2.
		LevelDuration: 300 * time.Millisecond, Warmup: 100 * time.Millisecond, CheckLevelDuration: 300 * time.Millisecond,
		PrefillRequests: 2, Seed: 7, LogDir: t.TempDir(), Logger: quietLogger(),
	}
	res, err := Profile(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Engine.Simulated || !strings.Contains(res.Label(), "SIMULATED") {
		t.Fatalf("simulated profile not labelled simulated: %q", res.Label())
	}
	if len(res.Levels) != 4 || len(res.Prefill) != 2 {
		t.Fatalf("levels %d prefill %d, want 4 and 2", len(res.Levels), len(res.Prefill))
	}
	for _, lv := range res.Levels {
		if lv.Requests == 0 || lv.Completed < lv.Requests || lv.Failed != 0 || !(lv.WeightedTokensPerSec > 0) {
			t.Errorf("level %+v: want completed requests, no failures and throughput", lv)
		}
	}
	// The calibration check drove a simulated replica running the
	// calibrated parameters through the same levels.
	if !res.Check.Ran() || len(res.Check.Levels) != 4 || res.Check.Tolerance != calibrationTolerance {
		t.Fatalf("calibration check %+v", res.Check)
	}
	if c := res.Calibrated; math.IsNaN(res.LoadFit.TPOT) || math.IsNaN(res.LoadFit.TTFT) ||
		c.TimeFactorUnderLoad != factor(res.LoadFit.TPOT) || c.PrefillFactorUnderLoad != factor(res.LoadFit.TTFT) {
		t.Errorf("load fit %+v, calibrated factors decode %v prefill %v", res.LoadFit, c.TimeFactorUnderLoad, c.PrefillFactorUnderLoad)
	}
	if res.Prefill[0].Words != DefaultShortPrompt || res.Prefill[1].Words != 40 {
		t.Errorf("prefill lengths %+v, want %d words and In.Max = 40", res.Prefill, DefaultShortPrompt)
	}
	if math.IsNaN(res.CapacityTokensPerSec) || res.CapacityConcurrency < 1 {
		t.Errorf("no capacity derived: %v at %d", res.CapacityTokensPerSec, res.CapacityConcurrency)
	}
	if err := res.Validate(); err != nil {
		t.Errorf("calibrated engine invalid: %v (%+v)", err, res.Calibrated)
	}
	if res.Calibrated.MaxNumSeqs != 2 || res.Calibrated.StartupDuration != "50ms" {
		t.Errorf("calibrated %+v: want maxNumSeqs 2 and the measured 50ms cold start", res.Calibrated)
	}
	var text bytes.Buffer
	if err := res.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "replicaCapacityTokensPerSec:") || !strings.Contains(text.String(), "interTokenLatency:") {
		t.Errorf("text output lacks the YAML snippet:\n%s", text.String())
	}
	dir := t.TempDir()
	path, err := res.WriteFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(path)
	if err != nil || filepath.Base(path) != "profile.md" || !bytes.Contains(md, []byte("SIMULATED")) {
		t.Fatalf("profile.md %s: %v\n%s", path, err, md)
	}
	for _, want := range []string{"## Calibration check", "measured / SIMULATED", "calibrated from this replica",
		"prompt tokens/word", "completed in window", "least squares"} {
		if !bytes.Contains(md, []byte(want)) {
			t.Errorf("profile.md lacks %q", want)
		}
	}
	if bytes.Contains(md, []byte("reproducing this replica")) {
		t.Error("profile.md claims the calibration reproduces the replica without the check saying so")
	}
}

func TestProfileDerive(t *testing.T) {
	r := ProfileResult{
		Slots: 4, ColdStart: 1156 * time.Millisecond, CapacityTokensPerSec: math.NaN(),
		Plan:    ProfilePlan{Config: fastConfig()},
		Prefill: []PrefillPoint{{Words: 16, TTFTP50: 0.14}, {Words: 160, TTFTP50: 1.292}},
		// TTFT does not grow with concurrency (fit 1), TPOT grows linearly
		// to 4x at the 4 slots (fit 4).
		Levels: []Level{
			{Concurrency: 1, WeightedTokensPerSec: 100, TTFTP50: 0.5, TPOTP50: 0.02, MeetsSLO: true},
			{Concurrency: 2, WeightedTokensPerSec: 110, TTFTP50: 0.5, TPOTP50: 0.04, MeetsSLO: true},
			{Concurrency: 3, WeightedTokensPerSec: 115, TTFTP50: 0.5, TPOTP50: 0.06, MeetsSLO: false},
			{Concurrency: 4, WeightedTokensPerSec: 120, TTFTP50: 0.5, TPOTP50: 0.08, MeetsSLO: true},
			{Concurrency: 5, WeightedTokensPerSec: 121, TTFTP50: 2.5, TPOTP50: 0.09, MeetsSLO: false},
		},
	}
	r.derive()
	if r.CapacityTokensPerSec != 120 || r.CapacityConcurrency != 4 {
		t.Errorf("C = %v at %d, want 120 at the highest level meeting the SLO (4)", r.CapacityTokensPerSec, r.CapacityConcurrency)
	}
	c := r.Calibrated
	// slope (1.292-0.14)/(160-16) = 8ms per word; overhead 0.14 - 16*0.008 = 12ms.
	// Load factor: x = 1/3, 2/3, 1 at c = 2..4; TTFT ratios 1, TPOT ratios
	// 2, 3, 4: prefill factor 1, decode factor 4. Joint (reference only):
	// F-1 = (1/3*1 + 2/3*2 + 1*3) / (2 * (1/9 + 4/9 + 1)) = 1.5.
	if c.PrefillPerToken != "8ms" || c.PrefillOverhead != "12ms" || c.InterTokenLatency != "20ms" ||
		c.TimeFactorUnderLoad != 4 || c.PrefillFactorUnderLoad != 1 || c.MaxNumSeqs != 4 || c.StartupDuration != "1.156s" {
		t.Errorf("calibrated %+v", c)
	}
	const eps = 1e-9
	if f := r.LoadFit; math.Abs(f.TTFT-1) > eps || math.Abs(f.TPOT-4) > eps || math.Abs(f.Joint-2.5) > eps ||
		math.Abs(f.TTFTGrowth-1) > eps || math.Abs(f.TPOTGrowth-4) > eps {
		t.Errorf("load fit %+v, want TTFT 1, TPOT 4, joint 2.5, growth 1 and 4", f)
	}
	if err := r.Validate(); err != nil {
		t.Error(err)
	}
	none := ProfileResult{CapacityTokensPerSec: math.NaN(), Plan: ProfilePlan{Config: fastConfig()},
		Levels: []Level{{Concurrency: 1, TPOTP50: 0.02}}}
	none.derive()
	if !math.IsNaN(none.CapacityTokensPerSec) || strings.Contains(none.YAMLSnippet(), "replicaCapacityTokensPerSec") {
		t.Errorf("no level meets the SLO, yet C = %v:\n%s", none.CapacityTokensPerSec, none.YAMLSnippet())
	}
}
