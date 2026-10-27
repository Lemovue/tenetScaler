package forecast

import (
	"math"
	"testing"
)

// With all smoothing factors at 0 the state never adapts, so forecasts
// expose the initialisation exactly: level = mean(s1) = 25,
// trend = (mean(s2)-mean(s1))/m = 1, seasonal = s1 - 25 = [-15 -5 5 15],
// then the level advances by the trend over the 4 season-2 updates to 29.
func TestHoltWintersInitialisation(t *testing.T) {
	hw := NewHoltWinters(0, 0, 0, 4)
	observeAll(hw, []float64{10, 20, 30, 40, 14, 24, 34, 44})
	if !hw.Ready() {
		t.Fatal("not ready after two seasons")
	}
	want := []float64{29 + 1 - 15, 29 + 2 - 5, 29 + 3 + 5, 29 + 4 + 15, 29 + 5 - 15}
	for i, w := range want {
		if got := hw.Forecast(i + 1); math.Abs(got-w) > 1e-9 {
			t.Errorf("Forecast(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestHoltWintersRecoversSeasonalTrendAndBeatsBaselines(t *testing.T) {
	const season = 8
	series := seasonalTrend(12*season, season)
	train := 6 * season
	// Smoothing factors are the config defaults (config.Default()).
	newHW := func() Forecaster { return NewHoltWinters(0.5, 0.1, 0.3, season) }

	hw := Evaluate(series, train, newHW())
	naive := Evaluate(series, train, NewNaive())
	snaive := Evaluate(series, train, NewSeasonalNaive(season))
	t.Logf("held-out MAPE: holt-winters %.3f%%, naive %.3f%%, seasonal-naive %.3f%%", hw.MAPE, naive.MAPE, snaive.MAPE)

	if hw.N != len(series)-train {
		t.Fatalf("scored %d points, want %d", hw.N, len(series)-train)
	}
	if hw.MAPE >= 1 {
		t.Errorf("Holt-Winters MAPE = %.3f%% on a clean seasonal+trend series, want < 1%%", hw.MAPE)
	}
	if hw.MAPE*5 >= naive.MAPE {
		t.Errorf("Holt-Winters MAPE %.3f%% does not beat Naive %.3f%% by 5x", hw.MAPE, naive.MAPE)
	}
	if hw.MAPE >= snaive.MAPE || hw.MAE >= snaive.MAE {
		t.Errorf("Holt-Winters (MAPE %.3f%%, MAE %.3f) does not beat SeasonalNaive (MAPE %.3f%%, MAE %.3f) on a trending series",
			hw.MAPE, hw.MAE, snaive.MAPE, snaive.MAE)
	}

	// Multi-step: the fitted model extrapolates two full seasons.
	f := newHW()
	observeAll(f, series)
	for h := 1; h <= 2*season; h++ {
		want := trueValue(len(series)-1+h, season)
		if got := f.Forecast(h); math.Abs(got-want) > 0.02*want {
			t.Errorf("Forecast(%d) = %.2f, want within 2%% of %.2f", h, got, want)
		}
	}
}

func TestHoltWintersSanitisesParameters(t *testing.T) {
	hw := NewHoltWinters(-1, 2, math.NaN(), 0)
	if hw.alpha != 0 || hw.beta != 1 || hw.gamma != 0 || hw.season != 1 {
		t.Fatalf("parameters = (%v, %v, %v, %d), want (0, 1, 0, 1)", hw.alpha, hw.beta, hw.gamma, hw.season)
	}
	observeAll(hw, []float64{5, 7, 9})
	if !hw.Ready() {
		t.Fatal("season 1 should be ready after 2 observations")
	}
	if got := hw.Forecast(3); !finite(got) || got < 0 {
		t.Errorf("Forecast(3) = %v, want finite and >= 0", got)
	}
}
