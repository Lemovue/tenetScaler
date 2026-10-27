package forecast

import (
	"math"
	"slices"
	"testing"
)

// spy is a Forecaster that records every call. Its observations are values
// of the form 1000+index, so each one identifies its own position in the
// series. It forecasts the last observed value.
type spy struct {
	t        *testing.T
	observed []float64
	// seenAtForecast[k] is how many values had been observed when the k-th
	// Forecast call was made.
	seenAtForecast []int
	horizons       []int
}

func (s *spy) Observe(v float64) {
	if want := 1000 + float64(len(s.observed)); v != want {
		s.t.Errorf("observation #%d is %v, want %v (out of order or skipped)", len(s.observed), v, want)
	}
	s.observed = append(s.observed, v)
}

func (s *spy) Forecast(h int) float64 {
	s.seenAtForecast = append(s.seenAtForecast, len(s.observed))
	s.horizons = append(s.horizons, h)
	if len(s.observed) == 0 {
		return 0
	}
	return s.observed[len(s.observed)-1]
}

func (s *spy) Ready() bool  { return len(s.observed) > 0 }
func (s *spy) Name() string { return "spy" }

// TestEvaluateNoLeakage proves that Evaluate forecasts index t using only
// indices < t: the forecast for t is requested when exactly t values (0..t-1)
// have been observed, and series[t] is observed only afterwards.
func TestEvaluateNoLeakage(t *testing.T) {
	series := make([]float64, 30)
	for i := range series {
		series[i] = 1000 + float64(i)
	}
	original := slices.Clone(series)
	const train = 12
	s := &spy{t: t}
	m := Evaluate(series, train, s)

	if len(s.seenAtForecast) != len(series)-train {
		t.Fatalf("Forecast called %d times, want %d (one per held-out point)", len(s.seenAtForecast), len(series)-train)
	}
	for k, seen := range s.seenAtForecast {
		target := train + k
		if seen != target {
			t.Errorf("forecast for index %d made after observing %d values, want exactly %d (indices 0..%d)",
				target, seen, target, target-1)
		}
		if s.horizons[k] != 1 {
			t.Errorf("forecast for index %d used horizon %d, want 1", target, s.horizons[k])
		}
	}
	if !slices.Equal(s.observed, series) {
		t.Errorf("observed %v, want every value exactly once in order", s.observed)
	}
	if !slices.Equal(series, original) {
		t.Error("Evaluate modified the series")
	}
	// The spy predicts the previous value and the series rises by 1 per
	// step, so an honest evaluation has MAE exactly 1. Observing series[t]
	// before forecasting it would make the spy perfect (MAE 0).
	if m.MAE != 1 || m.N != len(series)-train {
		t.Errorf("metrics = %+v, want MAE 1 over %d points", m, len(series)-train)
	}
}

func TestEvaluateMetricsByHand(t *testing.T) {
	// Naive predictions for t=1..4: 10, 20, 10, 0 against 20, 10, 0, 5.
	// Absolute errors 10, 10, 10, 5; percentage errors 0.5, 1, (skipped:
	// actual 0), 1; symmetric errors 20/30, 20/30, 20/10, 10/5.
	m := Evaluate([]float64{10, 20, 10, 0, 5}, 1, NewNaive())
	want := Metrics{
		MAPE:  100 * (0.5 + 1 + 1) / 3,
		SMAPE: 100 * (20.0/30 + 20.0/30 + 2 + 2) / 4,
		MAE:   35.0 / 4,
		N:     4,
	}
	if m.N != want.N || !near(m.MAPE, want.MAPE) || !near(m.SMAPE, want.SMAPE) || !near(m.MAE, want.MAE) {
		t.Errorf("Evaluate = %+v, want %+v", m, want)
	}
}

func TestEvaluateEdgeCases(t *testing.T) {
	series := []float64{4, 6, 8}
	for _, train := range []int{3, 10} {
		m := Evaluate(series, train, NewNaive())
		if m.N != 0 || !math.IsNaN(m.MAPE) || !math.IsNaN(m.SMAPE) || !math.IsNaN(m.MAE) {
			t.Errorf("train=%d: %+v, want N=0 and NaN errors", train, m)
		}
	}
	// A negative train is clamped to 0: index 0 is forecast from nothing (0).
	if m := Evaluate(series, -2, NewNaive()); m.N != 3 || !near(m.MAE, (4+2+2)/3.0) {
		t.Errorf("train=-2: %+v, want N=3 and MAE %v", m, (4+2+2)/3.0)
	}
	// A NaN actual is not scored, and Naive imputes it with its forecast.
	if m := Evaluate([]float64{10, math.NaN(), 10}, 1, NewNaive()); m.N != 1 || m.MAE != 0 {
		t.Errorf("NaN in tail: %+v, want N=1 and MAE 0", m)
	}
	// All-zero actuals: MAPE undefined, SMAPE 0 for perfect zero forecasts.
	if m := Evaluate([]float64{0, 0, 0}, 1, NewNaive()); !math.IsNaN(m.MAPE) || m.SMAPE != 0 || m.MAE != 0 {
		t.Errorf("all zeros: %+v, want MAPE NaN, SMAPE 0, MAE 0", m)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }
