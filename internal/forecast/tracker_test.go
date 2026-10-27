package forecast

import (
	"math"
	"testing"
)

func TestTrackerTrust(t *testing.T) {
	type rec struct{ predicted, actual float64 }
	repeat := func(r rec, n int) []rec {
		out := make([]rec, n)
		for i := range out {
			out[i] = r
		}
		return out
	}
	cases := []struct {
		name    string
		window  int
		records []rec
		trusted bool
		samples int
		meanAPE float64
	}{
		{"empty", 3, nil, false, 0, math.NaN()},
		{"window not yet full", 3, repeat(rec{10, 10}, 2), false, 2, 0},
		{"accurate and full", 3, repeat(rec{10, 10}, 3), true, 3, 0},
		{"off by more than 2x", 3, repeat(rec{25, 10}, 3), false, 3, 1.5},
		{"exactly 2x is the boundary", 3, repeat(rec{20, 10}, 3), true, 3, 1},
		{"under-forecast APE is at most 1", 3, repeat(rec{0, 10}, 3), true, 3, 1},
		{"one-sided gate: 10x under-forecast is still trusted", 3, repeat(rec{1, 10}, 3), true, 3, 0.9},
		{"mean decides, not the worst point", 4,
			[]rec{{30, 10}, {10, 10}, {10, 10}, {11, 10}}, true, 4, (2 + 0 + 0 + 0.1) / 4},
		{"one bad point among good", 2, []rec{{50, 10}, {10, 10}}, false, 2, 2},
		{"recovers once bad points leave the window", 3,
			append(repeat(rec{50, 10}, 3), repeat(rec{10, 10}, 3)...), true, 3, 0},
		{"zero actuals are skipped", 3,
			[]rec{{5, 0}, {10, 10}, {7, 1e-9}, {10, 10}, {10, 10}}, true, 3, 0},
		{"non-finite actuals are skipped", 2,
			[]rec{{10, math.NaN()}, {10, math.Inf(1)}, {10, 10}}, false, 1, 0},
		{"non-finite prediction is never trusted", 3,
			[]rec{{math.NaN(), 10}, {10, 10}, {10, 10}}, false, 3, math.Inf(1)},
		{"window below 1 means 1", 0, []rec{{12, 10}}, true, 1, 0.2},
	}
	for _, c := range cases {
		tr := NewTracker(c.window)
		for _, r := range c.records {
			tr.Record(r.predicted, r.actual)
		}
		if got := tr.Trusted(); got != c.trusted {
			t.Errorf("%s: Trusted() = %v, want %v", c.name, got, c.trusted)
		}
		if got := tr.Samples(); got != c.samples {
			t.Errorf("%s: Samples() = %d, want %d", c.name, got, c.samples)
		}
		got := tr.MeanAPE()
		bothNaN := math.IsNaN(got) && math.IsNaN(c.meanAPE)
		if !bothNaN && got != c.meanAPE && !near(got, c.meanAPE) {
			t.Errorf("%s: MeanAPE() = %v, want %v", c.name, got, c.meanAPE)
		}
	}
}

// A live loop: the tracker trusts Holt-Winters on its own periodic series
// and stops trusting it when demand collapses to a tenth of the pattern.
func TestTrackerGatesLiveForecaster(t *testing.T) {
	const season = 8
	hw := NewHoltWinters(0.5, 0.1, 0.3, season)
	tr := NewTracker(season)
	step := func(actual float64) {
		if hw.Ready() {
			tr.Record(hw.Forecast(1), actual)
		}
		hw.Observe(actual)
	}
	for _, v := range seasonalTrend(6*season, season) {
		step(v)
	}
	if !tr.Trusted() {
		t.Fatalf("not trusted on a clean periodic series (mean APE %v)", tr.MeanAPE())
	}
	for i := 0; i < 3; i++ {
		step(10)
	}
	if tr.Trusted() {
		t.Errorf("still trusted after demand collapsed (mean APE %v)", tr.MeanAPE())
	}
}
