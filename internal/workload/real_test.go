package workload

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"tenantscaler/internal/model"
)

func TestRealScenarios(t *testing.T) {
	tests := []struct {
		name     string
		sc       Scenario
		duration time.Duration
	}{
		{"real", RealScenario(), 240 * time.Second},
		{"real-demo", RealDemoScenario(), 120 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := tc.sc
			if err := sc.Validate(); err != nil {
				t.Fatal(err)
			}
			if sc.Name != tc.name || sc.Duration != tc.duration {
				t.Fatalf("name %q duration %v, want %q and %v", sc.Name, sc.Duration, tc.name, tc.duration)
			}
			start, end, ok := sc.SpikeWindow("C")
			if !ok {
				t.Fatal("C is not a spike tenant")
			}
			if mid := (start + end) / 2; mid < sc.Duration/3 || mid > 2*sc.Duration/3 || start < sc.Duration/4 || end > 3*sc.Duration/4 {
				t.Errorf("spike %v-%v is not in the middle of %v", start, end, sc.Duration)
			}
			a := Generate(sc, 42)
			if b := Generate(sc, 42); !reflect.DeepEqual(a, b) {
				t.Fatal("Generate is not deterministic")
			}
			if c := Generate(sc, 43); reflect.DeepEqual(a, c) {
				t.Error("different seeds gave the same trace")
			}
			checkRealLengths(t, a)
			// The spike must multiply the offered load (weighted tokens/s)
			// several times over the base load.
			base, spike := weightedRate(a, 0, start), weightedRate(a, start, end)
			if !(spike > 2.5*base) {
				t.Errorf("spike load %.0f weighted tok/s is not > 2.5 x base load %.0f", spike, base)
			}
		})
	}
}

// measuredC is the per-replica capacity C in weighted tokens/s that
// `tenantscaler profile -config config/real.yaml` measured on the
// development machine (results/real/profile.md, 2026-10-26); the rate
// derivation in real.go uses the same value. Update both after
// re-profiling.
const measuredC = 117

// TestRealScenarioSizing checks the sizing documented in real.go on the
// traces the real runs replay (seeds 1-5): the base load (before the
// spike) fits in 2 replicas in every run and in 80% of them on average,
// and in every run the spike offers more than the full 4-replica pool
// serves.
func TestRealScenarioSizing(t *testing.T) {
	const seeds = 5
	for _, sc := range []Scenario{RealScenario(), RealDemoScenario()} {
		start, end, ok := sc.SpikeWindow("C")
		if !ok {
			t.Fatalf("%s: C is not a spike tenant", sc.Name)
		}
		meanBase := 0.0
		for seed := int64(1); seed <= seeds; seed++ {
			trace := Generate(sc, seed)
			base, spike := weightedRate(trace, 0, start), weightedRate(trace, start, end)
			meanBase += base / seeds
			if !(base < 2*measuredC) {
				t.Errorf("%s seed %d: base load %.0f weighted tok/s exceeds 2 replicas (%d)", sc.Name, seed, base, 2*measuredC)
			}
			if !(spike > 4*measuredC) {
				t.Errorf("%s seed %d: spike load %.0f weighted tok/s does not overload the 4-replica pool (%d)", sc.Name, seed, spike, 4*measuredC)
			}
		}
		if !(meanBase < 0.8*2*measuredC) {
			t.Errorf("%s: mean base load %.0f weighted tok/s, want < 80%% of 2 replicas (%.0f)", sc.Name, meanBase, 0.8*2*measuredC)
		}
	}
}

// checkRealLengths checks the length bounds and medians of a real trace.
func checkRealLengths(t *testing.T, specs []Spec) {
	t.Helper()
	in, out := RealLengths()
	var ins, outs []int
	for _, s := range specs {
		if s.InputTokens < in.Min || s.InputTokens > in.Max || s.OutputTokens < out.Min || s.OutputTokens > out.Max {
			t.Fatalf("spec %+v outside the length bounds", s)
		}
		ins, outs = append(ins, s.InputTokens), append(outs, s.OutputTokens)
	}
	var weighted float64
	for _, s := range specs {
		weighted += model.WeightedTokens(float64(s.InputTokens), float64(s.OutputTokens))
	}
	// The rate derivation in real.go assumes about 186 weighted tokens per
	// request.
	if mean := weighted / float64(len(specs)); mean < 170 || mean > 200 {
		t.Errorf("mean weighted tokens per request %.1f, want about 186", mean)
	}
	slices.Sort(ins)
	slices.Sort(outs)
	if mi, mo := ins[len(ins)/2], outs[len(outs)/2]; mi < 70 || mi > 90 || mo < 42 || mo > 54 {
		t.Errorf("median lengths in=%d out=%d, want about 80 and 48", mi, mo)
	}
}

// weightedRate is the offered weighted tokens/s of the requests arriving
// in [from, to).
func weightedRate(specs []Spec, from, to time.Duration) float64 {
	var sum float64
	for _, s := range specs {
		if s.Offset >= from && s.Offset < to {
			sum += model.WeightedTokens(float64(s.InputTokens), float64(s.OutputTokens))
		}
	}
	return sum / (to - from).Seconds()
}
