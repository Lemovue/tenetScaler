package workload

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"tenantscaler/internal/model"
)

func steady(name string, rps, cv float64) TenantPattern {
	return TenantPattern{Tenant: name, Kind: KindSteady, BaseRPS: rps, PeakRPS: rps, CV: cv,
		Mix: PriorityMix{Standard: 1}, In: defaultIn, Out: defaultOut}
}

func TestGenerateDeterministic(t *testing.T) {
	for _, s := range []Scenario{DefaultScenario(), ReclaimScenario()} {
		a, b := Generate(s, 42), Generate(s, 42)
		if len(a) == 0 || !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: same seed gave different traces (%d vs %d specs)", s.Name, len(a), len(b))
		}
		if reflect.DeepEqual(a, Generate(s, 43)) {
			t.Errorf("%s: seeds 42 and 43 gave identical traces", s.Name)
		}
	}
}

func TestGenerateWellFormed(t *testing.T) {
	s := DefaultScenario()
	specs := Generate(s, 1)
	byTenant := map[string]TenantPattern{}
	for _, p := range s.Tenants {
		byTenant[p.Tenant] = p
	}
	for i, sp := range specs {
		if i > 0 && sp.Offset < specs[i-1].Offset {
			t.Fatalf("spec %d at %v before spec %d at %v: not sorted", i, sp.Offset, i-1, specs[i-1].Offset)
		}
		if sp.Offset < 0 || sp.Offset >= s.Duration {
			t.Fatalf("spec %d offset %v outside [0, %v)", i, sp.Offset, s.Duration)
		}
		p, ok := byTenant[sp.Tenant]
		if !ok {
			t.Fatalf("spec %d has unknown tenant %q", i, sp.Tenant)
		}
		if sp.InputTokens < p.In.Min || sp.InputTokens > p.In.Max || sp.OutputTokens < p.Out.Min || sp.OutputTokens > p.Out.Max {
			t.Fatalf("spec %d lengths %d/%d outside the tenant's ranges", i, sp.InputTokens, sp.OutputTokens)
		}
		switch sp.Priority {
		case model.PriorityCritical, model.PriorityStandard, model.PrioritySheddable:
		default:
			t.Fatalf("spec %d has priority %d", i, sp.Priority)
		}
	}
}

// gapStats returns the mean and coefficient of variation of the gaps
// between consecutive offsets.
func gapStats(specs []Spec) (mean, cv float64) {
	var gaps []float64
	for i := 1; i < len(specs); i++ {
		gaps = append(gaps, (specs[i].Offset - specs[i-1].Offset).Seconds())
	}
	for _, g := range gaps {
		mean += g
	}
	mean /= float64(len(gaps))
	var ss float64
	for _, g := range gaps {
		ss += (g - mean) * (g - mean)
	}
	return mean, math.Sqrt(ss/float64(len(gaps)-1)) / mean
}

func TestGenerateGammaArrivals(t *testing.T) {
	for _, cv := range []float64{0.5, 1, 1.5, 2} {
		s := Scenario{Duration: 10000 * time.Second, Tenants: []TenantPattern{steady("A", 2, cv)}}
		specs := Generate(s, 3)
		n := float64(len(specs))
		// A renewal process with rate 2 over 10000 s: about 20000 arrivals,
		// standard deviation sqrt(20000)*CV.
		if math.Abs(n-20000) > 4*math.Sqrt(20000)*cv {
			t.Errorf("CV %v: %v arrivals, want about 20000", cv, n)
		}
		mean, gotCV := gapStats(specs)
		if math.Abs(mean-0.5) > 0.03 {
			t.Errorf("CV %v: mean gap %.4f s, want 0.5 s", cv, mean)
		}
		if math.Abs(gotCV-cv)/cv > 0.07 {
			t.Errorf("CV %v: measured gap CV %.3f", cv, gotCV)
		}
	}
}

