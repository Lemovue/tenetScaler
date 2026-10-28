package workload

import (
	"math"
	"testing"
)

// FitSamples brings a trace much larger than the scenario down to the
// scenario's mean lengths, within its limits, keeping the order of the
// requests' sizes and the pairing of inputs and outputs.
func TestFitSamples(t *testing.T) {
	in, out := RealLengths()
	var samples []LengthSample
	for i := range 2000 { // inputs 10..20000, outputs 1000..1 (paired in reverse)
		samples = append(samples, LengthSample{In: 10 * (i + 1), Out: 1000 - i/2})
	}
	fitted, fit := FitSamples(samples, in, out)
	if len(fitted) != len(samples) {
		t.Fatalf("%d fitted samples, want %d", len(fitted), len(samples))
	}
	if math.Abs(fit.MeanIn-fit.TargetIn) > 1 || math.Abs(fit.MeanOut-fit.TargetOut) > 1 {
		t.Errorf("fitted means %.2f/%.2f, want the scenario's %.2f/%.2f", fit.MeanIn, fit.MeanOut, fit.TargetIn, fit.TargetOut)
	}
	if fit.InFactor >= 1 || fit.OutFactor >= 1 || fit.TraceMeanIn != 10005 {
		t.Errorf("fit %+v: want both factors below 1 and the trace's mean input 10005", fit)
	}
	for i, s := range fitted {
		if s.In < in.Min || s.In > in.Max || s.Out < out.Min || s.Out > out.Max {
			t.Fatalf("sample %d = %+v outside [%d, %d] x [%d, %d]", i, s, in.Min, in.Max, out.Min, out.Max)
		}
		if i > 0 && (s.In < fitted[i-1].In || s.Out > fitted[i-1].Out) {
			t.Fatalf("samples %d and %d changed order: %+v then %+v", i-1, i, fitted[i-1], s)
		}
	}
	if fit.ClampedPct <= 0 || fit.ClampedPct >= 100 {
		t.Errorf("clamped %.1f%%: this spread must clamp some samples, not all", fit.ClampedPct)
	}
	again, fit2 := FitSamples(samples, in, out)
	if fit2 != fit || len(again) != len(fitted) || again[777] != fitted[777] {
		t.Error("FitSamples is not deterministic")
	}
}

// A trace with smaller requests than the scenario is scaled up, and one
// that is all zeros ends at the scenario's minimum.
func TestFitSamplesScalesUp(t *testing.T) {
	in, out := RealLengths()
	_, fit := FitSamples([]LengthSample{{In: 5, Out: 2}, {In: 10, Out: 4}, {In: 20, Out: 8}}, in, out)
	if fit.InFactor <= 1 || fit.OutFactor <= 1 || math.Abs(fit.MeanIn-fit.TargetIn) > 5 {
		t.Errorf("fit %+v: want factors above 1 and the scenario's mean input", fit)
	}
	zeros, _ := FitSamples([]LengthSample{{In: 0, Out: 0}}, in, out)
	if zeros[0].In != in.Min || zeros[0].Out != out.Min {
		t.Errorf("an all-zero trace fits to %+v, want the minimums %d/%d", zeros[0], in.Min, out.Min)
	}
	if none, fit := FitSamples(nil, in, out); none != nil || fit.TargetIn <= 0 {
		t.Errorf("no samples: %v, %+v", none, fit)
	}
}
