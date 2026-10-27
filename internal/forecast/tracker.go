package forecast

import "math"

// MaxTrustedAPE is the largest mean absolute percentage error (as a
// fraction) a Tracker still trusts. For a non-negative forecast of a
// positive actual, an APE above 1.0 means the forecast was more than twice
// the actual value; an under-forecast never exceeds 1.0.
const MaxTrustedAPE = 1.0

// Tracker keeps the absolute percentage errors (APEs, as fractions) of the
// most recent one-step-ahead forecasts of a live forecaster and decides
// whether the controller may act on its forecasts. Use it as: forecast
// p = Forecast(1) in one cycle; in the next cycle, Record(p, actual) and
// only then Observe(actual).
//
// APE is asymmetric: an under-forecast costs at most 1, an over-forecast is
// unbounded. That matches how the controller uses forecasts (demand =
// max(reactive, forecast)): under-forecasting just falls back to the
// reactive estimate, while over-forecasting wastes replicas and is what
// trust gating must catch.
type Tracker struct {
	window int
	apes   []float64
	next   int
}

// NewTracker returns a Tracker over the last window scored forecasts. A
// window below 1 is treated as 1.
func NewTracker(window int) *Tracker {
	window = max(window, 1)
	return &Tracker{window: window, apes: make([]float64, 0, window)}
}

// Record scores one forecast against the actual value it predicted. Actuals
// that are non-finite or below Epsilon in magnitude are skipped (no relative
// error is defined for them). A non-finite prediction counts as an infinite
// error, so a broken forecaster is never trusted while it is in the window.
func (t *Tracker) Record(predicted, actual float64) {
	if !finite(actual) || math.Abs(actual) < Epsilon {
		return
	}
	ape := math.Inf(1)
	if finite(predicted) {
		ape = math.Abs(predicted-actual) / math.Abs(actual)
	}
	if len(t.apes) < t.window {
		t.apes = append(t.apes, ape)
	} else {
		t.apes[t.next] = ape
	}
	t.next = (t.next + 1) % t.window
}

// Samples returns how many scored forecasts the window holds (at most the
// window size).
func (t *Tracker) Samples() int { return len(t.apes) }

// MeanAPE returns the mean APE (a fraction) over the window, or NaN when
// nothing has been recorded.
func (t *Tracker) MeanAPE() float64 {
	if len(t.apes) == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, a := range t.apes {
		sum += a
	}
	return sum / float64(len(t.apes))
}

// Trusted reports whether the window is full and its mean APE is at most
// MaxTrustedAPE. It is false until the window has filled, and false while
// forecasts over-predict by more than 2x on average (mean APE > 1). The
// gate is one-sided: an under-forecast has an APE of at most 1 (exactly 1
// for a forecast of 0), so a window of under-forecasts alone is trusted
// however far below the actuals they are. That is harmless because the
// controller acts on max(reactive, forecast).
func (t *Tracker) Trusted() bool {
	return len(t.apes) == t.window && t.MeanAPE() <= MaxTrustedAPE
}