func TestGenerateFollowsRate(t *testing.T) {
	periodic := TenantPattern{Tenant: "B", Kind: KindPeriodic, BaseRPS: 1, PeakRPS: 4,
		PeriodSeconds: 100, BurstSeconds: 25, PhaseSeconds: 10, CV: 1.5,
		Mix: PriorityMix{Standard: 1}, In: defaultIn, Out: defaultOut}
	s := Scenario{Duration: 20000 * time.Second, Tenants: []TenantPattern{periodic}}
	in, out := 0, 0
	for _, sp := range Generate(s, 5) {
		if RateAt(periodic, sp.Offset.Seconds()) == 4 {
			in++
		} else {
			out++
		}
	}
	// 200 periods: 25 s at 4 req/s (20000 expected) and 75 s at 1 req/s (15000).
	if math.Abs(float64(in)-20000)/20000 > 0.05 || math.Abs(float64(out)-15000)/15000 > 0.05 {
		t.Errorf("arrivals in/out of bursts = %d/%d, want about 20000/15000", in, out)
	}
}

func TestDefaultScenarioSpikeCounts(t *testing.T) {
	s := DefaultScenario()
	start, end, _ := s.SpikeWindow("C")
	const seeds = 20
	inSpike, before := 0, 0
	for seed := int64(1); seed <= seeds; seed++ {
		for _, sp := range Generate(s, seed) {
			switch {
			case sp.Tenant != "C":
			case sp.Offset >= start && sp.Offset < end:
				inSpike++
			case sp.Offset < start:
				before++
			}
		}
	}
	// Expected per run: 5 req/s * 180 s = 900 in the spike, 1 req/s * 180 s before it.
	if got := float64(inSpike) / seeds; math.Abs(got-900)/900 > 0.05 {
		t.Errorf("C requests in spike = %.1f per run, want about 900", got)
	}
	if got := float64(before) / seeds; math.Abs(got-180)/180 > 0.1 {
		t.Errorf("C requests before spike = %.1f per run, want about 180", got)
	}
}

func TestGenerateZeroRates(t *testing.T) {
	if specs := Generate(Scenario{Duration: time.Hour, Tenants: []TenantPattern{steady("A", 0, 1)}}, 1); len(specs) != 0 {
		t.Errorf("zero rate produced %d specs", len(specs))
	}
	if specs := Generate(Scenario{Duration: 0, Tenants: []TenantPattern{steady("A", 5, 1)}}, 1); len(specs) != 0 {
		t.Errorf("zero duration produced %d specs", len(specs))
	}
	onlySpike := TenantPattern{Tenant: "C", Kind: KindSpike, BaseRPS: 0, PeakRPS: 3,
		SpikeStartSeconds: 100, SpikeDurationSeconds: 50, CV: 2, Mix: PriorityMix{Sheddable: 1}, In: defaultIn, Out: defaultOut}
	specs := Generate(Scenario{Duration: 300 * time.Second, Tenants: []TenantPattern{onlySpike}}, 9)
	if len(specs) == 0 {
		t.Fatal("spike-only tenant produced no requests")
	}
	for _, sp := range specs {
		if sp.Offset < 100*time.Second || sp.Offset >= 150*time.Second {
			t.Fatalf("arrival at %v outside the only non-zero window [100s, 150s)", sp.Offset)
		}
		if sp.Priority != model.PrioritySheddable {
			t.Fatalf("priority %d, want sheddable", sp.Priority)
		}
	}
}

func TestGenerateInvalidCVFallsBackToPoisson(t *testing.T) {
	bad := Scenario{Duration: 5000 * time.Second, Tenants: []TenantPattern{steady("A", 2, 0)}}
	_, cv := gapStats(Generate(bad, 11))
	if math.Abs(cv-1) > 0.07 {
		t.Errorf("CV 0 gave measured CV %.3f, want about 1 (Poisson)", cv)
	}
}

func TestSamplePriority(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	counts := map[model.Priority]int{}
	const n = 40000
	for range n {
		counts[samplePriority(PriorityMix{Critical: 1, Standard: 2, Sheddable: 1}, r)]++
	}
	want := map[model.Priority]float64{model.PriorityCritical: 0.25, model.PriorityStandard: 0.5, model.PrioritySheddable: 0.25}
	for p, frac := range want {
		if got := float64(counts[p]) / n; math.Abs(got-frac) > 0.01 {
			t.Errorf("priority %s fraction %.3f, want %.2f", p.Class(), got, frac)
		}
	}
	for _, mix := range []PriorityMix{{}, {Critical: -1, Sheddable: math.NaN()}} {
		if p := samplePriority(mix, r); p != model.PriorityStandard {
			t.Errorf("mix %+v gave %s, want standard", mix, p.Class())
		}
	}
}

