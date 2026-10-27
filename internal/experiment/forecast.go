package experiment

import (
	"fmt"
	"math"
	"os"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/forecast"
	"tenantscaler/internal/model"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

// ForecastPlan configures the demand-forecaster evaluation (R17).
type ForecastPlan struct {
	Config config.Config
	// Scenario must have a periodic tenant; its spike tenant, if any, is
	// the subject of the failure analysis.
	Scenario workload.Scenario
	// Runs seeds (BaseSeed, BaseSeed+1, ...) are evaluated (>= 1).
	Runs     int
	BaseSeed int64
	// ColdStart is used for the S7 vs S7F simulations (> 0).
	ColdStart time.Duration
	// SeriesDuration is the length of the traces the accuracy series come
	// from; zero means DefaultSeriesDuration. It is never shorter than the
	// scenario.
	SeriesDuration time.Duration
	// OutDir receives forecast.md.
	OutDir string
}

// Forecast evaluation constants.
const (
	// DefaultSeriesDuration gives the forecaster 20 seasons of the default
	// scenario's 120 s period.
	DefaultSeriesDuration = 2400 * time.Second
	// TrainFraction is the chronological training share of each series.
	TrainFraction = 0.6
	// SpikeShiftFraction places the spike tenant's spike at this fraction
	// of the series, inside the held-out part, for the failure analysis.
	SpikeShiftFraction = 0.75
	// trustWindow mirrors the controller's forecast trust window
	// (internal/control: 4 scored buckets).
	trustWindow = 4
)

// MethodAccuracy is one forecaster's held-out error over the seeds.
type MethodAccuracy struct {
	Method           string
	MAPE, SMAPE, MAE eval.Stat
}

// SpikeProbe is how one forecaster sees the unannounced spike: its
// one-step forecast divided by the actual value at the spike's first
// bucket (Onset) and at the first bucket after it (After).
type SpikeProbe struct {
	Method       string
	Onset, After eval.Stat
}

// ControlEffect is one setup's outcome for the periodic and spike tenants.
type ControlEffect struct {
	Scenario        string
	ScenarioSeconds float64
	Setup           string
	// PeriodicP99TTFT (seconds) and PeriodicViolationPct (non-shed requests
	// missing the SLO, whole run) describe the periodic tenant.
	PeriodicP99TTFT, PeriodicViolationPct eval.Stat
	ReplicaMinutes                        eval.Stat
	// ForecastRaisedPeriodic / Spike count control cycles in which the
	// forecast raised that tenant's demand above the reactive estimate.
	ForecastRaisedPeriodic, ForecastRaisedSpike eval.Stat
}

// ForecastReport is the outcome of Forecast.
type ForecastReport struct {
	Periodic, Spike             string
	BucketSeconds               float64
	Season                      int
	SeriesBuckets, TrainBuckets int
	Seeds                       []int64
	PeriodicAccuracy            []MethodAccuracy
	// Spike* are empty when the scenario has no spike tenant.
	SpikeStartSeconds    float64
	SpikeAccuracy        []MethodAccuracy
	SpikeProbes          []SpikeProbe
	TrustedAfterSpikePct eval.Stat
	Control              []ControlEffect
	Spec                 config.ForecastSpec
}

// method builds a fresh forecaster.
type method func() forecast.Forecaster

// Forecast evaluates the forecaster and writes OutDir/forecast.md.
func Forecast(p ForecastPlan) (ForecastReport, error) {
	if err := p.validate(); err != nil {
		return ForecastReport{}, err
	}
	spec := sim.ForecastSpecFor(p.Config.Controller, p.Scenario)
	rep := ForecastReport{BucketSeconds: spec.BucketSeconds, Season: spec.SeasonBuckets, Spec: spec}
	for _, t := range p.Scenario.Tenants {
		if t.Kind == workload.KindPeriodic && rep.Periodic == "" {
			rep.Periodic = t.Tenant
		}
	}
	rep.Spike = sim.ScenarioRoles(p.Scenario).Noisy
	bucket := time.Duration(spec.BucketSeconds * float64(time.Second))
	long := p.Scenario
	long.Name, long.Duration = p.Scenario.Name+"-long", max(p.SeriesDuration, p.Scenario.Duration)
	rep.SeriesBuckets = int(long.Duration / bucket)
	rep.TrainBuckets = int(TrainFraction * float64(rep.SeriesBuckets))
	if rep.TrainBuckets < 2*rep.Season || rep.SeriesBuckets-rep.TrainBuckets < 1 {
		return rep, fmt.Errorf("series of %d buckets is too short: Holt-Winters needs 2 seasons (%d buckets) of training data", rep.SeriesBuckets, 2*rep.Season)
	}
	methods := []method{
		func() forecast.Forecaster { return forecast.NewNaive() },
		func() forecast.Forecaster { return forecast.NewSeasonalNaive(spec.SeasonBuckets) },
		func() forecast.Forecaster {
			return forecast.NewHoltWinters(spec.Alpha, spec.Beta, spec.Gamma, spec.SeasonBuckets)
		},
	}
	shifted, err := shiftSpike(long, rep.Spike, bucket)
	if err != nil {
		return rep, err
	}
	periodic := map[int][]forecast.Metrics{}
	spike := map[int][]forecast.Metrics{}
	onset, after := map[int][]float64{}, map[int][]float64{}
	var trusted []float64
	for r := range p.Runs {
		seed := p.BaseSeed + int64(r)
		rep.Seeds = append(rep.Seeds, seed)
		ps := rateSeries(workload.Generate(long, seed), rep.Periodic, bucket, rep.SeriesBuckets)
		for i, m := range methods {
			periodic[i] = append(periodic[i], forecast.Evaluate(ps, rep.TrainBuckets, m()))
		}
		if rep.Spike == "" {
			continue
		}
		start, end, _ := shifted.SpikeWindow(rep.Spike)
		rep.SpikeStartSeconds = start.Seconds()
		k0, k1 := int(start/bucket), int(end/bucket)
		ss := rateSeries(workload.Generate(shifted, seed), rep.Spike, bucket, rep.SeriesBuckets)
		for i, m := range methods {
			spike[i] = append(spike[i], forecast.Evaluate(ss, rep.TrainBuckets, m()))
			onset[i] = append(onset[i], oneStepRatio(ss, k0, m()))
			after[i] = append(after[i], oneStepRatio(ss, k1, m()))
		}
		trusted = append(trusted, trustedPct(ss, k1, k1+spec.SeasonBuckets, methods[2]()))
	}
	for i, m := range methods {
		name := m().Name()
		rep.PeriodicAccuracy = append(rep.PeriodicAccuracy, accuracy(name, periodic[i]))
		if rep.Spike != "" {
			rep.SpikeAccuracy = append(rep.SpikeAccuracy, accuracy(name, spike[i]))
			rep.SpikeProbes = append(rep.SpikeProbes, SpikeProbe{Method: name, Onset: eval.Summarize(onset[i]), After: eval.Summarize(after[i])})
		}
	}
	rep.TrustedAfterSpikePct = eval.Summarize(trusted)
	scenarios := []workload.Scenario{p.Scenario}
	if long.Duration > p.Scenario.Duration {
		scenarios = append(scenarios, long)
	}
	for _, sc := range scenarios {
		for _, setup := range []string{sim.SetupFull, sim.SetupFullForecast} {
			ce, err := controlEffect(p, sc, setup, rep)
			if err != nil {
				return rep, err
			}
			rep.Control = append(rep.Control, ce)
		}
	}
	if err := os.MkdirAll(p.OutDir, 0o755); err != nil {
		return rep, fmt.Errorf("create %s: %w", p.OutDir, err)
	}
	md, err := forecastMarkdown(rep)
	if err != nil {
		return rep, err
	}
	return rep, writeFile(p.OutDir, "forecast.md", md)
}

func (p *ForecastPlan) validate() error {
	if p.SeriesDuration == 0 {
		p.SeriesDuration = DefaultSeriesDuration
	}
	switch {
	case p.Runs < 1:
		return fmt.Errorf("runs must be >= 1, got %d", p.Runs)
	case p.ColdStart <= 0:
		return fmt.Errorf("cold start must be > 0, got %v", p.ColdStart)
	case p.SeriesDuration < 0:
		return fmt.Errorf("series duration must be >= 0, got %v", p.SeriesDuration)
	case p.OutDir == "":
		return fmt.Errorf("output directory is required")
	}
	if _, err := sim.Resolve(sim.Options{Setup: sim.SetupFullForecast, Config: p.Config, Scenario: p.Scenario}); err != nil {
		return err
	}
	for _, t := range p.Scenario.Tenants {
		if t.Kind == workload.KindPeriodic {
			return nil
		}
	}
	return fmt.Errorf("scenario %q has no periodic tenant to forecast", p.Scenario.Name)
}

// shiftSpike moves the spike tenant's spike to SpikeShiftFraction of the
// series (aligned to a bucket), so its onset lies in the held-out part.
func shiftSpike(sc workload.Scenario, tenant string, bucket time.Duration) (workload.Scenario, error) {
	out := sc
	out.Tenants = append([]workload.TenantPattern(nil), sc.Tenants...)
	for i := range out.Tenants {
		t := &out.Tenants[i]
		if t.Tenant != tenant {
			continue
		}
		start := (time.Duration(SpikeShiftFraction*float64(sc.Duration)) / bucket) * bucket
		if start+time.Duration(t.SpikeDurationSeconds*float64(time.Second)) > sc.Duration {
			return out, fmt.Errorf("spike of %gs does not fit after %v in a %v series", t.SpikeDurationSeconds, start, sc.Duration)
		}
		t.SpikeStartSeconds = start.Seconds()
	}
	return out, nil
}

// rateSeries buckets a tenant's arrivals as weighted tokens per second
// (input + 2 x max output): the demand signal the controller forecasts.
// A trailing partial bucket is dropped.
func rateSeries(specs []workload.Spec, tenant string, bucket time.Duration, buckets int) []float64 {
	s := make([]float64, buckets)
	for _, sp := range specs {
		if i := int(sp.Offset / bucket); sp.Tenant == tenant && i < buckets {
			s[i] += model.WeightedTokens(float64(sp.InputTokens), float64(sp.OutputTokens))
		}
	}
	for i := range s {
		s[i] /= bucket.Seconds()
	}
	return s
}

// oneStepRatio is the one-step forecast of series[k] from series[:k],
// divided by series[k] (NaN when series[k] is 0 or k is out of range).
func oneStepRatio(series []float64, k int, f forecast.Forecaster) float64 {
	if k <= 0 || k >= len(series) || series[k] == 0 {
		return math.NaN()
	}
	for _, v := range series[:k] {
		f.Observe(v)
	}
	return f.Forecast(1) / series[k]
}

// trustedPct replays f's one-step forecasts through a Tracker, as the
// controller does, and returns the share of buckets in [from, to) after
// which the forecast was trusted.
func trustedPct(series []float64, from, to int, f forecast.Forecaster) float64 {
	tr := forecast.NewTracker(trustWindow)
	n, ok := 0, 0
	for t, v := range series {
		if f.Ready() {
			tr.Record(f.Forecast(1), v)
		}
		f.Observe(v)
		if t >= from && t < to {
			n++
			if tr.Trusted() {
				ok++
			}
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return 100 * float64(ok) / float64(n)
}

func accuracy(name string, ms []forecast.Metrics) MethodAccuracy {
	var mape, smape, mae []float64
	for _, m := range ms {
		mape, smape, mae = append(mape, m.MAPE), append(smape, m.SMAPE), append(mae, m.MAE)
	}
	return MethodAccuracy{Method: name, MAPE: eval.Summarize(mape), SMAPE: eval.Summarize(smape), MAE: eval.Summarize(mae)}
}

// controlEffect simulates one setup over the plan's seeds.
func controlEffect(p ForecastPlan, sc workload.Scenario, setup string, rep ForecastReport) (ControlEffect, error) {
	var p99, viol, minutes, raisedP, raisedS []float64
	for r := range p.Runs {
		o := sim.Options{Setup: setup, Config: p.Config, Scenario: sc, Seed: p.BaseSeed + int64(r), ColdStart: p.ColdStart}
		res, err := sim.Simulate(o)
		if err != nil {
			return ControlEffect{}, fmt.Errorf("simulate %v: %w", o, err)
		}
		m := eval.Compute(res.Data)
		p99 = append(p99, m.P99TTFT[rep.Periodic])
		viol = append(viol, tenantViolationPct(res.Data, rep.Periodic))
		minutes = append(minutes, m.ReplicaMinutes)
		raisedP = append(raisedP, float64(res.Diagnostics.ForecastRaisedCycles[rep.Periodic]))
		raisedS = append(raisedS, float64(res.Diagnostics.ForecastRaisedCycles[rep.Spike]))
	}
	return ControlEffect{
		Scenario: sc.Name, ScenarioSeconds: sc.Duration.Seconds(), Setup: setup,
		PeriodicP99TTFT: eval.Summarize(p99), PeriodicViolationPct: eval.Summarize(viol),
		ReplicaMinutes: eval.Summarize(minutes), ForecastRaisedPeriodic: eval.Summarize(raisedP),
		ForecastRaisedSpike: eval.Summarize(raisedS),
	}, nil
}

// tenantViolationPct is the share of a tenant's non-shed requests that
// missed its SLO (failed ones count as misses) over the whole run.
func tenantViolationPct(d eval.RunData, tenant string) float64 {
	slo, ok := d.SLOs[tenant]
	served, miss := 0, 0
	for _, r := range d.Records {
		if r.Tenant != tenant || r.Shed || !ok {
			continue
		}
		served++
		if !r.MetSLO(slo) {
			miss++
		}
	}
	if served == 0 {
		return math.NaN()
	}
	return 100 * float64(miss) / float64(served)
}
