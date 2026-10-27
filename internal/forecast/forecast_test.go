package forecast

import (
	"math"
	"math/big"
	"testing"
)

// seasonalTrend is a clean additive series: level 100, trend 0.5 per step
// and a sine season of amplitude 30 and length season.
func seasonalTrend(n, season int) []float64 {
	out := make([]float64, n)
	for t := range out {
		out[t] = trueValue(t, season)
	}
	return out
}

func trueValue(t, season int) float64 {
	return 100 + 0.5*float64(t) + 30*math.Sin(2*math.Pi*float64(t)/float64(season))
}

func observeAll(f Forecaster, series []float64) {
	for _, v := range series {
		f.Observe(v)
	}
}

func TestReadyTimingAndNames(t *testing.T) {
	const season = 5
	cases := []struct {
		f          Forecaster
		readyAfter int
		name       string
	}{
		{NewNaive(), 1, "naive"},
		{NewSeasonalNaive(season), season, "seasonal-naive"},
		{NewHoltWinters(0.5, 0.1, 0.3, season), 2 * season, "holt-winters"},
	}
	for _, c := range cases {
		if c.f.Name() != c.name {
			t.Errorf("Name() = %q, want %q", c.f.Name(), c.name)
		}
		if c.f.Ready() {
			t.Errorf("%s: ready before any observation", c.name)
		}
		for i := 1; i <= 3*season; i++ {
			c.f.Observe(float64(10 + i%season))
			if got, want := c.f.Ready(), i >= c.readyAfter; got != want {
				t.Errorf("%s: Ready() after %d observations = %v, want %v", c.name, i, got, want)
			}
		}
	}
}

func TestFallbackBeforeReady(t *testing.T) {
	for _, f := range []Forecaster{NewNaive(), NewSeasonalNaive(4), NewHoltWinters(0.5, 0.1, 0.3, 4)} {
		if got := f.Forecast(1); got != 0 {
			t.Errorf("%s: Forecast before any data = %v, want 0", f.Name(), got)
		}
		f.Observe(7)
		f.Observe(3)
		if got := f.Forecast(2); got != 3 {
			t.Errorf("%s: fallback forecast = %v, want last value 3", f.Name(), got)
		}
	}
}

func TestSeasonalNaiveExactOnPureSeasonalSeries(t *testing.T) {
	pattern := []float64{3, 7, 1, 9, 4}
	season := len(pattern)
	series := make([]float64, 6*season)
	for i := range series {
		series[i] = pattern[i%season]
	}
	m := Evaluate(series, season, NewSeasonalNaive(season))
	if m.N != 5*season || m.MAE != 0 || m.MAPE != 0 || m.SMAPE != 0 {
		t.Fatalf("SeasonalNaive on a pure seasonal series: %+v, want zero error over %d points", m, 5*season)
	}
	f := NewSeasonalNaive(season)
	observeAll(f, series[:13])
	for h := 1; h <= 3*season; h++ {
		if got, want := f.Forecast(h), pattern[(13-1+h)%season]; got != want {
			t.Errorf("Forecast(%d) = %v, want %v", h, got, want)
		}
	}
}

func TestForecastsNeverNegative(t *testing.T) {
	declining := make([]float64, 40)
	for i := range declining {
		declining[i] = math.Max(0, 200-6*float64(i)+5*float64(i%4))
	}
	for _, f := range []Forecaster{NewNaive(), NewSeasonalNaive(4), NewHoltWinters(0.5, 0.3, 0.3, 4)} {
		observeAll(f, declining)
		for h := 0; h <= 60; h++ {
			if got := f.Forecast(h); got < 0 || !finite(got) {
				t.Errorf("%s: Forecast(%d) = %v, want finite and >= 0", f.Name(), h, got)
			}
		}
	}
	hw := NewHoltWinters(0.5, 0.3, 0.3, 4)
	observeAll(hw, declining)
	if got := hw.Forecast(60); got != 0 {
		t.Errorf("Holt-Winters extrapolating a falling trend: Forecast(60) = %v, want clamp to 0", got)
	}
	n := NewNaive()
	n.Observe(-5)
	if got := n.Forecast(1); got != 0 {
		t.Errorf("Naive after a negative observation = %v, want 0", got)
	}
}

