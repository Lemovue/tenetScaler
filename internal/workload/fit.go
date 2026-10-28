package workload

import (
	"math"
	"math/rand/v2"
)

// meanDraws is how many lengths LengthDist.mean draws.
const meanDraws = 100_000

// Fit is how FitSamples adapted a trace's token lengths to a scenario.
type Fit struct {
	// InFactor and OutFactor multiply the trace's input and output lengths
	// before they are clamped.
	InFactor  float64 `json:"inFactor"`
	OutFactor float64 `json:"outFactor"`
	// TraceMeanIn and TraceMeanOut are the trace's own mean lengths,
	// MeanIn and MeanOut the fitted ones, TargetIn and TargetOut the
	// scenario's (the means of its LengthDists).
	TraceMeanIn  float64 `json:"traceMeanIn"`
	TraceMeanOut float64 `json:"traceMeanOut"`
	MeanIn       float64 `json:"meanIn"`
	MeanOut      float64 `json:"meanOut"`
	TargetIn     float64 `json:"targetIn"`
	TargetOut    float64 `json:"targetOut"`
	// ClampedPct is the percentage of samples with a length clamped to the
	// scenario's [Min, Max].
	ClampedPct float64 `json:"clampedPct"`
}

// FitSamples scales a trace's token lengths to a scenario whose lengths
// follow in and out, so that a trace recorded on large GPU deployments can
// be replayed on the scenario's replicas: every input length is multiplied
// by one factor and every output length by another, rounded and clamped to
// the LengthDist's [Min, Max], with the factors chosen so that the fitted
// means equal the LengthDists' means. The load the scenario was sized for
// is kept; what the trace decides is how it is spread over requests (each
// request keeps its rank among the trace's short and long ones, and its
// input stays paired with its output).
func FitSamples(samples []LengthSample, in, out LengthDist) ([]LengthSample, Fit) {
	fit := Fit{TargetIn: in.mean(), TargetOut: out.mean()}
	if len(samples) == 0 {
		return nil, fit
	}
	ins := make([]int, len(samples))
	outs := make([]int, len(samples))
	for i, s := range samples {
		ins[i], outs[i] = s.In, s.Out
	}
	fit.TraceMeanIn, fit.TraceMeanOut = meanInt(ins), meanInt(outs)
	fit.InFactor = fitFactor(ins, in, fit.TargetIn)
	fit.OutFactor = fitFactor(outs, out, fit.TargetOut)
	fitted := make([]LengthSample, len(samples))
	clamped := 0
	var sumIn, sumOut float64
	for i, s := range samples {
		fi, ci := scaleClamp(s.In, fit.InFactor, in)
		fo, co := scaleClamp(s.Out, fit.OutFactor, out)
		fitted[i] = LengthSample{In: fi, Out: fo}
		sumIn += float64(fi)
		sumOut += float64(fo)
		if ci || co {
			clamped++
		}
	}
	n := float64(len(samples))
	fit.MeanIn, fit.MeanOut = sumIn/n, sumOut/n
	fit.ClampedPct = 100 * float64(clamped) / n
	return fitted, fit
}

// fitFactor finds by bisection the factor k for which the mean of
// scaleClamp(v, k, d) over values is target; that mean grows with k, from
// d's lower bound at k = 0 to its upper bound for large k.
func fitFactor(values []int, d LengthDist, target float64) float64 {
	mean := func(k float64) float64 {
		sum := 0
		for _, v := range values {
			x, _ := scaleClamp(v, k, d)
			sum += x
		}
		return float64(sum) / float64(len(values))
	}
	lo, hi := 0.0, 1.0
	for mean(hi) < target && hi < 1e9 {
		lo, hi = hi, hi*2
	}
	for range 60 {
		mid := (lo + hi) / 2
		if mean(mid) < target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// scaleClamp returns round(v*k) clamped to d's [max(Min, 1), max(Max, Min)],
// as LengthDist.sample clamps, and whether it was clamped.
func scaleClamp(v int, k float64, d LengthDist) (int, bool) {
	lo := max(d.Min, 1)
	hi := max(d.Max, lo)
	x := math.Round(float64(v) * k)
	switch {
	case !(x >= float64(lo)): // also catches NaN
		return lo, true
	case x > float64(hi):
		return hi, true
	default:
		return int(x), false
	}
}

// mean is the mean length d draws, estimated from meanDraws draws of a
// fixed stream (deterministic).
func (d LengthDist) mean() float64 {
	r := rand.New(rand.NewPCG(1, 2))
	sum := 0
	for range meanDraws {
		sum += d.sample(r)
	}
	return float64(sum) / meanDraws
}

func meanInt(values []int) float64 {
	sum := 0
	for _, v := range values {
		sum += v
	}
	return float64(sum) / float64(len(values))
}
