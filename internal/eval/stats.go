package eval

import (
	"encoding/json"
	"math"
	"slices"
)

// Stat summarizes repeated measurements of one metric.
type Stat struct {
	// Mean and Std are the sample mean and sample standard deviation
	// (n-1 denominator). CI95 is the half-width of the 95% confidence
	// interval of the mean: TCritical95(N-1) * Std / sqrt(N).
	Mean, Std, CI95 float64
	// N counts the non-NaN values.
	N int
}

// Summarize computes mean, standard deviation and the t-distribution 95%
// confidence half-width of values, skipping NaNs. With no values every
// field is NaN; with one value Std and CI95 are NaN (undefined).
func Summarize(values []float64) Stat {
	xs := withoutNaN(values)
	n := len(xs)
	if n == 0 {
		return Stat{Mean: math.NaN(), Std: math.NaN(), CI95: math.NaN()}
	}
	var sum float64
	for _, v := range xs {
		sum += v
	}
	mean := sum / float64(n)
	if n == 1 {
		return Stat{Mean: mean, Std: math.NaN(), CI95: math.NaN(), N: 1}
	}
	var ss float64
	for _, v := range xs {
		ss += (v - mean) * (v - mean)
	}
	std := math.Sqrt(ss / float64(n-1))
	return Stat{Mean: mean, Std: std, CI95: TCritical95(n-1) * std / math.Sqrt(float64(n)), N: n}
}

// statJSON is the JSON form of Stat: the default field names, with null for
// values that are not finite.
type statJSON struct {
	Mean, Std, CI95 *float64
	N               int
}

// MarshalJSON encodes s under its field names, writing NaN and infinite
// values as null: encoding/json rejects NaN, and an undefined CI95 (one
// run) or Mean (no data) is routine.
func (s Stat) MarshalJSON() ([]byte, error) {
	return json.Marshal(statJSON{Mean: jsonFloat(s.Mean), Std: jsonFloat(s.Std), CI95: jsonFloat(s.CI95), N: s.N})
}

// UnmarshalJSON decodes the form MarshalJSON writes; null or a missing
// value becomes NaN.
func (s *Stat) UnmarshalJSON(data []byte) error {
	var v statJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*s = Stat{Mean: floatOrNaN(v.Mean), Std: floatOrNaN(v.Std), CI95: floatOrNaN(v.CI95), N: v.N}
	return nil
}

func jsonFloat(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func floatOrNaN(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

// tTable holds the two-sided 95% critical values t(0.975, df), df = 1..30.
var tTable = [...]float64{
	12.706205, 4.302653, 3.182446, 2.776445, 2.570582,
	2.446912, 2.364624, 2.306004, 2.262157, 2.228139,
	2.200985, 2.178813, 2.160369, 2.144787, 2.131450,
	2.119905, 2.109816, 2.100922, 2.093024, 2.085963,
	2.079614, 2.073873, 2.068658, 2.063899, 2.059539,
	2.055529, 2.051831, 2.048407, 2.045230, 2.042272,
}

// TCritical95 returns the Student-t quantile t(0.975, df): exact table
// values for df 1..30 and the normal value 1.96 above that. It returns NaN
// for df < 1.
func TCritical95(df int) float64 {
	switch {
	case df < 1:
		return math.NaN()
	case df <= len(tTable):
		return tTable[df-1]
	default:
		return 1.96
	}
}

// Jain returns Jain's fairness index (sum x)^2 / (n * sum x^2), skipping
// NaNs. It is 1 when all values are equal and 1/n when one value takes
// everything; it returns NaN for no values or when all values are zero.
func Jain(xs []float64) float64 {
	vals := withoutNaN(xs)
	var sum, sumSq float64
	for _, v := range vals {
		sum += v
		sumSq += v * v
	}
	if len(vals) == 0 || sumSq == 0 {
		return math.NaN()
	}
	return sum * sum / (float64(len(vals)) * sumSq)
}

// Percentile returns the nearest-rank p-th percentile of xs (p in percent,
// 0..100), skipping NaNs: the smallest value with at least p% of the values
// at or below it, i.e. sorted[ceil(p/100*n)-1], with p = 0 giving the
// minimum. It returns NaN for no values or p outside [0, 100].
func Percentile(xs []float64, p float64) float64 {
	vals := withoutNaN(xs)
	if len(vals) == 0 || !(p >= 0 && p <= 100) {
		return math.NaN()
	}
	slices.Sort(vals)
	rank := p * float64(len(vals)) / 100
	// Absorb floating-point error in p (99.9 is not exact in binary) so an
	// exact rank is not pushed up to the next value.
	if r := math.Round(rank); math.Abs(rank-r) < 1e-9 {
		rank = r
	}
	i := max(int(math.Ceil(rank)), 1)
	return vals[i-1]
}

// withoutNaN returns a copy of xs without NaNs.
func withoutNaN(xs []float64) []float64 {
	out := make([]float64, 0, len(xs))
	for _, v := range xs {
		if !math.IsNaN(v) {
			out = append(out, v)
		}
	}
	return out
}
