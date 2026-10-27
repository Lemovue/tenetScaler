package realrun

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/model"
	"tenantscaler/internal/workload"
)

// TestMeasureLevelThroughput replays an exact closed loop: 3 workers, each
// sending back-to-back 5 s requests of 20 weighted tokens from phases 0,
// 1.7 and 3.4 s, measured over [5 s, 65 s). The true work rate is
// 3 x 20 / 5 = 12 weighted tokens/s. Every worker completes exactly 12
// requests inside the window, but only 11 of them also arrived inside it,
// so an arrival-and-completion estimator would read 11.
func TestMeasureLevelThroughput(t *testing.T) {
	base := time.Unix(1000, 0)
	winStart, winEnd := base.Add(5*time.Second), base.Add(65*time.Second)
	const dur = 5 * time.Second
	var reqs []sentRequest
	for w := range 3 {
		phase := time.Duration(w) * 1700 * time.Millisecond
		for sent := base.Add(phase); sent.Before(winEnd); sent = sent.Add(dur) {
			r := sentRequest{
				spec: workload.Spec{InputTokens: 10, OutputTokens: 5}, sent: sent, arrivalAbs: sent,
				rec: eval.Record{FirstToken: time.Second, Done: dur, InputTokens: 12, OutputTokens: 5},
				end: sent.Add(dur),
			}
			if r.end.After(winEnd) { // aborted at the window's end
				r.rec = eval.Record{Failed: true}
				r.end, r.failure = winEnd, "aborted"
			}
			reqs = append(reqs, r)
		}
	}
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 1000, TargetFraction: 0.99}
	lv := measureLevel(3, reqs, winStart, winEnd, slo)
	if lv.Completed != 36 || lv.Requests != 33 || lv.Failed != 0 {
		t.Fatalf("completed %d, latency sample %d, failed %d; want 36, 33 and 0 (the aborted ones end at the window's end)",
			lv.Completed, lv.Requests, lv.Failed)
	}
	if math.Abs(lv.WeightedTokensPerSec-12) > 1e-9 || math.Abs(lv.OutputTokensPerSec-3) > 1e-9 {
		t.Errorf("weighted %v output %v tokens/s, want 12 and 3", lv.WeightedTokensPerSec, lv.OutputTokensPerSec)
	}
	if lv.TTFTP50 != 1 || math.Abs(lv.PromptTokensPerWord-1.2) > 1e-9 || !lv.MeetsSLO {
		t.Errorf("level %+v", lv)
	}
}

func TestFitLoadFactorUnmeasured(t *testing.T) {
	for name, levels := range map[string][]Level{
		"no levels":    nil,
		"no level 1":   {{Concurrency: 2, TTFTP50: 1, TPOTP50: 0.1}},
		"zero medians": {{Concurrency: 1}, {Concurrency: 2}, {Concurrency: 4}},
	} {
		f := fitLoadFactor(levels, 4)
		if !math.IsNaN(f.TTFT) || !math.IsNaN(f.TPOT) || !math.IsNaN(f.Joint) || !math.IsNaN(f.TPOTGrowth) {
			t.Errorf("%s: fit %+v, want NaN", name, f)
		}
	}
	// One slot: the simulated engine has no load factor to fit.
	if f := fitLoadFactor([]Level{{Concurrency: 1, TTFTP50: 1, TPOTP50: 0.1}}, 1); !math.IsNaN(f.Joint) {
		t.Errorf("one slot: fit %+v", f)
	}
}

func TestCompareLevels(t *testing.T) {
	measured := []Level{
		{Concurrency: 1, TTFTP50: 0.6, TPOTP50: 0.020, WeightedTokensPerSec: 100, MeetsSLO: true},
		{Concurrency: 2, TTFTP50: 0.7, TPOTP50: 0.040, WeightedTokensPerSec: 120, MeetsSLO: true},
		{Concurrency: 3, TTFTP50: 0.8, TPOTP50: 0.060, WeightedTokensPerSec: 120, MeetsSLO: true},
	}
	sim := []Level{
		{Concurrency: 1, TTFTP50: 0.65, TPOTP50: 0.021, WeightedTokensPerSec: 90, MeetsSLO: true}, // within 25%
		{Concurrency: 2, TTFTP50: 1.4, TPOTP50: 0.040, WeightedTokensPerSec: 120, MeetsSLO: false},
	}
	got := compareLevels(measured, sim, 0.25)
	want := []string{
		"c=2 p50 TTFT: simulated 1.400 s vs measured 0.700 s (+100%)",
		"c=2 SLO verdict: simulated false vs measured true",
		"c=3: not simulated",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("mismatches:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	r := ProfileResult{Levels: measured[:1], Check: CalibrationCheck{Tolerance: 0.25, Levels: sim[:1]}}
	if v := r.checkVerdict(); !strings.Contains(v, "reproduces the measurement") || strings.Contains(v, "NOT") {
		t.Errorf("verdict %q", v)
	}
	r.Check.Mismatches = got
	if v := r.checkVerdict(); !strings.Contains(v, "does NOT reproduce") || !strings.Contains(v, "3 deviations") {
		t.Errorf("verdict %q", v)
	}
	if v := (ProfileResult{Check: CalibrationCheck{Skipped: "disabled by the plan"}}).checkVerdict(); !strings.Contains(v, "not run: disabled") {
		t.Errorf("verdict %q", v)
	}
}

// TestProfileRejectsGPU checks that a real profile refuses GPU offload
// before anything is launched (its report says "no GPU").
func TestProfileRejectsGPU(t *testing.T) {
	cfg := fastConfig()
	cfg.Backend.Kind = config.BackendLlama
	cfg.Backend.Llama.GPULayers = 8
	in := workload.LengthDist{Median: 20, Sigma: 0.3, Min: 4, Max: 40}
	_, err := Profile(context.Background(), ProfilePlan{Config: cfg, In: in, Out: in, Logger: quietLogger()})
	if err == nil || !strings.Contains(err.Error(), "gpuLayers is 8") {
		t.Fatalf("err %v, want a gpuLayers error", err)
	}
}
