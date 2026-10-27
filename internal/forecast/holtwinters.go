package forecast

// HoltWinters is the additive Holt-Winters (triple exponential smoothing)
// forecaster: a level, a linear trend and one additive seasonal offset per
// position in the season.
//
// Initialisation (classic simple method, needs two full seasons): with s1
// and s2 the first and second season of observations and m the season
// length,
//
//	level      = mean(s1)
//	trend      = (mean(s2) - mean(s1)) / m
//	seasonal_i = s1[i] - level
//
// This state describes the end of season 1; the season-2 observations are
// then passed through the update below, so when Ready first reports true
// every observation has been absorbed exactly once.
//
// Update for an observation y at seasonal position p (Winters' form):
//
//	level' = alpha*(y - seasonal_p) + (1-alpha)*(level + trend)
//	trend' = beta*(level' - level)  + (1-beta)*trend
//	seasonal_p' = gamma*(y - level') + (1-gamma)*seasonal_p
//
// Forecast h steps ahead: level + h*trend + seasonal at the target's
// position, clamped at zero because demand is never negative. Before the
// model is ready, Forecast falls back to the last observed value (Naive).
type HoltWinters struct {
	alpha, beta, gamma float64
	season             int

	// warm buffers the first two seasons until the model is initialised.
	warm         []float64
	level, trend float64
	// seasonal is indexed by observation index modulo season; it is nil
	// until the model is initialised.
	seasonal []float64
	n        int
	last     float64
}

// NewHoltWinters returns an additive Holt-Winters forecaster with smoothing
// factors alpha (level), beta (trend) and gamma (seasonal) and a season of
// the given length in observations. Smoothing factors are clamped to [0, 1]
// (NaN becomes 0) and a season below 1 is treated as 1, so a misconfigured
// forecaster still behaves predictably; config.Validate rejects such values
// before they get here.
func NewHoltWinters(alpha, beta, gamma float64, season int) *HoltWinters {
	season = max(season, 1)
	return &HoltWinters{
		alpha:  clamp01(alpha),
		beta:   clamp01(beta),
		gamma:  clamp01(gamma),
		season: season,
		warm:   make([]float64, 0, 2*season),
	}
}

// Observe appends v and updates the model.
func (f *HoltWinters) Observe(v float64) {
	if !finite(v) {
		v = f.Forecast(1)
	}
	f.last = v
	if f.Ready() {
		f.update(v, f.n%f.season)
		f.n++
		return
	}
	f.warm = append(f.warm, v)
	f.n++
	if len(f.warm) == 2*f.season {
		f.initialise()
	}
}

// Forecast predicts the value h steps after the last observation.
func (f *HoltWinters) Forecast(h int) float64 {
	if !f.Ready() {
		return nonNegative(f.last)
	}
	h = steps(h)
	return nonNegative(f.level + float64(h)*f.trend + f.seasonal[targetPos(f.n, h, f.season)])
}

// Ready reports whether two full seasons have been observed.
func (f *HoltWinters) Ready() bool { return f.seasonal != nil }

// Name returns "holt-winters".
func (f *HoltWinters) Name() string { return "holt-winters" }

func (f *HoltWinters) initialise() {
	m := f.season
	mean1, mean2 := mean(f.warm[:m]), mean(f.warm[m:])
	f.level = mean1
	f.trend = (mean2 - mean1) / float64(m)
	f.seasonal = make([]float64, m)
	for i, v := range f.warm[:m] {
		f.seasonal[i] = v - mean1
	}
	// Observation m+i sits at seasonal position i.
	for i, v := range f.warm[m:] {
		f.update(v, i)
	}
	f.warm = nil
}

func (f *HoltWinters) update(y float64, pos int) {
	prev := f.level
	f.level = f.alpha*(y-f.seasonal[pos]) + (1-f.alpha)*(f.level+f.trend)
	f.trend = f.beta*(f.level-prev) + (1-f.beta)*f.trend
	f.seasonal[pos] = f.gamma*(y-f.level) + (1-f.gamma)*f.seasonal[pos]
}

func mean(xs []float64) float64 {
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// clamp01 limits a smoothing factor to [0, 1]; NaN becomes 0.
func clamp01(x float64) float64 {
	switch {
	case !(x >= 0):
		return 0
	case x > 1:
		return 1
	}
	return x
}