func TestNonFiniteObservationsAreImputed(t *testing.T) {
	n := NewNaive()
	n.Observe(12)
	n.Observe(math.NaN())
	if got := n.Forecast(1); got != 12 {
		t.Errorf("Naive after NaN = %v, want 12", got)
	}

	sn := NewSeasonalNaive(3)
	observeAll(sn, []float64{1, 2, 3, math.Inf(1)})
	// Index 3 is at seasonal position 0 and was imputed from index 0.
	if got := sn.Forecast(3); got != 1 {
		t.Errorf("SeasonalNaive imputed value = %v, want 1", got)
	}

	hw := NewHoltWinters(0.5, 0.1, 0.3, 8)
	observeAll(hw, seasonalTrend(48, 8))
	hw.Observe(math.NaN())
	for h := 1; h <= 16; h++ {
		if got := hw.Forecast(h); !finite(got) {
			t.Fatalf("Holt-Winters Forecast(%d) after NaN = %v, want finite", h, got)
		}
	}
	// Imputing the model's own forecast keeps it on course.
	if got, want := hw.Forecast(1), trueValue(49, 8); math.Abs(got-want) > 0.05*want {
		t.Errorf("Holt-Winters forecast after an imputed NaN = %v, want within 5%% of %v", got, want)
	}
}

func TestHorizonBelowOneMeansOneStep(t *testing.T) {
	for _, f := range []Forecaster{NewHoltWinters(0.5, 0.1, 0.3, 8), NewSeasonalNaive(8)} {
		observeAll(f, seasonalTrend(40, 8))
		if f.Forecast(0) != f.Forecast(1) || f.Forecast(-3) != f.Forecast(1) {
			t.Errorf("%s: Forecast(h<1) differs from Forecast(1)", f.Name())
		}
	}
}

// A huge horizon must neither overflow the seasonal index (n-1+h wraps
// negative near math.MaxInt) nor land on the wrong seasonal position.
func TestHugeHorizon(t *testing.T) {
	pattern := []float64{10, 20, 30, 40}
	season := len(pattern)
	series := make([]float64, 3*season)
	for i := range series {
		series[i] = pattern[i%season]
	}
	horizons := []int{math.MaxInt, math.MaxInt - 1, math.MaxInt/2 + 3}
	// With no smoothing and a flat trend Holt-Winters forecasts exactly
	// level + seasonal, which is the pattern, like SeasonalNaive.
	for _, f := range []Forecaster{NewSeasonalNaive(season), NewHoltWinters(0, 0, 0, season)} {
		observeAll(f, series)
		for _, h := range horizons {
			if got, want := f.Forecast(h), pattern[bigPos(len(series), h, season)]; got != want {
				t.Errorf("%s: Forecast(%d) = %v, want %v", f.Name(), h, got, want)
			}
		}
	}
	for _, f := range []Forecaster{NewNaive(), NewSeasonalNaive(8), NewHoltWinters(0.5, 0.1, 0.3, 8)} {
		observeAll(f, seasonalTrend(40, 8))
		for _, h := range horizons {
			if got := f.Forecast(h); !finite(got) || got < 0 {
				t.Errorf("%s: Forecast(%d) = %v, want finite and >= 0", f.Name(), h, got)
			}
		}
	}
}

// bigPos is (n-1+h) mod season in arbitrary precision: an overflow-free
// reference that does not share code with targetPos.
func bigPos(n, h, season int) int {
	idx := new(big.Int).Add(big.NewInt(int64(n-1)), big.NewInt(int64(h)))
	return int(idx.Mod(idx, big.NewInt(int64(season))).Int64())
}
