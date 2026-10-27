// Package workload generates the synthetic multi-tenant request traces that
// TenantScaler is evaluated on: per-tenant rate patterns (steady, periodic
// bursts, a sudden spike), Gamma-distributed inter-arrival times, priority
// mixes and log-normal (or trace-sampled) token lengths. Generation is
// deterministic: the same scenario and seed always yield the same trace.
//
// No public LLM trace carries tenant IDs, so the tenants here are synthetic.
// Token lengths can instead be drawn from the BurstGPT or Azure LLM inference
// traces when the user supplies those CSV files (see LoadBurstGPT, LoadAzure).
package workload

import (
	"fmt"
	"math"
	"time"

	"tenantscaler/internal/model"
)

// Tenant pattern kinds.
const (
	// KindSteady keeps BaseRPS for the whole run.
	KindSteady = "steady"
	// KindPeriodic switches to PeakRPS for BurstSeconds every PeriodSeconds.
	KindPeriodic = "periodic"
	// KindSpike switches to PeakRPS once, inside the spike window.
	KindSpike = "spike"
	// KindSeries follows Series: Series[i] requests/s during
	// [i, i+1) x SeriesBucketSeconds, and 0 after the series ends (a
	// rescaled real trace, for the forecaster's evaluation).
	KindSeries = "series"
)

// Spec is one generated request.
type Spec struct {
	// Offset is the arrival time relative to the start of the run.
	Offset       time.Duration
	Tenant       string
	Priority     model.Priority
	InputTokens  int
	OutputTokens int
}

// LengthDist is a log-normal token-length distribution clamped to
// [Min, Max]: length = round(Median * exp(Sigma * Z)), Z ~ N(0, 1).
type LengthDist struct {
	Median, Sigma float64
	Min, Max      int
}

// PriorityMix holds the relative weights of the three priority classes.
// The weights need not sum to one.
type PriorityMix struct {
	Critical, Standard, Sheddable float64
}

// TenantPattern describes one tenant's offered load.
type TenantPattern struct {
	Tenant string
	// Kind is KindSteady, KindPeriodic, KindSpike or KindSeries.
	Kind string
	// BaseRPS is the request rate outside bursts and spikes; PeakRPS the
	// rate inside them.
	BaseRPS, PeakRPS float64
	// Periodic: a burst of PeakRPS lasting BurstSeconds starts at
	// PhaseSeconds + k*PeriodSeconds for every integer k.
	PeriodSeconds, BurstSeconds, PhaseSeconds float64
	// Spike: PeakRPS inside [SpikeStartSeconds, SpikeStartSeconds+SpikeDurationSeconds).
	SpikeStartSeconds, SpikeDurationSeconds float64
	// Series: the request rate per bucket of SeriesBucketSeconds.
	Series              []float64
	SeriesBucketSeconds float64
	// CV is the coefficient of variation of the Gamma inter-arrival times
	// (1 = Poisson, above 1 = burstier than Poisson).
	CV float64
	// Mix is the priority-class mix of this tenant's requests.
	Mix PriorityMix
	// In and Out are the input and output length distributions, used when
	// the scenario has no Samples.
	In, Out LengthDist
}

// LengthSample is one (input, output) token-length pair taken from a trace.
// Out may be 0 (an Azure trace row that generated nothing); Generate
// replays such a sample as a one-token request.
type LengthSample struct{ In, Out int }

// Scenario is a complete workload: the tenants' patterns over a run of the
// given duration. When Samples is non-empty, request lengths are drawn
// uniformly from it (keeping each In/Out pair together) instead of from the
// tenants' LengthDists.
type Scenario struct {
	Name     string
	Duration time.Duration
	Tenants  []TenantPattern
	Samples  []LengthSample
}

// ReclaimOwner is the tenant whose return ReclaimScenario measures.
const ReclaimOwner = "A"

const reclaimReturnSeconds = 240

// ReclaimReturn is when ReclaimOwner's demand comes back in
// ReclaimScenario; use it as eval.RunData.OwnerReturn.
func ReclaimReturn() time.Duration { return seconds(reclaimReturnSeconds) }

// Shared length distributions: prompts with a median of 256 tokens and
// answers with a median of 128 tokens. A request then costs about 600
// weighted tokens (model.WeightedTokens) on average, so with the default
// controller settings (400 weighted tokens/s per replica, 20% headroom)
// 1 req/s needs 2 replicas, 2 req/s needs 4 and 5 req/s needs 9.
var (
	defaultIn  = LengthDist{Median: 256, Sigma: 0.6, Min: 16, Max: 2048}
	defaultOut = LengthDist{Median: 128, Sigma: 0.5, Min: 8, Max: 512}
)

// Priority mixes: interactive tenants send mostly standard traffic with
// some critical requests; the spiking tenant's extra load is batch work and
// mostly sheddable.
var (
	interactiveMix = PriorityMix{Critical: 0.2, Standard: 0.8}
	periodicMix    = PriorityMix{Critical: 0.15, Standard: 0.8, Sheddable: 0.05}
	batchMix       = PriorityMix{Critical: 0.05, Standard: 0.25, Sheddable: 0.7}
)

// DefaultScenario is the problem statement's trace for a 10-replica pool
// with guarantees A=3, B=3, C=3 over a 600 s run:
//
//   - A is steady at 1 req/s (about 2 replicas of demand);
//   - B runs at 1 req/s with 30 s bursts of 2 req/s every 120 s starting at
//     t=90 s (the 120 s period matches the default forecaster season);
//   - C runs at 1 req/s and spikes to 5 req/s for 180 s from t=180 s,
//     roughly three times its 3-replica share, mostly with sheddable work.
//
// All arrivals are Gamma with CV > 1 (burstier than Poisson): A 1.2,
// B 1.5, C 2.0. The spike window is DefaultScenario().SpikeWindow("C").
func DefaultScenario() Scenario {
	return Scenario{
		Name:     "default",
		Duration: 600 * time.Second,
		Tenants: []TenantPattern{
			{Tenant: "A", Kind: KindSteady, BaseRPS: 1, PeakRPS: 1, CV: 1.2,
				Mix: interactiveMix, In: defaultIn, Out: defaultOut},
			{Tenant: "B", Kind: KindPeriodic, BaseRPS: 1, PeakRPS: 2,
				PeriodSeconds: 120, BurstSeconds: 30, PhaseSeconds: 90, CV: 1.5,
				Mix: periodicMix, In: defaultIn, Out: defaultOut},
			{Tenant: "C", Kind: KindSpike, BaseRPS: 1, PeakRPS: 5,
				SpikeStartSeconds: 180, SpikeDurationSeconds: 180, CV: 2,
				Mix: batchMix, In: defaultIn, Out: defaultOut},
		},
	}
}

// ReclaimScenario measures reclaim latency over a 480 s run: A is nearly
// idle (0.05 req/s) until ReclaimReturn (t=240 s) and then sends 1.5 req/s,
// enough to need its full 3-replica guarantee; B is steady at 2 req/s; C
// spikes to 5 req/s from t=60 s to the end and borrows A's idle share.
func ReclaimScenario() Scenario {
	const duration = 480
	return Scenario{
		Name:     "reclaim",
		Duration: seconds(duration),
		Tenants: []TenantPattern{
			{Tenant: ReclaimOwner, Kind: KindSpike, BaseRPS: 0.05, PeakRPS: 1.5,
				SpikeStartSeconds: reclaimReturnSeconds, SpikeDurationSeconds: duration - reclaimReturnSeconds,
				CV: 1.2, Mix: interactiveMix, In: defaultIn, Out: defaultOut},
			{Tenant: "B", Kind: KindSteady, BaseRPS: 2, PeakRPS: 2, CV: 1.5,
				Mix: periodicMix, In: defaultIn, Out: defaultOut},
			{Tenant: "C", Kind: KindSpike, BaseRPS: 1, PeakRPS: 5,
				SpikeStartSeconds: 60, SpikeDurationSeconds: duration - 60, CV: 2,
				Mix: batchMix, In: defaultIn, Out: defaultOut},
		},
	}
}

// SpikeWindow returns the spike window [start, end) of the named spike
// tenant, with end clamped to the scenario duration. ok is false when the
// tenant does not exist or is not a spike tenant.
func (s Scenario) SpikeWindow(tenant string) (start, end time.Duration, ok bool) {
	for _, p := range s.Tenants {
		if p.Tenant != tenant || p.Kind != KindSpike {
			continue
		}
		start = seconds(p.SpikeStartSeconds)
		end = min(seconds(p.SpikeStartSeconds+p.SpikeDurationSeconds), s.Duration)
		return start, end, true
	}
	return 0, 0, false
}

// RateAt returns the pattern's request rate (requests/s) at t seconds into
// the run. Negative or non-finite rates count as zero, and a periodic or
// spike pattern whose timing parameters are not finite and positive never
// leaves BaseRPS.
func RateAt(p TenantPattern, t float64) float64 {
	r := p.BaseRPS
	switch p.Kind {
	case KindPeriodic:
		if hasBursts(p) {
			m := math.Mod(t-p.PhaseSeconds, p.PeriodSeconds)
			if m < 0 {
				m += p.PeriodSeconds
			}
			if m < p.BurstSeconds {
				r = p.PeakRPS
			}
		}
	case KindSpike:
		if hasSpike(p) && t >= p.SpikeStartSeconds && t < p.SpikeStartSeconds+p.SpikeDurationSeconds {
			r = p.PeakRPS
		}
	case KindSeries:
		r = 0
		if i := seriesIndex(p, t); i >= 0 {
			r = p.Series[i]
		}
	}
	if !finite(r) || r < 0 {
		return 0
	}
	return r
}

// seriesIndex is the Series bucket holding t, or -1 outside the series.
func seriesIndex(p TenantPattern, t float64) int {
	if !finite(p.SeriesBucketSeconds) || p.SeriesBucketSeconds <= 0 || !finite(t) || t < 0 {
		return -1
	}
	i := int(t / p.SeriesBucketSeconds)
	if i >= len(p.Series) {
		return -1
	}
	return i
}

func hasBursts(p TenantPattern) bool {
	return finite(p.PeriodSeconds) && p.PeriodSeconds > 0 &&
		finite(p.BurstSeconds) && p.BurstSeconds > 0 && finite(p.PhaseSeconds)
}

func hasSpike(p TenantPattern) bool {
	return finite(p.SpikeStartSeconds) && finite(p.SpikeDurationSeconds) && p.SpikeDurationSeconds > 0
}

// Validate checks that a scenario is well formed. Generate accepts any
// scenario, but only a valid one produces the documented trace.
func (s Scenario) Validate() error {
	if s.Duration <= 0 {
		return fmt.Errorf("scenario %q: duration must be > 0, got %v", s.Name, s.Duration)
	}
	if len(s.Tenants) == 0 {
		return fmt.Errorf("scenario %q: at least one tenant is required", s.Name)
	}
	seen := map[string]bool{}
	for _, p := range s.Tenants {
		if p.Tenant == "" {
			return fmt.Errorf("scenario %q: a tenant has an empty name", s.Name)
		}
		if seen[p.Tenant] {
			return fmt.Errorf("scenario %q: duplicate tenant %q", s.Name, p.Tenant)
		}
		seen[p.Tenant] = true
		if err := p.validate(); err != nil {
			return fmt.Errorf("scenario %q: tenant %q: %w", s.Name, p.Tenant, err)
		}
	}
	for i, smp := range s.Samples {
		if smp.In < 0 || smp.Out < 0 {
			return fmt.Errorf("scenario %q: sample %d: want In >= 0 and Out >= 0, got In=%d Out=%d", s.Name, i, smp.In, smp.Out)
		}
	}
	return nil
}

func (p TenantPattern) validate() error {
	if !finite(p.BaseRPS) || !finite(p.PeakRPS) || p.BaseRPS < 0 || p.PeakRPS < 0 {
		return fmt.Errorf("baseRPS and peakRPS must be finite and >= 0")
	}
	switch p.Kind {
	case KindSteady:
	case KindPeriodic:
		if !finite(p.PeriodSeconds) || p.PeriodSeconds < 1 {
			return fmt.Errorf("periodSeconds must be >= 1, got %v", p.PeriodSeconds)
		}
		if !hasBursts(p) || p.BurstSeconds > p.PeriodSeconds {
			return fmt.Errorf("burstSeconds must be in (0, periodSeconds] and phaseSeconds finite")
		}
	case KindSpike:
		if !hasSpike(p) || p.SpikeStartSeconds < 0 {
			return fmt.Errorf("spikeStartSeconds must be >= 0 and spikeDurationSeconds > 0")
		}
	case KindSeries:
		if len(p.Series) == 0 || !finite(p.SeriesBucketSeconds) || p.SeriesBucketSeconds <= 0 {
			return fmt.Errorf("series needs at least one value and seriesBucketSeconds > 0")
		}
		for i, r := range p.Series {
			if !finite(r) || r < 0 {
				return fmt.Errorf("series[%d] must be finite and >= 0, got %v", i, r)
			}
		}
	default:
		return fmt.Errorf("kind must be %q, %q, %q or %q, got %q", KindSteady, KindPeriodic, KindSpike, KindSeries, p.Kind)
	}
	if !finite(p.CV) || p.CV <= 0 {
		return fmt.Errorf("cv must be finite and > 0, got %v", p.CV)
	}
	m := p.Mix
	for _, w := range []float64{m.Critical, m.Standard, m.Sheddable} {
		if !finite(w) || w < 0 {
			return fmt.Errorf("priority mix weights must be finite and >= 0")
		}
	}
	if m.Critical+m.Standard+m.Sheddable <= 0 {
		return fmt.Errorf("priority mix needs at least one positive weight")
	}
	if err := p.In.validate(); err != nil {
		return fmt.Errorf("input lengths: %w", err)
	}
	if err := p.Out.validate(); err != nil {
		return fmt.Errorf("output lengths: %w", err)
	}
	return nil
}

func (d LengthDist) validate() error {
	if !finite(d.Median) || d.Median <= 0 || !finite(d.Sigma) || d.Sigma < 0 {
		return fmt.Errorf("median must be > 0 and sigma >= 0, got median=%v sigma=%v", d.Median, d.Sigma)
	}
	if d.Min < 1 || d.Max < d.Min {
		return fmt.Errorf("want 1 <= min <= max, got min=%d max=%d", d.Min, d.Max)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func seconds(s float64) time.Duration { return time.Duration(math.Round(s * float64(time.Second))) }