func TestLengthDistSample(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	tests := []struct {
		name string
		d    LengthDist
		want int // every draw, or -1 to skip
	}{
		{"zero sigma gives the median", LengthDist{Median: 100, Min: 1, Max: 1000}, 100},
		{"clamped to max", LengthDist{Median: 1000, Sigma: 0.1, Min: 1, Max: 500}, 500},
		{"clamped to min", LengthDist{Median: 1, Sigma: 0.1, Min: 10, Max: 50}, 10},
		{"invalid median gives min", LengthDist{Median: -5, Sigma: 1, Min: 3, Max: 9}, 3},
		{"zero value gives one", LengthDist{}, 1},
	}
	for _, tc := range tests {
		for range 200 {
			if got := tc.d.sample(r); got != tc.want {
				t.Fatalf("%s: sample = %d, want %d", tc.name, got, tc.want)
			}
		}
	}
	// Median of a wide log-normal draw stays at Median.
	d := LengthDist{Median: 200, Sigma: 0.8, Min: 1, Max: 100000}
	below := 0
	const n = 20000
	for range n {
		if d.sample(r) < 200 {
			below++
		}
	}
	if frac := float64(below) / n; math.Abs(frac-0.5) > 0.02 {
		t.Errorf("fraction below the median = %.3f, want 0.5", frac)
	}
}

func TestDefaultMeanWeightedTokens(t *testing.T) {
	// The DefaultScenario doc comment sizes demand on ~600 weighted tokens per request.
	var sum float64
	specs := Generate(DefaultScenario(), 2)
	for _, sp := range specs {
		sum += model.WeightedTokens(float64(sp.InputTokens), float64(sp.OutputTokens))
	}
	if mean := sum / float64(len(specs)); mean < 560 || mean > 640 {
		t.Errorf("mean weighted tokens per request = %.1f, want about 600", mean)
	}
}

func TestGenerateWithSamples(t *testing.T) {
	s := DefaultScenario()
	s.Samples = []LengthSample{{In: 10, Out: 20}, {In: 0, Out: 7}, {In: -3, Out: 0}}
	allowed := map[[2]int]bool{{10, 20}: true, {0, 7}: true, {0, 1}: true}
	seen := map[[2]int]bool{}
	withSamples := Generate(s, 8)
	for _, sp := range withSamples {
		pair := [2]int{sp.InputTokens, sp.OutputTokens}
		if !allowed[pair] {
			t.Fatalf("lengths %v are not drawn from the samples", pair)
		}
		seen[pair] = true
	}
	if len(seen) != len(allowed) {
		t.Errorf("drew %d distinct samples, want all %d", len(seen), len(allowed))
	}
	// Lengths use their own stream: arrivals and priorities are unchanged.
	plain := Generate(DefaultScenario(), 8)
	if len(plain) != len(withSamples) {
		t.Fatalf("sample source changed the number of arrivals: %d vs %d", len(plain), len(withSamples))
	}
	for i := range plain {
		if plain[i].Offset != withSamples[i].Offset || plain[i].Priority != withSamples[i].Priority || plain[i].Tenant != withSamples[i].Tenant {
			t.Fatalf("spec %d changed arrival or priority when only the length source changed", i)
		}
	}
}

func TestTenantStreamsIndependent(t *testing.T) {
	base := DefaultScenario()
	changed := DefaultScenario()
	changed.Tenants[2].PeakRPS = 9 // only C changes
	onlyA := func(specs []Spec) []Spec {
		var out []Spec
		for _, sp := range specs {
			if sp.Tenant == "A" {
				out = append(out, sp)
			}
		}
		return out
	}
	if !reflect.DeepEqual(onlyA(Generate(base, 4)), onlyA(Generate(changed, 4))) {
		t.Error("changing tenant C moved tenant A's requests")
	}
}

func TestSampleGammaMoments(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for _, k := range []float64{0.25, 1, 4} {
		const n = 200000
		var sum, sumSq float64
		for range n {
			x := sampleGamma(r, k)
			if x < 0 || math.IsNaN(x) {
				t.Fatalf("k=%v: invalid draw %v", k, x)
			}
			sum += x
			sumSq += x * x
		}
		mean := sum / n
		variance := sumSq/n - mean*mean
		// Gamma(k, 1) has mean k and variance k.
		if math.Abs(mean-k)/k > 0.02 || math.Abs(variance-k)/k > 0.06 {
			t.Errorf("k=%v: mean %.4f variance %.4f, want %v and %v", k, mean, variance, k, k)
		}
	}
}
