package forecast

import "math"

// Metrics summarises rolling one-step-ahead forecast errors on a held-out
// tail of a series.
type Metrics struct {
	// MAPE is the mean absolute percentage error, in percent, over scored
	// points with |actual| >= Epsilon (others are skipped); NaN when no
	// point qualifies.
	MAPE float64
	// SMAPE is the symmetric MAPE, in percent (0..200):
	// 200*|p-a|/(|p|+|a|), where a point with p = a = 0 contributes 0.
	SMAPE float64
	// MAE is the mean absolute error in the series' own unit.
	MAE float64
	// N is the number of scored points (finite actuals in the tail).
	N int
}

// Evaluate scores f on series without leakage. It observes series[:train]
// (the training part) and then, for every t >= train, asks for Forecast(1)
// BEFORE observing series[t], so each prediction uses only values with
// index < t. train is clamped to [0, len(series)]. Non-finite actuals in the
// tail are not scored but are still passed to Observe (the forecaster
// imputes them). With nothing scored, all errors are NaN and N is 0.
//
// f must be freshly constructed: Evaluate assumes it has seen nothing yet.
func Evaluate(series []float64, train int, f Forecaster) Metrics {
	train = min(max(train, 0), len(series))
	for _, v := range series[:train] {
		f.Observe(v)
	}
	var acc errorSums
	for _, actual := range series[train:] {
		acc.add(f.Forecast(1), actual)
		f.Observe(actual)
	}
	return acc.metrics()
}

// errorSums accumulates the sums behind Metrics.
type errorSums struct {
	absErr, pctErr, symErr float64
	n, pctN                int
}

func (s *errorSums) add(predicted, actual float64) {
	if !finite(actual) {
		return
	}
	diff := math.Abs(predicted - actual)
	s.absErr += diff
	s.n++
	if math.Abs(actual) >= Epsilon {
		s.pctErr += diff / math.Abs(actual)
		s.pctN++
	}
	if denom := math.Abs(predicted) + math.Abs(actual); denom > 0 {
		s.symErr += 2 * diff / denom
	}
}

func (s *errorSums) metrics() Metrics {
	m := Metrics{MAPE: math.NaN(), SMAPE: math.NaN(), MAE: math.NaN(), N: s.n}
	if s.n > 0 {
		m.MAE = s.absErr / float64(s.n)
		m.SMAPE = 100 * s.symErr / float64(s.n)
	}
	if s.pctN > 0 {
		m.MAPE = 100 * s.pctErr / float64(s.pctN)
	}
	return m
}
