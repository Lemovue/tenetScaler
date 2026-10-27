// Package forecast is TenantScaler's machine-learning component (requirement
// R17): a per-tenant demand forecaster. The controller feeds it one demand
// value per control cycle (one bucket) and, when the forecaster has earned
// trust, pre-scales for the predicted peak over the cold-start horizon, so a
// periodic burst finds replicas already loaded instead of waiting for them.
//
// The main model is additive Holt-Winters (triple exponential smoothing). It
// is compared against two baselines, Naive (last value) and SeasonalNaive
// (value one season ago), with Evaluate, which scores rolling one-step-ahead
// forecasts on a held-out tail without leaking future values. Tracker gates
// a live forecaster: the controller falls back to the reactive estimate
// whenever recent forecasts have over-predicted badly. Under-forecasts need
// no gate, because the controller acts on max(reactive, forecast).
//
// Every forecaster in this package treats the series as demand, so
// forecasts are clamped at zero, and a non-finite observation (a missing
// measurement) is replaced by the forecaster's own one-step forecast so that
// the seasonal position keeps advancing without corrupting the model state.
package forecast

import "math"

// Forecaster predicts the next values of a univariate series that it sees
// one value at a time.
type Forecaster interface {
	// Observe appends the next actual value of the series.
	Observe(v float64)
	// Forecast predicts the value h steps after the last observed one
	// (h >= 1; smaller h is treated as 1), using only the data observed so
	// far. The result is never negative.
	Forecast(h int) float64
	// Ready reports whether enough data has been observed for the model to
	// be fitted. Before that, forecasts are a documented fallback.
	Ready() bool
	// Name identifies the method in results tables.
	Name() string
}

// Epsilon is the smallest actual magnitude for which a relative error is
// defined. MAPE and Tracker skip actual values with |actual| < Epsilon,
// because a relative error against (near) zero demand is meaningless.
const Epsilon = 1e-6

// Naive forecasts every horizon as the last observed value. It is the
// baseline any learned forecaster must beat.
type Naive struct {
	last float64
	n    int
}

// NewNaive returns an empty Naive forecaster.
func NewNaive() *Naive { return &Naive{} }

// Observe records v as the latest value.
func (f *Naive) Observe(v float64) {
	if !finite(v) {
		v = f.Forecast(1)
	}
	f.last = v
	f.n++
}

// Forecast returns the last observed value (clamped at zero), or 0 before
// anything has been observed.
func (f *Naive) Forecast(int) float64 { return nonNegative(f.last) }

// Ready reports whether at least one value has been observed.
func (f *Naive) Ready() bool { return f.n > 0 }

// Name returns "naive".
func (f *Naive) Name() string { return "naive" }

// SeasonalNaive forecasts a value as the observation exactly one season
// earlier at the same seasonal position. It is exact on a purely periodic
// series and is the natural baseline for a seasonal model.
type SeasonalNaive struct {
	season int
	// ring holds the last season observations; position i holds the most
	// recent observation whose index is congruent to i modulo season.
	ring []float64
	n    int
}

// NewSeasonalNaive returns a SeasonalNaive forecaster for the given season
// length in observations. A season below 1 is treated as 1 (Naive).
func NewSeasonalNaive(season int) *SeasonalNaive {
	season = max(season, 1)
	return &SeasonalNaive{season: season, ring: make([]float64, season)}
}

// Observe appends v.
func (f *SeasonalNaive) Observe(v float64) {
	if !finite(v) {
		v = f.Forecast(1)
	}
	f.ring[f.n%f.season] = v
	f.n++
}

// Forecast returns the most recent observation at the seasonal position of
// the target index. Before one full season has been observed it falls back
// to the last value (Naive); before any observation it returns 0.
func (f *SeasonalNaive) Forecast(h int) float64 {
	switch {
	case f.n == 0:
		return 0
	case !f.Ready():
		return nonNegative(f.ring[f.n-1])
	}
	// The ring stores the most recent value at the target's seasonal
	// position.
	return nonNegative(f.ring[targetPos(f.n, steps(h), f.season)])
}

// Ready reports whether one full season has been observed.
func (f *SeasonalNaive) Ready() bool { return f.n >= f.season }

// Name returns "seasonal-naive".
func (f *SeasonalNaive) Name() string { return "seasonal-naive" }

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func nonNegative(v float64) float64 { return math.Max(v, 0) }

// steps normalises a forecast horizon: anything below 1 means one step.
func steps(h int) int { return max(h, 1) }

// targetPos returns the seasonal position (index modulo season) of the
// value h >= 1 steps after the last of n observations, that is of index
// n-1+h. Reducing h-1 modulo season before adding keeps the sum from
// overflowing for any h up to math.MaxInt.
func targetPos(n, h, season int) int { return (n + (h-1)%season) % season }
